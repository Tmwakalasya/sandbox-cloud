package cloud

import (
	"context"
	"errors"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

const testID = "sbx-0123456789abcdef01234567"

func setup(t *testing.T) (*Kubernetes, *fake.Clientset) {
	t.Helper()
	c := fake.NewClientset()
	k := &Kubernetes{Client: c.CoreV1(), Namespace: "workspaces", Image: "example.test/workspace@sha256:abcd", Now: func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }}
	serverMetadata(t, c, k.now)
	return k, c
}
func TestCreateIdempotentAndRestrictedPod(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	w, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "pending" || w.ExpiresAt.Sub(k.now()) != 20*time.Minute {
		t.Fatalf("workspace: %+v", w)
	}
	p, err := c.CoreV1().Pods(k.Namespace).Get(ctx, testID, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *p.Spec.AutomountServiceAccountToken || p.Spec.RestartPolicy != core.RestartPolicyNever || !*p.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("unsafe Pod defaults")
	}
	if *p.Spec.ActiveDeadlineSeconds != 1200 || *p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation || !*p.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("missing deadline/security boundary")
	}
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 1200}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 600}); err == nil {
		t.Fatal("accepted different settings under same ID")
	}
	p.Spec.NodeName = "worker-1"
	p.Status.Phase = core.PodRunning
	p.Status.PodIP = "10.0.0.2"
	p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	if _, err := c.CoreV1().Pods(k.Namespace).Update(ctx, p, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	w, err = k.Get(ctx, testID)
	if err != nil || w.State != "ready" {
		t.Fatalf("ready: %+v %v", w, err)
	}
	address, err := k.RunnerAddress(ctx, testID)
	if err != nil || address != "http://10.0.0.2:8090" {
		t.Fatalf("address %s %v", address, err)
	}
}
func TestReapAfterRestartAndOwnership(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	p, err := c.CoreV1().Pods(k.Namespace).Get(ctx, testID, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	originalUID := p.UID
	if _, err := c.CoreV1().Pods(k.Namespace).Update(ctx, p, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	other := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "sbx-abcdefabcdefabcdefabcdef", Namespace: k.Namespace}}
	if _, err := c.CoreV1().Pods(k.Namespace).Create(ctx, other, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.Destroy(ctx, other.Name); err == nil {
		t.Fatal("destroyed foreign Pod")
	}
	fresh := &Kubernetes{Client: c.CoreV1(), Namespace: k.Namespace, Image: k.Image, Now: func() time.Time { return k.now().Add(2 * time.Minute) }}
	if _, err := fresh.RunnerAddress(ctx, testID); err == nil {
		t.Fatal("expired workspace executable")
	}
	sawUID := false
	c.PrependReactor("delete", "pods", func(a ktest.Action) (bool, runtime.Object, error) {
		action := a.(ktest.DeleteAction)
		opts := action.GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != originalUID {
			t.Error("deletion missing UID precondition")
		} else {
			sawUID = true
		}
		return false, nil, nil
	})
	if err := fresh.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if !sawUID {
		t.Fatal("expired Pod not deleted")
	}
	if _, err := c.CoreV1().Pods(k.Namespace).Get(ctx, testID, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired Pod exists: %v", err)
	}
	if _, err := c.CoreV1().Pods(k.Namespace).Get(ctx, other.Name, meta.GetOptions{}); err != nil {
		t.Fatal("foreign Pod removed", err)
	}
}
func TestValidationAndCreateFailure(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	for _, req := range []CreateRequest{{ID: "../../escape", TTLSeconds: 60}, {ID: testID, TTLSeconds: 1}, {ID: testID, TTLSeconds: 86401}} {
		if _, err := k.Create(ctx, req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	c.PrependReactor("create", "pods", func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if _, err := k.Create(ctx, CreateRequest{ID: testID, TTLSeconds: 60}); err == nil {
		t.Fatal("lost create error")
	}
}
func TestMissingExpirationIsNotDeleted(t *testing.T) {
	k, c := setup(t)
	ctx := context.Background()
	p := k.pod(CreateRequest{ID: testID, TTLSeconds: 60})
	delete(p.Annotations, expiresAnnotation)
	if _, err := c.CoreV1().Pods(k.Namespace).Create(ctx, p, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.Reap(ctx); err == nil {
		t.Fatal("invalid expiration not reported")
	}
	w, err := k.Get(ctx, testID)
	if err != nil || w.State != "unavailable" {
		t.Fatalf("workspace %+v %v", w, err)
	}
}
