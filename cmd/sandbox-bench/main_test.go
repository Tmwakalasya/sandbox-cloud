package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

type testCluster struct {
	mu       sync.Mutex
	claimMap map[string]core.ConfigMap
	podMap   map[string]core.Pod
}

func newTestCluster() *testCluster {
	return &testCluster{claimMap: make(map[string]core.ConfigMap), podMap: make(map[string]core.Pod)}
}

func (k *testCluster) claims(context.Context) (*core.ConfigMapList, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	list := &core.ConfigMapList{}
	for _, c := range k.claimMap {
		list.Items = append(list.Items, *c.DeepCopy())
	}
	return list, nil
}

func (k *testCluster) pods(_ context.Context, selector string) (*core.PodList, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	list := &core.PodList{}
	for _, p := range k.podMap {
		if strings.Contains(selector, "!sandbox-cloud/claim-uid") && p.Labels["sandbox-cloud/claim-uid"] != "" {
			continue
		}
		list.Items = append(list.Items, *p.DeepCopy())
	}
	return list, nil
}

func (*testCluster) quota(context.Context) (*core.ResourceQuota, error) {
	return &core.ResourceQuota{}, nil
}

func (k *testCluster) assign(id string, warm bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	uid, podUID := types.UID("claim-"+id), types.UID("pod-"+id)
	p := core.Pod{ObjectMeta: meta.ObjectMeta{
		Name: id, UID: podUID, Labels: map[string]string{"sandbox-cloud/claim-uid": string(uid)},
		OwnerReferences: []meta.OwnerReference{{Kind: "ConfigMap", Name: id, UID: uid}},
	}, Spec: core.PodSpec{Containers: []core.Container{{Name: "runner", Image: "test@sha256:digest"}}}}
	if warm {
		p.Labels["sandbox-cloud/pool"] = "true"
		p.Name = "warm-" + id
	}
	k.podMap[p.Name] = p
	k.claimMap[id] = core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: id, UID: uid}, Data: map[string]string{"phase": "active", "pod": p.Name, "podUID": string(podUID)}}
}

func TestBurstHoldsTenAndCleansUpOnFailure(t *testing.T) {
	for _, failure := range []string{"none", "lost-create-response", "command-exit", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			k := newTestCluster()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			created, commands, deleted := 0, 0, 0
			allStarted := make(chan struct{})
			firstID := ""
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode([]cloud.Workspace{})
			})
			mux.HandleFunc("POST /v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
				var req cloud.CreateRequest
				json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				created++
				index := created
				if index == 1 {
					firstID = req.ID
				}
				k.assign(req.ID, index <= 2)
				if created == 10 {
					close(allStarted)
					if failure == "cancel" {
						cancel()
					}
				}
				mu.Unlock()
				select {
				case <-allStarted:
				case <-r.Context().Done():
					return
				}
				if index == 1 && failure == "lost-create-response" {
					http.Error(w, "response lost after allocation", http.StatusBadGateway)
					return
				}
				json.NewEncoder(w).Encode(cloud.Workspace{ID: req.ID, State: "pending"})
			})
			mux.HandleFunc("GET /v1/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(cloud.Workspace{ID: r.PathValue("id"), State: "ready"})
			})
			mux.HandleFunc("PUT /v1/workspaces/{id}/files", func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				if string(data) != r.PathValue("id") || r.URL.Query().Get("path") != "benchmark-id" {
					t.Error("upload must identify this workspace")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("POST /v1/workspaces/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
				var req runner.Request
				json.NewDecoder(r.Body).Decode(&req)
				if strings.Join(req.Command, " ") != "cat benchmark-id" {
					t.Error("unexpected verification command")
				}
				mu.Lock()
				commands++
				mu.Unlock()
				json.NewEncoder(w).Encode(runner.Result{ID: req.ID, State: "running"})
			})
			mux.HandleFunc("GET /v1/workspaces/{id}/commands/{cmd}/events", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				fail := failure == "command-exit" && r.PathValue("id") == firstID
				mu.Unlock()
				exit, state := 0, "succeeded"
				if fail {
					exit, state = 1, "failed"
				}
				enc := json.NewEncoder(w)
				enc.Encode(runner.Event{Stream: "stdout", Data: r.PathValue("id")})
				enc.Encode(runner.Event{Stream: "result", Result: &runner.Result{ID: r.PathValue("cmd"), State: state, ExitCode: &exit}})
			})
			mux.HandleFunc("DELETE /v1/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if created != 10 || failure == "none" && commands != 10 {
					t.Error("cleanup started before the full burst completed")
				}
				deleted++
				k.mu.Lock()
				defer k.mu.Unlock()
				claim, exists := k.claimMap[r.PathValue("id")]
				if !exists {
					t.Errorf("deleted an ID not created by this run: %s", r.PathValue("id"))
				}
				delete(k.podMap, claim.Data["pod"])
				delete(k.claimMap, claim.Name)
				w.WriteHeader(http.StatusAccepted)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c, _ := client.New(srv.URL)
			o := options{Count: 10, Rounds: 1, TTL: 15 * time.Minute, Timeout: time.Second, Poll: time.Millisecond, Namespace: "test"}
			r, err := measure(ctx, c, k, o, "test-context", io.Discard)
			if (err == nil) != (failure == "none") {
				t.Fatalf("failure=%s returned %v", failure, err)
			}
			if created != 10 || deleted != 10 || len(k.claimMap) != 0 || len(k.podMap) != 0 {
				t.Fatalf("cleanup: created=%d deleted=%d claims=%d pods=%d", created, deleted, len(k.claimMap), len(k.podMap))
			}
			if failure == "none" && (r.Summary.Succeeded != 10 || r.Summary.Warm != 2 || r.Summary.Cold != 8 || len(r.Bursts[0].Samples) != 10) {
				t.Fatalf("incorrect measurement: %+v", r.Summary)
			}
			if failure == "lost-create-response" || failure == "command-exit" {
				if r.Summary.Failures != 1 || r.Summary.Succeeded != 9 {
					t.Fatalf("failure omitted from report: %+v", r.Summary)
				}
			}
		})
	}
}

func TestReadinessDeadlineStillCleansUp(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cloud.Workspace{State: "pending"})
	})
	mux.HandleFunc("GET /v1/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cloud.Workspace{State: "pending"})
	})
	mux.HandleFunc("DELETE /v1/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := client.New(srv.URL)
	samples, err := executeBurst(context.Background(), c, options{Count: 1, TTL: 15 * time.Minute, Timeout: 20 * time.Millisecond, Poll: time.Millisecond})
	if err != nil || samples[0].Error == "" || samples[0].ReadyMS != nil {
		t.Fatalf("pending workspace reported ready: %+v %v", samples, err)
	}
	if err := cleanup(c, newTestCluster(), "test", samples); err != nil || !deleted {
		t.Fatalf("cleanup: %v, deleted=%v", err, deleted)
	}
}

func TestObserveRejectsChangedPodIdentity(t *testing.T) {
	k := newTestCluster()
	k.assign("workspace", true)
	p := k.podMap["warm-workspace"]
	p.UID = "replacement"
	k.podMap[p.Name] = p
	samples := []sample{{ID: "workspace", Source: "unknown"}}
	if err := observe(context.Background(), k, "test", samples); err != nil {
		t.Fatal(err)
	}
	if samples[0].Error == "" || samples[0].Source != "unknown" {
		t.Fatalf("replacement Pod treated as valid evidence: %+v", samples)
	}
}

func TestSummaryKeepsFailuresAndSeparatesSources(t *testing.T) {
	var b burst
	for i := 1; i <= 20; i++ {
		b.Samples = append(b.Samples, sample{ID: fmt.Sprint(i), Source: "warm", ReadyMS: milliseconds(time.Duration(i) * time.Millisecond), ExecutionMS: milliseconds(time.Millisecond)})
	}
	b.Samples = append(b.Samples, sample{Source: "cold", Error: "quota denied"}, sample{Source: "unknown", Error: "transport error", CleanupError: "delete denied"})
	s := summarize([]burst{b})
	if s.Attempts != 22 || s.Failures != 2 || s.CleanupErrors != 1 || s.Readiness["warm"].P50MS != 10 || s.Readiness["warm"].P95MS != 19 || s.WarmHitRate == nil || *s.WarmHitRate != 20.0/21 {
		t.Fatalf("incorrect summary: %+v", s)
	}
	if _, ok := s.Readiness["cold"]; ok {
		t.Fatal("failed create included as a zero-latency sample")
	}
}

type unavailableReader struct{ *testCluster }

func (unavailableReader) claims(context.Context) (*core.ConfigMapList, error) {
	return nil, errors.New("Kubernetes unavailable")
}

func TestCleanupRequiresEvidenceBeyondDeleteAcknowledgement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c, _ := client.New(srv.URL)
	samples := []sample{{ID: "attempted-workspace"}}
	err := cleanup(c, unavailableReader{newTestCluster()}, "test", samples)
	if err == nil || samples[0].CleanupError == "" {
		t.Fatalf("unverified deletion reported as successful: %v %+v", err, samples)
	}
}
