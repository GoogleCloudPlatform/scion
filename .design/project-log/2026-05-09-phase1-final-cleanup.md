# Phase 1 - Final Cleanup: Rename Grove to Project

## Summary
Renamed remaining internal Go functions, variables, constants, and internal directory names from "Grove" to "Project". 
This cleanup focused on consistency within the codebase while preserving the external CLI surface, JSON/YAML tags, and container labels to maintain backward compatibility.

## Changes
- **pkg/config**:
    - Renamed functions: `GenerateGroveID` -> `GenerateProjectID`, `initExternalGrove` -> `initExternalProject`, etc.
    - Renamed constants: `GroveTypeGlobal` -> `ProjectTypeGlobal`, `GroveStatusOK` -> `ProjectStatusOK`, etc.
    - Renamed directory: `grove-configs` -> `project-configs` in home directory paths.
    - Updated tests to unset Hub environment variables to avoid pollution from the container environment.
- **pkg/agent**:
    - Renamed functions: `StopGroveContainers` -> `StopProjectContainers`.
    - Updated internal logic to use `project-configs` for state migration and external storage.
- **pkg/hub**:
    - Renamed all internal handlers from `handleGrove*` to `handleProject*`.
    - Renamed event types: `GroveCreatedEvent` -> `ProjectCreatedEvent`, etc.
    - Renamed methods on `EventPublisher` interface and implementations.
    - Renamed `GitHubAppTokenMinter` method to `MintGitHubAppTokenForProject`.
    - Renamed `HealthStats.Groves` to `HealthStats.Projects`.
- **pkg/runtimebroker**:
    - Renamed internal directory: `groves` -> `projects` in home directory paths.
    - Updated handlers to use `ProjectID` and `ProjectSlug` fields (preserving JSON tags).
- **cmd**:
    - Renamed internal helper functions like `findGroveByName` -> `findProjectByName`.
    - Updated `gitCloneWorkspace` to support `SCION_WORKSPACE_PATH` override for easier testing.
- **extras**:
    - Renamed utility functions in `agent-viz`, `fs-watcher-tool`, and `scion-a2a-bridge`.

## Verification Results
- `go build ./...` passes.
- `go test ./...` passes for all core packages (`pkg/config`, `pkg/agent`, `pkg/hub`, `pkg/runtimebroker`, `pkg/hubsync`, `cmd`).
- Verified that JSON tags and environment variable names (e.g., `SCION_GROVE_ID`) are preserved where they form part of the API surface.

## Observations
- Environment pollution (especially `SCION_GROVE_ID` and `SCION_OTEL_ENDPOINT`) in the sandbox container caused several test failures that required explicit unsetting in the tests.
- Reverting accidental JSON key renames in tests was necessary to maintain compatibility with unchanged struct tags.
