# Workspace Path Fallback: projects -> groves

**Date:** 2026-05-11
**Branch:** fix/workspace-path-fallback

## Problem

The grove-to-project rename (V50 SQL migration) renamed database tables and columns but did not include a filesystem migration. On deployed instances, workspace files still reside under `~/.scion/groves/{slug}`, while the code was updated to look under `~/.scion/projects/{slug}`. This caused workspace files to become invisible in the Web UI.

## Solution

Applied a filesystem fallback pattern to all 6 code paths that resolve hub-native project/grove paths. The pattern is: try `projects/` first, if it doesn't exist check `groves/`, default to `projects/`. This matches the existing pattern already used in `ExternalProjectPath()` (`pkg/config/project_marker.go`).

For directory-scanning functions (`findAgentInHubNativeGroves`, `discoverAuxiliaryRuntimes`), both `projects/` and `groves/` directories are scanned and results merged.

## Files Changed

| File | Function | Change |
|------|----------|--------|
| `pkg/hub/handlers.go` | `hubNativeGrovePath()` | Added fallback from `projects/` to `groves/` |
| `pkg/runtimebroker/start_context.go` | `buildStartContext()` | Added fallback for `in.GrovePath` resolution |
| `pkg/runtimebroker/handlers.go` | `createAgent()` line ~390 | Added fallback for `req.GrovePath` resolution |
| `pkg/runtimebroker/handlers.go` | `createAgent()` line ~598 | Added fallback for `workspaceDir` resolution |
| `pkg/runtimebroker/handlers.go` | `deleteGrove()` | Added fallback + updated path traversal check to use `filepath.Dir()` |
| `pkg/runtimebroker/handlers.go` | `findAgentInHubNativeGroves()` | Scans both `projects/` and `groves/` directories |
| `pkg/runtimebroker/server.go` | `discoverAuxiliaryRuntimes()` | Scans both `projects/` and `groves/` directories |

## Notes

- Variable names were preserved (e.g., `grovePath`, `grovesDir`) to maintain readability and minimize diff noise.
- The `deleteGrove()` path traversal protection was updated to derive its base directory from the resolved path using `filepath.Dir()`, ensuring the security check works correctly regardless of which directory was selected.
- The `os` package was already imported in all affected files; no import changes were needed.
