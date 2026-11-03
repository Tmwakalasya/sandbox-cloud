# Sandbox Cloud

**Disposable Kubernetes workspaces, controlled from a Go CLI.** Run a public
repository at a pinned commit, or upload local files and execute commands in a
workspace. Stream results, reuse files between commands, and clean up explicitly
or through a TTL.

- Animated terminal views, plain-text output, and JSON for scripting.
- Remote command execution with deadlines, cancellation, and output replay.
- Optional warm pool with exclusive Pod claims and automatic replenishment.
- Kubernetes-backed workspace inventory and expiration that survives API restarts.
- Cloud Build and GKE deployment without a local container runtime.

[Deployment](deploy/README.md) · [Architecture](#architecture) ·
[CLI](#cli-reference) · [HTTP API](docs/api.md) ·
[Lifecycle guarantees](docs/lifecycle.md) · [Benchmarks](docs/performance.md)

Sandbox Cloud is designed for trusted development workloads. It does not provide
hostile-code isolation or a multi-user authentication layer.

## Getting started

### Build the CLI

Requires Go 1.26 and a Unix-like host. The runner supports Linux and macOS process
behavior; deployed workspaces run on Linux.

```sh
GOTOOLCHAIN=local go mod download
make build
./bin/sandbox preview
```

`make build` produces `bin/sandbox`, `bin/sandboxd`, `bin/sandbox-runner`, and
the operator benchmark tool `bin/sandbox-bench`.
`preview` runs an animated simulation offline. Build and check targets disable
module and toolchain downloads, so dependencies must be installed first.

### Run a remote workload

Follow the [GKE deployment guide](deploy/README.md) to build images remotely and
install the control API. With the cluster's kubeconfig selected, keep this running:

```sh
kubectl -n sandbox-control port-forward service/sandbox-api 8080:8080
```

In another terminal:

```sh
./bin/sandbox create --ttl 20m

# Replace the value with the ID returned by create.
WORKSPACE=sbx-0123456789abcdef01234567
./bin/sandbox put "$WORKSPACE" ./examples/hello .
./bin/sandbox exec --timeout 180s "$WORKSPACE" -- go test ./...
./bin/sandbox inspect "$WORKSPACE"
./bin/sandbox destroy "$WORKSPACE"
```

The CLI defaults to `http://127.0.0.1:8080`. Override it with `SANDBOX_API` or
`--api`. HTTP is accepted only for loopback addresses; remote URLs require HTTPS.
The application API has no authentication and is intended to stay internal,
accessed through Kubernetes port-forwarding.

### Run a repository at a pinned commit

For a public HTTPS repository, `run` owns the workspace lifecycle:

```sh
./bin/sandbox run \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --ref FULL_40_CHARACTER_COMMIT_SHA \
  --timeout 5m -- go test ./...
```

It creates a workspace, waits for readiness, fetches the exact commit at depth
one, checks out detached HEAD, executes the command inside the checkout, streams
output, and requests workspace deletion. Branches, tags, embedded credentials,
and repository URLs with query strings are rejected before allocation. The remote
Git server must allow fetching the requested commit. Command arguments are passed
literally; use an explicit `sh -c` for shell syntax.

The default `--wait 3m` bounds allocation and readiness. `--timeout 5m` bounds Git
setup and execution together. `--ttl 20m` is the cleanup backstop; it must cover
wait + timeout + one minute. Cleanup also runs after failures and Ctrl-C, using a
separate 30-second deadline, including when the create response is lost. Deletion
is asynchronous: the CLI reports a deletion request, not verified Pod removal.
Abrupt process termination can prevent cleanup; TTL still applies.

Use `--keep-on-failure` to retain a failed or interrupted workspace until its TTL
for inspection. Successful runs are still deleted. Interrupted retained commands
may continue until their execution deadline. Workspace and command IDs are printed
before requests. `sandbox inspect`, `logs`, and `result` work while the retained
workspace exists. With default cleanup, output must be captured by the caller;
there is no persistent run history after deletion.

Global `--json` emits NDJSON lifecycle records (`allocating`, `executing`, and
`cleanup` or `retained`) alongside the existing runner output/result events. A
successful command result can still be followed by a cleanup failure and nonzero
CLI exit status. Git hooks, interactive authentication, submodules, redirects,
and Git LFS downloads are not enabled by this workflow. Dependencies are prepared
only if the submitted command does so. Warm Pods accelerate allocation; repository
and dependency caches are not part of this version. Run only trusted code.

## Architecture

```text
                         +-----------------------+
                         | sandbox (Go CLI)      |
                         +-----------+-----------+
                                     |
                         HTTP / kubectl port-forward
                                     |
                                     v
                   +------------------------------------+
                   | sandboxd (control API)             |
                   | allocate | proxy | reconcile       |
                   +--------+------------------+--------+
                            |                  |
             client-go / ServiceAccount        | HTTP :8090
                            |                  |
                            v                  v
 +----------------------------------+   +-------------------------------+
 | Kubernetes API                   |   | Workspace Pod                 |
 |                                  |   |                               |
 | Claim ConfigMaps                 |   | sandbox-runner                |
 | workspace ID -> Pod name + UID   |   |     |                         |
 |                                  |   |     +-> command process group |
 | Pod specs and status             |-->|     +-> /workspace (emptyDir)  |
 +----------------------------------+   +-------------------------------+
```

| Binary | Responsibility | State |
| --- | --- | --- |
| `sandbox` | Terminal UI, uploads, command submission, output display | No authoritative execution state |
| `sandboxd` | Workspace allocation, runner routing, pool and expiry reconciliation | Claim ConfigMaps and Pods in Kubernetes |
| `sandbox-runner` | Command execution, files, output, results | Runner memory and the workspace volume |

The API uses its in-cluster ServiceAccount through `rest.InClusterConfig()`.
The CLI talks to the application API, not directly to Kubernetes. There is no
application database, custom resource, or persistent command queue.

### Why a Pod per workspace?

A workspace stays alive across multiple commands, so its lifecycle differs from
a run-to-completion Job. Each workspace receives a dedicated Pod with
`restartPolicy: Never`. Automatically replacing a failed workspace would lose its
files and command history while appearing to recover the same workspace.

The control API runs as a Deployment: replacing it restores service because
Kubernetes owns the workspace inventory. Existing runner processes continue
independently. A workspace receives either an unused ready Pod from the warm pool
or a newly created Pod.

### Warm pool

The supplied deployment sets `--warm-pool-size=2`. The server flag defaults to
zero, which disables prewarming. The target counts both ready and starting idle
Pods; it does not guarantee that a ready Pod is always available.

A create request first reserves its public ID in a ConfigMap. The allocator
selects a ready Pod with the configured image, then uses Kubernetes
`resourceVersion` checks to assign it exclusively. The claim records the physical
Pod name and UID, so retries and API restarts preserve the assignment. If no ready
Pod is available, or another workspace wins the selected Pod, allocation falls
back to a fresh Pod.

For warm workspaces, the user TTL begins when the claim becomes active. Cold
workspaces retain a TTL measured from Pod creation. Idle Pods are hidden from
`sandbox list`, cannot be accessed through workspace endpoints, and are never
returned to the pool after use. Destruction removes the Pod and its files.

A successful claim wakes replenishment immediately; reconciliation also runs on
startup and every five seconds. Idle Pods rotate after ten minutes. Failed,
retired, excess, and outdated-image idle Pods are removed. Claim and cleanup writes
use version checks so cleanup cannot delete a Pod that was just assigned.

The target is a soft reserve. Concurrent controllers can temporarily overfill it;
subsequent passes remove excess idle Pods, and the namespace quota remains the
hard capacity bound. Idle Pods consume resources and count toward that quota.
Replenishment failures are logged and do not invalidate active workspaces.

### Request flow

1. The CLI generates and prints a workspace ID before submitting the request.
   The API reserves a claim and assigns a warm or new Pod. A successful response
   means the claim has a Pod; only a `ready` response establishes readiness.
2. The CLI polls once per second until the Pod is ready or its wait expires.
   Scheduling and container waiting reasons are surfaced in workspace status.
3. The API forwards uploads and commands to the ready runner's private Pod IP.
   Uploads are confined to `/workspace` and committed with a temporary file and
   rename.
4. The runner admits one command at a time and starts a process group. A separate
   NDJSON stream replays retained output, follows new output, and ends with a
   command result.
5. Later commands see files left by earlier commands. At expiration, the API
   rejects new runner operations and the expiry loop requests Pod deletion.
   Removing the Pod also removes its ephemeral volumes.

### Execution guarantees

Each runner accepts one active command at a time; competing commands and uploads
receive HTTP 409. Commands survive client disconnects and API restarts, but runner
results and workspace files are lost when the Pod is removed. IDs support bounded
deduplication, not durable exactly-once execution.

TTL controls admission and cleanup eligibility. Physical deletion is eventual and
depends on cluster availability. See [lifecycle and failure behavior](docs/lifecycle.md)
for claim recovery, cancellation, retention limits, and warm-Pod deadline semantics.

## CLI reference

Global flags precede the command; command flags precede positional arguments.
`WORKSPACE` and `COMMAND_ID` below stand for returned IDs.

| Command | Purpose |
| --- | --- |
| `sandbox create --ttl 20m --wait 3m` | Create a workspace and wait for readiness |
| `sandbox run --repo HTTPS_URL --ref COMMIT -- COMMAND [ARGS...]` | Checkout a pinned commit, execute, and request cleanup |
| `sandbox list` | List managed workspaces |
| `sandbox inspect WORKSPACE` | Inspect state, expiration, node, and resources |
| `sandbox watch [WORKSPACE]` | Watch one workspace or the inventory |
| `sandbox put WORKSPACE LOCAL_PATH REMOTE_PATH` | Upload a file or directory |
| `sandbox exec --timeout 60s WORKSPACE -- COMMAND [ARGS...]` | Execute and attach to output |
| `sandbox result WORKSPACE COMMAND_ID` | Retrieve a retained command result |
| `sandbox logs WORKSPACE COMMAND_ID` | Replay output and follow a running command |
| `sandbox cancel WORKSPACE COMMAND_ID` | Cancel remote execution |
| `sandbox destroy WORKSPACE` | Request workspace deletion |
| `sandbox preview` | Run the offline terminal simulation |

For `create`, use `--wait 0` to return immediately after creation. Interrupting its readiness
wait or exiting `watch` does not delete the workspace. Commands are executable
and argument arrays; shell syntax requires an explicit `sh -c`.

Interactive views use Bubble Tea and Lip Gloss. Non-interactive output uses
plain text. Global `--json` disables animations and emits JSON; execution and log
streams use NDJSON. IDs and diagnostics go to stderr. Remote exit codes 1–125
propagate to the CLI; other unsuccessful outcomes use a nonzero fallback. A local
interrupt exits 130.

```sh
sandbox --json list
sandbox --json create --ttl 10m --wait 0
sandbox --api http://127.0.0.1:8080 inspect WORKSPACE
```

Uploads accept regular files and directories, reject symlinks, and resolve remote
paths relative to `/workspace`. Files become mode 0644. Empty directories are not
preserved, and no ignore rules are applied. Each file replacement is atomic; a
directory upload is sequential and can leave earlier files if a later file fails.

## Resource and security boundaries

| Workspace setting | Default or limit |
| --- | --- |
| TTL | 20 minutes; range 1 minute–24 hours |
| Command timeout | 60 seconds; range 1 second–1 hour |
| CPU request / limit | 100 millicores / 1 core |
| Memory request / limit | 128 MiB / 512 MiB |
| Ephemeral storage request / limit | 1 GiB / 1 GiB |
| `/workspace` / `/tmp` volume limits | 512 MiB / 256 MiB |
| Upload maximum | 8 MiB per file |
| Runtime UID/GID | 65532 |
| Termination grace | 5 seconds |

Workspace Pods run without mounted ServiceAccount tokens, service-link environment
variables, Linux capabilities, or privilege escalation. They use RuntimeDefault
seccomp and a read-only root filesystem; `/workspace` and `/tmp` remain writable.
Storage limits rely on kubelet accounting and enforcement rather than per-write
filesystem quotas. Cluster admission can adjust resource requests.

The supplied deployment separates control and workspace namespaces, restricts
runner ingress to control API Pods, and grants the API namespaced Pod
`create/get/list/update/delete` permissions on Pods and ConfigMaps. Its
ResourceQuota caps the workspace namespace at **12 Pod objects** and **16 ConfigMap
objects**, including Kubernetes-created ConfigMaps. Idle and terminal Pods count
until deleted.
There is no application admission queue. See the
[deployment guide](deploy/README.md) for the manifests and capacity limits.

Commands share the runner's UID and normally the worker's kernel. They can
interfere with the runner. Egress is unrestricted, and there is no per-user
authorization or durable audit log. Keep the API internal and use dedicated
namespaces without unrelated credentials or workloads. Ownership labels are not
an authorization boundary against principals allowed to modify those Pods.

## Performance

Three recorded GKE bursts of ten workspaces completed 30/30 attempts. Six warm
allocations had a 494 ms request-to-ready median; 24 cold allocations had an
8,744 ms median. These are small observational samples, including network and
polling overhead, rather than a general latency guarantee.

See [measurements and methodology](docs/performance.md) and the
[live-cluster benchmark guide](docs/benchmark.md) for raw reports, resource
settings, failure counts, and reproduction commands.

## Development

```sh
make check
```

Runs `go build ./...`, `go vet ./...`, `go test ./...`, and `go test -race ./...`.
The integration suite exercises CLI → HTTP API → runner with a fake Kubernetes
client, loopback servers, and real local subprocesses. Runner tests cover
serialization, output bounds, uploads, deadlines, cancellation, and subscribers;
cloud tests cover ownership, concurrent claims, interrupted allocations, expiry,
and stale deletions. Their fake API explicitly models version conflicts and delete
preconditions; it does not replace validation against a real API server.

These tests do not exercise cluster admission, scheduling, image pulls, IAM, or
NetworkPolicy enforcement. Use the [deployment acceptance checks](deploy/README.md#6-acceptance-checks)
to validate those boundaries on GKE.

### Source reading path

1. [`internal/cli/cli.go`](internal/cli/cli.go): command dispatch and the
   create/upload/execute flow; [`run.go`](internal/cli/run.go) owns pinned repository
   runs and cleanup; [`ui.go`](internal/cli/ui.go) handles terminal views.
2. [`internal/client/client.go`](internal/client/client.go): HTTP requests,
   deadlines, event decoding, and final-result handling.
3. [`internal/cloud/claims.go`](internal/cloud/claims.go), then
   [`pool.go`](internal/cloud/pool.go): allocation, exclusive assignment, recovery,
   and replenishment. [`kubernetes.go`](internal/cloud/kubernetes.go) resolves
   workspace IDs to Pods and reconciles expiration.
4. [`internal/runner/runner.go`](internal/runner/runner.go) and
   [`process_unix.go`](internal/runner/process_unix.go): admission, notifications,
   file writes, and process-group cleanup.
5. [`internal/cli/integration_test.go`](internal/cli/integration_test.go): a complete
   execution trace across the components.

Entry points under `cmd/` wire configuration, signals, and HTTP server shutdown.
