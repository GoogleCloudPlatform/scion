# Code Review: Hub-Broker Protocol Mismatch Fixes

## Executive Summary
**Verdict:** REQUEST CHANGES
**Risk Level:** HIGH (due to backward compatibility regressions in heartbeat and agent listing)

This PR addresses several critical protocol mismatches between the Hub and Runtime Broker introduced during the "grove" to "project" rename. The implementation of dual-field support via custom JSON marshaling is a step in the right direction. However, the backward compatibility is currently asymmetrical: while the system is now capable of emitting both new and legacy fields, it frequently fails to correctly **unmarshal** incoming legacy fields from older system components. This will lead to broken agent status tracking (heartbeats) and broken project listings when communicating with un-migrated brokers.

## Critical Issues

### 1. Broken Backward Compatibility for Incoming Heartbeats
**Files:** `pkg/hubclient/runtime_brokers.go`, `pkg/hub/handlers.go`

The `BrokerHeartbeat` and `ProjectHeartbeat` structs in `hubclient` (used by Brokers) and the `brokerHeartbeatRequest` in the Hub's handlers have been updated to use `projects` and `projectId` JSON tags. While `hubclient` now includes `MarshalJSON` to emit both keys, it lacks `UnmarshalJSON` to read from older Brokers that only send `groves` and `groveId`.

*   **Impact:** A newer Hub will fail to process heartbeats from an older Broker. The `Projects` slice will be empty upon unmarshaling, causing the Hub to ignore all agent status updates. This breaks real-time observability across the entire platform.
*   **Suggested Fix:** Implement `UnmarshalJSON` for `ListBrokerProjectsResponse`, `BrokerHeartbeat`, and `ProjectHeartbeat` in `pkg/hubclient/runtime_brokers.go` and for `brokerHeartbeatRequest` in `pkg/hub/handlers.go`.

### 2. `listAgents` Missing `projectId` Query Parameter Support
**File:** `pkg/runtimebroker/handlers.go:217`

While `handleAgentByID` and `handleAgentAttach` were correctly updated to support both `projectId` and `groveId` query parameters, the `listAgents` handler was overlooked.

*   **Impact:** Clients attempting to list agents using the new `projectId` parameter will receive a full list of all agents on the broker instead of a filtered list. This is a functional regression and a potential security concern regarding project isolation.
*   **Suggested Fix:**
    ```go
    // pkg/runtimebroker/handlers.go
    projectID := query.Get("projectId")
    if projectID == "" {
        projectID = query.Get("groveId")
    }
    if projectID != "" {
        filter["scion.project_id"] = projectID
    }
    ```

## Important Issues

### 3. Missing `projectId` JSON Tag Migration in Templates
**File:** `pkg/hubclient/templates.go`

Unlike `tokens.go` and `notifications.go`, the structs in `templates.go` (e.g., `CreateTemplateRequest`, `CloneTemplateRequest`) still use `json:"groveId"` as their primary tag and have not been updated to `json:"projectId"` with a compatibility shim.

*   **Impact:** Inconsistent API across `hubclient`. New code attempting to use `projectId` in JSON for template operations will fail.
*   **Suggested Fix:** Rename tags to `projectId` and implement custom `MarshalJSON`/`UnmarshalJSON` to maintain `groveId` support, consistent with the rest of the `hubclient` package.

### 4. `ListBrokerProjectsResponse` Missing `UnmarshalJSON`
**File:** `pkg/hubclient/runtime_brokers.go:81`

The Hub calls `ListProjects` on the Broker using this struct. Because it lacks `UnmarshalJSON`, it will return an empty list when communicating with an older Broker that returns `groves`.

*   **Impact:** Broken project discovery for older brokers.
*   **Suggested Fix:** Add `UnmarshalJSON` to `ListBrokerProjectsResponse`.

## Observations

- **MessageRequest Implementation:** The custom unmarshaling for `MessageRequest` in `pkg/runtimebroker/types.go` is well-implemented, supporting `project_id`, `projectId`, and `grove_id`. This should serve as the template for other compatibility shims.
- **Route Aliasing:** The addition of the `/api/v1/workspace/project-upload` route as an alias for `grove-upload` in `pkg/runtimebroker/server.go` is a robust and clean way to handle the route migration.

## Positive Feedback

- **Comprehensive Query Param Fallbacks:** The implementation of dual query parameter support in `handleAgentByID` and `handleAgentAttach` ensures a smooth transition for the core agent control paths.
- **Client-Side Robustness:** `hubclient` now correctly sends both `projectId` and `groveId` in all `List` operations, ensuring it can talk to any version of the Hub/Broker.

## Final Verdict
The PR addresses the immediate protocol mismatch bugs but introduces several regressions in backward compatibility by failing to handle incoming legacy payloads. These must be addressed to ensure a zero-downtime migration of the Scion fleet.

**Status: REQUEST CHANGES**
