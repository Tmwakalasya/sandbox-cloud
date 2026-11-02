// sandbox-bench measures live workspace bursts through the public HTTP API.
// Kubernetes access is read-only evidence collection; all mutations use the API.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	core "k8s.io/api/core/v1"
	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

const managed = "app.kubernetes.io/managed-by=sandbox-cloud"

type options struct {
	Count     int           `json:"concurrency"`
	Rounds    int           `json:"rounds"`
	Idle      int           `json:"minimum_ready_reserve"`
	TTL       time.Duration `json:"ttl_ns"`
	Timeout   time.Duration `json:"readiness_timeout_ns"`
	Poll      time.Duration `json:"poll_interval_ns"`
	Hold      time.Duration `json:"hold_after_burst_ns"`
	Namespace string        `json:"namespace"`
}

type sample struct {
	ID           string                     `json:"id"`
	Source       string                     `json:"source"`
	ReadyMS      *float64                   `json:"request_to_ready_ms"`
	ExecutionMS  *float64                   `json:"command_round_trip_ms"`
	Pod          string                     `json:"pod,omitempty"`
	PodUID       string                     `json:"pod_uid,omitempty"`
	Node         string                     `json:"node,omitempty"`
	Image        string                     `json:"image,omitempty"`
	ImageID      string                     `json:"image_id,omitempty"`
	Resources    *core.ResourceRequirements `json:"admitted_resources,omitempty"`
	Error        string                     `json:"error,omitempty"`
	CleanupError string                     `json:"cleanup_error,omitempty"`
}

type distribution struct {
	Samples int     `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
}

type summary struct {
	Attempts      int                     `json:"attempts"`
	Succeeded     int                     `json:"succeeded"`
	Failures      int                     `json:"failures"`
	CleanupErrors int                     `json:"cleanup_errors"`
	Warm          int                     `json:"warm_allocations"`
	Cold          int                     `json:"cold_allocations"`
	Unknown       int                     `json:"unknown_allocations"`
	WarmHitRate   *float64                `json:"warm_hit_rate_among_classified"`
	Readiness     map[string]distribution `json:"readiness_by_source"`
}

type burst struct {
	Number      int      `json:"number"`
	ReadyBefore []string `json:"ready_reserve_before"`
	ReadyAfter  []string `json:"ready_reserve_after"`
	RecoveryMS  *float64 `json:"post_cleanup_reserve_wait_ms"`
	Samples     []sample `json:"samples"`
	Error       string   `json:"error,omitempty"`
}

type report struct {
	StartedAt  time.Time              `json:"started_at"`
	FinishedAt time.Time              `json:"finished_at"`
	Context    string                 `json:"kubernetes_context"`
	Options    options                `json:"options"`
	Quota      core.ResourceQuotaSpec `json:"quota"`
	Bursts     []burst                `json:"bursts"`
	Summary    summary                `json:"summary"`
	Error      string                 `json:"error,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.IntVar(&o.Count, "count", 10, "concurrent workspaces per burst (1–100)")
	flag.IntVar(&o.Rounds, "rounds", 3, "number of bursts (1–100)")
	flag.IntVar(&o.Idle, "idle", 2, "minimum ready reserve to wait for; does not change the pool")
	flag.DurationVar(&o.TTL, "ttl", 15*time.Minute, "workspace expiry backstop")
	flag.DurationVar(&o.Timeout, "timeout", 5*time.Minute, "readiness and reserve wait deadline")
	flag.DurationVar(&o.Poll, "poll", 250*time.Millisecond, "HTTP readiness polling interval")
	flag.DurationVar(&o.Hold, "hold", 0, "keep the completed burst alive for inspection")
	flag.StringVar(&o.Namespace, "namespace", "sandbox-workspaces", "workspace namespace")
	kubeconfig := flag.String("kubeconfig", "", "required explicit kubeconfig file")
	kubecontext := flag.String("context", "", "required explicit Kubernetes context")
	api := flag.String("api", "http://127.0.0.1:8080", "control API URL")
	output := flag.String("output", "", "required new JSON report file; never overwritten")
	flag.Parse()
	if flag.NArg() != 0 || *kubeconfig == "" || *kubecontext == "" || *output == "" {
		return errors.New("--kubeconfig, --context, and --output are required; no positional arguments")
	}
	if err := o.validate(); err != nil {
		return err
	}
	c, err := client.New(*api)
	if err != nil {
		return err
	}
	if _, err := os.Stat(*kubeconfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return err
	}
	kube := kubectlReader{Kubeconfig: *kubeconfig, Context: *kubecontext, Namespace: o.Namespace}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, runErr := measure(ctx, c, kube, o, *kubecontext, os.Stderr)
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(r)
	closeErr := f.Close()
	fmt.Fprintf(os.Stderr, "Report: %s; %d/%d passed, %d warm, %d cold, %d unknown, %d cleanup errors\n", *output, r.Summary.Succeeded, r.Summary.Attempts, r.Summary.Warm, r.Summary.Cold, r.Summary.Unknown, r.Summary.CleanupErrors)
	for _, source := range []string{"warm", "cold", "unknown"} {
		if d, ok := r.Summary.Readiness[source]; ok {
			fmt.Fprintf(os.Stderr, "  %-7s readiness: p50 %.0fms | p95 %.0fms | n=%d\n", source, d.P50MS, d.P95MS, d.Samples)
		}
	}
	return errors.Join(runErr, writeErr, closeErr)
}

func (o options) validate() error {
	if o.Count < 1 || o.Count > 100 || o.Rounds < 1 || o.Rounds > 100 || o.Idle < 0 || o.Idle > 16 || o.Namespace == "" {
		return errors.New("count and rounds must be 1–100, idle 0–16, and namespace nonempty")
	}
	if o.Poll < 50*time.Millisecond || o.Poll > 5*time.Second || o.Timeout <= 0 || o.Hold < 0 || o.TTL > 24*time.Hour || o.TTL%time.Second != 0 || o.TTL < o.Timeout+o.Hold+3*time.Minute {
		return errors.New("poll must be 50ms–5s; timeout positive; hold nonnegative; whole-second TTL must cover timeout + hold + 3m and be at most 24h")
	}
	return nil
}

func measure(ctx context.Context, c *client.Client, kube clusterReader, o options, kubecontext string, progress io.Writer) (r report, runErr error) {
	r = report{StartedAt: time.Now().UTC(), Context: kubecontext, Options: o}
	defer func() {
		r.FinishedAt = time.Now().UTC()
		r.Summary = summarize(r.Bursts)
		if runErr != nil {
			r.Error = runErr.Error()
		}
	}()
	claims, err := kube.claims(ctx)
	if err != nil {
		return r, err
	}
	workspaces, err := c.List(ctx)
	if err != nil {
		return r, err
	}
	if len(claims.Items) != 0 || len(workspaces) != 0 {
		return r, errors.New("benchmark requires no existing workspaces or claims; nothing was changed")
	}
	quota, err := kube.quota(ctx)
	if err != nil {
		return r, err
	}
	r.Quota = quota.Spec
	for n := 1; n <= o.Rounds; n++ {
		b := burst{Number: n}
		b.ReadyBefore, err = waitReserve(ctx, kube, o)
		if err != nil {
			return r, fmt.Errorf("before burst %d: %w", n, err)
		}
		fmt.Fprintf(progress, "Burst %d/%d: %d ready reserve; launching %d workspaces\n", n, o.Rounds, len(b.ReadyBefore), o.Count)
		b.Samples, err = executeBurst(ctx, c, o)
		if err == nil {
			err = observe(ctx, kube, o.Namespace, b.Samples)
			if err != nil {
				for i := range b.Samples {
					b.Samples[i].Error = joinMessage(b.Samples[i].Error, "allocation evidence: "+err.Error())
				}
			}
		}
		for _, s := range b.Samples {
			status := "PASS"
			if s.Error != "" {
				status = s.Error
			}
			ready := "not ready"
			if s.ReadyMS != nil {
				ready = fmt.Sprintf("ready %.0fms", *s.ReadyMS)
			}
			fmt.Fprintf(progress, "  %s  %-7s  %-15s  %s\n", s.ID, s.Source, ready, status)
		}
		if err == nil && o.Hold > 0 {
			err = pause(ctx, o.Hold)
		}
		// Keep every attempted ID until the full burst completes. Cleanup uses
		// a fresh context even after cancellation or a lost create response.
		cleanupErr := cleanup(c, kube, o.Namespace, b.Samples)
		if cleanupErr == nil && ctx.Err() == nil {
			started := time.Now()
			var reserveErr error
			b.ReadyAfter, reserveErr = waitReserve(ctx, kube, o)
			if reserveErr == nil {
				b.RecoveryMS = milliseconds(time.Since(started))
			}
			err = errors.Join(err, reserveErr)
		}
		err = errors.Join(err, cleanupErr, ctx.Err())
		if err != nil {
			b.Error = err.Error()
		}
		r.Bursts = append(r.Bursts, b)
		if err != nil {
			return r, err
		}
		for _, s := range b.Samples {
			if s.Error != "" {
				return r, errors.New("burst contained failures; see per-workspace results")
			}
		}
	}
	return r, nil
}

func executeBurst(ctx context.Context, c *client.Client, o options) ([]sample, error) {
	samples := make([]sample, o.Count)
	for i := range samples {
		id, err := randomID("sbx")
		if err != nil {
			return nil, err
		}
		samples[i] = sample{ID: id, Source: "unknown"}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range samples {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := executeOne(ctx, c, o, &samples[i]); err != nil {
				samples[i].Error = err.Error()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return samples, nil
}

func executeOne(ctx context.Context, c *client.Client, o options, s *sample) error {
	readyCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	started := time.Now()
	w, err := c.Create(readyCtx, cloud.CreateRequest{ID: s.ID, TTLSeconds: int64(o.TTL.Seconds())})
	for err == nil && w.State != "ready" {
		switch w.State {
		case "pending", "starting":
		default:
			return fmt.Errorf("readiness: %s: %s", w.State, w.Reason)
		}
		if err = pause(readyCtx, o.Poll); err != nil {
			break
		}
		w, err = c.Get(readyCtx, s.ID)
	}
	if err != nil {
		return fmt.Errorf("create/readiness: %w", err)
	}
	s.ReadyMS = milliseconds(time.Since(started))
	commandCtx, commandCancel := context.WithTimeout(ctx, time.Minute)
	defer commandCancel()
	if err := c.Put(commandCtx, s.ID, "benchmark-id", strings.NewReader(s.ID)); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	commandID, err := randomID("cmd")
	if err != nil {
		return err
	}
	started = time.Now()
	_, err = c.Start(commandCtx, s.ID, runner.Request{ID: commandID, TimeoutSeconds: 30, Command: []string{"cat", "benchmark-id"}})
	if err != nil {
		return fmt.Errorf("command start: %w", err)
	}
	var stdout strings.Builder
	result, err := c.Events(commandCtx, s.ID, commandID, func(e runner.Event) error {
		if e.Stream == "stdout" {
			if stdout.Len()+len(e.Data) > 4096 {
				return errors.New("unexpectedly large command output")
			}
			stdout.WriteString(e.Data)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("command output: %w", err)
	}
	if result.State != "succeeded" || result.ExitCode == nil || *result.ExitCode != 0 || stdout.String() != s.ID {
		return fmt.Errorf("command verification: state=%s exit=%v; output matches workspace=%v", result.State, result.ExitCode, stdout.String() == s.ID)
	}
	s.ExecutionMS = milliseconds(time.Since(started))
	return nil
}

func observe(ctx context.Context, kube clusterReader, namespace string, samples []sample) error {
	claims, err := kube.claims(ctx)
	if err != nil {
		return err
	}
	pods, err := kube.pods(ctx, managed)
	if err != nil {
		return err
	}
	claimByID := make(map[string]core.ConfigMap)
	podByName := make(map[string]core.Pod)
	for _, c := range claims.Items {
		claimByID[c.Name] = c
	}
	for _, p := range pods.Items {
		podByName[p.Name] = p
	}
	seen := make(map[string]int)
	for i := range samples {
		s := &samples[i]
		c, exists := claimByID[s.ID]
		p, found := podByName[c.Data["pod"]]
		owned := false
		for _, ref := range p.OwnerReferences {
			if ref.Kind == "ConfigMap" && ref.Name == s.ID && ref.UID == c.UID && c.UID != "" {
				owned = true
			}
		}
		if !exists || !found || !owned || c.Data["phase"] != "active" || p.UID == "" || string(p.UID) != c.Data["podUID"] || p.Labels["sandbox-cloud/claim-uid"] != string(c.UID) || len(p.Spec.Containers) != 1 {
			s.Error = joinMessage(s.Error, "cannot verify active claim and Pod ownership")
			continue
		}
		s.Pod, s.PodUID, s.Node = p.Name, string(p.UID), p.Spec.NodeName
		s.Source = "cold"
		if p.Labels["sandbox-cloud/pool"] == "true" {
			s.Source = "warm"
		}
		s.Image = p.Spec.Containers[0].Image
		s.Resources = p.Spec.Containers[0].Resources.DeepCopy()
		if len(p.Status.ContainerStatuses) > 0 {
			s.ImageID = p.Status.ContainerStatuses[0].ImageID
		}
		if previous, duplicate := seen[s.PodUID]; duplicate {
			s.Error = joinMessage(s.Error, "duplicate Pod assignment")
			samples[previous].Error = joinMessage(samples[previous].Error, "duplicate Pod assignment")
		}
		seen[s.PodUID] = i
	}
	return nil
}

func cleanup(c *client.Client, kube clusterReader, namespace string, samples []sample) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	unverified := func(err error) error {
		message := "cleanup could not be verified: " + err.Error()
		for i := range samples {
			samples[i].CleanupError = joinMessage(samples[i].CleanupError, message)
		}
		return errors.New(message)
	}
	var wg sync.WaitGroup
	for i := range samples {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.Destroy(ctx, samples[i].ID); err != nil {
				samples[i].CleanupError = err.Error()
			}
		}(i)
	}
	wg.Wait()
	// DELETE acknowledges a request, not physical removal. Wait for both the
	// claim and its owned Pods, including late allocations after a lost response.
	for {
		claims, err := kube.claims(ctx)
		if err != nil {
			return unverified(err)
		}
		pods, err := kube.pods(ctx, managed)
		if err != nil {
			return unverified(err)
		}
		remaining := make(map[string]bool)
		for _, claim := range claims.Items {
			remaining[claim.Name] = true
		}
		for _, p := range pods.Items {
			remaining[p.Name] = true
			for _, ref := range p.OwnerReferences {
				if ref.Kind == "ConfigMap" {
					remaining[ref.Name] = true
				}
			}
		}
		pending := false
		for i := range samples {
			pending = pending || remaining[samples[i].ID]
		}
		if !pending {
			for _, s := range samples {
				if s.CleanupError != "" {
					return errors.New("cleanup request failed; resources are absent, see sample errors")
				}
			}
			return nil
		}
		if err := pause(ctx, time.Second); err != nil {
			for i := range samples {
				if remaining[samples[i].ID] {
					samples[i].CleanupError = joinMessage(samples[i].CleanupError, "claim or Pod still present after cleanup deadline")
				}
			}
			return err
		}
	}
}

func waitReserve(ctx context.Context, kube clusterReader, o options) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for {
		pods, err := kube.pods(ctx, managed+",sandbox-cloud/pool=true,!sandbox-cloud/claim-uid")
		if err != nil {
			return nil, err
		}
		ready := []string{}
		for _, p := range pods.Items {
			expires, err := time.Parse(time.RFC3339, p.Annotations["sandbox-cloud/expires-at"])
			if err != nil || !expires.After(time.Now()) || p.DeletionTimestamp != nil || p.Labels["sandbox-cloud/retired"] == "true" || p.Status.PodIP == "" || p.Status.Phase != core.PodRunning {
				continue
			}
			for _, condition := range p.Status.Conditions {
				if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
					ready = append(ready, p.Name)
				}
			}
		}
		if len(ready) >= o.Idle {
			slices.Sort(ready)
			return ready, nil
		}
		if err := pause(ctx, time.Second); err != nil {
			return nil, fmt.Errorf("reserve has %d ready Pods, need %d: %w", len(ready), o.Idle, err)
		}
	}
}

func summarize(bursts []burst) summary {
	s := summary{Readiness: make(map[string]distribution)}
	values := make(map[string][]float64)
	for _, b := range bursts {
		for _, sample := range b.Samples {
			s.Attempts++
			if sample.Error != "" || sample.ReadyMS == nil || sample.ExecutionMS == nil {
				s.Failures++
			} else {
				s.Succeeded++
			}
			if sample.CleanupError != "" {
				s.CleanupErrors++
			}
			switch sample.Source {
			case "warm":
				s.Warm++
			case "cold":
				s.Cold++
			default:
				s.Unknown++
			}
			if sample.ReadyMS != nil {
				values[sample.Source] = append(values[sample.Source], *sample.ReadyMS)
			}
		}
	}
	if classified := s.Warm + s.Cold; classified > 0 {
		rate := float64(s.Warm) / float64(classified)
		s.WarmHitRate = &rate
	}
	for source, v := range values {
		slices.Sort(v)
		s.Readiness[source] = distribution{Samples: len(v), P50MS: v[int(math.Ceil(.50*float64(len(v))))-1], P95MS: v[int(math.Ceil(.95*float64(len(v))))-1]}
	}
	return s
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomID(prefix string) (string, error) {
	var id [12]byte
	_, err := rand.Read(id[:])
	return fmt.Sprintf("%s-%x", prefix, id), err
}

func milliseconds(d time.Duration) *float64 {
	v := float64(d) / float64(time.Millisecond)
	return &v
}

func joinMessage(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
