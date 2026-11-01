//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

// Opt-in only: never selects the user's default kubeconfig or API.
func TestWorkspaceLifecycle(t *testing.T) {
	base, config, kubeContext := os.Getenv("SANDBOX_E2E_API"), os.Getenv("SANDBOX_E2E_KUBECONFIG"), os.Getenv("SANDBOX_E2E_CONTEXT")
	if base == "" || config == "" || kubeContext == "" {
		t.Fatal("use make e2e; explicit API, kubeconfig and context are required")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: config}, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube, err := typed.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(base)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := filepath.Abs("../../bin/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 190*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--api", base, "--json"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			return output, fmt.Errorf("sandbox %v: %w: %s", args, err, stderr.String())
		}
		return output, nil
	}
	must := func(args ...string) []byte {
		t.Helper()
		output, err := invoke(args...)
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	create := func(ttl string) cloud.Workspace {
		t.Helper()
		var w cloud.Workspace
		// Register cleanup before waiting so scheduling/readiness failures cannot leak Pods.
		if err := json.Unmarshal(must("create", "--ttl", ttl, "--wait", "0"), &w); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := c.Destroy(ctx, w.ID); err != nil {
				t.Errorf("cleanup %s: %v", w.ID, err)
			}
		})
		return w
	}
	wait := func(label string, timeout time.Duration, condition func() (bool, error)) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			ok, err := condition()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if ok {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("timed out waiting for %s", label)
	}
	podGone := func(id string) func() (bool, error) {
		return func() (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := kube.Pods("sandbox-workspaces").Get(ctx, id, meta.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
	}
	w := create("5m")
	wait("runner readiness", 180*time.Second, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		current, err := c.Get(ctx, w.ID)
		return current.State == "ready", err
	})
	t.Log("real workspace ready:", w.ID)
	must("put", w.ID, "../../examples/hello", ".")
	output := must("exec", "--timeout", "60s", w.ID, "--", "go", "test", "./...")
	if !strings.Contains(string(output), `"state":"succeeded"`) {
		t.Fatalf("missing successful result: %s", output)
	}
	output = must("exec", w.ID, "--", "go", "run", ".")
	if !strings.Contains(string(output), "hello, sandbox") {
		t.Fatalf("uploaded files did not persist: %s", output)
	}
	if _, err := invoke("exec", w.ID, "--", "sh", "-c", "exit 7"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("expected exit 7: %v", err)
		}
	} else {
		t.Fatal("failed command returned success")
	}
	output, err = invoke("exec", "--timeout", "1s", w.ID, "--", "sleep", "30")
	if err == nil || !strings.Contains(string(output), `"state":"timed_out"`) {
		t.Fatalf("timeout: %s, %v", output, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commandID := fmt.Sprintf("cmd-%024x", time.Now().UnixNano())
	if _, err := c.Start(ctx, w.ID, runner.Request{ID: commandID, Command: []string{"sleep", "60"}, TimeoutSeconds: 90}); err != nil {
		t.Fatal(err)
	}
	must("cancel", w.ID, commandID)
	wait("command cancellation", 15*time.Second, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := c.Command(ctx, w.ID, commandID)
		return result.State == "cancelled", err
	})
	must("destroy", w.ID)
	wait("explicit Pod deletion", 30*time.Second, podGone(w.ID))
	expired := create("1m")
	wait("TTL reconciliation and Pod deletion", 100*time.Second, podGone(expired.ID))
	t.Log("verified upload, execution, retention, failure, timeout, cancellation, deletion and expiry")
}
