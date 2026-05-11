# Project Log: Code Review v2 DB Fixes

**Date:** 2026-05-11
**Task:** Fix DB index column order inconsistency (HIGH 2)

## Problem
Inconsistency between Ent schema and SQLite store regarding the column order of the unique index on agents (slug/agent_id and project_id).
- Ent schema (Source of Truth for Hub): `(slug, project_id)`
- SQLite store (CLI): `(project_id, agent_id)`

Note: SQLite `agent_id` maps to Ent `slug`.

## Findings
- The Ent schema in `pkg/ent/schema/agent.go` defines the index as `index.Fields("slug", "project_id").Unique()`.
- The SQLite store in `pkg/store/sqlite/sqlite.go` had `idx_agents_grove_slug ON agents(grove_id, agent_id)` in the base schema and `idx_agents_project_slug ON agents(project_id, agent_id)` in migration `V48`.
- While functionally equivalent for uniqueness, the column order should match for consistency and predictable performance across storage implementations.

## Actions Taken
1.  **Modified `pkg/store/sqlite/sqlite.go`**:
    - Updated the base schema for `agents` to use `(agent_id, grove_id)` order.
    - Updated `migrationV48` to use `(agent_id, project_id)` order.
    - Added `DROP INDEX IF EXISTS idx_agents_project_slug` to `migrationV48` before `CREATE` to ensure the index is recreated with the correct order even if it already exists (e.g. if the migration was partially applied or applied before this fix).
    - Added explanatory comments to both locations referencing the Ent schema consistency.
2.  **Fixed `pkg/store/models.go`**:
    - Discovered that `pkg/store/models.go` was missing `import "encoding/json"`, causing build failures when running tests. Fixed by adding the missing import.
3.  **Verification**:
    - Ran `go test ./pkg/store/sqlite/... -v -run TestMigration` - **PASSED**.
    - Ran `go test ./pkg/store/sqlite/... -v -run TestAgentCRUD` - **PASSED**.

## Conclusion
The SQLite store is now consistent with the Ent schema regarding the agent uniqueness index. The fix to `models.go` ensures the package remains buildable.
