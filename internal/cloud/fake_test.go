package cloud

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

// client-go's tracker does not enforce resourceVersion or delete preconditions.
// Model those API-server contracts so concurrency tests can reject stale writes.
// Fake.Invokes serializes reactor execution; no extra test lock is required.
func serverMetadata(t *testing.T, c *fake.Clientset, now func() time.Time) {
	t.Helper()
	var version int
	c.PrependReactor("*", "*", func(a ktest.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource != "pods" && a.GetResource().Resource != "configmaps" {
			return false, nil, nil
		}
		gvr, ns := a.GetResource(), a.GetNamespace()
		conflict := func(name string) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(gvr.GroupResource(), name, errors.New("stale resource version or UID"))
		}
		switch action := a.(type) {
		case ktest.CreateActionImpl:
			obj := action.GetObject().DeepCopyObject()
			m, _ := meta.Accessor(obj)
			version++
			m.SetUID(types.UID(fmt.Sprintf("uid-%d", version)))
			m.SetResourceVersion(strconv.Itoa(version))
			m.SetNamespace(ns)
			stamp := m.GetCreationTimestamp()
			stamp.Time = now()
			m.SetCreationTimestamp(stamp)
			err := c.Tracker().Create(gvr, obj, ns)
			return true, obj, err
		case ktest.UpdateActionImpl:
			obj := action.GetObject().DeepCopyObject()
			m, _ := meta.Accessor(obj)
			old, err := c.Tracker().Get(gvr, ns, m.GetName())
			if err != nil {
				return true, nil, err
			}
			oldMeta, _ := meta.Accessor(old)
			if m.GetResourceVersion() != oldMeta.GetResourceVersion() || m.GetUID() != oldMeta.GetUID() {
				return conflict(m.GetName())
			}
			version++
			m.SetResourceVersion(strconv.Itoa(version))
			err = c.Tracker().Update(gvr, obj, ns)
			return true, obj, err
		case ktest.DeleteActionImpl:
			old, err := c.Tracker().Get(gvr, ns, action.GetName())
			if err != nil {
				return true, nil, err
			}
			m, _ := meta.Accessor(old)
			pre := action.GetDeleteOptions().Preconditions
			if pre != nil && (pre.UID != nil && *pre.UID != m.GetUID() || pre.ResourceVersion != nil && *pre.ResourceVersion != m.GetResourceVersion()) {
				return conflict(m.GetName())
			}
			return true, nil, c.Tracker().Delete(gvr, ns, action.GetName())
		}
		return false, nil, nil
	})
}
