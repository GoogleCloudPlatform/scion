# Project Log: Phase 4 - Database Migration (V48)

**Date:** 2026-05-09
**Agent:** developer (scion-gteam)
**Task:** Add migration V48 for grove-to-project rename

## Work Completed
1.  **Updated `pkg/store/sqlite/sqlite.go`**:
    *   Appended `migrationV48` to the `migrations` slice in `Migrate()`.
    *   Added `48: true` to the `foreignKeysOffMigrations` map to ensure foreign key constraints are disabled during the rename operation.
    *   Defined `migrationV48` constant with comprehensive SQL to:
        *   Rename tables: `groves` -> `projects`, `grove_contributors` -> `project_contributors`, `grove_sync_state` -> `project_sync_state`.
        *   Rename columns: `grove_id` -> `project_id` across 12 tables.
        *   Update data values: Changed "grove" to "project" in `scope`, `scope_type`, and `group_type` fields.
        *   Rename/Recreate indexes: Dropped 14 legacy "idx_grove_*" indexes and recreated them as "idx_project_*".

## Observations
*   The migration requires `foreign_keys=OFF` because it involves renaming both parent and child tables/columns simultaneously, which can trigger intermediate constraint violations in SQLite.
*   `ALTER TABLE ... RENAME COLUMN` is used for precision.
*   Indexes must be dropped and recreated as SQLite does not support renaming them directly.
*   Added `ALTER TABLE project_sync_state RENAME COLUMN grove_id TO project_id;` which was in the inventory but missing from the raw SQL block in the sub-design doc.

## Next Steps
*   Run database migration tests to verify the schema update.
*   Coordinate with Ent ORM schema updates (Phase 4).
