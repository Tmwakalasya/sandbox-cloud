# Sandbox Cloud

**Disposable Kubernetes workspaces, controlled from a Go CLI.** Upload a project,
run commands with streamed output, and keep files between executions. Destroy the
workspace when finished, or let its TTL expire.

- Animated terminal views, plain-text output, and JSON for scripting.
- Remote command execution with deadlines, cancellation, and output replay.
- Optional warm pool with exclusive Pod claims and automatic replenishment.
- Kubernetes-backed workspace inventory and expiration that survives API restarts.
- Cloud Build and GKE deployment without a local container runtime.

[Deployment guide](deploy/README.md) · [CLI reference](#cli-reference) ·
[Architecture](#architecture) · [Source reading path](#source-reading-path)

![Live GKE demo: create a workspace, upload Go code, test, run, and destroy it](docs/demo.gif)

Recorded against GKE with the warm pool enabled. After building the CLI and
starting the port-forward below, run `vhs demo-v2.tape` to record the same flow
with VHS, ttyd, ffmpeg, and Chrome installed. It writes `demo-v2.gif` and
`demo-v2.mp4` in the repository root.

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

`make build` produces `bin/sandbox`, `bin/sandboxd`, and `bin/sandbox-runner`.
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

The supplied deployment sets `--warm-pool-size=1`. The server flag defaults to
zero, which disables prewarming. The target counts both ready and starting idle
Pods; it does not guarantee that a ready Pod is always available.

A create request first reserves its public ID in a ConfigMap. The allocator
selects a ready Pod with the configured image, then uses Kubernetes
`resourceVersion` checks to assign it exclusively. The claim records the physical
Pod name and UID, so retries and API restarts preserve the assignment. If no ready
Pod is available, or another workspace wins the selected Pod, allocation falls
back to a fresh Pod. CLI commands and HTTP request bodies are unchanged.

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

## Lifecycle and guarantees

### Workspace state

| State | Meaning |
| --- | --- |
| `pending` | Allocation is in progress, or the assigned Pod is unscheduled |
| `starting` | Node assigned; runner is not ready |
| `ready` | Pod is Running with its Ready condition true |
| `terminated` | Pod or runner has terminated |
| `unavailable` | Assigned Pod is missing/replaced, Pod state is Unknown, or lifecycle metadata is invalid |
| `expired` | Absolute expiration timestamp has passed |
| `terminating` | Deletion has been accepted and removal is in progress |

Ownership labels identify managed Pods and claim ConfigMaps. Claims store the
requested image and TTL, allocation phase, Pod name/UID, and expiration. A pending
reservation has two minutes to complete. Reconciliation resumes interrupted
allocations; a failed submission can therefore complete later. Inspect the printed
ID before submitting a different request. Active claims are never reassigned to a
replacement Pod, and retries do not extend their lifetime.

The expiry loop reads Kubernetes state on startup and every five seconds, with a
15-second deadline per reconciliation pass. It checks expiration again when
marking a claim for deletion, because binding can replace the pending deadline
with the user's full TTL. Invalid lifecycle metadata is reported rather than
guessed. Pods from older versions retain their annotation-based expiration.

Deletion checks object UIDs. Idle cleanup also checks `resourceVersion`, protecting
a newly claimed Pod from a stale idle snapshot. Pod owner references and orphan
reconciliation clean up late allocations whose claim was already deleted.

`activeDeadlineSeconds` is a kubelet backstop measured from Pod start. Cold Pods
use the requested TTL. Warm Pods use **24 hours 12 minutes**, covering the maximum
workspace TTL plus idle and allocation time; the shorter user TTL is enforced by
the control API. During a prolonged API outage, a warm workspace can therefore
keep executing past its requested TTL until kubelet cleanup occurs.

Scheduling delays, outages, and termination grace can delay physical cleanup.
**TTL controls admission and cleanup eligibility; physical deletion is eventual
and depends on cluster availability.**

### Execution and concurrency

Each runner admits **one active command per workspace**. A competing command or
file upload receives HTTP 409. A mutex protects admission, command results, and
output, preventing two callers from both observing an idle workspace.

Commands outlive their submitting HTTP requests. Disconnecting or pressing Ctrl-C
in the CLI stops the local stream; explicit cancellation or the runner-owned
deadline stops the remote command. Cancellation kills its Unix process group.
Remaining members of that group are also cleaned up when execution ends. A
process that deliberately detaches into another group is outside the trusted
workload contract; Pod deletion is the broader cleanup boundary.

Output subscribers capture a snapshot and a notification channel under the same
mutex. Writers signal that channel on output and completion. This prevents a
missed wakeup between checking for output and waiting for more.

Results are `running`, `succeeded`, `failed`, `timed_out`, or `cancelled`. An
executable that cannot start returns HTTP 422 and releases the active slot.
Completion and cancellation racing with each other are classified from the
runner's observed process result and context; they have no durable global order.

### Retention and retries

- Each runner retains up to 32 command records in memory.
- Each command retains up to 1 MiB of combined stdout/stderr and 4096 chunks.
  Excess output is drained and discarded, with `truncated` set on the result.
- Reconnecting replays retained output from the beginning before following new
  output. There is no offset-based resume.
- A retained command ID with an identical request returns the existing command;
  different settings return 409. Eviction or runner loss forgets the ID.
- Workspace creation is idempotent while its claim exists. A reused ID with
  different image or TTL settings returns 409. A missing assigned Pod is reported
  as unavailable and is not silently replaced.
- The CLI prints IDs before submission and does not automatically retry an
  uncertain command submission. Inspect the ID after a lost response.

These are bounded deduplication guarantees, not durable exactly-once execution.
Workspace files, command results, and output are lost when the Pod is removed.

## CLI reference

Global flags precede the command; command flags precede positional arguments.
`WORKSPACE` and `COMMAND_ID` below stand for returned IDs.

| Command | Purpose |
| --- | --- |
| `sandbox create --ttl 20m --wait 3m` | Create a workspace and wait for readiness |
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

Use `--wait 0` to return immediately after creation. Interrupting the readiness
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

## HTTP API

| Method and path | Result |
| --- | --- |
| `GET /healthz` | HTTP process health, not Kubernetes connectivity |
| `POST /v1/workspaces` | Create or retrieve a workspace with matching settings |
| `GET /v1/workspaces` | List managed workspaces |
| `GET /v1/workspaces/{id}` | Inspect workspace state |
| `DELETE /v1/workspaces/{id}` | Request asynchronous deletion |
| `PUT /v1/workspaces/{id}/files?path=relative/path` | Upload one file |
| `POST /v1/workspaces/{id}/commands` | Start a command |
| `GET /v1/workspaces/{id}/commands/{command}` | Read a retained result |
| `GET /v1/workspaces/{id}/commands/{command}/events` | Replay and follow NDJSON output |
| `POST /v1/workspaces/{id}/commands/{command}/cancel` | Request cancellation |

Workspace creation body:

```json
{"id":"sbx-0123456789abcdef01234567","ttl_seconds":1200}
```

Command submission body:

```json
{"id":"cmd-0123456789abcdef01234567","command":["go","test","./..."],"timeout_seconds":60}
```

Events carry either output or a final result:

```json
{"stream":"stdout","data":"ok example\n"}
{"stream":"result","result":{"id":"cmd-0123456789abcdef01234567","state":"succeeded","exit_code":0,"started_at":"2026-10-31T12:00:00Z","finished_at":"2026-10-31T12:00:01Z","truncated":false}}
```

Errors use `{"error":"message"}`. Validation errors return 400, missing objects
404, conflicts 409, oversized uploads 413, and process-start failures 422.
Kubernetes permission and quota failures are surfaced to callers. A broken stream
may end without a final result; the CLI reports an unknown outcome.

## Resource and security boundaries

| Workspace setting | Default or limit |
| --- | --- |
| TTL | 20 minutes; range 1 minute–24 hours |
| Command timeout | 60 seconds; range 1 second–1 hour |
| CPU request / limit | 100 millicores / 1 core |
| Memory request / limit | 128 MiB / 512 MiB |
| Ephemeral storage request / limit | 128 MiB / 1 GiB |
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
ResourceQuota caps the workspace namespace at **four Pod objects** and **16 claim
ConfigMaps**. Idle and terminal Pods count until deleted.
There is no application admission queue. See the
[deployment guide](deploy/README.md) for the manifests and capacity limits.

Commands share the runner's UID and normally the worker's kernel. They can
interfere with the runner. Egress is unrestricted, and there is no per-user
authorization or durable audit log. Keep the API internal and use dedicated
namespaces without unrelated credentials or workloads. Ownership labels are not
an authorization boundary against principals allowed to modify those Pods.

## Failure behavior

| Failure | Behavior |
| --- | --- |
| CLI disconnect | Command continues; reconnect using its ID while retained |
| API restart | Pods and commands remain; claims recover allocation and expiration from Kubernetes |
| Empty warm pool | Create a fresh Pod and wait for ordinary Kubernetes readiness |
| Replenishment quota failure | Active workspaces continue; reserve remains below target and an error is logged |
| Runner crash | In-memory results are lost; workspace Pod is not automatically restarted |
| Node loss | Workspace can be unavailable or lost; files have no durable recovery |
| Command timeout | Runner kills the process group independently of the client |
| Image pull failure | Pod waiting reason is exposed; workspace does not become ready |
| Kubernetes unavailable | Operations fail; expiration cleanup resumes when access returns |
| Concurrent submissions | One command is admitted; the other receives 409 |
| Lost submission response | Inspect the printed ID before deciding whether to retry |
| Interrupted or oversized upload | Existing target survives; temporary file is cleaned up |
| Stale cleanup request | UID precondition protects a replacement Pod with the same name |

## Performance

A local benchmark compared a 50 ms output-polling implementation with the current
state-change notifications. Each iteration submits `true` over loopback HTTP,
starts the process, attaches to the event stream, and consumes the final result.
Both versions use the same workload and completion/output contract.

Environment: Apple M4 Pro, macOS/arm64, Go 1.26.0, 12 logical CPUs reported by the
runtime. Three samples of 20 iterations per version, without the race detector:

| Sample | Polling | Notifications |
| --- | ---: | ---: |
| 1 | 54.572 ms/op | 2.065 ms/op |
| 2 | 53.453 ms/op | 1.672 ms/op |
| 3 | 55.959 ms/op | 1.619 ms/op |

The notification version allocated approximately 122–123 KiB and 318–320
allocations per operation. These are small diagnostic samples without a
statistical significance test. They exclude server startup, Kubernetes scheduling,
container startup, image pulls, and cloud networking; they do not measure
workspace startup latency or cloud capacity.

Run the current benchmark:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test ./internal/runner -run '^$' \
  -bench BenchmarkCommandRoundTrip -benchmem -benchtime=20x -count=3
```

Cluster benchmarks should separately measure request-to-ready and command-start
latency, distinguish cold and cached images, and record failures alongside
p50/p95/p99. Record image digest, resource settings, and concurrency. CLI readiness
polling has one-second granularity and is unsuitable for precise startup timing.

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
   create/upload/execute flow; [`ui.go`](internal/cli/ui.go) handles terminal views.
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
