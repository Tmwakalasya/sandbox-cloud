package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	core "k8s.io/api/core/v1"
)

// Only observation is available here. Workspace mutations always use the public
// application API, and the benchmark never changes quota or pool configuration.
type clusterReader interface {
	claims(context.Context) (*core.ConfigMapList, error)
	pods(context.Context, string) (*core.PodList, error)
	quota(context.Context) (*core.ResourceQuota, error)
}

type kubectlReader struct {
	Kubeconfig, Context, Namespace string
}

func (k kubectlReader) read(ctx context.Context, output any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base := []string{"--kubeconfig", k.Kubeconfig, "--context", k.Context, "--namespace", k.Namespace, "--request-timeout=15s", "get"}
	cmd := exec.CommandContext(ctx, "kubectl", append(append(base, args...), "-o", "json")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("kubectl %v: %w: %s", args, err, stderr.String())
	}
	return json.Unmarshal(data, output)
}

func (k kubectlReader) claims(ctx context.Context) (*core.ConfigMapList, error) {
	var result core.ConfigMapList
	err := k.read(ctx, &result, "configmaps", "-l", managed)
	return &result, err
}

func (k kubectlReader) pods(ctx context.Context, selector string) (*core.PodList, error) {
	var result core.PodList
	err := k.read(ctx, &result, "pods", "-l", selector)
	return &result, err
}

func (k kubectlReader) quota(ctx context.Context) (*core.ResourceQuota, error) {
	var result core.ResourceQuota
	err := k.read(ctx, &result, "resourcequota", "workspace-budget")
	return &result, err
}
