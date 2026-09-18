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
- `internal/bridge/pgstore_crossprocess_test.go` (~1400 lines) — 16 integration
  tests covering cross-process HTTP-level behavior, execution lease, crash
  recovery, retention, dedup, event delivery, caller isolation.
- `internal/bridge/testdata/a2a-testserver/main.go` (~210 lines) — Subprocess
  test server with `-caller-id` flag, `/test/append-event` and `/test/read-events`
  endpoints, `READY <pid> <port>` protocol.

### Modified files
- `internal/bridge/pgstore.go` (~617 lines) — execution lease columns
  (`exec_owner`, `exec_heartbeat`), `ClaimExecution`, `HeartbeatExecution`,
  `ReleaseExecution`, `ReapStaleTasks`, `PurgeTasksAndEvents`.
- `internal/bridge/bridge.go` — Added `sdkTaskStore` field, `SetSDKTaskStore`
  method, `reapStaleSDKExecutions` in janitor, `PurgeTasksAndEvents` in
  `RunSweep`, heartbeat during `waitForTaskEvent`.
- `internal/bridge/executor.go` — Wired `ClaimExecution` before Hub send,
  heartbeat during poll via `waitForTaskEvent`, `ReleaseExecution` on
  completion/error via defer.
- `internal/bridge/caller.go` — Exported `WithCallerIdentity` for test
  infrastructure.
- `cmd/scion-a2a-bridge/main.go` — `SetSDKTaskStore(pgTaskStore)` wiring,
  startup `ReapStaleTasks` recovery.
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
    exec_heartbeat TIMESTAMPTZ
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
6. **Separate connection pools** — Bridge state and SDK task store use
   independent `*sql.DB` pools. README corrected to state this.
7. **Dedup at broker boundary** — Event log uses `dedup_key` with
   `ON CONFLICT DO NOTHING`.

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
