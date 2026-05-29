# Tier 1 Grove→Project Rename: pkg/config/

**Date:** 2026-05-29
**Scope:** `pkg/config/` (including test files)

## Summary

Completed Tier 1 (internal identifiers) rename of "grove" to "project" across all Go files in `pkg/config/`. This is part of the broader project rename effort on the `grove-rename2` branch.

## What Changed

### Source Files (10 files)
- **`state.go`**: Renamed `grovePath` params to `projectPath`, updated comments
- **`settings.go`**: Renamed `grovePath` params in `LoadSettings`, `SaveSettings`, `SaveSettingsJSON`, `UpdateSetting`, `DeleteHubConnection` to `projectPath`; updated error strings ("grove path required" → "project path required"); updated comments
- **`settings_v1.go`**: Renamed `grovePath` in `RequireImageRegistry`; updated error strings ("grove state" → "project state"); updated comments ("grove-level" → "project-level", "grove scope" → "project scope")
- **`harness_config.go`**: Renamed `grovePath` params in `FindHarnessConfigDir` and `ListHarnessConfigDirs`; renamed local var `groveHarnessConfigDir` → `projectHarnessConfigDir`; updated comments
- **`paths.go`**: Updated comments and error string in `RequireProjectPath`
- **`project_marker.go`**: Renamed local var `grovePath` → `legacyPath` in `ExternalProjectPath`
- **`project_discovery.go`**: Renamed local var `groveConfigsDir` → `legacyConfigsDir` in `DiscoverProjects` and `RemoveProjectConfig`
- **`remote_templates.go`**: Updated TODO comments
- **`koanf.go`**: No Tier 1 changes needed (all references are Tier 2/3 config keys)
- **`shared_dirs.go`**: No Tier 1 changes needed

### Test Files (9 files)
- Renamed test local variables: `groveDir` → `projectDir`, `groveScionDir` → `projectScionDir`, `groveSettingsYAML` → `projectSettingsYAML`, `groveSettings` → `projectSettings`, `groveConfigDir` → `projectConfigDir`, `groveHCDir` → `projectHCDir`, etc.
- Renamed test function names: `TestWriteGroveSettings_*` → `TestWriteProjectSettings_*`, `TestGetAgentHomePath_GitGroveSplitStorage` → `TestGetAgentHomePath_GitProjectSplitStorage`, `TestResolveAgentDir_FallsBackToInGroveWhenExternalAbsent` → `TestResolveAgentDir_FallsBackToInProjectWhenExternalAbsent`
- Updated comments and human-readable strings in test assertions
- Updated test directory path strings (`"my-grove"` → `"my-project"`)

## What Was NOT Changed (Tier 2/3)
- Exported constants: `GroveConfigsDir`, `GrovesDir`
- Exported struct fields with JSON/YAML tags: `ProjectInfo.GroveID`, `V1HubClientConfig.ProjectID` (json:"grove_id")
- Config key strings: `"grove_id"`, `"hub.grove_id"`, `"hub.groveId"`
- Environment variable strings: `"SCION_GROVE_ID"`, `"SCION_HUB_GROVE_ID"`
- Filesystem path strings: `"grove-configs"`, `"grove-id"`, `"groves/"`, `"default_grove_settings.yaml"`
- YAML struct tags: `yaml:"grove-id"`, `yaml:"grove-name"`, `yaml:"grove-slug"`
- Backward compatibility logic and comments documenting it
- Test fixture data values containing "grove" (e.g., `"test-grove-uuid-1234"`)
- The `"grove"` case in templates.go switch statement (CLI backward compat)

## Verification
- `go build ./pkg/config/...` — passes
- `go vet ./pkg/config/...` — passes
- `go test ./pkg/config/...` — passes (1.386s)

## Stats
- 19 files modified
- 444 insertions, 448 deletions
