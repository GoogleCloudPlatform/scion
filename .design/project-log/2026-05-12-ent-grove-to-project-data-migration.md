# Ent Database: Grove→Project Data Migration

**Date:** 2026-05-12
**Branch:** fix/workspace-path-fallback
**Scope:** pkg/ent/entc, cmd/server_foreground.go

## Problem

The V50 migration renamed `grove` → `project` in the manual-migration database (hub.db), and the Ent auto-migration updated the schema (column names) in hub.db_ent. However, the **data** in the Ent database was not migrated:

- Group slugs still used `grove:<slug>:members` / `grove:<slug>:agents` — but all code now queries with `project:` prefix
- `group_type` values contained `grove_agents` / `grove_members` — but the Ent enum only accepts `explicit` and `project_agents`
- `project_id` column (renamed from `grove_id`) was NULL for groups that belong to projects

This made all non-creator project memberships invisible.

## Solution

Created an idempotent data migration (`MigrateGroveToProjectData`) that runs at startup after Ent's `AutoMigrate()` and before `NewCompositeStore()`. The migration:

1. **Merges duplicate groups** — handles the case where both `grove:X:agents` and `project:X:agents` exist (e.g., if new code lazily created `project:` groups while old `grove:` groups persisted). Memberships from the old group are merged into the new one using `INSERT OR IGNORE`, then the old group is deleted.

2. **Renames slugs** — `grove:` → `project:` via `REPLACE()` with a `WHERE` guard for idempotency.

3. **Fixes group_type values** — `grove_agents` → `project_agents`, `grove_members` → `explicit`.

4. **Backfills project_id** — cross-database lookup: parses the project slug from the group slug, calls `GetProjectBySlug()` on the main store, and updates the Ent record.

All operations run in a single transaction. The function is safe to run multiple times (no-op when data is already correct).

## Files Changed

- **`pkg/ent/entc/migrate_grove_to_project.go`** — New file: migration function + `ProjectSlugResolver` interface
- **`pkg/ent/entc/migrate_grove_to_project_test.go`** — New file: 7 test cases covering slug rename, type fix, duplicate merge, project_id backfill, idempotency, empty DB, and slug parsing
- **`cmd/server_foreground.go`** — Wired `MigrateGroveToProjectData()` into the startup sequence after `AutoMigrate()`

## Design Decisions

- Used raw `database/sql` instead of the Ent client to bypass Ent's enum validation (old enum values like `grove_agents` would cause Ent to error)
- Defined a minimal `ProjectSlugResolver` interface rather than depending on the full `store.Store` — keeps the migration loosely coupled
- Split the public API (`MigrateGroveToProjectData`) from the internal DSN-accepting function (`migrateGroveToProjectDataWithDSN`) to enable in-memory database testing
- Generated new UUIDs for moved memberships during merge (via SQLite's `randomblob`) to avoid primary key conflicts

## Verification

- `go build ./...` passes
- `go vet ./...` passes
- All 7 new tests pass
- All existing entc tests continue to pass
