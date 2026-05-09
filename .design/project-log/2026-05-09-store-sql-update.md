# Project Log - 2026-05-09 - Store SQL Update

## Task
Update all hardcoded SQL references in `pkg/store/` to match the new 'project'-based schema.

## Changes
- Renamed tables: `groves` -> `projects`, `grove_contributors` -> `project_contributors`, `grove_sync_state` -> `project_sync_state`.
- Renamed columns: `grove_id` -> `project_id`.
- Updated data values: `scope = 'grove'` -> `scope = 'project'`, `group_type = 'grove_agents'` -> `group_type = 'project_agents'`.
- Updated Go struct fields and JSON tags in `pkg/store/models.go` to be consistent with the schema rename (e.g., `Grove` field renamed to `Project`).
- Updated all migrations (V1-V47) in `pkg/store/sqlite/sqlite.go` to use the new names for fresh installs.
- Maintained/Restored `migrationV48` to correctly handle renaming from the old names to the new names for existing databases.
- Verified that the project builds with `go build ./...`.

## Findings
- Aggressive renaming of "grove" to "project" in `pkg/store/` required careful handling of `migrationV48` which specifically renames the OLD entities.
- Some Go struct fields in `pkg/store/models.go` were already named `ProjectID` but had JSON tags as `groveId`; these were updated to `projectId`.
- The `Grove` enriched field in `store.Agent` was renamed to `Project` to match the new naming convention, which is consistent with usages in `pkg/hub/handlers.go`.
