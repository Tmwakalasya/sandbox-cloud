# Performance measurements

[Back to README](../README.md)

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

Use [`sandbox-bench`](benchmark.md) for concurrent live-cluster bursts. It
measures HTTP request-to-ready and command round-trip time, classifies allocations
from claim and Pod metadata, checks exclusive assignment, and cleans up its own
workspaces. Reports retain failed attempts alongside per-source p50/p95 and
admitted resource settings. The tool does not provision capacity or change the
pool target; see the guide before running it against a cluster.

In the [recorded GKE run](benchmark.md#recorded-gke-run-november-1-2026), three
bursts of ten workspaces completed 30/30 attempts: six warm allocations had a
494 ms readiness median; 24 cold allocations had an 8,744 ms median. These small,
observational groups include polling and network overhead. The guide records the
failed client-limiter baseline, image digests, admitted resources, and raw reports.
