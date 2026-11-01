package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	data := `apiVersion: v1
kind: Config
current-context: other
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:6443
- name: other
  cluster:
    server: https://example.invalid
contexts:
- name: local
  context:
    cluster: local
    user: dev
- name: other
  context:
    cluster: other
    user: dev
users:
- name: dev
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	cfg, err := kubernetesConfig(path, "local")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://127.0.0.1:6443" || cfg.BearerToken != "test-token" {
		t.Fatalf("unexpected config: host %q", cfg.Host)
	}
	for _, tc := range [][2]string{{"", "local"}, {path, "missing"}, {path + "-missing", "local"}} {
		if _, err := kubernetesConfig(tc[0], tc[1]); err == nil {
			t.Fatalf("accepted invalid config %v", tc)
		}
	}
}
