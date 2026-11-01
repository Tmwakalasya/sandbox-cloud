package cloud

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const idleTTL = 10 * time.Minute

func (k *Kubernetes) wakePool() {
	select {
	case k.PoolWake <- struct{}{}:
	default:
	}
}

func (k *Kubernetes) warmCandidate(ctx context.Context, image string) (*core.Pod, error) {
	if k.WarmPoolSize <= 0 {
		return nil, nil
	}
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: podSelector + "," + poolLabel + "=true,!" + claimLabel})
	if err != nil {
		return nil, err
	}
	// Stable order also makes competing creates contend on the same candidates
	// instead of consuming different Pods for duplicate requests.
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for i := range pods.Items {
		p := &pods.Items[i]
		if k.usableIdle(p, image) && k.view(p).State == "ready" && net.ParseIP(p.Status.PodIP) != nil {
			return p, nil
		}
	}
	return nil, nil
}

func (k *Kubernetes) usableIdle(p *core.Pod, image string) bool {
	expires, err := time.Parse(time.RFC3339, p.Annotations[expiresAnnotation])
	return managedPod(p) && p.UID != "" && p.Labels[poolLabel] == "true" && p.Labels[claimLabel] == "" &&
		p.Labels[retiredLabel] != "true" && p.DeletionTimestamp == nil && err == nil && k.now().Before(expires) &&
		len(p.Spec.Containers) == 1 && p.Spec.Containers[0].Image == image &&
		p.Status.Phase != core.PodFailed && p.Status.Phase != core.PodSucceeded && p.Status.Phase != core.PodUnknown &&
		k.view(p).State != "terminated"
}

// MaintainPool converges ready+starting idle Pods to a target. This is a soft
// reserve, not a capacity limit: multiple replicas can briefly overfill it.
// Namespace quota bounds all Pods, and subsequent passes remove excess idle Pods.
func (k *Kubernetes) MaintainPool(ctx context.Context) error {
	if k.WarmPoolSize < 0 {
		return errors.New("warm pool size cannot be negative")
	}
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: podSelector + "," + poolLabel + "=true,!" + claimLabel})
	if err != nil {
		return err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		iReady, jReady := k.view(&pods.Items[i]).State == "ready", k.view(&pods.Items[j]).State == "ready"
		if iReady != jReady {
			return iReady
		}
		return pods.Items[i].Name < pods.Items[j].Name
	})
	count := 0
	var errs []error
	for i := range pods.Items {
		p := &pods.Items[i]
		if k.usableIdle(p, k.Image) && count < k.WarmPoolSize {
			count++
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		// UID alone is insufficient: a claim can update this same Pod between
		// List and Delete. resourceVersion prevents deleting a newly claimed Pod.
		err := k.Client.Pods(k.Namespace).Delete(ctx, p.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &p.UID, ResourceVersion: &p.ResourceVersion}})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, err)
		}
	}
	for ; count < k.WarmPoolSize; count++ {
		var id [12]byte
		if _, err := rand.Read(id[:]); err != nil {
			return errors.Join(append(errs, err)...)
		}
		p := k.pod(CreateRequest{ID: fmt.Sprintf("warm-%x", id), TTLSeconds: int64(idleTTL.Seconds())})
		p.Labels[poolLabel] = "true"
		// Pod deadlines are relative to Pod start and cannot be restarted at
		// claim time. The claim record enforces the user's TTL; this is a wider
		// kubelet backstop covering idle time, interrupted binding, and max TTL.
		deadline := int64((idleTTL + allocationTTL + 24*time.Hour).Seconds())
		p.Spec.ActiveDeadlineSeconds = &deadline
		if _, err := k.Client.Pods(k.Namespace).Create(ctx, p, meta.CreateOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("replenish warm pool: %w", err))
			break // Quota/admission errors are reported, not retried in a loop.
		}
	}
	return errors.Join(errs...)
}
