# Grove→Project Rename: pkg/hubsync/

**Date:** 2026-05-29
**Branch:** grove-rename2
**Scope:** Tier 1 rename in `pkg/hubsync/`

## Summary

Renamed all internal Go identifiers, comments, and human-readable strings from "grove" to "project" in the `pkg/hubsync/` package. This is part of the broader project-wide rename effort.

## Files Modified

- **prompt.go** — 80 replacements: renamed all `groveName` params to `projectName`, `groves`/`groveNames`/`grovePath` params to `projects`/`projectNames`/`projectPath`, updated all user-facing strings ("grove" → "project", "Grove" → "Project") and comments
- **sync.go** — 146 replacements: renamed `groveID` local var to `projectID`, `groveName` to `projectName`, `grovePath` params to `projectPath`, `groveState` to `projectState`, updated all debug strings, error messages, comments, and user-facing output
- **resolve_test.go** — 17 replacements: renamed test functions `TestResolveGroveOnHub_*` → `TestResolveProjectOnHub_*`, local `grove` vars to `project`, test fixture names
- **sync_test.go** — 55 replacements: renamed test functions `TestCleanupGroveBrokerCredentials_*` → `TestCleanupProjectBrokerCredentials_*`, `TestIsGroveRegistered_*` → `TestIsProjectRegistered_*`, `TestFindGroveByID_*` → `TestFindProjectByID_*`, test fixture IDs, error message assertions, comments

## What Was NOT Renamed (Tier 2/3 — Intentionally Preserved)

- Environment variable strings: `"SCION_GROVE_ID"`, `"SCION_HUB_GROVE_ID"`, `"SCION_PROJECT_ID"`
- Settings/config keys: `"grove_id"`, `"hub.groveId"`, `"hub.grove_id"`
- API paths in test fixtures: `"/api/v1/groves/..."`
- JSON response keys in test fixtures: `"groves": [...]`
- Test fixture ID values referencing wire protocol (e.g., `"git-grove-1"`)

## Verification

- `go build ./pkg/hubsync/...` — passes
- `go vet ./pkg/hubsync/...` — passes
- `go test ./pkg/hubsync/...` — passes (0.058s)
- `go build ./...` — full project build passes
