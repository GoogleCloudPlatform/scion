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
recovery, and cursor-based pagination.

## Changes

### New files
- `internal/bridge/pgstore.go` (~617 lines) — `PostgresTaskStore` implementing
  `taskstore.Store` backed by `a2a_sdk_tasks` table. Schema auto-migrates.
  Ownership derived via `buildOwnerKey(ctx)` (same logic as `ScopedTaskStore`).
  CAS updates via `WHERE version = $prev`. Execution lease via `exec_owner` and
  `exec_heartbeat` columns. Methods: `ClaimExecution`, `HeartbeatExecution`,
  `ReleaseExecution`, `ReapStaleTasks`, `PurgeTasksAndEvents`.
- `internal/bridge/pgstore_test.go` (~672 lines) — 15 real-Postgres store-level tests.
- `internal/bridge/pgstore_crossprocess_test.go` (~1050 lines) — 10 real-Postgres
  tests covering cross-process, execution lease, crash recovery, retention,
  dedup, and event delivery.
- `internal/bridge/testdata/a2a-testserver/main.go` (~167 lines) — Subprocess
  test server binary for two-OS-process tests. Prints `READY <pid> <port>`,
  exits on stdin close or SIGTERM.

### Modified files
- `cmd/scion-a2a-bridge/main.go` — Standalone mode now creates
  `PostgresTaskStore` instead of `InMemory` + `ScopedTaskStore`.
- `README.md` — Rewritten "Standalone mode and horizontal scaling" section.
  Corrected false transactional consistency claims. Documents execution
  lease, crash recovery, retention, dedup, SSE cursor semantics, and limitations.
- `.gitignore` — Added `/a2a-testserver` to prevent test binary from being committed.

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
-- Indexes: owner_key, (context_id, owner_key), (owner_key, updated_at DESC, id DESC)
```

## Design decisions

1. **Owner key at SQL level** — Replaces `ScopedTaskStore`'s process-local
   ownership map with a DB column. Cross-replica ownership enforcement is now
   transactional.
2. **JSONB payload** — Full `a2a.Task` stored as JSONB, enabling SQL-level
   status/timestamp filters without separate columns.
3. **CAS via version column** — `UPDATE ... WHERE version = $prev RETURNING version`
   distinguishes not-found from version-mismatch with a fallback existence check.
4. **Execution lease (exec_owner/exec_heartbeat)** — Only tasks with stale
   execution claims are reaped. Long-running legitimate work without claims is
   never affected. Atomic CAS on both version and exec_owner prevents double-reap.
5. **Single-transaction retention** — `PurgeTasksAndEvents` deletes from both
   `a2a_task_events` and `a2a_sdk_tasks` atomically in one SQL transaction.
6. **Separate connection pools** — Bridge state and SDK task store use independent
   `*sql.DB` pools. Not transactionally consistent by default. README corrected.
7. **Dedup at broker boundary** — Bridge event log uses `dedup_key` (from
   upstream message ID or SHA256 hash) with `ON CONFLICT DO NOTHING`.

## Test provisioning

Tests require `TEST_DATABASE_URL` env var pointing to a real Postgres instance.
Tests skip when unset. Local test setup:
```bash
sudo service postgresql start
TEST_DATABASE_URL="postgresql://scion:scion@localhost:5432/a2a_test?sslmode=disable" \
  go test -v -count=1 -run TestPostgresTaskStore ./internal/bridge/
```

## Residual risks

- Hub side-effect replay: crash after Hub send but before completion record
  leaves the side effect un-replayed; task transitions to `failed` via reaper.
- SSE stream locality: streams are process-local; resubscription from another
  replica creates a new stream reading from the shared event log.
- `go.mod`/`go.sum` updated for `grpc-gateway` and OTel version bumps
  triggered by `go mod tidy` (all indirect, no new direct deps).
