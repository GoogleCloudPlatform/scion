# Isolate delete --force workaround for clean removal

**Date:** 2026-08-26
**Phase:** P4a amendment
**Author:** sn-impl-em2

## What changed

Restructured the `sandbox delete --force` workaround code so it lives in a
single file (`pkg/runtime/cloudrun_sandbox_delete_workaround.go`) that can be
`git rm`'d when the upstream platform bug is fixed.

### Changes

1. **File isolation.** Moved `DefaultDeleteTimeout`, `deleteWithTimeout`,
   `killProcessGroup`, `reapOrphanedRunsc` into a new workaround file with
   comprehensive header documenting exit criteria, known-bad runsc build
   (`google-958767651`), and removal instructions.

2. **Conditional reaping (safety fix).** `reapOrphanedRunsc` now only runs
   in the timeout branch of the select — never when the delete completed
   normally. Previously it ran unconditionally, which would SIGKILL a healthy
   in-flight `runsc` operation the moment the platform bug is fixed.

3. **Self-detecting.** Added a `sync.Once` WARN log when delete returns
   normally, signaling that the upstream defect may be fixed. This is the
   primary removal trigger since there is no public bug to watch.

4. **Runtime kill switch.** `SCION_CLOUDRUN_DELETE_WORKAROUND=off` env var
   bypasses the workaround entirely, using a plain `exec` path bounded by
   the caller's context. Allows validation without a rebuild.

5. **Extracted `isOrphanedRunscProcess` helper.** The matching logic uses
   NUL-split `/proc/<pid>/cmdline` with exact equality on the final argv
   element — fixing the previous dead-code path-segment match that assumed
   a `/run/sandbox/<name>/` path not present in real orphan argv. 7-case
   table-driven test verifies against real captured argv from the defect
   investigation.

6. **Defect evidence in repo.** Copied `defect-sandbox-delete-hang.md` into
   `.design/project-log/` so the evidence file referenced by the workaround
   code is in the repo, not just on the scratchpad volume.

### Files

- `pkg/runtime/cloudrun_sandbox_delete_workaround.go` — NEW (all workaround code)
- `pkg/runtime/cloudrun_sandbox_delete_workaround_test.go` — NEW (all workaround tests)
- `pkg/runtime/cloudrun_sandbox_runtime.go` — MODIFIED (removed moved code, added `deleteOrWorkaround`/`deletePlain` dispatch)
- `pkg/runtime/cloudrun_sandbox_runtime_test.go` — MODIFIED (removed moved tests)
- `.design/project-log/defect-sandbox-delete-hang.md` — NEW (copied from scratchpad)

## Why

The architect (sn-impl-arch) relayed ptone's instruction: the delete hang is a
filed Cloud Run bug. The workaround should be structured for clean removal when
the platform fix ships. Specifically:

- A someone finding this file in a year should know what "fixed" means without
  reconstructing context
- The workaround must not begin doing damage when it becomes unnecessary (the
  unconditional reaping was exactly this hazard)
- Validation should be possible without a rebuild (kill switch)
- Removal should be a `git rm` + one-line revert, not picking apart interleaved code

## Decisions

- **Bug reference:** No public issue exists. The defect is tracked internally by
  the Cloud Run team. `deleteDefectRef` cites our own evidence file instead.
- **Exit criteria anchored on runsc version:** `google-958767651` is the
  known-bad build. Without a public bug to watch, the self-detecting WARN log
  is the primary signal for removal.
- **Watcher cancel in `deleteOrWorkaround`:** Placed before the dispatch so
  neither `deleteWithTimeout` nor `deletePlain` needs it — removing the
  workaround file cannot lose watcher cancel logic.
