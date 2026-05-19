# Task 4: Coordination & Execution Layer Renames (Hub & Broker)

Date: 2026-05-09
Agent: dev

## Summary of Changes

Renamed symbols and files in `pkg/hub/`, `pkg/runtimebroker/`, and `pkg/agent/` related to "Grove" to "Project" as specified in Phase 1 - Task 4.

### Files Renamed
- `pkg/hub/grove_cache.go` -> `pkg/hub/project_cache.go`
- `pkg/hub/grove_cache_test.go` -> `pkg/hub/project_cache_test.go`
- `pkg/hub/grove_settings_handlers.go` -> `pkg/hub/project_settings_handlers.go`
- `pkg/hub/grove_settings_handlers_test.go` -> `pkg/hub/project_settings_handlers_test.go`
- `pkg/hub/grove_workspace_handlers.go` -> `pkg/hub/project_workspace_handlers.go`
- `pkg/hub/grove_workspace_handlers_test.go` -> `pkg/hub/project_workspace_handlers_test.go`

### Symbols Renamed
- `GroveCache` -> `ProjectCache` (and related subtypes like `GroveCacheStatusResponse`)
- `GroveSettings` -> `ProjectSettings` (and related subtypes like `GroveResourceSpec`)
- `BrokerGroveInfo` -> `BrokerProjectInfo`
- `GroveWorkspace*` -> `ProjectWorkspace*` (for consistency in renamed files)
- Handler functions: `handleGroveCache*` -> `handleProjectCache*`, `handleGroveSettings` -> `handleProjectSettings`, `handleGroveWorkspace*` -> `handleProjectWorkspace*`
- Fields: `GroveID` -> `ProjectID`, `GroveName` -> `ProjectName` (where applicable in Hub/Broker types)

### Other Changes
- Updated `pkg/agent/provision.go` and `pkg/agent/manager.go` to use renamed symbols from Task 1 (`config.GetProjectName`, `config.ReadProjectID`).
- Updated `pkg/hub/project_workspace_handlers.go` to use `config.ResolveProjectPath`.
- Updated route registrations in `pkg/runtimebroker/server.go`.

### Verification Results
- Build attempted with `go build ./pkg/hub/... ./pkg/runtimebroker/... ./pkg/agent/...`.
- The build currently fails due to pre-existing/parallel inconsistencies in `pkg/store` (Task 3) and `pkg/api` (Task 2). Specifically:
    - `pkg/store` uses `Project` but only defines `Grove`.
    - `pkg/config` expects `Grove` field in `api.AgentInfo` but it was renamed to `Project`.
- These failures are outside the scope of Task 4 and are being addressed by other tasks.
- Verified that all requested renames in the assigned files were completed and JSON tags were preserved.

## Observations
- Many "Grove" references remain in local variables and comments; these were left as-is unless part of a type or handler rename.
- Kept REST API path strings as `"/api/v1/groves"` as per CRITICAL requirement.
