# Postgres stress / integration test suite

This package exercises the Postgres-backed store under realistic, adversarial
conditions that **do not exist on the single-writer SQLite backend**: row-level
contention, transaction isolation, connection-pool saturation, LISTEN/NOTIFY
delivery, large-dataset migration, strict type/schema edge cases, and
multi-process coordination.

It complements — rather than duplicates — the CRUD-parity suites in
`pkg/store/storetest` and `pkg/store/entadapter`, which run the *same* tests
against both backends to prove behavioral parity. Everything here is Postgres-only
and asserts behavior that only a real, concurrent, multi-writer database exhibits.

## Running

All tests are gated by the `integration` build tag **and** require a live
Postgres reachable via `SCION_TEST_POSTGRES_URL`. Without that variable every
test skips (and the default `go test ./...` build sees the package as empty).

```sh
# Local Postgres
SCION_TEST_POSTGRES_URL='postgres://scion:scion@localhost:5432/scion?sslmode=disable' \
  go test -tags integration ./pkg/store/integrationtest/...

# CloudSQL (e.g. via the auth proxy on localhost)
SCION_TEST_POSTGRES_URL='postgres://USER:PASS@127.0.0.1:5432/DB?sslmode=disable' \
  go test -tags integration -timeout 20m ./pkg/store/integrationtest/...
```

The suite provisions one ephemeral database per package run (created and dropped
automatically) and an isolated schema per test, so it never touches existing data
and parallel runs never collide.

### Knobs

| Variable                   | Default | Meaning                                            |
| -------------------------- | ------- | -------------------------------------------------- |
| `SCION_TEST_POSTGRES_URL`  | (unset) | Live Postgres DSN; unset ⇒ all tests skip.         |
| `SCION_TEST_CONCURRENCY`   | `10`    | Worker count for contention/pool tests (≥ 2).      |

Target wall-clock: **< 5 min** against a local Postgres, **< 15 min** against
CloudSQL, at the default concurrency.

## What's covered

1. **Contention** (`contention_test.go`) — `state_version` CAS race (no lost
   updates; ≥ N-1 retries; final version == 1+N), SKIP-LOCKED / conditional-UPDATE
   scheduled-event claim (single winner; disjoint drain of a pool), and unique
   constraint races on project slug / user email / agent (slug, project_id).
2. **Transaction isolation** (`isolation_test.go`) — SERIALIZABLE conflict with
   `RunSerializable` retry recovery, REPEATABLE READ snapshot stability (no
   phantom), READ COMMITTED dirty-read prevention.
3. **Connection pool** (`pool_test.go`) — exhaustion + queued recovery, saturated
   pool honoring the context deadline, long transaction not starving short
   queries, and pool healing after backends are killed with
   `pg_terminate_backend`.
4. **LISTEN/NOTIFY** (`notify_test.go`) — ordered burst delivery with no drops,
   the 8000-byte payload limit (which motivates the publisher's
   reference-and-refetch offload), listener reconnect/resume, and cross-channel
   isolation. (The higher-level `PostgresEventPublisher` refetch/resubscribe
   behavior is covered in `pkg/hub/events_postgres_test.go`.)
5. **Migration** (`migration_test.go`) — 1000+ row dataset with correct counts
   and bounded-memory listing, and idempotent re-migration that preserves data
   (the property that makes a killed/restarted migration safe).
6. **Schema / type edge cases** (`schema_test.go`) — NULL semantics, Unicode/emoji
   round-trip, nested JSON with special characters, large-text non-truncation, and
   TIMESTAMPTZ microsecond precision.
7. **Multi-process** (`multiprocess_test.go`) — forks the test binary so two
   separate OS processes contend for an advisory lock (exactly one wins) and a
   child-published NOTIFY is delivered to a listener in the parent.
