# Project Log: 2026-05-11 v9 JSON Fixes

## Overview
Fixed critical JSON shadowing issues in Hub response types and added legacy field support for heartbeats and client responses to ensure backward compatibility during the "grove" to "project" rename.

## Changes

### 1. Hub Response Types (`pkg/hub/response_types.go`)
- Added `MarshalJSON` and `UnmarshalJSON` to all wrapper types that embed `store` models:
    - `AgentWithCapabilities`
    - `ProjectWithCapabilities`
    - `TemplateWithCapabilities`
    - `GroupWithCapabilities`
    - `UserWithCapabilities`
    - `PolicyWithCapabilities`
    - `RuntimeBrokerWithCapabilities`
- **Fix:** Used a local alias for embedded types to avoid infinite recursion and bypass the embedded type's `MarshalJSON` during wrapper marshaling. This ensures that wrapper-specific fields (like `_capabilities`) are included in the final JSON.
- **Legacy Support:** Explicitly added `groveId`, `grove`, and `groveName` fields to these methods where applicable to maintain backward compatibility with older clients.

### 2. Hub Heartbeats (`pkg/hub/handlers.go`)
- Added `UnmarshalJSON` to `brokerHeartbeatRequest` and `brokerProjectHeartbeat`.
- **Legacy Support:** These types now correctly handle `groves` (legacy for `projects`) and `groveId` (legacy for `projectId`) sent by older Brokers.

### 3. Hubclient List Response (`pkg/hubclient/runtime_brokers.go`)
- Added `UnmarshalJSON` to `ListBrokerProjectsResponse`.
- **Legacy Support:** Correctly handles the `groves` key in responses from older Hubs.

## Verification Results

### Tests Passed
- `pkg/hub`:
    - `TestAgentWithCapabilities_JSONStructure`
    - `TestProjectWithCapabilities_JSONStructure`
    - `TestBrokerHeartbeatRequest_UnmarshalJSON`
    - `TestBrokerProjectHeartbeat_UnmarshalJSON`
- `pkg/hubclient`:
    - `TestListBrokerProjectsResponse_MarshalJSON`
    - `TestListBrokerProjectsResponse_UnmarshalJSON`

### Build
- `go build ./...` passes successfully.

## Observations
- JSON shadowing in Go when embedding types with custom `MarshalJSON` is a subtle but common issue. The pattern of using a local alias of the embedded type (without methods) is the standard way to fix it while still leveraging default marshaling for the fields.
- Ensuring bidirectional legacy support (both `MarshalJSON` for older clients and `UnmarshalJSON` for older servers/brokers) is essential for a smooth migration during the "grove" to "project" rename.
