# M1 Lifecycle Hooks Port — Data Model + Store

**Date:** 2026-06-08
**Agent:** lh-port-m1
**Branch:** scion/lifecycle-hooks-port
**Issue:** #35

## What was done

Ported milestone M1 (data model + store layer) of the configurable agent lifecycle hooks feature from the reference branch `origin/scion/architect-lifecycle-hooks` onto current `main`.

### Sub-task A: Ent schemas + regeneration
- Created `pkg/ent/schema/lifecyclehook.go` — LifecycleHook entity (UUID id, name, scope_type/scope_id, selector/action JSON, trigger enum, execution_identity, enabled, timestamps, state_version for optimistic locking).
- Created `pkg/ent/schema/lifecyclehookagentphase.go` — NEW ent entity replacing the reference's raw-SQL `lifecycle_hook_agent_phase` table. Fields: agent_id (string, unique, immutable), last_phase, updated_at. Uses `entsql.Annotation` for table name.
- Added `LifecycleHookSelector` and `LifecycleHookAction` types to `pkg/ent/schema/types.go`.
- Ran `go generate ./pkg/ent/...` — 25 files changed.

### Sub-task B: Models, store interface, CRUD
- Added `LifecycleHook`, `LifecycleHookSelector`, `LifecycleHookAction` structs + scope/trigger/action-type/on-error constants to `pkg/store/models.go`.
- Added `LifecycleHookStore` interface to `pkg/store/store.go` with CRUD + `CompareAndSetHookPhase` + `DeleteHookPhase`.
- Ported `pkg/store/entadapter/lifecyclehook_store.go` — full ent-backed CRUD (Create/Get/Update/Delete/List with optimistic locking).
- Wired `LifecycleHookStore` into `CompositeStore` via embedding.

### Sub-task C: CAS dedup + tests
- Implemented `CompareAndSetHookPhase` using ent transactions with conditional `ForUpdate()` (Postgres only; SQLite relies on single-writer serialization). Dialect detection runs before tx open to avoid deadlock on SQLite's MaxOpenConns=1.
- Implemented `DeleteHookPhase` using ent Delete with Where filter.
- 18 tests total, all green: 12 CRUD + 6 CAS dedup (including concurrent goroutine race test).

## Deviations from reference

1. **LifecycleHookAgentPhase is an ent entity**, not a raw-SQL table. The reference used `migrationV55` DDL + raw `INSERT...ON CONFLICT DO UPDATE WHERE` in `sqlite.go`. The port uses `pkg/ent/schema/lifecyclehookagentphase.go` (auto-migrated) and ent transactions for CAS.
2. **CAS uses tx + query + conditional update** instead of raw SQL `INSERT...ON CONFLICT DO UPDATE WHERE last_phase IS NOT excluded.last_phase`. The ent upsert API doesn't expose a WHERE clause on the DO UPDATE, so the transactional approach achieves equivalent atomicity.
3. **Dialect-aware ForUpdate**: added `usesRowLocks()` helper (matching `AgentStore` pattern) to avoid `SELECT...FOR UPDATE` errors on SQLite.
4. **CompositeStore wiring**: reference delegated `CompareAndSetHookPhase`/`DeleteHookPhase` to `c.Store` (raw SQL base store). The port embeds `*LifecycleHookStore` directly in `CompositeStore` — all methods are promoted, no explicit delegation needed.

## Observations

- The `enttest.NewClient(t)` pattern + `entc.OpenSQLite` with `MaxOpenConns: 1` means any code that opens a transaction and then tries to run a separate query on `s.client` will deadlock. The fix (matching AgentStore) is to call dialect detection before `s.client.Tx(ctx)`.
