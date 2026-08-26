# P4a: Timeout-bounded sandbox delete

**Date:** 2026-08-26
**Branch:** `scion/p4a-delete-v2`
**Base:** `scion/dev-rebase-1294`

## Summary

Applied timeout-bounded `sandbox delete --force` to the existing
`cloudrun_sandbox_runtime.go` on the integration branch. The previous
`Stop()` and `Delete()` methods called `runSimpleCommand(ctx, r.bin,
"delete", "--force", id)` which blocks forever due to a known platform
defect where the sandbox CLI hangs after deletion completes.

## Changes

### `pkg/runtime/cloudrun_sandbox_runtime.go`

1. **Added imports:** `bytes`, `log/slog`, `syscall` for process group
   management and orphan reaping.

2. **Added `DefaultDeleteTimeout` constant:** 10-second timeout for
   `sandbox delete --force`. Value is a conservative guess since the
   command never completes normally.

3. **Added `deleteTimeout` field** to `CloudRunSandboxRuntime` struct
   for configurable timeout (0 = use default).

4. **Added `watchCancels` map** to track per-sandbox watcher cancel
   functions, allowing `deleteWithTimeout` to cancel the watcher when
   a sandbox is force-deleted.

5. **Replaced `Stop()` and `Delete()`** to delegate to `deleteWithTimeout`.

6. **Added `deleteWithTimeout` method:** Starts the delete command in its
   own process group, waits for completion with a timeout, and treats
   timeout as success (the sandbox IS deleted despite the CLI hang).
   On timeout, kills the entire process group and reaps orphaned runsc
   processes.

7. **Added `killProcessGroup` helper:** Sends SIGKILL to the entire
   process group of a command.

8. **Added `reapOrphanedRunsc` helper:** Scans /proc for lingering runsc
   processes associated with a deleted sandbox and kills them.

9. **Updated `watchSandbox`** to accept a context parameter. The watcher
   now exits early if its context is cancelled (by deleteWithTimeout),
   preventing it from hanging on `sandbox wait` after the sandbox is
   deleted.

10. **Updated `Run()`** to create a cancellable context for the watcher
    goroutine and register it in `watchCancels`.

### `pkg/runtime/cloudrun_sandbox_runtime_test.go`

Added 8 new test cases:
- `TestCloudRunSandboxDeleteWithTimeout_NormalCompletion` — process exits cleanly
- `TestCloudRunSandboxDeleteWithTimeout_TimeoutTreatedAsSuccess` — hanging process is killed after timeout
- `TestCloudRunSandboxDeleteWithTimeout_ContextCancellation` — context deadline returns error
- `TestCloudRunSandboxDeleteWithTimeout_ProcessErrorNonFatal` — non-zero exit is logged but not fatal
- `TestCloudRunSandboxDeleteWithTimeout_CancelsWatcher` — watcher cancel is called and removed
- `TestCloudRunSandboxDeleteWithTimeout_DefaultTimeout` — zero value falls back to constant
- `TestKillProcessGroup_NilProcess` — nil process does not panic
- `TestKillProcessGroup_RunningProcess` — running process is killed
- `TestReapOrphanedRunsc_NoProc` — graceful no-op

Updated all existing test struct literals to include the new `watchCancels` field.

## Verification

- `go build ./...` — passes
- `go vet ./pkg/runtime/...` — passes
- `go test ./pkg/runtime/... -run TestCloudRunSandbox -count=1` — all 22 tests pass
- `TestKillProcessGroup` and `TestReapOrphanedRunsc` — pass

## Design decisions

- Used `slog` (not `runtimeLog`) in new helper functions for consistency
  with the brief's specification. The existing watcher and state store
  code continues to use `runtimeLog`.
- Watcher cancellation happens BEFORE the delete command starts, ensuring
  the watcher doesn't observe a partially-deleted sandbox.
- Process error from `delete --force` is non-fatal (logged as warning)
  because the deletion is effective regardless of the CLI exit code.
