# Postgres-backed SDK task store for A2A bridge standalone mode

**Date:** 2026-09-17
**Issue:** #1618
**Branch:** `scion/dev-a2a-taskstore`
**Author:** dev-a2a-taskstore agent

## Summary

Implemented a durable PostgreSQL-backed `taskstore.Store` for the A2A bridge's
standalone mode. Previously, standalone mode used `taskstore.NewInMemory` wrapped
by `ScopedTaskStore` for process-local ownership enforcement — task state was
lost on restart and invisible across replicas. The new `PostgresTaskStore`
persists full `a2a.Task` payloads in a shared Postgres database with
transactional ownership enforcement, CAS/version semantics, and cursor-based
pagination.

## Changes

### New files
- `internal/bridge/pgstore.go` — `PostgresTaskStore` implementing
  `taskstore.Store` backed by `a2a_sdk_tasks` table. Schema auto-migrates.
  Ownership derived via `buildOwnerKey(ctx)` (same logic as `ScopedTaskStore`).
  CAS updates via `WHERE version = $prev`. List supports contextID, status,
  timestamp filters and cursor-based pagination.
- `internal/bridge/pgstore_test.go` — 15 real-Postgres tests covering:
  CreateAndGet, DuplicateCreate, UpdateCAS, CrossReplicaCreateReadList,
  CrossReplicaCancelContinue, RouteIsolation, CallerIsolation, ConcurrentCAS,
  RestartRecovery, UpdateNonexistent, GetNonexistent, EmptyCallerRejected,
  NoRouteRejected, ListPagination, HistoryPreservation.

### Modified files
- `cmd/scion-a2a-bridge/main.go` — Standalone mode now creates
  `PostgresTaskStore` instead of `InMemory` + `ScopedTaskStore`.
- `README.md` — Added "Standalone mode and horizontal scaling" section
  documenting task ownership, CAS semantics, cross-replica access, restart
  recovery, and in-flight execution limitations.

## Schema

```sql
CREATE TABLE a2a_sdk_tasks (
    id TEXT PRIMARY KEY,
    context_id TEXT NOT NULL DEFAULT '',
    owner_key TEXT NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
4. **No in-flight execution lease** — Clearly documented as a limitation. Tasks
   have durable terminal state but no active execution handoff between replicas.

## Test provisioning

Tests require `TEST_DATABASE_URL` env var pointing to a real Postgres instance.
Tests skip when unset. Local test setup:
```bash
sudo service postgresql start
TEST_DATABASE_URL="postgresql://scion:scion@localhost:5432/a2a_test?sslmode=disable" \
  go test -v -count=1 -run TestPostgresTaskStore ./internal/bridge/
```

## Residual risks

- In-flight execution on a dying replica is lost; task state is preserved but
  work-in-progress is not resumed.
- SSE stream resubscription is replica-local; cross-replica stream migration
  requires external routing coordination.
- `go.mod`/`go.sum` updated for `grpc-gateway` and `otelslog` version bumps
  triggered by `go mod tidy`.
