package cloud

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	ktest "k8s.io/client-go/testing"
)

func readyPool(t *testing.T, k *Kubernetes) *core.Pod {
	t.Helper()
	ctx := context.Background()
	k.WarmPoolSize = 1
	if err := k.MaintainPool(ctx); err != nil {
		t.Fatal(err)
	}
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: poolLabel + "=true"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pool: %+v %v", pods, err)
	}
	p := pods.Items[0].DeepCopy()
	p.Spec.NodeName = "worker"
	p.Status.Phase = core.PodRunning
	p.Status.PodIP = "10.0.0.2"
	p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	p, err = k.Client.Pods(k.Namespace).Update(ctx, p, meta.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func podCount(t *testing.T, k *Kubernetes, want int) {
	t.Helper()
	pods, err := k.Client.Pods(k.Namespace).List(context.Background(), meta.ListOptions{})
	if err != nil || len(pods.Items) != want {
		t.Fatalf("Pods = %+v, error = %v; want %d", pods, err, want)
	}
}

func TestWarmClaimTTLReplenishmentAndDestruction(t *testing.T) {
	k, _ := setup(t)
	ctx := context.Background()
	p := readyPool(t, k)
	items, err := k.List(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("idle Pods exposed: %+v %v", items, err)
	}
	if _, err := k.Get(ctx, p.Name); err == nil {
		t.Fatal("idle Pod exposed by name")
	}
	claimedAt := k.now().Add(5 * time.Minute)
	k.Now = func() time.Time { return claimedAt }
	k.PoolWake = make(chan struct{}, 1)
	w, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != testID || w.State != "ready" || !w.CreatedAt.Equal(claimedAt) || !w.ExpiresAt.Equal(claimedAt.Add(20*time.Minute)) {
		t.Fatalf("warm workspace: %+v", w)
	}
	podCount(t, k, 1)
	if _, err := k.Client.Pods(k.Namespace).Get(ctx, testID, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected cold Pod: %v", err)
	}
	address, err := k.RunnerAddress(ctx, testID)
	if err != nil || address != "http://10.0.0.2:8090" {
		t.Fatalf("routing: %s %v", address, err)
	}
	select {
	case <-k.PoolWake:
	default:
		t.Fatal("claim did not wake replenishment")
	}
	if err := k.MaintainPool(ctx); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 2)
	items, err = k.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != testID {
		t.Fatalf("inventory: %+v %v", items, err)
	}
	if err := k.Destroy(ctx, testID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Client.Pods(k.Namespace).Get(ctx, p.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("claimed Pod was recycled or retained: %v", err)
	}
	if _, err := k.Get(ctx, testID); !apierrors.IsNotFound(err) {
		t.Fatalf("claim survived destruction: %v", err)
	}
	podCount(t, k, 1)
}

func TestConcurrentCreateAcrossControllers(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate=%v", sameID), func(t *testing.T) {
			k, _ := setup(t)
			p := readyPool(t, k)
			const n = 8
			start := make(chan struct{})
			results := make(chan error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					controller := *k // Different controller instances, same Kubernetes API.
					id := testID
					if !sameID {
						id = fmt.Sprintf("sbx-%024x", i)
					}
					<-start
					w, err := controller.Create(context.Background(), CreateRequest{ID: id, TTLSeconds: 600})
					if err == nil && w.ID != id {
						err = fmt.Errorf("wrong ID: %s", w.ID)
					}
					results <- err
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			want := n
			if sameID {
				want = 1
			}
			podCount(t, k, want)
			items, err := k.List(context.Background())
			if err != nil || len(items) != want {
				t.Fatalf("inventory: %d %v", len(items), err)
			}
			warm := 0
			for _, w := range items {
				c, err := k.getClaim(context.Background(), w.ID)
				if err != nil {
					t.Fatal(err)
				}
				if c.Data["pod"] == p.Name {
					warm++
				}
			}
			if warm != 1 {
				t.Fatalf("warm Pod assigned to %d workspaces", warm)
			}
		})
	}
}

func TestWarmClaimRecoveryAndExpiry(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	p := readyPool(t, k)
	failed := false
	c.PrependReactor("update", "configmaps", func(a ktest.Action) (bool, runtime.Object, error) {
		cm := a.(ktest.UpdateAction).GetObject().(*core.ConfigMap)
		if cm.Data["phase"] == "active" && !failed {
			failed = true
			return true, nil, errors.New("API response unavailable during binding")
		}
		return false, nil, nil
	})
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60}); err == nil {
		t.Fatal("binding failure hidden")
	}
	fresh := *k
	if err := fresh.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := fresh.Get(ctx, testID)
	if err != nil || w.State != "ready" {
		t.Fatalf("recovery: %+v %v", w, err)
	}
	podCount(t, k, 1)
	// Neither a retry nor a process restart resets an active workspace's TTL.
	later := k.now().Add(30 * time.Second)
	fresh.Now = func() time.Time { return later }
	replayed, err := fresh.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60})
	if err != nil || !replayed.ExpiresAt.Equal(w.ExpiresAt) {
		t.Fatalf("retry reset TTL: %+v %v", replayed, err)
	}
	if _, err := fresh.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 120}); err == nil {
		t.Fatal("accepted changed settings")
	}
	later = later.Add(time.Minute)
	if _, err := fresh.RunnerAddress(ctx, testID); err == nil {
		t.Fatal("expired workspace routable")
	}
	if err := fresh.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().Pods(k.Namespace).Get(ctx, p.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired Pod remains: %v", err)
	}
	podCount(t, k, 0)
}

// Hooks run outside Fake.Invokes' mutex, allowing a genuine interleaving between
// the API's list/create operation and the following conditional write.
type hookedCore struct {
	typed.CoreV1Interface
	afterList    func()
	beforeCreate func()
}

func (c hookedCore) Pods(ns string) typed.PodInterface {
	return hookedPods{c.CoreV1Interface.Pods(ns), c.afterList, c.beforeCreate}
}

type hookedPods struct {
	typed.PodInterface
	afterList    func()
	beforeCreate func()
}

func (p hookedPods) List(ctx context.Context, opts meta.ListOptions) (*core.PodList, error) {
	items, err := p.PodInterface.List(ctx, opts)
	if err == nil && p.afterList != nil {
		p.afterList()
	}
	return items, err
}
func (p hookedPods) Create(ctx context.Context, pod *core.Pod, opts meta.CreateOptions) (*core.Pod, error) {
	if p.beforeCreate != nil {
		p.beforeCreate()
	}
	return p.PodInterface.Create(ctx, pod, opts)
}

func TestPoolCleanupCannotDeleteNewlyClaimedPod(t *testing.T) {
	k, _ := setup(t)
	readyPool(t, k)
	cleaner := *k
	cleaner.WarmPoolSize = 0
	cleaner.Client = hookedCore{CoreV1Interface: k.Client, afterList: func() {
		if _, err := k.Create(context.Background(), CreateRequest{ID: testID, TTLSeconds: 60}); err != nil {
			t.Fatal(err)
		}
	}}
	if err := cleaner.MaintainPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := k.Get(context.Background(), testID)
	if err != nil || w.State != "ready" {
		t.Fatalf("cleanup deleted claimed Pod: %+v %v", w, err)
	}
	podCount(t, k, 1)
}

func TestDestroyDuringAllocationCleansLatePod(t *testing.T) {
	k, _ := setup(t)
	ctx := context.Background()
	allocator := *k
	allocator.Client = hookedCore{CoreV1Interface: k.Client, beforeCreate: func() {
		if err := k.Destroy(ctx, testID); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := allocator.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60}); err == nil {
		t.Fatal("returned destroyed allocation")
	}
	podCount(t, k, 1) // Fake API has no garbage collector; exercise orphan cleanup.
	if _, err := k.Get(ctx, testID); err == nil {
		t.Fatal("late Pod became a visible workspace")
	}
	if err := k.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 0)
}

func TestMissingAssignedPodDoesNotResurrectWorkspace(t *testing.T) {
	k, _ := setup(t)
	p := readyPool(t, k)
	ctx := context.Background()
	req := CreateRequest{ID: testID, TTLSeconds: 60}
	if _, err := k.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := k.delete(ctx, p.Name, p.UID); err != nil {
		t.Fatal(err)
	}
	w, err := k.Create(ctx, req)
	if err != nil || w.State != "unavailable" {
		t.Fatalf("missing Pod: %+v %v", w, err)
	}
	podCount(t, k, 0)
}

func TestUnusableWarmPodsFallBackToCold(t *testing.T) {
	cases := map[string]func(*core.Pod){
		"old image": func(p *core.Pod) { p.Spec.Containers[0].Image = "old-image" },
		"not ready": func(p *core.Pod) { p.Status.Conditions = nil },
		"failed":    func(p *core.Pod) { p.Status.Phase = core.PodFailed },
		"bad IP":    func(p *core.Pod) { p.Status.PodIP = "invalid" },
		"expired":   func(p *core.Pod) { p.Annotations[expiresAnnotation] = "2000-01-01T00:00:00Z" },
		"retired":   func(p *core.Pod) { p.Labels[retiredLabel] = "true" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			k, _ := setup(t)
			p := readyPool(t, k)
			change(p)
			if _, err := k.Client.Pods(k.Namespace).Update(context.Background(), p, meta.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			w, err := k.Create(context.Background(), CreateRequest{ID: testID, TTLSeconds: 60})
			if err != nil || w.State != "pending" {
				t.Fatalf("fallback: %+v %v", w, err)
			}
			c, err := k.getClaim(context.Background(), testID)
			if err != nil || c.Data["pod"] != testID {
				t.Fatalf("wrong fallback target: %+v %v", c, err)
			}
		})
	}
}

func TestPoolQuotaFailureLeavesClaimUsable(t *testing.T) {
	k, c := setup(t)
	readyPool(t, k)
	ctx := context.Background()
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("create", "pods", func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "workspace", errors.New("quota exceeded"))
	})
	if err := k.MaintainPool(ctx); !apierrors.IsForbidden(err) {
		t.Fatalf("quota error lost: %v", err)
	}
	w, err := k.Get(ctx, testID)
	if err != nil || w.State != "ready" {
		t.Fatalf("replenishment broke claim: %+v %v", w, err)
	}
	id := "sbx-abcdefabcdefabcdefabcdef"
	if _, err := k.Create(ctx, CreateRequest{ID: id, TTLSeconds: 60}); !apierrors.IsForbidden(err) {
		t.Fatalf("cold quota error lost: %v", err)
	}
	later := k.now().Add(3 * time.Minute)
	k.Now = func() time.Time { return later }
	if err := k.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	claims, err := c.CoreV1().ConfigMaps(k.Namespace).List(ctx, meta.ListOptions{})
	if err != nil || len(claims.Items) != 0 {
		t.Fatalf("failed reservations leaked: %+v %v", claims, err)
	}
}

func TestPoolRotationAndDisable(t *testing.T) {
	k, _ := setup(t)
	original := readyPool(t, k)
	k.Image = "new-image"
	if err := k.MaintainPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 1)
	if _, err := k.Client.Pods(k.Namespace).Get(context.Background(), original.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old image kept: %v", err)
	}
	k.WarmPoolSize = 0
	if err := k.MaintainPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 0)
}

func TestExpiryRechecksTTLWhenAllocationBindsConcurrently(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	readyPool(t, k)
	failBinding := true
	c.PrependReactor("update", "configmaps", func(a ktest.Action) (bool, runtime.Object, error) {
		cm := a.(ktest.UpdateAction).GetObject().(*core.ConfigMap)
		if cm.Data["phase"] == "active" && failBinding {
			failBinding = false
			return true, nil, errors.New("interrupted binding")
		}
		return false, nil, nil
	})
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 1200}); err == nil {
		t.Fatal("expected interrupted allocation")
	}
	later := k.now().Add(allocationTTL)
	k.Now = func() time.Time { return later }
	interleaved := false
	c.PrependReactor("update", "configmaps", func(a ktest.Action) (bool, runtime.Object, error) {
		cm := a.(ktest.UpdateAction).GetObject().(*core.ConfigMap)
		if cm.Data["phase"] == "deleting" && !interleaved {
			// A bind that was already in flight wins just before the stale expiry
			// update. The next reactor enforces the resulting version conflict.
			interleaved = true
			bound := cm.DeepCopy()
			bound.ResourceVersion = "concurrent-binding"
			bound.Data["phase"] = "active"
			bound.Data["created"] = later.Format(time.RFC3339Nano)
			bound.Data["expires"] = later.Add(20 * time.Minute).Format(time.RFC3339Nano)
			if err := c.Tracker().Update(core.SchemeGroupVersion.WithResource("configmaps"), bound, k.Namespace); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil, nil
	})
	if err := k.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := k.Get(ctx, testID)
	if !interleaved || err != nil || w.State != "ready" {
		t.Fatalf("stale expiry removed new binding: %+v %v", w, err)
	}
	podCount(t, k, 1)
}

func TestPoolConvergesWithoutDiscardingReadyCapacity(t *testing.T) {
	k, _ := setup(t)
	ctx := context.Background()
	ready := readyPool(t, k)
	pending := k.pod(CreateRequest{ID: "warm-000000000000000000000000", TTLSeconds: 600})
	pending.Labels[poolLabel] = "true"
	if _, err := k.Client.Pods(k.Namespace).Create(ctx, pending, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.MaintainPool(ctx); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 1)
	if _, err := k.Client.Pods(k.Namespace).Get(ctx, ready.Name, meta.GetOptions{}); err != nil {
		t.Fatalf("discarded ready reserve: %v", err)
	}
	later := k.now().Add(idleTTL + time.Second)
	k.Now = func() time.Time { return later }
	if err := k.MaintainPool(ctx); err != nil {
		t.Fatal(err)
	}
	podCount(t, k, 1)
	if _, err := k.Client.Pods(k.Namespace).Get(ctx, ready.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("aged idle Pod not rotated: %v", err)
	}
}
