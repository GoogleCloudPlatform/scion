# Postgres-backed SDK task store for A2A bridge standalone mode

**Date:** 2026-09-18
**Issue:** #1618
**Branch:** `scion/dev-a2a-taskstore`
**Author:** dev-a2a-taskstore agent

## Summary

Implemented a durable PostgreSQL-backed `taskstore.Store` for the A2A bridge's
standalone mode. Previously, standalone mode used `taskstore.NewInMemory` wrapped
by `ScopedTaskStore` for process-local ownership enforcement — task state was
lost on restart and invisible across replicas. The new `PostgresTaskStore`
persists full `a2a.Task` payloads in a shared Postgres database with SQL-level
ownership enforcement, CAS/version semantics, execution lease/heartbeat crash
recovery, retention cleanup, and cursor-based pagination.

## Changes

### New files
- `internal/bridge/pgstore_crossprocess_test.go` (~2100 lines) — 22 integration
  tests covering cross-process HTTP-level behavior, execution lease, crash
  recovery, retention, dedup, event delivery, caller isolation, plus regression
  tests for CRIT-2, CRIT-3, REQ-2, REQ-6, OPT-1.
- `internal/bridge/testdata/a2a-testserver/main.go` (~170 lines) — Subprocess
  test server with `-caller-id` flag, `READY <pid> <port>` protocol. Backdoor
  test endpoints removed per REQ-1.

### Modified files
- `internal/bridge/pgstore.go` (~650 lines) — execution lease columns
  (`exec_owner`, `exec_heartbeat`), `ClaimExecution`, `HeartbeatExecution`,
  `ReleaseExecution`, `ReapStaleTasks` (returns `[]string` for REQ-6),
  `PurgeTasksAndEvents`. Advisory lock on migration (REQ-4). Partial index
  `idx_a2a_sdk_tasks_exec` (REQ-5).
- `internal/bridge/bridge.go` — Added `sdkTaskStore` field, `SetSDKTaskStore`
  method, `reapStaleSDKExecutions` in janitor (emits terminal failure events
  per REQ-6), `PurgeTasksAndEvents` in `RunSweep` (standalone-only per REQ-2),
  heartbeat during `waitForTaskEvent` (CRIT-4), SDK-only task correlation
  fallback (CRIT-1).
- `internal/bridge/executor.go` — Wired `ClaimExecution` with retry loop
  (CRIT-2), fail-closed on all errors (CRIT-3), heartbeat during poll,
  `ReleaseExecution` on completion/error via defer.
- `internal/bridge/translate.go` — Deterministic artifact/message IDs via
  content hash (OPT-1).
- `internal/bridge/caller.go` — Exported `WithCallerIdentity` for test
  infrastructure.
- `internal/state/postgres.go` — Added `DB()` method for shared pool (REQ-4).
- `cmd/scion-a2a-bridge/main.go` — `SetSDKTaskStore(pgTaskStore)` wiring,
  shared pool via `NewPostgresTaskStoreWithDB` (REQ-4), startup recovery.
- `README.md` — Corrected false transactional consistency claims. Documents
  execution lease, crash recovery, retention, dedup, SSE cursor semantics.
- `.gitignore` — Added `/a2a-testserver`.

## Production call sites

| Method | Call site | File:Line |
|--------|-----------|-----------|
| `ClaimExecution` | Before Hub send in executor | `executor.go` Execute() |
| `HeartbeatExecution` | During waitForTaskEvent poll loop | `bridge.go` waitForTaskEvent() |
| `ReleaseExecution` | defer after successful claim | `executor.go` Execute() |
| `ReapStaleTasks` | janitor tick + startup recovery | `bridge.go` reapStaleSDKExecutions(), `main.go` serveStandalone() |
| `PurgeTasksAndEvents` | RunSweep | `bridge.go` RunSweep() |
| `SetSDKTaskStore` | standalone initialization | `main.go` serveStandalone() |
| `PrepareBarrier` / `Await` / `Cancel` | Barrier lifecycle in executor | `executor.go` Execute() |
| `GetByIDAndAgent` | Durable broker correlation | `bridge.go` correlateToTask() |
| `GetOwnedTaskSnapshotAndCursor` | Durable subscribe | `durable_handler.go` SubscribeToTask() |
| `NewBarrierTaskStore` | Wraps PostgresTaskStore | `main.go` serveStandalone() |
| `NewDurableRequestHandler` | Wraps SDK handler | `main.go` serveStandalone() |

## Schema

```sql
CREATE TABLE a2a_sdk_tasks (
    id TEXT PRIMARY KEY,
    context_id TEXT NOT NULL DEFAULT '',
    owner_key TEXT NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    exec_owner TEXT,
    exec_heartbeat TIMESTAMPTZ,
    project_id TEXT NOT NULL DEFAULT '',
    agent_slug TEXT NOT NULL DEFAULT '',
    caller_user_id TEXT NOT NULL DEFAULT '',
    last_event_cursor BIGINT NOT NULL DEFAULT 0
);
```

## Design decisions

1. **Owner key at SQL level** — Cross-replica ownership enforcement.
2. **JSONB payload** — Full `a2a.Task` stored as JSONB for SQL-level filtering.
3. **CAS via version column** — Distinguishes not-found from version-mismatch.
4. **Execution lease (exec_owner/exec_heartbeat)** — Only stale-lease tasks
   reaped; long-running legitimate work never affected. CAS on version +
   exec_owner prevents double-reap.
5. **Single-transaction retention** — `PurgeTasksAndEvents` deletes from both
   tables atomically.
6. **Shared connection pool** — Bridge state and SDK task store share a
   single `*sql.DB` pool via `NewPostgresTaskStoreWithDB` / `state.DB()` (REQ-4).
7. **Dedup at broker boundary** — Event log uses `dedup_key` with
   `ON CONFLICT DO NOTHING`.
8. **Deterministic artifact IDs** — `TranslateScionToA2A` uses content-hash-based
   IDs instead of random UUIDs, enabling reliable dedup_key generation (OPT-1).
9. **Fail-closed lease semantics** — All ClaimExecution errors prevent Hub sends
   (CRIT-3). Lease loss mid-execution returns error (CRIT-4).
10. **Terminal failure events on reap** — `ReapStaleTasks` returns reaped IDs;
    caller emits cross-replica-visible failure events to `a2a_task_events` (REQ-6).
11. **Active-task-safe retention** — Standalone mode skips unconditional
    `PurgeTaskEvents`; only terminal task events are purged via
    `PurgeTasksAndEvents` (REQ-2).
12. **BarrierTaskStore (R3)** — Deterministic create-completion signaling via
    sync.Once channel-based barrier. Replaces timed retry loop with zero timing
    assumptions. PrepareBarrier → yield → Await → single ClaimExecution.
13. **DurableRequestHandler (R3)** — Wraps SDK RequestHandler, intercepting
    SubscribeToTask with ownership-enforcing durable subscribe. Reads
    snapshot + last_event_cursor atomically, streams only new events.
14. **Per-event cursor tracking (R3)** — `_bridgeEventID` carried through SDK
    event metadata. Update uses `GREATEST(last_event_cursor, eventID)` to
    advance cursor monotonically to the specific event applied, not MAX(id).
15. **Durable correlation columns (R3)** — project_id, agent_slug, caller_user_id
    stored explicitly for cross-replica broker correlation and topic user
    validation. Pre-migration rows terminalized with fail-closed semantics.
16. **Heartbeat fence ordering (R3)** — waitForTaskEvent reordered:
    heartbeat→read→verify→return. No post-loss SDK event may be yielded.
17. **Store wrapper chain (R3)** — SDK→ScopedTaskStore→BarrierTaskStore→
    PostgresTaskStore. ScopedTaskStore is redundant compatibility only;
    PostgresTaskStore is the authoritative owner enforcer.

## Test provisioning

```bash
sudo service postgresql start
TEST_DATABASE_URL="postgresql://scion:scion@localhost:5432/a2a_test?sslmode=disable" \
  go test -v -count=1 -run TestPostgresTaskStore ./internal/bridge/
```

## Residual risks

- Hub side-effect replay: crash after Hub send but before completion record
  leaves the side effect un-replayed; task transitions to `failed` via reaper.
- SSE stream locality: streams are process-local.
- `go.mod`/`go.sum` updated for `grpc-gateway` and OTel version bumps
  (all indirect, no new direct deps).
