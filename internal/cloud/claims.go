package cloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const (
	componentLabel = "app.kubernetes.io/component"
	claimLabel     = "sandbox-cloud/claim-uid"
	poolLabel      = "sandbox-cloud/pool"
	retiredLabel   = "sandbox-cloud/retired"
	claimSelector  = ownerLabel + "=" + owner + "," + componentLabel + "=claim"
	podSelector    = ownerLabel + "=" + owner + "," + componentLabel + "=workspace"
	allocationTTL  = 2 * time.Minute
)

// A claim is a namespaced ConfigMap named after the public workspace ID. Its
// resourceVersion serializes allocation for that ID across API replicas. A Pod
// is assigned using a separate resourceVersion check and never becomes idle again.
func (k *Kubernetes) getClaim(ctx context.Context, id string) (*core.ConfigMap, error) {
	c, err := k.Client.ConfigMaps(k.Namespace).Get(ctx, id, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	if c.Labels[ownerLabel] != owner || c.Labels[componentLabel] != "claim" || c.UID == "" {
		return nil, &Error{409, "workspace claim is not owned by this service"}
	}
	return c, nil
}

func (k *Kubernetes) createClaim(ctx context.Context, req CreateRequest) (Workspace, error) {
	c := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{
		Name: req.ID, Labels: map[string]string{ownerLabel: owner, componentLabel: "claim"},
	}, Data: map[string]string{
		"phase": "allocating", "image": k.Image, "ttl": strconv.FormatInt(req.TTLSeconds, 10),
		"created": k.now().Format(time.RFC3339Nano), "expires": k.now().Add(allocationTTL).Format(time.RFC3339Nano),
	}}
	c, err := k.Client.ConfigMaps(k.Namespace).Create(ctx, c, meta.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		c, err = k.getClaim(ctx, req.ID)
	}
	if err != nil {
		return Workspace{}, err
	}
	if c.Data["ttl"] != strconv.FormatInt(req.TTLSeconds, 10) || c.Data["image"] != k.Image {
		return Workspace{}, &Error{409, "workspace ID already exists with different settings"}
	}
	if err := k.allocate(ctx, c.Name, c.UID); err != nil {
		return Workspace{}, err
	}
	return k.Get(ctx, c.Name)
}

// Retry only optimistic-concurrency conflicts. Lost responses, permission errors,
// and other transport failures are returned; the durable claim permits recovery.
func (k *Kubernetes) allocate(ctx context.Context, id string, uid types.UID) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		c, err := k.getClaim(ctx, id)
		if err != nil {
			return err
		}
		if c.UID != uid {
			return &Error{409, "workspace claim was replaced"}
		}
		if c.Data["phase"] == "active" {
			return nil
		}
		expires, err := time.Parse(time.RFC3339Nano, c.Data["expires"])
		if err != nil || c.Data["phase"] != "allocating" || c.DeletionTimestamp != nil || !k.now().Before(expires) {
			return &Error{409, "workspace allocation is expired, deleting, or invalid"}
		}
		ttl, err := strconv.ParseInt(c.Data["ttl"], 10, 64)
		if err != nil || ttl < 60 || ttl > 86400 || c.Data["image"] == "" {
			return &Error{409, "invalid workspace claim settings"}
		}
		if c.Data["pod"] == "" {
			// Commit one target before touching any Pod. Concurrent retries of this
			// request must attempt the same target, even after an API restart.
			p, err := k.warmCandidate(ctx, c.Data["image"])
			if err != nil {
				return err
			}
			c.Data["pod"] = id
			if p != nil {
				c.Data["pod"], c.Data["podUID"] = p.Name, string(p.UID)
			}
			c, err = k.Client.ConfigMaps(k.Namespace).Update(ctx, c, meta.UpdateOptions{})
			if err != nil {
				return err
			}
		}
		p, err := k.acquirePod(ctx, c, ttl)
		if err != nil {
			return err
		}
		if p == nil {
			// The preferred warm Pod is irreversibly unavailable (claimed by
			// another ID, retired, or deleted). Falling back is a one-way change.
			c.Data["pod"], c.Data["podUID"] = id, ""
			c, err = k.Client.ConfigMaps(k.Namespace).Update(ctx, c, meta.UpdateOptions{})
			if err != nil {
				return err
			}
			p, err = k.acquirePod(ctx, c, ttl)
			if err != nil {
				return err
			}
		}
		now := k.now()
		if p.Labels[poolLabel] != "true" {
			now = p.CreationTimestamp.Time
		}
		c.Data["phase"], c.Data["podUID"] = "active", string(p.UID)
		c.Data["created"] = now.Format(time.RFC3339Nano)
		c.Data["expires"] = now.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339Nano)
		_, err = k.Client.ConfigMaps(k.Namespace).Update(ctx, c, meta.UpdateOptions{})
		if err == nil {
			source := "cold"
			if p.Labels[poolLabel] == "true" {
				source = "warm"
			}
			log.Printf("workspace %s allocated: source=%s pod=%s", id, source, p.Name)
			k.wakePool()
		}
		return err
	})
}

func (k *Kubernetes) acquirePod(ctx context.Context, c *core.ConfigMap, ttl int64) (*core.Pod, error) {
	p, err := k.Client.Pods(k.Namespace).Get(ctx, c.Data["pod"], meta.GetOptions{})
	if c.Data["pod"] == c.Name { // Cold fallback has a deterministic, unique name.
		if apierrors.IsNotFound(err) {
			p = k.pod(CreateRequest{ID: c.Name, TTLSeconds: ttl})
			p.Spec.Containers[0].Image = c.Data["image"]
			attachClaim(p, c)
			p, err = k.Client.Pods(k.Namespace).Create(ctx, p, meta.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				p, err = k.Client.Pods(k.Namespace).Get(ctx, c.Name, meta.GetOptions{})
			}
		}
		if err != nil {
			return nil, err
		}
		if !podBelongsTo(p, c) || p.UID == "" {
			return nil, &Error{409, "workspace Pod belongs to a different claim"}
		}
		return p, nil
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if string(p.UID) != c.Data["podUID"] || !managedPod(p) || p.DeletionTimestamp != nil || p.Labels[retiredLabel] == "true" {
		return nil, nil
	}
	if podBelongsTo(p, c) {
		return p, nil
	}
	if p.Labels[claimLabel] != "" || p.Labels[poolLabel] != "true" {
		return nil, nil
	}
	expires, parseErr := time.Parse(time.RFC3339, p.Annotations[expiresAnnotation])
	if parseErr != nil || !k.now().Before(expires) || len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != c.Data["image"] {
		// Retirement must win a CAS before changing the claim target. Otherwise
		// another handler with the old target could still assign this Pod.
		p.Labels[retiredLabel] = "true"
		_, err := k.Client.Pods(k.Namespace).Update(ctx, p, meta.UpdateOptions{})
		return nil, err
	}
	attachClaim(p, c)
	return k.Client.Pods(k.Namespace).Update(ctx, p, meta.UpdateOptions{})
}

func attachClaim(p *core.Pod, c *core.ConfigMap) {
	p.Labels[claimLabel] = string(c.UID)
	p.OwnerReferences = []meta.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: c.Name, UID: c.UID}}
}
func managedPod(p *core.Pod) bool {
	return p.Labels[ownerLabel] == owner && p.Labels[componentLabel] == "workspace"
}
func podBelongsTo(p *core.Pod, c *core.ConfigMap) bool {
	if !managedPod(p) || c.UID == "" || p.Labels[claimLabel] != string(c.UID) {
		return false
	}
	for _, ref := range p.OwnerReferences {
		if ref.APIVersion == "v1" && ref.Kind == "ConfigMap" && ref.Name == c.Name && ref.UID == c.UID {
			return true
		}
	}
	return false
}

func (k *Kubernetes) claimView(c *core.ConfigMap, p *core.Pod) Workspace {
	created, ce := time.Parse(time.RFC3339Nano, c.Data["created"])
	expires, ee := time.Parse(time.RFC3339Nano, c.Data["expires"])
	w := Workspace{ID: c.Name, State: "pending", Reason: "Allocating workspace", CreatedAt: created, ExpiresAt: expires, CPU: "1", Memory: "512Mi"}
	if ce != nil || ee != nil {
		w.State, w.Reason = "unavailable", "invalid claim timestamps"
	} else if c.Data["phase"] == "deleting" || c.DeletionTimestamp != nil {
		w.State, w.Reason = "terminating", ""
	} else if !k.now().Before(expires) {
		w.State, w.Reason = "expired", ""
	} else if c.Data["phase"] == "active" {
		if p == nil || !podBelongsTo(p, c) || string(p.UID) != c.Data["podUID"] {
			w.State, w.Reason = "unavailable", "assigned Pod is missing or has been replaced"
		} else {
			copy := p.DeepCopy()
			if copy.Annotations == nil {
				copy.Annotations = make(map[string]string)
			}
			copy.Annotations[expiresAnnotation] = expires.Format(time.RFC3339Nano)
			w = k.view(copy)
			w.ID, w.CreatedAt, w.ExpiresAt = c.Name, created, expires
		}
	} else if c.Data["phase"] != "allocating" {
		w.State, w.Reason = "unavailable", "invalid claim phase"
	}
	return w
}

func (k *Kubernetes) destroyClaim(ctx context.Context, c *core.ConfigMap, expiredOnly bool) error {
	id, uid := c.Name, c.UID
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := k.getClaim(ctx, id)
		if err != nil {
			return err
		}
		if current.UID != uid {
			return &Error{409, "workspace claim was replaced"}
		}
		if expiredOnly && current.Data["phase"] != "deleting" {
			expires, err := time.Parse(time.RFC3339Nano, current.Data["expires"])
			if err != nil {
				return fmt.Errorf("claim %s: invalid expiration: %w", id, err)
			}
			if k.now().Before(expires) {
				// Binding can extend a pending reservation into a full workspace
				// TTL after the reaper's List. Check again inside the CAS loop.
				c = nil
				return nil
			}
		}
		current.Data["phase"] = "deleting"
		c, err = k.Client.ConfigMaps(k.Namespace).Update(ctx, current, meta.UpdateOptions{})
		return err
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil || c == nil {
		return err
	}
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: podSelector + "," + claimLabel + "=" + string(uid)})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !podBelongsTo(p, c) {
			return fmt.Errorf("Pod %s has inconsistent claim ownership", p.Name)
		}
		if err := k.delete(ctx, p.Name, p.UID); err != nil {
			return err
		}
	}
	// Owner references also cover an in-flight create that arrives after this
	// list. Orphan reconciliation provides cleanup independent of GC timing.
	policy := meta.DeletePropagationBackground
	err = k.Client.ConfigMaps(k.Namespace).Delete(ctx, c.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid, ResourceVersion: &c.ResourceVersion}, PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		err = nil
	}
	k.wakePool()
	return err
}

func (k *Kubernetes) reapClaims(ctx context.Context) error {
	claims, err := k.Client.ConfigMaps(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: claimSelector})
	if err != nil {
		return err
	}
	var errs []error
	for i := range claims.Items {
		c := &claims.Items[i]
		expires, err := time.Parse(time.RFC3339Nano, c.Data["expires"])
		if err != nil {
			errs = append(errs, fmt.Errorf("claim %s: invalid expiration: %w", c.Name, err))
			continue
		}
		if c.Data["phase"] == "deleting" || !k.now().Before(expires) {
			err = k.destroyClaim(ctx, c, true)
		} else if c.Data["phase"] == "allocating" {
			err = k.allocate(ctx, c.Name, c.UID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("claim %s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}
