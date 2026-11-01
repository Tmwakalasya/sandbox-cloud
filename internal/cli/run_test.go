package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

func TestRunLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		keep, uploadFail, cleanupFail, lostCreate, cancelWait bool
		command                                               []string
		exit                                                  int
	}{
		{name: "success", command: []string{"cat", "hello.txt"}},
		{name: "exit code", command: []string{"sh", "-c", "exit 7"}, exit: 7},
		{name: "high exit code", command: []string{"sh", "-c", "exit 200"}, exit: 200},
		{name: "keep success", keep: true, command: []string{"cat", "hello.txt"}},
		{name: "keep failure", keep: true, command: []string{"sh", "-c", "exit 7"}, exit: 7},
		{name: "failure with failed cleanup", cleanupFail: true, command: []string{"sh", "-c", "exit 7"}, exit: 7},
		{name: "invalid upload", uploadFail: true, command: []string{"true"}},
		{name: "cleanup failure", cleanupFail: true, command: []string{"true"}},
		{name: "lost create response", lostCreate: true, command: []string{"true"}},
		{name: "interrupt during readiness", cancelWait: true, command: []string{"true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			local := t.TempDir()
			if err := os.WriteFile(filepath.Join(local, "hello.txt"), []byte("hello run"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.uploadFail {
				if err := os.Symlink("hello.txt", filepath.Join(local, "link")); err != nil {
					t.Fatal(err)
				}
			}
			r, err := runner.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			runnerHTTP := httptest.NewServer(r.Handler())
			defer runnerHTTP.Close()
			target, _ := url.Parse(runnerHTTP.URL)
			kube := fake.NewClientset()
			kube.PrependReactor("create", "pods", func(a kt.Action) (bool, runtime.Object, error) {
				p := a.(kt.CreateAction).GetObject().(*core.Pod)
				p.Status.Phase = core.PodRunning
				p.Status.PodIP = "10.0.0.1"
				p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
				return false, nil, nil
			})
			backend := (&cloud.Server{Kube: &cloud.Kubernetes{Client: kube.CoreV1(), Namespace: "test", Image: "test/workspace:v1"}, Transport: transportTo{target}}).Handler()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.cleanupFail && r.Method == "DELETE" {
					http.Error(w, "delete unavailable", 503)
					return
				}
				if tc.cancelWait && r.Method == "GET" {
					cancel()
					<-r.Context().Done()
					return
				}
				if tc.lostCreate && r.Method == "POST" && r.URL.Path == "/v1/workspaces" {
					// The real create is accepted but its response never reaches the client.
					backend.ServeHTTP(httptest.NewRecorder(), r)
					http.Error(w, "lost create response", 502)
					return
				}
				backend.ServeHTTP(w, r)
			}))
			defer api.Close()
			args := []string{"--api", api.URL, "--json", "run", "--wait", "2s"}
			if tc.keep {
				args = append(args, "--keep")
			}
			args = append(args, local, "--")
			args = append(args, tc.command...)
			var out, errout bytes.Buffer
			err = Run(ctx, args, &out, &errout)
			if tc.exit != 0 {
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != tc.exit {
					t.Fatalf("exit: %v", err)
				}
			} else if tc.cancelWait {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			} else if tc.uploadFail || tc.cleanupFail || tc.lostCreate {
				if err == nil {
					t.Fatal("expected failure")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !tc.uploadFail && !tc.cleanupFail && !tc.lostCreate && !tc.cancelWait && tc.exit == 0 && !strings.Contains(out.String(), "hello run") {
				t.Fatalf("output: %s", out.String())
			}
			creates, deletes := 0, 0
			for _, a := range kube.Actions() {
				if a.GetVerb() == "create" {
					creates++
				}
				if a.GetVerb() == "delete" {
					deletes++
				}
			}
			if creates != 1 {
				t.Fatalf("creates=%d", creates)
			}
			if tc.keep || tc.cleanupFail {
				if deletes != 0 {
					t.Fatalf("unexpected deletes=%d", deletes)
				}
			} else if deletes != 1 {
				t.Fatalf("deletes=%d; stderr=%s", deletes, errout.String())
			}
			if tc.keep && !strings.Contains(errout.String(), "Workspace retained") {
				t.Fatalf("missing retained ID: %s", errout.String())
			}
			if tc.cleanupFail && !strings.Contains(err.Error(), "TTL remains the fallback") {
				t.Fatalf("missing cleanup guidance: %v", err)
			}
		})
	}
}

func TestRunValidationBeforeCreate(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{}, {dir, "true"}, {"--wait", "0", dir, "--", "true"}, {"--ttl", "1s", dir, "--", "true"},
		{"--timeout", "0s", dir, "--", "true"}, {dir, "--"}, {filepath.Join(dir, "missing"), "--", "true"},
	} {
		var out, errout bytes.Buffer
		if err := Run(context.Background(), append([]string{"--api", "http://127.0.0.1:1", "run"}, args...), &out, &errout); err == nil || strings.Contains(errout.String(), "Workspace ID:") {
			t.Fatalf("validation for %v: %v, %s", args, err, errout.String())
		}
	}
}

func TestRunCancellationDuringCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deleted <- struct{}{}
			w.WriteHeader(202)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			cancel()
			<-r.Context().Done()
		case r.Method == "GET":
			w.Write([]byte(`{"state":"ready"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer api.Close()
	var out, errout bytes.Buffer
	err := Run(ctx, []string{"--api", api.URL, "run", t.TempDir(), "--", "sleep", "60"}, &out, &errout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not run after cancellation")
	}
}

func TestRunHelp(t *testing.T) {
	var out, errout bytes.Buffer
	if err := Run(context.Background(), []string{"run", "--help"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errout.String(), "-- COMMAND") || !strings.Contains(errout.String(), "keep") {
		t.Fatalf("help: %s", errout.String())
	}
}

func TestRunReadinessFailureCleansUp(t *testing.T) {
	for _, state := range []string{"pending", "terminated"} {
		t.Run(state, func(t *testing.T) {
			deleted := make(chan struct{}, 1)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "DELETE":
					deleted <- struct{}{}
					w.WriteHeader(202)
				case "GET":
					w.Write([]byte(`{"state":"` + state + `"}`))
				default:
					w.Write([]byte(`{}`))
				}
			}))
			defer api.Close()
			var out, errout bytes.Buffer
			err := Run(context.Background(), []string{"--api", api.URL, "run", "--wait", "20ms", t.TempDir(), "--", "true"}, &out, &errout)
			if err == nil {
				t.Fatal("expected readiness failure")
			}
			if state == "pending" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected wait timeout: %v", err)
			}
			select {
			case <-deleted:
			default:
				t.Fatal("readiness failure did not request cleanup")
			}
		})
	}
}
