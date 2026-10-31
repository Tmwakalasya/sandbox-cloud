// Package cloud owns workspace Pods. Command state belongs to each Pod's runner.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
)

const ownerLabel = "app.kubernetes.io/managed-by"
const owner = "sandbox-cloud"
const expiresAnnotation = "sandbox-cloud/expires-at"
const ttlAnnotation = "sandbox-cloud/ttl-seconds"

var validID = regexp.MustCompile(`^sbx-[a-f0-9]{24}$`)

type CreateRequest struct {
	ID         string `json:"id"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

type Workspace struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Node      string    `json:"node,omitempty"`
	CPU       string    `json:"cpu"`
	Memory    string    `json:"memory"`
}

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

type Kubernetes struct {
	Client    typed.CoreV1Interface
	Namespace string
	Image     string
	Now       func() time.Time
}

func (k *Kubernetes) now() time.Time {
	if k.Now != nil {
		return k.Now().UTC()
	}
	return time.Now().UTC()
}

func (k *Kubernetes) Create(ctx context.Context, req CreateRequest) (Workspace, error) {
	if !validID.MatchString(req.ID) {
		return Workspace{}, &Error{400, "id must be sbx- followed by 24 lowercase hexadecimal characters"}
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = 1200
	}
	if req.TTLSeconds < 60 || req.TTLSeconds > 86400 {
		return Workspace{}, &Error{400, "ttl_seconds must be between 60 and 86400"}
	}
	if k.Image == "" {
		return Workspace{}, errors.New("workspace image is not configured")
	}
	p := k.pod(req)
	created, err := k.Client.Pods(k.Namespace).Create(ctx, p, meta.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = k.getPod(ctx, req.ID)
		if err == nil && (created.Annotations[ttlAnnotation] != strconv.FormatInt(req.TTLSeconds, 10) || len(created.Spec.Containers) != 1 || created.Spec.Containers[0].Image != k.Image) {
			return Workspace{}, &Error{409, "workspace ID already exists with different settings"}
		}
	}
	if err != nil {
		return Workspace{}, err
	}
	return k.view(created), nil
}

func (k *Kubernetes) pod(req CreateRequest) *core.Pod {
	no, yes := false, true
	uid := int64(65532)
	grace := int64(5)
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: req.ID, Labels: map[string]string{ownerLabel: owner, "app.kubernetes.io/component": "workspace"}, Annotations: map[string]string{expiresAnnotation: k.now().Add(time.Duration(req.TTLSeconds) * time.Second).Format(time.RFC3339), ttlAnnotation: strconv.FormatInt(req.TTLSeconds, 10)}},
		Spec: core.PodSpec{
			RestartPolicy: core.RestartPolicyNever, AutomountServiceAccountToken: &no, EnableServiceLinks: &no,
			ActiveDeadlineSeconds: &req.TTLSeconds, TerminationGracePeriodSeconds: &grace,
			SecurityContext: &core.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}},
			Containers: []core.Container{{Name: "runner", Image: k.Image, ImagePullPolicy: core.PullIfNotPresent, Command: []string{"/usr/local/bin/sandbox-runner", "--listen=:8090", "--workdir=/workspace"},
				Env:             []core.EnvVar{{Name: "HOME", Value: "/workspace"}, {Name: "GOCACHE", Value: "/tmp/go-cache"}, {Name: "GOPATH", Value: "/workspace/go"}, {Name: "GOTOOLCHAIN", Value: "local"}},
				Ports:           []core.ContainerPort{{Name: "http", ContainerPort: 8090}},
				Resources:       core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("128Mi"), core.ResourceEphemeralStorage: resource.MustParse("128Mi")}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("1"), core.ResourceMemory: resource.MustParse("512Mi"), core.ResourceEphemeralStorage: resource.MustParse("1Gi")}},
				SecurityContext: &core.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}},
				ReadinessProbe:  &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8090)}}, PeriodSeconds: 1, TimeoutSeconds: 1},
				VolumeMounts:    []core.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes: []core.Volume{{Name: "workspace", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: quantity("512Mi")}}}, {Name: "tmp", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: quantity("256Mi")}}}},
		},
	}
}
func quantity(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func (k *Kubernetes) getPod(ctx context.Context, id string) (*core.Pod, error) {
	if !validID.MatchString(id) {
		return nil, &Error{400, "invalid workspace ID"}
	}
	p, err := k.Client.Pods(k.Namespace).Get(ctx, id, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	if p.Labels[ownerLabel] != owner || p.Labels["app.kubernetes.io/component"] != "workspace" {
		return nil, &Error{404, "workspace not found"}
	}
	return p, nil
}
func (k *Kubernetes) Get(ctx context.Context, id string) (Workspace, error) {
	p, err := k.getPod(ctx, id)
	if err != nil {
		return Workspace{}, err
	}
	return k.view(p), nil
}
func (k *Kubernetes) List(ctx context.Context) ([]Workspace, error) {
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: ownerLabel + "=" + owner + ",app.kubernetes.io/component=workspace"})
	if err != nil {
		return nil, err
	}
	result := make([]Workspace, 0, len(pods.Items))
	for i := range pods.Items {
		result = append(result, k.view(&pods.Items[i]))
	}
	return result, nil
}
func (k *Kubernetes) Destroy(ctx context.Context, id string) error {
	p, err := k.getPod(ctx, id)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return k.delete(ctx, p.Name, p.UID)
}
func (k *Kubernetes) delete(ctx context.Context, id string, uid types.UID) error {
	err := k.Client.Pods(k.Namespace).Delete(ctx, id, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
func (k *Kubernetes) RunnerAddress(ctx context.Context, id string) (string, error) {
	p, err := k.getPod(ctx, id)
	if err != nil {
		return "", err
	}
	w := k.view(p)
	if w.State != "ready" {
		return "", &Error{409, "workspace is " + w.State + ": " + w.Reason}
	}
	if net.ParseIP(p.Status.PodIP) == nil {
		return "", &Error{503, "workspace has no valid Pod IP"}
	}
	return "http://" + net.JoinHostPort(p.Status.PodIP, "8090"), nil
}

func (k *Kubernetes) view(p *core.Pod) Workspace {
	expires, err := time.Parse(time.RFC3339, p.Annotations[expiresAnnotation])
	w := Workspace{ID: p.Name, State: "pending", CreatedAt: p.CreationTimestamp.Time, ExpiresAt: expires, Node: p.Spec.NodeName, CPU: "1", Memory: "512Mi"}
	if err != nil {
		w.State = "unavailable"
		w.Reason = "missing or invalid expiration"
		return w
	}
	if p.DeletionTimestamp != nil {
		w.State = "terminating"
		return w
	}
	if !k.now().Before(expires) {
		w.State = "expired"
		return w
	}
	if p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
		w.State = "terminated"
		w.Reason = p.Status.Reason
		return w
	}
	if p.Status.Phase == core.PodUnknown {
		w.State = "unavailable"
		w.Reason = p.Status.Message
		return w
	}
	if p.Spec.NodeName != "" {
		w.State = "starting"
	}
	for _, c := range p.Status.Conditions {
		if c.Type == core.PodScheduled && c.Status == core.ConditionFalse {
			w.Reason = c.Reason + ": " + c.Message
		}
		if c.Type == core.PodReady && c.Status == core.ConditionTrue && p.Status.Phase == core.PodRunning {
			w.State = "ready"
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.State.Waiting != nil {
			w.Reason = c.State.Waiting.Reason + ": " + c.State.Waiting.Message
		}
		if c.State.Terminated != nil {
			w.State = "terminated"
			w.Reason = c.State.Terminated.Reason
		}
	}
	return w
}

// Reap is a reconciliation pass, not an in-memory timer. Restarting the API does
// not reset lifetimes. UID preconditions prevent deleting a replacement object.
func (k *Kubernetes) Reap(ctx context.Context) error {
	pods, err := k.Client.Pods(k.Namespace).List(ctx, meta.ListOptions{LabelSelector: ownerLabel + "=" + owner + ",app.kubernetes.io/component=workspace"})
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range pods.Items {
		expires, err := time.Parse(time.RFC3339, p.Annotations[expiresAnnotation])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid expiration: %w", p.Name, err))
			continue
		}
		if !k.now().Before(expires) && p.DeletionTimestamp == nil {
			if err := k.delete(ctx, p.Name, p.UID); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}
