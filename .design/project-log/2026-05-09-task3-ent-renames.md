# Task 3: Ent Schema & Store Models Renames

## Status
Completed on 2026-05-09.

## Changes
- Renamed `pkg/ent/schema/grove.go` to `project.go`.
- Renamed Ent entity `Grove` to `Project`.
- Added `entsql.Annotation{Table: "groves"}` to `Project` schema to maintain DB compatibility.
- Updated `pkg/ent/schema/agent.go` and `pkg/ent/schema/group.go` to use `project_id` field with `StorageKey("grove_id")`.
- Mass renamed `Grove` to `Project` and `GroveID` to `ProjectID` in:
    - `pkg/store/models.go`
    - `pkg/store/store.go`
    - `pkg/store/sqlite/*.go`
    - `pkg/store/entadapter/*.go`
- Renamed `GroveSyncState` to `ProjectSyncState` and updated related files.
- Renamed `GroveStore` interface to `ProjectStore`.
- Renamed methods like `GetGrove` to `GetProject`, `ListGroves` to `ListProjects`, etc.
- Renamed constants:
    - `SubscriptionScopeGrove` -> `SubscriptionScopeProject`
    - `PolicyScopeGrove` -> `PolicyScopeProject`
    - `HarnessConfigScopeGrove` -> `HarnessConfigScopeProject`
    - `TemplateScopeGrove` -> `TemplateScopeProject`
    - `GroupTypeGroveAgents` -> `GroupTypeProjectAgents` (Go constant name only, string value kept as `"grove_agents"`)
- Renamed `api.GroveInfo` to `api.ProjectInfo` usage.
- Ran `go generate ./pkg/ent` to update generated code.
- Fixed test failures in `policy_store_test.go` and `maintenance_test.go`.

## Verification Results
- `go build ./pkg/ent/... ./pkg/store/...` passed.
- `go test -v ./pkg/store/...` passed (all 150+ tests).

## Notes
- Database table names (`groves`, `grove_contributors`, `grove_sync_state`) and column names (`grove_id`) were preserved as per instructions to avoid breaking the DB in Phase 1.
- Pre-existing test failure in `TestMaintenanceOperationsSeeded` was fixed (it expected 4 seeded operations but migration V47 added a 5th one).
- Pre-existing test failure in `TestGetPolicy` and others in `entadapter` were fixed (they were trying to use `"project"` for `scope_type` which is still `"grove"` in Ent schema).
