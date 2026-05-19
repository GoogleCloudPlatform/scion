# Task 1: Core Config & Discovery Renames

Date: 2026-05-09
Agent: dev-phase1-task1

## Summary of Changes

Renamed core symbols and files in `pkg/config/` related to "Grove" to "Project" as specified in Phase 1 - Task 1.

### Files Renamed
- `pkg/config/grove_discovery.go` -> `pkg/config/project_discovery.go`
- `pkg/config/grove_discovery_test.go` -> `pkg/config/project_discovery_test.go`
- `pkg/config/grove_marker.go` -> `pkg/config/project_marker.go`
- `pkg/config/grove_marker_test.go` -> `pkg/config/project_marker_test.go`

### Symbols Renamed
- `GroveID` -> `ProjectID`
- `ResolveGrovePath` -> `ResolveProjectPath`
- `GetGroveName` -> `GetProjectName`
- `DiscoverGroves` -> `DiscoverProjects`
- `GroveInfo` -> `ProjectInfo`
- `ReadGroveID` -> `ReadProjectID`
- `IsGroveMarkerFile` -> `IsProjectMarkerFile`
- `IsInsideGrove` -> `IsInsideProject`
- `GetEnclosingGrovePath` -> `GetEnclosingProjectPath`
- `GroveType` -> `ProjectType`
- `GroveStatus` -> `ProjectStatus`

Additionally renamed related test functions for consistency:
- `TestDiscoverGroves` -> `TestDiscoverProjects`
- `TestGetGroveName` -> `TestGetProjectName`
- `TestResolveGrovePath` -> `TestResolveProjectPath`
- `TestRequireGrovePath` -> `TestRequireProjectPath`
- `TestIsInsideGrove` -> `TestIsInsideProject`
- `TestGetEnclosingGrovePath` -> `TestGetEnclosingProjectPath`
- `TestReadGroveID` -> `TestReadProjectID`

### Verification Results
- `go build ./pkg/config/...` passes.
- `go test ./pkg/config/...` shows pre-existing failures related to the environment (`SCION_HUB_ENDPOINT`, `SCION_GROVE_ID`, `SCION_OTEL_ENDPOINT` being set), but no new regressions were identified.
- JSON/YAML/Koanf tags were preserved as requested.

## Observations
- Many other "Grove" references remain (e.g., `GroveMarker` struct, `GenerateGroveID`, `WriteGroveID`, and various local variables/comments). These were not in the explicit list for this task and were left as-is to avoid over-scoping, except where they were part of the requested renames (like field renames).
