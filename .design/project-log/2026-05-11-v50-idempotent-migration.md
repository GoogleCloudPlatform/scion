# V50 Migration Made Idempotent

**Date:** 2026-05-11
**Author:** Developer Agent
**Scope:** `pkg/store/sqlite/sqlite.go`

## Problem

The V50 migration (grove-to-project rename) crashed hubs on startup because it executed all `ALTER TABLE RENAME COLUMN/TABLE` statements as a single SQL string. If any statement failed (e.g., `gcp_service_accounts.grove_id` didn't exist on some instances, or the migration had already partially run), the entire migration failed and blocked hub startup.

## Solution

### 1. Function-based migration support

Changed the migration slice type from `[]string` to `[]any` and added a type-switch in the execution loop to support both:
- `string` migrations: executed via `tx.ExecContext` as before
- `func(ctx context.Context, tx *sql.Tx) error` migrations: called directly with the transaction

This preserves full backward compatibility for all 49 existing string migrations while enabling V50 to use programmatic logic.

### 2. Idempotent V50 function

Replaced the `const migrationV50` string with a `migrateV50` function that checks before acting:

- **Table renames:** Queries `sqlite_master` to check if the old table name exists before renaming.
- **Column renames:** Uses `PRAGMA table_info()` to check if the old column name exists before renaming.
- **Data updates:** Already idempotent (`UPDATE ... WHERE` is a no-op when no matching rows exist). Kept as-is.
- **Index recreation:** Already idempotent (`DROP IF EXISTS` / `CREATE IF NOT EXISTS`). Kept as-is.

### 3. Helper functions

Added two unexported helpers:
- `tableExists(ctx, tx, tableName)` — checks `sqlite_master`
- `columnExists(ctx, tx, tableName, columnName)` — scans `PRAGMA table_info`

## Verification

- `go build ./...` passes
- `go vet ./...` passes
- `TestMigration` passes (runs Migrate twice to verify idempotency)
- `TestMigrationV40PreservesAgents` continues to pass

## Key Design Decisions

- Used `[]any` rather than defining a custom interface to keep the change minimal and Go-idiomatic.
- The FK-off path (`applyMigrationWithFKOff`) still only accepts strings since V50 does not require FK-off. If a future function migration needs FK-off, the path can be extended then.
- Column checks use the new table names (post-rename) since table renames always run first.
