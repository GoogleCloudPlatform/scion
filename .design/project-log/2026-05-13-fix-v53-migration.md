# Fix V53 Migration Crash

**Date**: 2026-05-13
**Task**: Fix V53 migration crash due to missing allow_list table

## Problem

Migration V53 (`CREATE INDEX ... ON allow_list`) crashed on existing databases because the `allow_list` table did not exist, despite V48 being recorded as applied in `schema_migrations`.

## Root Cause

Migrations V48 (allow_list table) and V49 (invite_codes table) were inserted into the migration sequence, pushing the grove-to-project rename migration from position 48 to position 50. Databases created before this insertion already had version 48 recorded (for the rename migration). When running the new code, the migration system skips V48 (already applied), so the `allow_list` table is never created. V53 then fails trying to create an index on a nonexistent table.

Evidence: The `foreignKeysOffMigrations` map had `48: true` with the comment "V48 renames tables and columns" — but the current V48 only creates a table. That comment describes the V50 rename migration, confirming V48 and V49 were inserted.

## Fix

1. **V53 now creates `allow_list` and `invite_codes` tables** (with `IF NOT EXISTS`) before adding the index. This is a no-op on fresh databases (tables already exist from V48/V49) but creates the missing tables on upgraded databases.
2. **Removed stale `foreignKeysOffMigrations[48]`** entry. The current V48 (`CREATE TABLE`) does not need `PRAGMA foreign_keys=OFF`; that was only needed when position 48 held the rename migration.

## Testing

- Added `TestMigrationV53_AllowListMissing` regression test that simulates the bug (drops allow_list, resets schema_migrations to version 48, verifies Migrate succeeds and creates the table + index).
- All existing migration tests continue to pass.

## Key Insight

Inserting migrations into the middle of a numbered sequence is dangerous when existing databases have already applied versions at those positions. The inserted migrations will be silently skipped on upgrade. Future migrations that depend on the inserted ones (like V53 depending on V48's table) will crash.
