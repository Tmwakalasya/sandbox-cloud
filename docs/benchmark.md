# Live workspace benchmark

`sandbox-bench` starts a burst of workspace requests through the existing HTTP
API, waits for readiness, uploads a workspace-specific file, executes `cat` to
verify its contents, and destroys only that run's workspaces. It keeps the entire
burst alive until every attempt finishes. Kubernetes reads verify each active
claim's Pod UID and collect allocation source, image, node, and admitted resources.

This is an operator tool for a dedicated development deployment. It requires the
CLI build, `kubectl`, an explicit kubeconfig and context, and a running API
port-forward. It refuses to start if existing workspaces or managed claim records
are present. It never edits a deployment, changes quota, or deletes unrelated
workspaces. No container images are downloaded to the machine running the tool.

## Capacity for ten active workspaces

Before running ten concurrent requests, configure capacity for **12 total Pod
objects** and a target of **two idle warm Pods**. At the current per-workspace
limits, a compatible namespace quota is:

| Quota | Value |
| --- | ---: |
| `count/pods` | 12 |
| `count/configmaps` | 16 |
| `requests.cpu`, `limits.cpu` | 12 |
| `requests.memory`, `limits.memory` | 6 GiB |
| `requests.ephemeral-storage`, `limits.ephemeral-storage` | 12 GiB |

These are ceilings, not per-Pod reservations. Idle and terminating Pods count
against quota. The warm target is a soft reserve, not a separate allocation:
requests can consume all available slots. GKE admission may adjust requests, and
node provisioning, available resources, or project quotas can still delay or
prevent readiness. The report records the resources actually admitted.

Set the API's `--warm-pool-size=2` when deploying this capacity. Changing quota or
the pool target is a separate deployment action; running the benchmark does not
perform it. Two warm Pods do not guarantee two warm hits in a concurrent burst:
requests can contend for the same candidate and fall back to cold allocation.
Replenishment may also supply additional candidates during the burst.

The API's Kubernetes client is bounded at 50 requests/second with a burst of 100.
Each workspace operation can issue several Kubernetes requests. The client-go
defaults of 5 requests/second and burst 10 caused nine of ten attempts in the
initial GKE burst to fail while waiting on the client limiter, before the HTTP
handler's 15-second deadline. Raising Pod quota alone did not remove that limit.
With the revised budget, the repeated three-burst GKE run completed all 30
attempts. This does not reserve Kubernetes API capacity or guarantee success at
arbitrary concurrency; the measured setup and limits are recorded below.

## Recorded GKE run: November 1, 2026

GKE Autopilot in `us-central1`, one control replica, two ready reserve Pods before
each burst, ten concurrent requests, three bursts, 250 ms readiness polling.
Each attempt uploaded its own ID and verified it through the same `cat` command.

| Allocation | Samples | Readiness p50 | Readiness p95 |
| --- | ---: | ---: | ---: |
| Warm | 6 | 494 ms | 874 ms |
| Cold | 24 | 8,744 ms | 13,206 ms |

All 30 attempts passed readiness, execution, and distinct Pod assignment checks.
All benchmark claims and owned Pods were removed. The reserve returned to two
ready Pods after every burst. Warm hits were 6/30 (20%); no allocations were
unclassified. The initial run with the default client limiter completed only one
of ten attempts; the other nine reported client-limiter deadline errors. Both
reports retain the failures and cleanup evidence:

- [Initial limiter baseline](benchmarks/gke-burst-before.json)
- [Three bursts after the limiter fix](benchmarks/gke-burst-after.json)

All measured workspaces requested 100m CPU, 128 MiB memory, and 128 MiB ephemeral
storage after GKE admission. Their admitted limits were one CPU, 512 MiB memory,
and 128 MiB ephemeral storage. The two observed node names and per-workspace
resource settings are retained in the report. The workspace image was held at
`sha256:c77fb2929924e2f67cb45cd0432e59e2f9c99817fde9976ad12ce5634f51ceed`;
the fixed control image was
`sha256:565e36093054e08017f8f87458076a7902dcbfdf6efe5398a4b98ec7f41c50e0`.

These are observed warm/cold groups within bursts, not randomized paired trials.
Six warm samples are too few for a stable tail-latency estimate. Image-cache and
node-provisioning states were not controlled. Treat the measurements as evidence
for this workload, not a general speedup claim or maximum-capacity result.

## Run

Build locally using cached Go dependencies:

```sh
make build
mkdir -p .local
```

Keep the API port-forward running with the same explicit cluster selection:

```sh
kubectl --kubeconfig "$HOME/.kube/sandbox-cloud.config" \
  --context YOUR_CONTEXT -n sandbox-control \
  port-forward service/sandbox-api 8080:8080
```

Run three bursts of ten requests:

```sh
./bin/sandbox-bench \
  --kubeconfig "$HOME/.kube/sandbox-cloud.config" \
  --context YOUR_CONTEXT \
  --count 10 --rounds 3 --idle 2 \
  --output .local/burst-10.json
```

The output path must not already exist. Progress goes to stderr; the JSON report
contains all attempts and is written even after a failed or interrupted run.
Reports contain workspace IDs, node names, context, and registry references;
review them before publishing. Use a new output filename for each run.

For a demo, add `--rounds 1 --hold 15s` and run `sandbox watch` in another terminal.
The hold happens after measurement, while all successfully created workspaces
remain alive. `--help` lists timeouts and polling controls.

## What the numbers mean

- **Request to ready:** client monotonic time from sending create to observing a
  ready response. HTTP, control-plane work, scheduling, startup, and readiness
  detection are included. Polling defaults to 250 ms; network and server latency
  add uncertainty. Observer reads happen after the timed work to limit interference.
- **Command round trip:** submitting the same `cat benchmark-id` command and
  consuming its successful final event. Upload time is excluded. The expected
  output is the exact ID of that workspace; a nonzero exit or mismatch fails it.
- **Warm/cold:** classified from the assigned Pod's pool label, checked against
  its claim's UID, Pod UID, and ownership. Missing or inconsistent evidence fails
  verification and remains `unknown`; latency never determines the category.
- **p50/p95:** nearest-rank percentiles of observed readiness, split by source.
  A workspace that never became ready is excluded from latency percentiles and
  retained as a failed attempt. Readiness remains recorded if a later execution
  check fails. Sample counts accompany every distribution.
- **Warm hit rate:** warm allocations divided by warm plus cold allocations.
  Unknown allocations and failed attempts are reported separately.
- **Reserve recovery:** time spent waiting for the requested minimum ready
  reserve **after cleanup completes**. This includes observation overhead and is
  not an isolated measurement of Pod startup or the controller's refill latency.

Three bursts are a diagnostic sample, especially for warm p95. Cold allocation
means a newly created Pod, not an uncached image or a new node. GKE may reuse
cached images or provision nodes during a run; do not label this as a controlled
speedup comparison. Keep command, image, resources, concurrency, polling, and
cluster conditions equivalent when comparing runs. A pool-disabled baseline needs
a separate deployment with target zero; `--idle 0` only disables reserve waiting.

## Failure and cleanup

Creates are submitted once, with a random ID generated before the request. An
error is recorded rather than retried into a successful measurement. Cleanup
includes every attempted ID, including a create whose response was lost.

After the whole burst finishes, cleanup requests run with their own two-minute
deadline, including on Ctrl-C. The tool checks that its claim records and owned
Pods disappear. It preserves cleanup errors, stops subsequent rounds on failure,
and exits nonzero. Each workspace also has a 15-minute TTL by default; abrupt
process termination can prevent the cleanup pass and report write, so TTL is the
backstop. Do not infer cleanup success solely from a DELETE acknowledgement.

The harness waits for the reserve before and after each burst. It stops if the
reserve cannot recover within `--timeout` (five minutes by default), rather than
silently continuing with different initial conditions.
