# P3 Fixes: CloudRun Sandbox Runtime

**Date:** 2026-08-25
**Branch:** `scion/p3-fixes` (based on `scion/p3-run-delete-list`)
**Commit:** 9886a86

## Summary

Applied 8 fixes to `pkg/runtime/cloudrun_sandbox_runtime.go` from code review,
security audit, and architect corrections.

## Changes

### Security Fixes

1. **FIX 1 – Per-agent mounts:** Replaced single `/scion` root bind mount with
   per-agent mounts (home, workspace, tmux, shared dirs). Prevents cross-agent
   tmux socket injection where agent A could inject keystrokes into agent B's
   session.

2. **FIX 2 – `--env` flags:** Switched from `/usr/bin/env K=V` to repeatable
   `--env K=V` flags. AC-0 retest confirmed `--env` is repeatable. Removes
   env var exposure in `/proc/pid/cmdline`.

3. **FIX 6 – State store permissions:** Changed from 0644 to 0600.

### Correctness Fixes

4. **FIX 3 – PATH env var:** Added default PATH since it's empty inside the
   sandbox. Set before harness env so harnesses can override.

5. **FIX 4 – `--force` for delete/stop:** `sandbox delete` without `--force`
   silently fails for running sandboxes. Both `Stop` and `Delete` now always
   use `--force`.

6. **FIX 5 – HOME symlink:** Creates `ln -sfn <agent-home> /home/scion` in the
   entrypoint before `sciontool init` runs. Fixes the disconnect where
   `sciontool init` hardcodes `HOME=/home/scion` but that path is on the rootfs
   overlay, invisible to the broker.

7. **FIX 7 – Atomic state writes:** State store now writes to a temp file then
   renames, preventing corruption from crashes mid-write.

8. **FIX 8 – Trailing hyphen trim:** `sanitizeSandboxName` now trims hyphens
   AFTER truncation to 63 chars, fixing names where the 63rd character is a
   hyphen.

## Tests

All tests updated and passing:
- `mountArg` tests replaced with `mountsFor` tests (basic paths + shared dirs)
- Env tests updated: removed `/usr/bin/env` checks, added `--env` checks, PATH tests
- Delete/Stop tests verify `--force` flag
- `buildEntrypoint` tests verify symlink setup and accept `agentHome` parameter
- `sanitizeSandboxName` tests updated for trailing-hyphen-after-truncation

## Files Modified

- `pkg/runtime/cloudrun_sandbox_runtime.go`
- `pkg/runtime/cloudrun_sandbox_runtime_test.go`
