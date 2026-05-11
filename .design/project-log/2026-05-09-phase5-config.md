# Phase 5 Config Refactor

Implemented Phase 5 changes for project configuration and discovery logic in `pkg/config/`.

## Changes

### `pkg/config/paths.go`
- Added constants for `ProjectConfigsDir`, `ProjectsDir`, `GroveConfigsDir`, and `GrovesDir`.
- Updated `FindProjectRoot` and other functions to use "project" terminology and handle markers resolving to external paths.

### `pkg/config/project_marker.go`
- Updated `ProjectMarker` struct with new YAML tags (`project-id`, `project-name`, `project-slug`).
- Implemented `UnmarshalYAML` for `ProjectMarker` to provide backward compatibility for legacy `grove-` tags using an inline alias struct.
- Updated `ExternalProjectPath()` to check `project-configs/` first, falling back to `grove-configs/`.
- Updated `ReadProjectID()` to check `project-id` file first, falling back to `grove-id`.
- Updated `WriteProjectID()` to write to `project-id` file.
- Updated `WriteWorkspaceMarker` and other functions to use "project" terminology in comments and error messages.


### `pkg/config/project_discovery.go`
- Updated `DiscoverProjects()` to scan both `project-configs/` and legacy `grove-configs/` directories.
- Implemented `scanConfigDir` helper to avoid duplicates by slug, preferring `project-configs/`.
- Updated `readWorkspaceMarkerForSlug()` to check `projects/` first, falling back to legacy `groves/`.
- Updated `ProjectInfo` JSON tag for `ProjectID` to `project_id`.
- Updated `RemoveProjectConfig` to allow removing from both `project-configs/` and `grove-configs/`.

### `pkg/config/shared_dirs.go`
- Updated `GetSharedDirsBasePath` to use constants and handle both `project-configs/` and `grove-configs/` structures.
- Updated comments to use "project" terminology.
- Updated comments to use "project" instead of "grove".

### `pkg/config/koanf.go`
- Updated comments and parameter names to use "project" terminology.
- Updated `LoadSettingsKoanf` to reflect "project" terminology in internal remapping logic comments.

## Verification Results

- All tests in `pkg/config` passed, confirming both new functionality and backward compatibility.
- `go test ./pkg/config/...` output: `ok github.com/GoogleCloudPlatform/scion/pkg/config 1.393s`
- `go test ./pkg/config/...` output: `ok github.com/GoogleCloudPlatform/scion/pkg/config 1.414s`
