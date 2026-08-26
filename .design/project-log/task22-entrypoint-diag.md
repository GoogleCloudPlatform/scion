# Task #22: Make Sandbox Death Diagnosable

**Date:** 2026-08-26
**Author:** dev-entrypoint-diag
**File:** `pkg/runtime/cloudrun_sandbox_runtime.go`

## Summary

When a sandbox dies shortly after launch, its entrypoint's stdout/stderr and
exit code were lost. The DOA (Dead-on-Arrival) probe detected death but could
not say WHY. This change captures entrypoint output and surfaces it through the
existing DOA probe error path.

## Changes

### 1. Package-level constants

Added `entrypointLogFile` (`.scion-entrypoint.log`) and `entrypointRCFile`
(`.scion-entrypoint.rc`) constants to avoid hardcoding filenames in multiple
places.

### 2. Entrypoint wrapping (`buildEntrypoint()`)

Wrapped the existing entrypoint command in a shell group with output
redirection:

```
/bin/sh -c '{ <symlink> && exec sciontool init -- /bin/sh -c <tmux>; } > <logPath> 2>&1; echo $? > <rcPath>'
```

Key semantics:
- On the **happy path**, `exec` replaces the shell with sciontool, which
  inherits the redirected file descriptors. The RC file is never written
  (the shell is gone). This is acceptable since tmux manages its own pty.
- On the **failure path** (symlink fails, sciontool not found, etc.), the
  shell continues past the group, the log file contains the error output,
  and the exit code is written to the RC file.

### 3. DOA probe diagnostics (`Run()`)

When the liveness probe detects the sandbox is dead:
- Reads the log file and RC file directly from the host filesystem using
  `os.ReadFile()` (not `sandbox exec`, which can't work on a dead sandbox).
- Truncates log output to the last 2000 bytes.
- Includes diagnostics in both the `runtimeLog.Error()` call and the
  returned error message.

## Verification

- `go build ./...` passes
- `golangci-lint run ./pkg/runtime/...` passes (0 issues)
- `make fmt-check` passes
