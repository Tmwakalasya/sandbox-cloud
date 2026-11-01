//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRunWorkspace(t *testing.T) {
	api, config, kubeContext := os.Getenv("SANDBOX_E2E_API"), os.Getenv("SANDBOX_E2E_KUBECONFIG"), os.Getenv("SANDBOX_E2E_CONTEXT")
	if api == "" || config == "" || kubeContext == "" {
		t.Fatal("explicit SANDBOX_E2E_API, SANDBOX_E2E_KUBECONFIG and SANDBOX_E2E_CONTEXT required")
	}
	binary, err := filepath.Abs("../../bin/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	kube := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", config, "--context", kubeContext, "-n", "sandbox-workspaces"}, args...)...).CombinedOutput()
	}
	identify := func(t *testing.T, stderr string) string {
		t.Helper()
		match := regexp.MustCompile(`Workspace ID: (sbx-[a-f0-9]{24})`).FindStringSubmatch(stderr)
		if len(match) != 2 {
			t.Fatalf("missing workspace ID: %s", stderr)
		}
		id := match[1]
		t.Cleanup(func() {
			if out, err := kube("delete", "pod", id, "--ignore-not-found", "--wait=true", "--timeout=30s"); err != nil {
				t.Errorf("cleanup %s: %v: %s", id, err, out)
			}
		})
		return id
	}
	gone := func(t *testing.T, id string) {
		t.Helper()
		if out, err := kube("wait", "--for=delete", "pod/"+id, "--timeout=30s"); err != nil {
			t.Fatalf("workspace not deleted: %v: %s", err, out)
		}
	}
	for _, tc := range []struct {
		name    string
		keep    bool
		command []string
		exit    int
	}{
		{name: "success", command: []string{"go", "test", "./..."}},
		{name: "failure", command: []string{"sh", "-c", "exit 200"}, exit: 200},
		{name: "keep failure", keep: true, command: []string{"sh", "-c", "exit 7"}, exit: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			args := []string{"--api", api, "--json", "run", "--ttl", "5m", "--timeout", "90s"}
			if tc.keep {
				args = append(args, "--keep")
			}
			args = append(args, "../../examples/hello", "--")
			args = append(args, tc.command...)
			cmd := exec.CommandContext(ctx, binary, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			id := identify(t, stderr.String())
			if tc.exit == 0 {
				if err != nil {
					t.Fatalf("run: %v: %s", err, stderr.String())
				}
				if !strings.Contains(stdout.String(), `"state":"succeeded"`) {
					t.Fatalf("missing success: %s", stdout.String())
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tc.exit {
					t.Fatalf("exit propagation: %v: %s", err, stderr.String())
				}
			}
			if tc.keep {
				if out, err := kube("get", "pod", id); err != nil {
					t.Fatalf("kept workspace missing: %v: %s", err, out)
				}
				if !strings.Contains(stderr.String(), "Workspace retained") {
					t.Fatalf("missing keep guidance: %s", stderr.String())
				}
			} else {
				gone(t, id)
			}
		})
	}
	t.Run("interrupt", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--api", api, "--json", "run", "--ttl", "5m", "../../examples/hello", "--", "sh", "-c", "echo started; sleep 60")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stdout)
		started := false
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "started") {
				started = true
				break
			}
		}
		if started {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Error(err)
			}
		}
		// Continue draining so Wait can finish even if more output was queued.
		for scanner.Scan() {
		}
		err = cmd.Wait()
		id := identify(t, stderr.String())
		var exit *exec.ExitError
		if !started || !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Fatalf("interrupt: started=%v, err=%v, stderr=%s", started, err, stderr.String())
		}
		gone(t, id)
	})
}
