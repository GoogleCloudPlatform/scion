# P4 Sandbox Exec Control Plane

**Date:** 2026-08-26
**Branch:** `scion/p4-exec`
**Base:** `scion/dev-rebase-1294`

## Summary

Implemented the `sandbox exec` control plane for the `cloudrun-sandbox` runtime,
enabling CLI attach, browser terminal, log capture, and arbitrary command
execution inside Cloud Run Sandboxes.

## Changes

### `pkg/runtime/cloudrun_sandbox_runtime.go`

- **`GetLogs()`** — Uses `sandbox exec` to run `tmux capture-pane -p -t scion -S -1000`
  inside the sandbox, capturing the last 1000 lines of scrollback.
- **`Attach()`** — Verifies the sandbox exists via state store, sets tmux
  `window-size latest`, then runs an interactive `sandbox exec` with
  `TERM=xterm-256color` and `tmux attach-session -t scion`.
- **`Exec()`** — Passes through arbitrary commands via `sandbox exec <id> -- <cmd...>`.
  No `--user` flag (sandbox CLI does not support it; process runs as the
  sandbox's configured user).

### `pkg/runtimebroker/pty_handlers.go`

Six stub sites replaced with working cloudrun-sandbox branches:

1. **`waitForTmuxSession()`** — Polls with `sandbox exec ... tmux has-session -t scion`
   until the tmux session is available.
2. **`queryTmuxActiveWindow()`** — Queries active window via
   `sandbox exec ... tmux display-message` with absolute paths (no `--user` flag).
3. **`LocalPTYSession.Run()`** — Routes to new `startCloudRunSandboxExec()` then
   falls through to the shared read/write/resize loop.
4. **`LocalPTYSession.startCloudRunSandboxExec()`** — New method: waits for tmux,
   builds sandbox exec command with `--env TERM=xterm-256color`, starts with
   `pty.StartWithSize`.
5. **`StreamPTYHandler.Run()` + `startCloudRunSandboxExec()`** — Same pattern for
   the control-channel stream path.
6. **Resize handling** — Both `LocalPTYSession` and `StreamPTYHandler` resize
   handlers now issue an out-of-band `sandbox exec ... tmux resize-window` when
   `runtimeCmd == "cloudrun-sandbox"`. SIGWINCH does not cross the sandbox
   boundary (PTY fd properties propagate but signal delivery does not), so the
   resize must be relayed explicitly. The launcher-side `pty.Setsize` is still
   called as well.

### Constant: `cloudRunSandboxBin`

Defined in `pty_handlers.go` as `/usr/local/gcp/bin/sandbox`. The unexported
`defaultSandboxBin` in `cloudrun_sandbox_runtime.go` is in a different package,
so a local constant was added.

## Five load-bearing details preserved

1. `--env TERM=xterm-256color` at every attach site with explaining comment.
2. Out-of-band `tmux resize-window` (not `refresh-client -C`) for resize.
3. All binary paths inside sandboxes are absolute (`/usr/bin/tmux`).
4. No `--user` flag on `sandbox exec`.
5. Sandbox names (slugs) used as identifiers, not Docker-style container IDs.

## Verification

- `go build ./...` — passes
- `go vet ./pkg/runtime/... ./pkg/runtimebroker/...` — passes
