# GKE deployment guide

Build the images in Cloud Build, store them in Artifact Registry, and deploy the
control API to an existing GKE cluster. No local Docker installation or container
image downloads are required. Run commands from the repository root.

## Deployment layout

- `sandbox-control`: one API Deployment, an internal ClusterIP Service, and a
  dedicated ServiceAccount.
- `sandbox-workspaces`: disposable Pods, workspace claim ConfigMaps, and a Role
  granting the API `create/get/list/update/delete` on those two resource types.
- ResourceQuota: 12 Pod objects, 16 ConfigMap objects, 12 CPU limits, and 6 GiB
  memory limits. Kubernetes-created ConfigMaps consume quota too; idle and
  terminal Pods count until deleted.
- Warm pool: target two unused Pods, with cold allocation when no ready Pod is
  available. Replenishment can be blocked by the same namespace quota.
- NetworkPolicies: runner ingress is limited to API Pods. Ordinary Pod ingress to
  the API is denied; clients connect through Kubernetes port-forwarding.

Egress is unrestricted for trusted workloads and dependency downloads. Keep these
namespaces dedicated to Sandbox Cloud. Autopilot can adjust Pod requests; inspect
admitted values when planning capacity or comparing benchmarks.

## 1. Prerequisites and target

Requirements:

- A Linux GKE cluster with enforced NetworkPolicies and a DNS control-plane
  endpoint for the connection command below.
- `gcloud`, `kubectl`, Python 3, and `gke-gcloud-auth-plugin` on PATH.
- GKE, Cloud Build, and Artifact Registry APIs enabled in the project.
- Permission to access the cluster, submit builds, and apply the supplied
  namespaces, RBAC, policies, quota, Service, and Deployment.
- The Go CLI built with `make build` from the repository root.

Set the target values for the deployment:

```sh
export SANDBOX_PROJECT=YOUR_PROJECT_ID
export SANDBOX_REGION=us-central1
export SANDBOX_CLUSTER=YOUR_CLUSTER_NAME
export SANDBOX_REGISTRY="${SANDBOX_REGION}-docker.pkg.dev/${SANDBOX_PROJECT}/sandbox-cloud"
```

The commands below override the registry default in `cloudbuild.yaml`.
Cloud Build, registry storage, and GKE resources incur usage charges.

## 2. Connect to the cluster

Use a separate kubeconfig to keep the default configuration unchanged:

```sh
export PATH="$(gcloud info --format='value(installation.sdk_root)')/bin:$PATH"
export KUBECONFIG="$HOME/.kube/sandbox-cloud.config"
gcloud container clusters get-credentials "$SANDBOX_CLUSTER" \
  --region="$SANDBOX_REGION" --project="$SANDBOX_PROJECT" --dns-endpoint
kubectl config current-context
kubectl get namespaces
```

Keep this `KUBECONFIG` selected for subsequent `kubectl` commands, including any
terminal used for port-forwarding.

## 3. Configure the registry and build remotely

The Cloud Build identity needs access to the uploaded source, permission to write
build logs, and write access to the image repository. The GKE node identity needs
repository read access. Use scoped permissions for the project's actual build and
node identities.

If the repository does not already exist:

```sh
gcloud artifacts repositories create sandbox-cloud \
  --repository-format=docker --location="$SANDBOX_REGION" \
  --project="$SANDBOX_PROJECT" --immutable-tags
```

Submit the build:

```sh
gcloud builds submit . --config=cloudbuild.yaml \
  --region="$SANDBOX_REGION" --project="$SANDBOX_PROJECT" \
  --substitutions="_REGISTRY=${SANDBOX_REGISTRY}"
```

Cloud Build runs the Go tests and builds two images: `control`, containing the
API binary, and `workspace`, containing the runner, Go toolchain, Git, and shell.
Both are tagged with the build ID and pushed to the selected repository. The
build does not deploy anything.

Record both image digests from the completed build. Deployment uses immutable
digest references. The Dockerfile's Go base tag is versioned but remains mutable;
reproducible release builds also require pinning that base image by digest.

## 4. Render and apply the deployment

Replace `CONTROL_DIGEST` and `WORKSPACE_DIGEST` below with the corresponding
64-character SHA-256 hashes, without the `sha256:` prefix:

```sh
python3 deploy/render.py \
  --control-image "${SANDBOX_REGISTRY}/control@sha256:CONTROL_DIGEST" \
  --workspace-image "${SANDBOX_REGISTRY}/workspace@sha256:WORKSPACE_DIGEST" \
  > deploy/api.rendered.yaml
```

The renderer validates image references and writes YAML; it does not contact the
cluster. The rendered file is ignored by Git. Review `deploy/foundation.yaml`
and the rendered Deployment before applying them to the selected cluster:

```sh
kubectl apply -f deploy/foundation.yaml
kubectl apply --dry-run=server -f deploy/api.rendered.yaml
kubectl apply -f deploy/api.rendered.yaml
kubectl -n sandbox-control rollout status deployment/sandbox-api --timeout=10m
kubectl -n sandbox-control port-forward service/sandbox-api 8080:8080
```

The foundation must exist before the namespaced Deployment can be validated.
For an upgrade, apply it first so the API has ConfigMap and Pod update permissions.
The Deployment uses `Recreate` to avoid serving requests through a mixture of old
Pod-only and new claim-aware APIs. This causes a brief control API interruption;
runner commands continue, and clients can reconnect by command ID.
Keep the application API internal: it has no application-level authentication.

The template enables two idle Pods with `--warm-pool-size=2`. Set this to zero to
disable prewarming; reconciliation removes unused pool Pods while active
workspaces retain their claims. The server accepts targets from zero to 16, but
the supplied 12-Pod quota still bounds total capacity. Do not roll back to a
Pod-only API while claim-backed workspaces are active.

Inspect the reserve and controller logs:

```sh
kubectl -n sandbox-workspaces get pods \
  -l 'sandbox-cloud/pool=true,!sandbox-cloud/claim-uid'
kubectl -n sandbox-control logs deployment/sandbox-api --tail=50
```

Successful allocation logs include `source=warm` or `source=cold`. Failed
replenishment appears in reconciliation errors. Process health at `/healthz`
does not imply that the reserve has a ready Pod.

## 5. Execute a workload

With port-forwarding running, use another terminal:

```sh
./bin/sandbox create --ttl 10m
# Replace WORKSPACE_ID with the ID returned by create.
./bin/sandbox put WORKSPACE_ID ./examples/hello .
./bin/sandbox exec --timeout 180s WORKSPACE_ID -- go test ./...
./bin/sandbox destroy WORKSPACE_ID
```

Deletion is asynchronous. Confirm removal with `sandbox list` and, from a
terminal using the cluster's kubeconfig:

```sh
kubectl -n sandbox-workspaces get pods
```

## 6. Acceptance checks

Local fake-client tests cannot establish cluster networking, admission, or
scheduling behavior. Validate the deployed system with these checks:

- Create a workspace and observe scheduling, image pulling, and readiness.
- Upload `examples/hello` and run `go test ./...` with streamed output.
- Run a command that exits nonzero and verify the CLI exit status.
- Disconnect from a long command, reconnect by command ID, then cancel it.
- Restart the API Deployment while a workspace is active; verify inventory and
  command access recover without recreating the workspace.
- Let a short TTL expire; verify the Pod is removed and runner operations fail.
- Verify one workspace cannot connect to another runner and has no mounted
  Kubernetes ServiceAccount credentials.
- Wait for an idle Pod to become Ready, create a workspace, and verify its claim
  points to that Pod. Check that another idle Pod is provisioned afterward.
- Submit concurrent creates and verify each claim has a distinct Pod UID. Repeat
  one request ID and verify it preserves the same assignment and expiration.
- Exhaust the ready reserve and verify a request falls back to cold allocation.
- Reach the namespace quota and inspect failures. Existing workspaces should stay
  usable while replenishment is blocked. Remove test workspaces afterward.

A workspace's physical Pod name is available from its claim:

```sh
kubectl -n sandbox-workspaces get configmap WORKSPACE_ID \
  -o jsonpath='{.data.pod}{"\n"}'
```

Measure request-to-ready latency separately for warm and cold allocations, using
the allocation log to classify each sample. Include pool replenishment time and
quota failures. Local runner benchmarks do not measure this improvement.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Connection refused on port 8080 | Port-forward process and selected kubeconfig |
| Workspace remains pending | Claim phase, reconciliation logs, scheduling events, quota, and node capacity |
| Warm pool stays empty | Pool setting, current image readiness, quota, and reconciliation errors |
| `ImagePullBackOff` | Image digest and node identity's repository access |
| Runner unavailable | Pod readiness, ingress policy, and control-to-workspace routing |
| Workspace busy | Existing command; wait for completion or cancel it |
| Retained command not found | IDs, record eviction, and runner termination |
| GCP permission error | Project ID, active account, and required IAM permissions |
| Pod remains after TTL | API reconciliation logs, API permissions, and cluster availability |

## Teardown

Set the pool target to zero before teardown so deleted idle Pods are not replaced.
Destroy active workspaces and verify their Pods and claim ConfigMaps disappear. To remove the service,
inspect both dedicated namespaces before deleting them; deleting the workspace
namespace also removes every remaining workspace and its ephemeral files.

Namespace deletion does not delete the GKE cluster, registry images, or build
source/log storage. Those resources have independent lifecycles and may continue
to incur charges. Remove only resources dedicated to this deployment.
