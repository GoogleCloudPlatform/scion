# Rebase Fix: Compilation and Vet Errors (2026-05-11)

## Summary

Fixed all compilation errors and `go vet` warnings introduced when rebasing `scion/rename-strategy` onto `main`.

## Root Causes

Three categories of breakage after the rebase:

### 1. Duplicate `ProjectID` in struct literals
When the rename branch added `GroveID` fields for backward compatibility alongside the renamed `ProjectID` fields, struct literals were incorrectly duplicating `ProjectID:` instead of using `GroveID:` for the second entry.

**Files fixed:** `pkg/hub/events.go` (10 struct literals)

### 2. Shadowed `ProjectID` in aux structs (custom JSON marshal/unmarshal)
The backward-compat JSON pattern uses anonymous aux structs embedding an `*Alias` type. When the aux struct declares a field named `ProjectID` with `json:"groveId"`, it shadows the promoted `Alias.ProjectID` field, making the condition `aux.ProjectID != ""` always equal to the already-populated primary field, and `aux.GroveID` undefined.

**Fix:** Renamed aux struct fields from `ProjectID` to `GroveID` so they don't shadow the promoted alias field.

**Files fixed:**
- `pkg/runtimebroker/types.go` (ProjectInfo, AgentResponse, CreateAgentRequest, MessageRequest)
- `pkg/runtimebroker/workspace_handlers.go` (ProjectWorkspaceUploadRequest)
- `pkg/hub/handlers.go` (RegisterProjectRequest, brokerProjectHeartbeat)
- `pkg/hub/project_cache.go` (ProjectCacheRefreshResponse, ProjectCacheStatusResponse)
- `pkg/hub/project_webdav.go` (ProjectSyncStatusResponse)
- `pkg/hub/response_types.go` (AgentWithCapabilities, ProjectWithCapabilities, TemplateWithCapabilities, GroupWithCapabilities, PolicyWithCapabilities)
- `pkg/hubclient/types.go` (Template)

### 3. Escaped quotes in struct tags (`json:\"...\"`  inside backticks)
Go backtick-quoted strings don't process escape sequences, so `\"` inside a struct tag becomes literal `\"` characters, which `encoding/json` doesn't recognize. These tags silently broke JSON marshaling at runtime and produced `go vet` warnings.

**Files fixed:**
- `pkg/hubclient/agents.go`, `messages.go`, `notifications.go`, `runtime_brokers.go`, `scheduled_events.go`, `schedules.go`, `templates.go`, `tokens.go`
- `pkg/runtimebroker/types.go`, `workspace_handlers.go`
- `pkg/hub/events.go`

### 4. Misc fixes
- `pkg/hubclient/workspace.go`: Field named `GroveID` renamed to `ProjectID` with correct `json:"projectId"` tag
- `pkg/brokerclient/agents.go`: `opts.GroveID` → `opts.ProjectID`
- `pkg/hub/handlers_agent_test.go`: `brokerGroveHeartbeat` → `brokerProjectHeartbeat`, `Groves:` → `Projects:`

## Verification

- `go build ./...` passes with zero errors
- `go vet ./...` passes with zero warnings
- All tests pass: `pkg/hub`, `pkg/hubclient`, `pkg/runtimebroker`, `pkg/store/sqlite`, `pkg/templatecache`
