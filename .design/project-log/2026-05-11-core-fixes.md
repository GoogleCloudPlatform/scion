# Project Log: Core Hub and ProjectSync Fixes (2026-05-11)

## Task Overview
Fixed several issues identified in Code Review v3 related to the Hub and ProjectSync components. The primary focus was ensuring database migration completeness, API backward compatibility, and naming consistency across the codebase.

## Work Completed

### 1. SQLite Migration Update
- Updated `pkg/store/sqlite/sqlite.go`: Modified `migrationV48` to include the `gcp_service_accounts` table.
- Added `ALTER TABLE gcp_service_accounts RENAME COLUMN grove_id TO project_id;`.
- Added `DROP INDEX IF EXISTS idx_gcp_sa_grove;` and `CREATE INDEX IF NOT EXISTS idx_gcp_sa_project ON gcp_service_accounts(project_id);`.
- This ensures that upgraded installations correctly rename the `grove_id` column to `project_id`, matching the updated Go code.

### 2. Hub API Backward Compatibility
- Updated `pkg/hub/handlers.go`: Added legacy fields to response structures to support older clients.
- `ListProjectsResponse`: Added `LegacyGroves []ProjectWithCapabilities` with JSON tag `groves,omitempty`.
- `RegisterProjectResponse`: Added `LegacyProject *store.Project` with JSON tag `grove,omitempty`.
- Populated these legacy fields in `listProjects` and `handleProjectRegister` handlers.

### 3. ProjectSync URL and Naming Cleanup
- Updated `pkg/projectsync/projectsync.go`:
  - Changed WebDAV URL path from `/api/v1/groves/` to `/api/v1/projects/` in `buildWebDAVURL`.
  - Renamed `groveID` parameter to `projectID` in `buildWebDAVURL`.
  - Updated error message from "grove ID is required" to "project ID is required" in `Sync`.

### 4. API Types Compatibility
- Updated `pkg/api/types.go`: Added `MarshalJSON` for `ResolvedSecret`.
- If the `Source` field is `"project"`, the marshaled JSON now includes a `"grove": "project"` field.
- This maintains backward compatibility for clients expecting the "grove" terminology in secret resolution responses.

## Verification Results
- Ran `go build ./...`: SUCCESS
- Ran `go vet ./...`: SUCCESS
- Verified that `migrationV48` in `pkg/store/sqlite/sqlite.go` correctly handles the `gcp_service_accounts` table.
- Verified that legacy fields are correctly added and populated in `pkg/hub/handlers.go`.

## Observations
- The grove-to-project rename was comprehensive, but these few spots were missed in the initial pass.
- Using `MarshalJSON` and `UnmarshalJSON` for "dual-field" support is an effective strategy for maintaining compatibility during terminology migrations.
- Database migrations involving column renames and index updates are critical for a smooth upgrade path.
