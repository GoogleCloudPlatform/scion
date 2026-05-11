# Independent Code Review v8: Grove-to-Project Rename

**Verdict:** REQUEST CHANGES
**Summary:** Verified critical mismatches between Hub and Broker communication paths.

| Severity | Count | Category |
| :--- | :--- | :--- |
| **CRITICAL** | 2 | API/Protocol Mismatch |
| **HIGH** | 1 | Functional Isolation |
| **MEDIUM** | 0 | - |
| **LOW** | 0 | - |
| **INFO** | 0 | - |

## Critical Issues

### 1. Heartbeat JSON Payload Mismatch
The Hub and Broker have diverged on the JSON schema for heartbeats. Updated Hubs expect `projects` and `projectId` keys, but updated Brokers (using `hubclient`) still send `groves` and `groveId`.

*   **File (Broker-side):** `pkg/hubclient/runtime_brokers.go`
    ```go
    type BrokerHeartbeat struct {
        Status   string             `json:"status"`
        Projects []ProjectHeartbeat `json:"groves,omitempty"` // Incorrect tag: should be "projects"
    }
    type ProjectHeartbeat struct {
        ProjectID  string           `json:"groveId"` // Incorrect tag: should be "projectId"
    ```
*   **File (Hub-side):** `pkg/hub/handlers.go`
    ```go
    type brokerHeartbeatRequest struct {
        Projects []brokerProjectHeartbeat `json:"projects,omitempty"`
    }
    type brokerProjectHeartbeat struct {
        ProjectID    string                 `json:"projectId"`
    ```
*   **Impact:** Hub will fail to update agent statuses from heartbeats, breaking observability and state tracking for all agents.
*   **Suggested Fix:** In `pkg/hubclient/runtime_brokers.go`, update the JSON tags to `projects` and `projectId` respectively, OR add custom `MarshalJSON` to `BrokerHeartbeat` / `ProjectHeartbeat` to support both (similar to `AgentInfo`).

### 2. Workspace Project-Upload Route Mismatch
The route used by the Hub to trigger a project-level workspace upload does not match the route the Broker listens on.

*   **File (Hub-side):** `pkg/hub/project_cache.go:386`
    ```go
    tunnelProjectWorkspaceRequest(..., "POST", "/api/v1/workspace/project-upload", ...)
    ```
*   **File (Broker-side):** `pkg/runtimebroker/server.go:1445`
    ```go
    s.mux.HandleFunc("/api/v1/workspace/grove-upload", s.handleProjectWorkspaceUpload)
    ```
*   **Impact:** Cache refresh for linked projects will fail with a 404 error.
*   **Suggested Fix:** Update the route in `pkg/runtimebroker/server.go` to `/api/v1/workspace/project-upload`.

## High Issues

### 1. Agent Isolation Bypass via Query Parameter
The Hub has been updated to send `projectId` in query parameters for agent-scoped requests, but the Broker still only looks for `groveId`.

*   **File (Hub-side):** `pkg/hub/controlchannel_client.go:88` (and others)
    ```go
    path += "?projectId=" + url.QueryEscape(projectID)
    ```
*   **File (Broker-side):** `pkg/runtimebroker/handlers.go:788` (and others)
    ```go
    groveID := r.URL.Query().Get("groveId")
    ```
*   **Impact:** When the Hub calls the Broker to Stop or Delete an agent, the `groveID` variable on the Broker will be empty. This causes `resolveManagerForAgent` to ignore project scoping, potentially targeting an agent with the same name in a different project if a name collision exists.
*   **Suggested Fix:** Update Broker handlers (`handleAgentByID`, `listAgents`, `pty_handlers.go`) to check both `groveId` and `projectId` query parameters.

## What's Done Well
*   Excellent legacy support in `pkg/api/types.go` using custom `MarshalJSON`/`UnmarshalJSON` for `AgentInfo` and `ResolvedSecret`.
*   Methodical migration of database tables and columns in `pkg/store/sqlite/sqlite.go`.
*   Robust fallback logic in `pkg/hubclient/client.go` to handle `/projects` -> `/groves` API transitions.
*   Successful compilation and passing of core configuration and API tests.

## Verification Story
*   Verified compilation with `go build ./...` and `go vet ./...`.
*   Ran `go test ./pkg/config/ -count=1` and `go test ./pkg/api/ -count=1` (All Passed).
*   Manually traced protocol changes between `pkg/hub` and `pkg/runtimebroker` to identify mismatches.
*   Verified that Hub tests for heartbeats pass only because they use the Hub's internal types, masking the protocol break with the Broker.
