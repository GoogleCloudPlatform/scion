# Project Log - 2026-05-11 - Hub CLI Renames (CR v2)

## Changes
Fixed incomplete Hub CLI command renames as part of Code Review v2.

### Renamed 'groves' to 'projects' in Hub CLI
- Modified `cmd/hub.go`:
    - Renamed primary subcommands from `groves` to `projects`.
    - Added `groves` as a hidden alias for backward compatibility.
    - Updated all help and usage strings to use "project" instead of "grove".
    - Updated `scion hub projects create`, `scion hub projects info`, and `scion hub projects delete` (and their aliases).
    - Updated `scion hub link` and `scion hub unlink` messages.
    - Updated `runHubStatus` to report "Project ID" and "Project Context".

### Configuration System Updates
- Modified `pkg/config/settings.go`:
    - Updated `HubClientConfig` and `Settings` struct tags: `groveId` -> `projectId`, `grove_id` -> `project_id`.
    - Updated `UpdateSetting`, `GetSettingValue`, and `GetSettingsMap` to support both new and old keys for backward compatibility.
- Modified `pkg/config/init.go`:
    - Updated project initialization to use `project_id`.

### Template Management Updates
- Modified `cmd/templates.go`:
    - Renamed "grove" scope to "project" scope in output and comments.
    - Updated `syncTemplateToHub` to support both `project` and `grove` as scope identifiers.
- Modified `cmd/template_helpers.go`:
    - Renamed `GroveOnly` to `ProjectOnly` in `ResolveOpts`.
    - Updated `LocationLocalProject` and `LocationHubProject` values.
    - Updated human-readable location strings.

### Common CLI Helpers
- Modified `cmd/common.go`:
    - Renamed "grove" to "project" in comments and user-facing messages.
    - Updated `GetProjectID` to look for both `hub.projectId` and `project_id`.

## Verification
- Ran `go build ./...` successfully.
- Verified that all "scion hub" commands now use "project" terminology in help strings.
- Verified that hidden aliases for "groves" are preserved.
