package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

type transportTo struct{ target *url.URL }

func (t transportTo) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	r.URL = &u
	return http.DefaultTransport.RoundTrip(r)
}
func TestCLIControlAPIAndRunner(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%v", warm), func(t *testing.T) { testCLIControlAPIAndRunner(t, warm) })
	}
}
func testCLIControlAPIAndRunner(t *testing.T, warm bool) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := runner.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	runnerHTTP := httptest.NewServer(r.Handler())
	defer runnerHTTP.Close()
	target, _ := url.Parse(runnerHTTP.URL)
	kube := fake.NewClientset()
	var sequence int
	kube.PrependReactor("create", "*", func(a ktest.Action) (bool, runtime.Object, error) {
		// Supply metadata that a real API server sets on claims and Pods.
		m, err := apimeta.Accessor(a.(ktest.CreateAction).GetObject())
		if err != nil {
			return true, nil, err
		}
		sequence++
		m.SetUID(types.UID(fmt.Sprintf("uid-%d", sequence)))
		m.SetCreationTimestamp(meta.Now())
		return false, nil, nil
	})
	backend := &cloud.Kubernetes{Client: kube.CoreV1(), Namespace: "test", Image: "test/workspace:v1"}
	var warmName string
	if warm {
		backend.WarmPoolSize = 1
		if err := backend.MaintainPool(ctx); err != nil {
			t.Fatal(err)
		}
		pods, err := kube.CoreV1().Pods("test").List(ctx, meta.ListOptions{})
		if err != nil || len(pods.Items) != 1 {
			t.Fatalf("pool: %+v %v", pods, err)
		}
		p := pods.Items[0].DeepCopy()
		warmName = p.Name
		p.Spec.NodeName = "test-worker"
		p.Status.Phase = core.PodRunning
		p.Status.PodIP = "10.0.0.1"
		p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		if _, err := kube.CoreV1().Pods("test").Update(ctx, p, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	api := httptest.NewServer((&cloud.Server{Kube: backend, Transport: transportTo{target}}).Handler())
	defer api.Close()
	invoke := func(args ...string) (string, string, error) {
		var out, errout bytes.Buffer
		err := Run(ctx, append([]string{"--api", api.URL, "--json"}, args...), &out, &errout)
		return out.String(), errout.String(), err
	}
	output, _, err := invoke("create", "--ttl", "2m", "--wait", "0")
	if err != nil {
		t.Fatal(err)
	}
	var w cloud.Workspace
	if err := json.Unmarshal([]byte(output), &w); err != nil {
		t.Fatal(err)
	}
	expected := "pending"
	podName := w.ID
	if warm {
		expected, podName = "ready", warmName
	}
	if w.State != expected {
		t.Fatalf("state %s; want %s", w.State, expected)
	}
	if !warm {
		if _, _, err := invoke("exec", w.ID, "--", "true"); err == nil {
			t.Fatal("executed in pending workspace")
		}
	}
	p, err := kube.CoreV1().Pods("test").Get(ctx, podName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = core.PodRunning
	p.Status.PodIP = "10.0.0.1"
	p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	p.Spec.NodeName = "test-worker"
	if _, err := kube.CoreV1().Pods("test").Update(ctx, p, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(local, []byte("hello sandbox"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke("put", w.ID, local, "hello.txt"); err != nil {
		t.Fatal(err)
	}
	output, _, err = invoke("exec", w.ID, "--", "cat", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	var stdout string
	var result *runner.Result
	for {
		var e runner.Event
		err := decoder.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.Stream == "stdout" {
			stdout += e.Data
		}
		if e.Result != nil {
			result = e.Result
		}
	}
	if stdout != "hello sandbox" || result == nil || result.State != "succeeded" {
		t.Fatalf("bad output: %s", output)
	}
	if _, _, err := invoke("logs", w.ID, result.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = invoke("exec", w.ID, "--", "sh", "-c", "exit 7")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("exit propagation: %v", err)
	}
	if _, _, err := invoke("destroy", w.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke("inspect", w.ID); err == nil {
		t.Fatal("deleted workspace still exists")
	}
}
func TestOfflinePreviewAndValidation(t *testing.T) {
	var out, errout bytes.Buffer
	if err := Run(context.Background(), []string{"--api", "http://invalid.remote", "--json", "preview"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"simulated":true`) || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("preview: %s", out.String())
	}
	for _, args := range [][]string{{"create", "--ttl", "1s"}, {"exec", "--timeout", "0s", "id", "--", "true"}, {"put", "id"}, {"nonsense"}} {
		if err := Run(context.Background(), args, &out, &errout); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestDashboardShowsRealReasonAndSanitizes(t *testing.T) {
	m := dashboard{width: 90, started: time.Now(), wait: true, items: []cloud.Workspace{{ID: "sbx-example", State: "pending", Reason: "Unschedulable: insufficient memory\x1b[2J", ExpiresAt: time.Now().Add(time.Minute), CPU: "1", Memory: "512Mi"}}}
	view := m.View()
	if !strings.Contains(view, "Unschedulable: insufficient memory") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("view: %s", view)
	}
	next, cmd := m.Update(pollMsg{items: []cloud.Workspace{{State: "ready"}}})
	if !next.(dashboard).done || cmd == nil {
		t.Fatal("ready view did not finish")
	}
}
