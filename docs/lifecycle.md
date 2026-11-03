# Lifecycle and guarantees

[Back to README](../README.md)

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
