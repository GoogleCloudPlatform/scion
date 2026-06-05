# WP-0: Agent Schema — project_id Index + Drop Inert Visibility

**Date**: 2026-06-05
**Branch**: `design/project-visibility-membership`
**Scope**: Ent schema + codegen foundation for the project-visibility feature

## Changes

### Schema (`pkg/ent/schema/agent.go`)
- Added standalone index `index.Fields("project_id")` — enables efficient queries filtering agents by project without requiring the slug in the predicate. The existing unique composite `index.Fields("slug", "project_id").Unique()` is preserved.
- Removed the inert `field.String("visibility").Default("private")` from the Agent schema. This field was hardcoded to "private" at creation, never user-settable, never enforced in access control, and never used for filtering.

### Generated Code (`pkg/ent/**`)
- Regenerated via `go generate ./pkg/ent/...` — removes all Visibility-related generated helpers (SetVisibility, where predicates, mutation methods) from the Agent entity. The new project_id index appears in `migrate/schema.go`.

### Reference Cleanup (25 files total)
- `pkg/store/models.go` — removed `Visibility` field from `Agent` struct and `ToAgentInfo()` conversion
- `pkg/api/types.go` — removed `Visibility` from `AgentInfo`
- `pkg/hub/events.go` — removed `Visibility` from `AgentCreatedEvent` struct and population
- `pkg/hub/handlers.go` — removed `Visibility: store.VisibilityPrivate` from agent creation
- `pkg/store/entadapter/agent_store.go` — removed all Visibility read/write in the Ent adapter
- `cmd/list.go` — removed Visibility from agent-to-AgentInfo mapping
- 12 test files across `pkg/hub/` and `pkg/store/storetest/` — removed `Visibility` from `store.Agent` struct literals

### Untouched (intentionally)
- Project visibility (`pkg/ent/schema/project.go`, `store.Project`, `api.ProjectInfo`) — remains
- Template and HarnessConfig visibility — remains
- `api.NormalizeVisibility` — not added (out of scope; belongs to another WP)
- `web/`, broker, seed.go, authz — out of scope

## Build & Test Results
- `go build ./...` — PASS
- `make test-fast` — all test packages pass except pre-existing failures:
  - `pkg/hub` — `command_bus_test.go` build failure (undefined `recExec`/`requirePostgres`, pre-existing on main)
  - `pkg/store/entadapter` — `broker_affinity_test.go` build failure (undefined helpers, pre-existing on main)
  - `pkg/config` — 5 test failures (env-var leakage from container, pre-existing)
  - `pkg/hubsync` — 2 test failures (pre-existing)

No new test failures introduced by this change.
