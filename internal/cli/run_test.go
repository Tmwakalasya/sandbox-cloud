package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

// Serve a real local Git repository over trusted loopback HTTPS. No public
// network, replacement Git binary, or modified production checkout script.
func gitFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q", "--template=", "--initial-branch=main", ".")
	git("config", "user.name", "Sandbox test")
	git("config", "user.email", "sandbox@example.invalid")
	git("config", "commit.gpgsign", "false")
	git("config", "core.hooksPath", "/dev/null")
	if err := os.WriteFile(filepath.Join(dir, "value.txt"), []byte("pinned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "value.txt")
	git("commit", "-qm", "pinned commit")
	sha := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "value.txt"), []byte("newer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "newer commit")
	export := t.TempDir()
	bare := filepath.Join(export, "fixture.git")
	git("clone", "--quiet", "--bare", dir, bare)
	git("--git-dir="+bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	backend := filepath.Join(git("--exec-path"), "git-http-backend")
	srv := httptest.NewTLSServer(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + export, "GIT_HTTP_EXPORT_ALL=1"}})
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "git-ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", cert)
	return srv.URL + "/fixture.git", sha
}

func runFixture(t *testing.T, lostCreate, failDelete bool) (string, *atomic.Int32) {
	t.Helper()
	r, err := runner.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	deleted := new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workspaces", func(w http.ResponseWriter, req *http.Request) {
		var input cloud.CreateRequest
		json.NewDecoder(req.Body).Decode(&input)
		if lostCreate {
			http.Error(w, "create response lost", 502)
			return
		}
		json.NewEncoder(w).Encode(cloud.Workspace{ID: input.ID, State: "ready"})
	})
	mux.HandleFunc("DELETE /v1/workspaces/{id}", func(w http.ResponseWriter, req *http.Request) {
		deleted.Add(1)
		if failDelete {
			http.Error(w, "cleanup denied", 403)
			return
		}
		r.Close()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/v1/workspaces/{id}/{rest...}", func(w http.ResponseWriter, req *http.Request) {
		http.StripPrefix("/v1/workspaces/"+req.PathValue("id"), r.Handler()).ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, deleted
}

func TestRepositoryRun(t *testing.T) {
	repo, sha := gitFixture(t)
	for _, tc := range []struct {
		name, revision string
		command        []string
		keep, machine  bool
		wantCode       int
	}{
		{name: "pinned commit", revision: sha, command: []string{"cat", "value.txt"}},
		{name: "literal arguments", revision: sha, command: []string{"printf", "%s", "space ; $(not-a-command)"}},
		{name: "json", revision: sha, command: []string{"cat", "value.txt"}, machine: true},
		{name: "exit code", revision: sha, command: []string{"sh", "-c", "exit 7"}, wantCode: 7},
		{name: "keep failed", revision: sha, command: []string{"sh", "-c", "exit 7"}, keep: true, wantCode: 7},
		{name: "keep flag still cleans success", revision: sha, command: []string{"cat", "value.txt"}, keep: true},
		{name: "missing commit", revision: strings.Repeat("a", 40), command: []string{"echo", "MUST-NOT-RUN"}, wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, deleted := runFixture(t, false, false)
			args := []string{"--api", api}
			if tc.machine {
				args = append(args, "--json")
			}
			args = append(args, "run", "--repo", repo, "--ref", tc.revision)
			if tc.keep {
				args = append(args, "--keep-on-failure")
			}
			args = append(append(args, "--"), tc.command...)
			var out, errout bytes.Buffer
			err := Run(context.Background(), args, &out, &errout)
			if tc.wantCode == 0 && err != nil {
				t.Fatalf("run: %v\n%s", err, errout.String())
			}
			if tc.wantCode != 0 {
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != tc.wantCode {
					t.Fatalf("exit: %v; output=%s", err, errout.String())
				}
			}
			wantDelete := int32(1)
			if tc.keep && tc.wantCode != 0 {
				wantDelete = 0
				if !strings.Contains(errout.String(), "retained until TTL") {
					t.Fatal("missing retention information")
				}
			}
			if deleted.Load() != wantDelete {
				t.Fatalf("deletes=%d want=%d", deleted.Load(), wantDelete)
			}
			if tc.name == "pinned commit" && out.String() != "pinned\n" {
				t.Fatalf("did not execute pinned checkout: %q", out.String())
			}
			if tc.name == "literal arguments" && out.String() != tc.command[2] {
				t.Fatalf("arguments interpreted by shell: %q", out.String())
			}
			if tc.name == "missing commit" && strings.Contains(out.String(), "MUST-NOT-RUN") {
				t.Fatal("command ran after checkout failed")
			}
			if tc.machine {
				decoder := json.NewDecoder(&out)
				last := map[string]any{}
				for {
					var event map[string]any
					if err := decoder.Decode(&event); err == io.EOF {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					last = event
				}
				if last["stage"] != "cleanup" {
					t.Fatalf("missing cleanup event: %v", last)
				}
			}
		})
	}
}

type cancelOutput struct{ cancel context.CancelFunc }

func (w cancelOutput) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "started") {
		w.cancel()
	}
	return len(p), nil
}

func TestRunCancellationAndCleanupErrors(t *testing.T) {
	repo, sha := gitFixture(t)
	for _, tc := range []struct {
		name                  string
		lost, cleanup, cancel bool
	}{
		{"lost create response", true, false, false},
		{"cleanup failure", false, true, false},
		{"cancellation", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, deleted := runFixture(t, tc.lost, tc.cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command := []string{"true"}
			var output io.Writer = io.Discard
			if tc.cancel {
				command = []string{"sh", "-c", "echo started; sleep 30"}
				output = cancelOutput{cancel}
			}
			args := append([]string{"--api", api, "run", "--repo", repo, "--ref", sha, "--"}, command...)
			err := Run(ctx, args, output, io.Discard)
			if err == nil || deleted.Load() != 1 {
				t.Fatalf("error=%v deletes=%d", err, deleted.Load())
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if strings.Contains(err.Error(), "reconnect with") {
				t.Fatalf("suggested reconnect after cleanup: %v", err)
			}
		})
	}
}

func TestRunRejectsInvalidInputBeforeAllocation(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	sha := strings.Repeat("a", 40)
	for _, args := range [][]string{
		{"--repo", "file:///tmp/repo", "--ref", sha, "--", "true"},
		{"--repo", "https://user:password@example.invalid/repo", "--ref", sha, "--", "true"},
		{"--repo", "https://example.invalid/repo", "--ref", "main", "--", "true"},
		{"--repo", "https://example.invalid/repo", "--ref", sha, "--ttl", "1m", "--", "true"},
		{"--repo", "https://example.invalid/repo", "--ref", sha},
		{"--repo", "https://example.invalid/repo?token=secret", "--ref", sha, "--", "true"},
		{"--repo", "https://example.invalid/repo", "--ref", sha, "--", "echo", "\x00"},
	} {
		if err := Run(context.Background(), append([]string{"--api", srv.URL, "run"}, args...), io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid request contacted server %d times", requests.Load())
	}
}

func TestRunReadinessDeadlineCleansUp(t *testing.T) {
	var deletes, commands atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(r.URL.Path, "/commands"):
			commands.Add(1)
		default:
			json.NewEncoder(w).Encode(cloud.Workspace{State: "pending"})
		}
	}))
	defer srv.Close()
	err := Run(context.Background(), []string{"--api", srv.URL, "run", "--repo", "https://example.invalid/repo", "--ref", strings.Repeat("a", 40), "--wait", "10ms", "--", "true"}, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || deletes.Load() != 1 || commands.Load() != 0 {
		t.Fatalf("error=%v deletes=%d commands=%d", err, deletes.Load(), commands.Load())
	}
}
