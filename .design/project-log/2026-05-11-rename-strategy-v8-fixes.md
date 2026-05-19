# Rename Strategy: v8 Review Fixes

## Problem
Several protocol mismatches between the Scion Hub and Runtime Broker were identified where 'project' and 'grove' nomenclature were mixed, leading to communication failures.

## Solution
Implemented dual-support (backward compatibility) for both 'project' and 'grove' nomenclature in JSON payloads and query parameters across `hubclient` and `runtimebroker`.

### Changes

#### 1. Heartbeat & Broker Status JSON (pkg/hubclient/runtime_brokers.go)
- Updated `ListBrokerProjectsResponse`, `BrokerHeartbeat`, and `ProjectHeartbeat` to use `projects` and `projectId` JSON tags.
- Implemented `MarshalJSON` for these structs to emit both new tags and legacy `groves`/`groveId` tags.
- Updated `List` query parameters to send both `projectId` and `groveId`.

#### 2. Workspace Upload Route (pkg/runtimebroker/server.go)
- Added `/api/v1/workspace/project-upload` route as an alias for `/api/v1/workspace/grove-upload`.
- Both routes point to `handleProjectWorkspaceUpload`.

#### 3. Query Parameter Handling (pkg/runtimebroker/handlers.go, pty_handlers.go)
- Updated `handleAgentByID` and `handleAgentAttach` to check for `projectId` first, falling back to `groveId`.

#### 4. General Cleanup & Consistency (pkg/hubclient/, pkg/runtimebroker/types.go)
- Updated `List` methods in `agents.go`, `notifications.go`, and `templates.go` within `pkg/hubclient` to send both `projectId` and `groveId`.
- Updated `TokenInfo` and `CreateTokenRequest` in `pkg/hubclient/tokens.go` with dual JSON support.
- Updated `MessageRequest` in `pkg/runtimebroker/types.go` to use `projectId` tag and support both `project_id` and `grove_id` in `UnmarshalJSON`/`MarshalJSON`.

## Verification Results
- `go build ./...` passed successfully.
