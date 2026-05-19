# Fix Stop() scoping to use post-rename project naming

**Date:** 2026-05-12
**Commit:** fix(agent): update Stop() scoping to use post-rename project naming
**Branch:** fix/stop-project-scoping

## Summary

Commit 6bf33265 added projectPath scoping to `Stop()` to prevent
cross-project targeting, but used pre-rename "grove" naming that
referenced non-existent symbols (`config.GetGroveName`,
`matchAgentGrove`, and `grovePath` variable). This would cause
compilation failures.

## Changes

### Core fix (pkg/agent/manager.go)
- Renamed `grovePath` parameter to `projectPath` in both the `Manager`
  interface and `AgentManager` implementation
- Changed `config.GetGroveName()` → `config.GetProjectName()` (the
  actual function at pkg/config/paths.go:116)
- Changed `matchAgentGrove()` → `matchAgentProject()` (the actual
  function at pkg/agent/run.go:1004)
- Renamed local variable `stopGroveName` → `stopProjectName`

### Call sites (cmd/stop.go, cmd/suspend.go)
- Changed `grovePath` → `projectPath` at all 4 call sites (2 per file).
  `grovePath` did not exist as a variable in the cmd package; `projectPath`
  is the correct variable used throughout.

### Hub dispatcher (cmd/server_dispatcher.go)
- Added clarifying comment on the empty-string projectPath passed in
  hub-dispatched operations, explaining that the hub handles authorization.

### Test mocks (5 files)
- Updated all mock `Stop()` signatures from 2-parameter to 3-parameter
  form to match the updated `Manager` interface:
  - cmd/server_dispatcher_test.go
  - pkg/runtimebroker/handlers_test.go
  - pkg/runtimebroker/heartbeat_test.go
  - pkg/runtimebroker/protocol_mismatch_test.go
  - pkg/runtimebroker/workspace_handlers_test.go

## Verification

- `go build ./...` passes
- `go vet ./...` passes
- `go test ./pkg/agent/...` passes
- `go test ./pkg/runtimebroker/...` passes
- `go test ./cmd/... -run TestDispatch` passes
- Pre-existing cmd test failures (in delete, env, hub, list, message,
  sync tests) are unrelated to this change

## Observations

The grove→project rename has a long tail of references across the
codebase. New features that add symbols (like the Stop scoping commit)
need to be careful to use the post-rename names. A grep for remaining
`GetGroveName` or `matchAgentGrove` references could catch future
issues proactively.
