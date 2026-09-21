# Project Log: F1 Part 2 — Existing-Session Toolbar Sync

**Date**: 2026-09-21
**Branch**: scion/p3-1717-f1-part2
**Base**: 3d64ce6a (post-merge main)
**Commit**: 5c8755f
**Issue**: #1717

## Problem

Part 1 (published in PR #1795) added `.tmux.conf` template config (`set-titles
on`, `set-titles-string '#W'`) and a frontend OSC 0 handler, covering new agents.
Existing agent sessions created before the template update do not have
`set-titles` enabled, so tmux does not emit OSC 0 title updates on window switch,
and the web toolbar indicator remains stale after the user switches tmux windows.

## Approach: Attach-Time `tmux set-option` Activation

At attach time, before starting the docker exec tmux attach, run `tmux set-option
-t scion set-titles on` and `tmux set-option -t scion set-titles-string #W` via
docker exec. This activates the same title-emission behavior that new agents get
from their `.tmux.conf` template.

The activation is:

- **Best-effort**: failure logged at Debug, does not block terminal attach
- **Docker-only**: guarded by `isDockerCompatibleRuntime()`
- **Idempotent**: safe to call on sessions that already have set-titles enabled
- **3-second timeout**: bounded to prevent attach delays

## Changes

### Production (`pty_handlers.go`)

1. **`activateTmuxSetTitles()`** — new function (22 lines) after `activeWindowOSC()`:
   - Runs two `docker exec` commands with 3-second timeout
   - Sets `set-titles on` and `set-titles-string #W` on session `scion`
   - Failure returns silently (best-effort)

2. **`LocalPTYSession.Run()`** — 4-line call site in Docker else branch:
   - Before `startDockerExec()`, guarded by `isDockerCompatibleRuntime(s.runtimeCmd)`

3. **`StreamPTYHandler.Run()`** — 4-line call site in Docker else branch:
   - Before `startDockerExec()`, guarded by `isDockerCompatibleRuntime(runtimeCmd)`

### Tests (`pty_handlers_test.go`)

Added 6 tests and test infrastructure (helpers for fake docker script, tmux
server lifecycle, PTY pair opening, OSC 0 parsing):

1. **`TestActivateTmuxSetTitles_FailureBestEffort`** — bad runtime doesn't panic/block
2. **`TestActivateTmuxSetTitles_CancelledContext`** — cancelled context returns promptly
3. **`TestActivateTmuxSetTitles_RealTmux`** — real tmux server, verifies set-titles on + string #W at session level
4. **`TestActivateTmuxSetTitles_Idempotent`** — double call is safe, state unchanged
5. **`TestActivateTmuxSetTitles_NoSessionFallback`** — missing "scion" session returns silently
6. **`TestActivateTmuxSetTitles_WindowSwitchOSC0`** — full E2E: real tmux window switch emits OSC 0 with correct window names, strict `require.NotEmpty` + `require.Contains` assertions

Test helpers:

- `writeFakeDockerScript()` — creates shell script that strips docker exec prefix and forwards tmux commands with socket isolation
- `startTmuxServer()` — creates a real tmux server with cleanup
- `readOSC0()` — parses OSC 0 title sequences from PTY output with timeout
- `startPTYForTest()` — starts command with real PTY master/slave pair
- `openPTYPair()` — wraps `pty.Open()` from creack/pty

## Gates Passed

- `go vet ./pkg/runtimebroker/...` — clean (zero warnings)
- `go build ./pkg/runtimebroker/...` — clean
- `go test -race -count=1 ./pkg/runtimebroker/... -run TestActivateTmuxSetTitles` — 6/6 PASS
- `go test -race -count=1 ./pkg/runtimebroker/... -run TestPTYCleanup` — all PASS (no regressions)
- `go test -race -count=1 -tags no_sqlite ./pkg/hub/... -run TestPTYCleanup` — 5/5 PASS
- `make fmt` — clean

Pre-existing failures (EnvGather, ResolveManagerForOpts, BuildInfoProfiles tests)
confirmed to also fail on main — not introduced by this change.

## Delivery

- Bundle: `transfers/p3-1717-5c8755f.bundle`
- SHA256: `5f8371beb52bcc06b4ba19878ca32b714b8897d7ff3504655c83212931efa2e5`
