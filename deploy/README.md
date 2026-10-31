# GKE deployment runbook

Target: project `my-dev-server-507301`, cluster `sandbox-cluster1`, region
`us-central1`, DNS endpoint. These files are prepared, **not applied**. Building
and deploying incurs cloud usage; review the operations and identities first.

## What will change

- A regional Docker repository named `sandbox-cloud` (if not already present).
- A remote Cloud Build invocation uploading the allowlisted source files only.
- `sandbox-control` and `sandbox-workspaces` namespaces.
- One control API Deployment and one internal ClusterIP Service.
- A ServiceAccount with only create/get/list/delete on workspace Pods.
- Workspace quota: four Pod objects total, four CPU limits and 2 GiB memory limits.
- Ingress policies: only control API Pods can reach runners; ordinary Pod ingress
  to the control API is denied. Use Kubernetes port-forward for laptop access.

Egress remains unrestricted for trusted workloads and dependency downloads. This
is not a hostile-code deployment. Do not place unrelated workloads or credentials
in these namespaces. Count quota includes terminal Pods until deletion.
Autopilot may adjust requests; inspect admitted resource values before load tests.

## 1. Connect using an isolated kubeconfig

The auth plugin must be installed and its SDK bin directory available on PATH.
The following commands do not switch contexts in the default kubeconfig:

```sh
export PATH="$(gcloud info --format='value(installation.sdk_root)')/bin:$PATH"
export KUBECONFIG="$HOME/.kube/sandbox-cloud.config"
gcloud container clusters get-credentials sandbox-cluster1 \
  --region=us-central1 --project=my-dev-server-507301 --dns-endpoint
kubectl get namespaces
```

## 2. Review build identity and registry permissions

Before submitting, identify the Cloud Build service account and grant only the
required source-bucket read, log-writing and repository write permissions. Ensure
the GKE node identity can read the repository. Do not grant project-wide Editor
as a shortcut. Exact IAM bindings depend on the project's existing identities and
must be inspected and approved separately.

After approval, create the repository if it does not exist:

```sh
gcloud artifacts repositories create sandbox-cloud \
  --repository-format=docker --location=us-central1 \
  --project=my-dev-server-507301 --immutable-tags
```

## 3. Build remotely

```sh
gcloud builds submit . --config=cloudbuild.yaml \
  --region=us-central1 --project=my-dev-server-507301
```

Cloud Build builds and pushes both images. Docker is never invoked locally.
Record the returned build ID and both image digests. The Dockerfile pins Go to
1.26.0 but base tags themselves are mutable; pin their resolved digests before a
production release. This is a development baseline, not a hardened image supply
chain. Builds run the Go tests inside the remote builder.

## 4. Render and review the deployment

Replace DIGEST with each actual 64-character digest (no angle brackets):

```sh
python3 deploy/render.py \
  --control-image us-central1-docker.pkg.dev/my-dev-server-507301/sandbox-cloud/control@sha256:DIGEST \
  --workspace-image us-central1-docker.pkg.dev/my-dev-server-507301/sandbox-cloud/workspace@sha256:DIGEST \
  > deploy/api.rendered.yaml
```

Review `foundation.yaml` and the rendered deployment. After approval:

```sh
kubectl apply -f deploy/foundation.yaml
kubectl apply --dry-run=server -f deploy/api.rendered.yaml
kubectl apply -f deploy/api.rendered.yaml
kubectl -n sandbox-control rollout status deployment/sandbox-api --timeout=10m
kubectl -n sandbox-control port-forward service/sandbox-api 8080:8080
```

Namespace creation must precede namespaced server-side dry runs. Verify the
policies, quota and RBAC exist before starting workspaces. Never use a public
LoadBalancer for this unauthenticated application API.

## 5. First execution and cleanup

In another terminal:

```sh
./bin/sandbox create --ttl 10m
# Use the real returned workspace ID:
./bin/sandbox put WORKSPACE_ID ./examples/hello .
./bin/sandbox exec --timeout 180s WORKSPACE_ID -- go test ./...
./bin/sandbox destroy WORKSPACE_ID
```

Then verify timeout/cancellation, API-restart recovery, expiration, quota
rejection and cross-workspace ingress denial as listed in the root README.
Deletion is asynchronous. Confirm Pods disappear. Keep the first test small.

Teardown is a separate destructive operation: inspect both namespaces before
requesting their deletion. Registry images, build/source storage and the GKE
cluster survive namespace deletion and may continue incurring charges. Do not
delete unrelated resources from the existing project.
