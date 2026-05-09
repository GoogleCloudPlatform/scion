# Project Log - 2026-05-09 - Task 2: Hub Client & Common Types Renames

## Task Overview
Rename "Grove" related types and methods to "Project" in `pkg/hubclient` and `pkg/api`.

## Changes Completed
- Renamed "Grove" related types and fields in `pkg/hubclient/types.go`.
- Renamed `pkg/hubclient/groves.go` to `pkg/hubclient/projects.go` and updated its content.
- Updated `pkg/hubclient/runtime_brokers.go` with "Project" renames.
- Renamed "Grove" related fields and comments in `pkg/api/types.go` (excluding `ProjectInfo` which was already renamed).
- Renamed `GroveIDSeparator` to `ProjectIDSeparator` in `pkg/api/slug.go` and updated its occurrences.
- Updated `pkg/hubclient/client.go` with renamed methods (`Projects()`, `ProjectAgents()`) and fields.
- Updated `pkg/hubclient/agents.go` with "Project" renames.
- Updated `pkg/hubclient/messages.go`, `pkg/hubclient/scheduled_events.go`, `pkg/hubclient/schedules.go`, `pkg/hubclient/gcp_service_accounts.go`, `pkg/hubclient/notifications.go`, `pkg/hubclient/templates.go`, `pkg/hubclient/workspace.go`, `pkg/hubclient/tokens.go` with "Project" renames.
- Updated test files `pkg/hubclient/client_test.go` and `pkg/hubclient/workspace_test.go` with renamed types and test data.
- Updated `pkg/api/slug_test.go` with renamed methods and test cases.

## Verification
- Ran `go build ./pkg/api/... ./pkg/hubclient/...`.
- `pkg/api` and `pkg/hubclient` build successfully (ignoring expected errors in `pkg/store`).
- Serialization tags (JSON/YAML) and API strings were preserved as instructed.

## Notes
- `pkg/api/types.go` was restored by the manager during the task due to external corruption.
- Lingering "grove" strings in comments and test data were cleaned up in a final pass.
