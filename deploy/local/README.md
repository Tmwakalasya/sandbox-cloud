# Local development with OrbStack + k3d

Run Sandbox Cloud on your own machine. No GCP account, remote image registry, or
cloud compute is needed. Initial setup downloads images and Go dependencies.
Only trusted development code is supported; this is not hostile-code isolation.

## Prerequisites

- OrbStack running with its Docker engine selected. Check `docker context show`
  and `docker info`. If needed, select it yourself with `docker context use orbstack`.
  The scripts never change your Docker context.
- k3d v5.8+ and kubectl v1.35 (or a version compatible with Kubernetes 1.35).
- Python 3.9+, Make, and Go 1.26 for building the host CLI.
- Available loopback ports **6550** (Kubernetes), **5001** (image registry),
  and **8080** (when port-forwarding).

On macOS, install missing tools with your preferred package manager, for example
`brew install k3d kubectl python go`. Check that Go is version 1.26.
Do not enable OrbStack's own Kubernetes just for this project; k3d provides the cluster.

## Start

From the repository root:

```sh
GOTOOLCHAIN=local go mod download
make build
make local-up
```

`local-up` creates one K3s server container and a small local registry, builds both
Dockerfile targets for the Docker engine's native architecture, pushes them to
`127.0.0.1:5001`, and deploys immutable digests. Apple Silicon does not require
amd64 emulation. The pinned K3s image is `rancher/k3s:v1.35.2-k3s1`.

The registry container is named `sandbox-cloud-registry.localhost`, exactly as
specified in `k3d.yaml`. Images inside the cluster use
`sandbox-cloud-registry.localhost:5000`; k3d configures its HTTP registry mirror.
Do not add a `k3d-` prefix to this explicitly configured registry name.

The Kubernetes API and registry bind to **127.0.0.1**, not all host interfaces.
The local overlay reuses `deploy/foundation.yaml`; the helper renders it with
Kustomize's `LoadRestrictionsNone` to permit that checked-in parent-file reference,
then applies the result. This changes file loading, not cluster security policies.
The API stays inside the cluster. In another terminal:

```sh
make local-port-forward
```

Then:

```sh
./bin/sandbox create --ttl 10m
# Use the returned ID:
./bin/sandbox put WORKSPACE_ID ./examples/hello .
./bin/sandbox exec --timeout 180s WORKSPACE_ID -- go test ./...
./bin/sandbox destroy WORKSPACE_ID
```

The local quota admits **one workspace Pod**, with its usual one-core/512 MiB
limits and 1 GiB ephemeral-storage budget. It reserves no warm Pods. A second
allocation can fail on quota; there is no waiting queue. Failed allocation claims
may briefly remain until reconciliation. This is a small development environment,
not a performance benchmark or a hard memory cap on OrbStack itself.

## Rebuild, stop, delete

```sh
make local-up                    # reuse cluster, rebuild images, roll out API
make local-stop                  # stop runtime; interrupts active work
make local-up                    # start again and rebuild
make local-down CONFIRM=yes      # DELETE owned cluster and registry
```

Repeated `local-up` does not delete workspace Pods or reset their TTL. Existing
workspaces keep their original image; new ones use the rebuilt digest. API rollout
briefly interrupts connections, so restart your foreground port-forward afterward.
Stopping the cluster is not a reliable way to preserve active work or ephemeral
files. TTL may have expired when you restart.

Teardown deletes the project cluster and local registry only. It does not prune
Docker images/build caches, delete other clusters, or touch GKE. Removing an old
GKE deployment is a separate action.

All generated data lives under Git-ignored `.local/`: private kubeconfig, ownership
record, rendered Deployment, and smoke-check logs. Default kubeconfig and current
context stay unchanged. Commands use `k3d-sandbox-cloud-local` explicitly and verify
the Docker engine ID against the ownership record. Keep that record; the helper
refuses to adopt an identically named existing cluster without it. Run lifecycle
commands from one checkout.

For manual inspection:

```sh
kubectl --kubeconfig .local/kubeconfig --context k3d-sandbox-cloud-local get nodes
kubectl --kubeconfig .local/kubeconfig --context k3d-sandbox-cloud-local \
  -n sandbox-workspaces get pods,configmaps,resourcequota
kubectl --kubeconfig .local/kubeconfig --context k3d-sandbox-cloud-local \
  -n sandbox-control logs deployment/sandbox-api
```

## Validation

```sh
make deploy-check    # Python renderer/lifecycle unit tests, no cluster
make check           # existing Go build, vet, tests, race tests, plus deploy-check
make local-check     # real CLI smoke test, requires running cluster and built CLI
```

`local-check` opens its own temporary loopback port-forward. Stop any workloads
first; it refuses to run when existing workspaces are listed. It creates a
workspace, uploads the hello example, runs its Go tests, checks exit code 7 and a
timeout, restarts the API and inspects the surviving workspace, verifies deletion,
then checks physical cleanup after a one-minute TTL. Allow several minutes.
It does not claim to validate all networking or crash behavior.

Before relying on the local environment, also run the applicable
[deployment acceptance checks](../README.md#6-acceptance-checks):

- Inspect host bindings with `docker port k3d-sandbox-cloud-local-server-0`
  and `docker port sandbox-cloud-registry.localhost`.
- Confirm no warm Pods exist after setup and repeated `local-up`.
- Keep a workspace active, repeat `local-up`, then inspect it and execute a new command.
- Request another workspace while the first exists; inspect the quota error and
  confirm the first remains usable.
- Submit a long-running command, disconnect, reconnect with its command ID, and
  cancel it. Confirm the process stops.
- Verify a workspace has no ServiceAccount token. Use a temporary, unlabelled
  probe Pod (not carrying `app: sandbox-api`) to confirm access to a workspace's
  port 8090 is blocked, while ordinary CLI execution through the API still works.
  Remove the probe afterward. Do not disable K3s's network-policy controller.
- Compare your default kubeconfig/context before and after setup; they should not change.

These require a real Docker/k3d environment. Passing the unit tests alone does
not establish image pulling, scheduling, NetworkPolicy enforcement, or OrbStack compatibility.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Docker connection fails | OrbStack running, correct Docker context, `docker info` |
| Existing cluster refused | Keep `.local/owner.json`; use the original checkout and engine |
| Port in use | Stop the unrelated listener or adjust the checked-in local config and helper together |
| `ImagePullBackOff` | Registry running, Docker push succeeded, Pod image references `sandbox-cloud-registry.localhost:5000` |
| Go build fails | Go 1.26 available; dependencies downloaded before `make build` |
| Workspace pending | Pod events, node disk/memory, namespace quota, old terminating workspace |
| Stream ends after API rollout | Restart `make local-port-forward`, reconnect using command ID |
| Registry left after failed create | `make local-down CONFIRM=yes`, then retry `make local-up` |

### Recover an older registry-name mismatch

An earlier helper incorrectly added `k3d-` to the explicit registry name. This
caused `ImagePullBackOff` for images beginning with
`k3d-sandbox-cloud-registry.localhost:5000/`; a repeated `local-up` could also
incorrectly report a missing registry.

Keep the existing cluster, registry, and `.local/owner.json`. From your checkout,
update the helper and rerun setup:

```sh
git pull --ff-only origin main
make local-up
make local-check
```

The corrected helper reuses the same registry and deploys digest-pinned images
with its actual name. No cluster reset, registry rename, or TLS bypass is needed.

## Website

The landing page is independent of Kubernetes and the unauthenticated API:

```sh
make site             # http://127.0.0.1:3000
make site-check       # static-site tests; requires Node.js 18+
```

Its terminal preview is a labelled simulation, not a live build.
