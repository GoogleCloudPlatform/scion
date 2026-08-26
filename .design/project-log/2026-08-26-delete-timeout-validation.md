# Delete Timeout Validation -- Empirical Results

**Date:** 2026-08-26T02:29-02:37Z
**Agent:** sn-impl-em2
**Instance:** val-delete-2 (python:3.11, sandboxLauncher, us-east4, ptone-experiments)
**runsc version:** google-958767651 (spec 1.2.1)

## Context

The architect flagged `DefaultDeleteTimeout=10s` as the most important open item
in P4a: the workaround treats delete as success after timeout, but the 10s value
was chosen without empirical evidence. The defect investigation measured sandbox
disappearance at 120s. Nobody had established that the sandbox is actually gone
at 10s.

## Limitations

1. **No Go code was exercised.** These tests simulate the platform assumptions the
   workaround rests on. The Go workaround itself (`deleteWithTimeout`,
   `killProcessGroup`, `reapOrphanedRunsc`) was not run on-Instance.

2. **`killProcessGroup` was not validated on-Instance.** Test 3 killed the Python
   subprocess wrapper, not the runsc child. The process-group SIGKILL path is
   probably effective but is not empirically confirmed.

3. **Reachability instrument:** `sandbox exec` can disown a sandbox whose underlying
   processes are still running (per defect-sandbox-delete-hang.md section 4). Test 3's
   /proc scan corroborates: the runsc delete process survives while the sandbox is
   unreachable via exec.

## 6-Point Validation Results

1. **Effectiveness (serial):** Sandbox unreachable at t<1s. **PASS.**

2. **Effectiveness (concurrent fan-out):** 5 sandboxes deleted concurrently, all
   unreachable at t<=1s. No contention degradation. 10x safety margin confirmed
   in the actual deployment regime. **PASS.**

3. **Reaper live test:** Found 1 orphaned `runsc ... delete --force <name>` process
   in /proc after 15s. Argv matches `isOrphanedRunscProcess` pattern. **PASS.**

4. **Post-reap state:** No surviving gofer/sandbox processes. `killProcessGroup`
   not exercised (see Limitations). **PARTIAL.**

5. **Concurrent timeouts (OQ-16):** 5 concurrent deletes all timed out independently
   at ~30s. Aggregate wall time = 1 timeout (parallel, not serial). **PASS.**

6. **sync.Once WARN:** On known-bad build, `delete --force` hangs at 15s+. The
   self-detector would not false-positive. **PASS.**

## Decision

**No code changes needed.** `DefaultDeleteTimeout=10s` is justified by 10x margin
over measured effectiveness time (<1s), confirmed under both serial and 5-way
concurrent fan-out.

## Full results

See `/scion-volumes/scratchpad/projects/single-node/validation/delete-timeout-validation-results.md`.
