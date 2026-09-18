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
17. **Store wrapper chain (R3)** — SDK→BarrierTaskStore→PostgresTaskStore.
    ScopedTaskStore removed (R3) — its in-memory ownership map was redundant
    with SQL enforcement and grew monotonically without bound.

## Test provisioning

```bash
sudo service postgresql start
TEST_DATABASE_URL="postgresql://scion:scion@localhost:5432/a2a_test?sslmode=disable" \
  go test -v -count=1 -run TestPostgresTaskStore ./internal/bridge/
```

## Milestone: Two-Replica Production Proof (RED-to-GREEN)

**Commits:** `e1cfa96` (RED), `09a759e` (GREEN)
**Branch:** `scion/dev-a2a-taskstore`, base `346b1f7` (origin/main), tip `12d063e`
**Proof artifact:** `/scion-volumes/scratchpad/projects/ge-a2a/taskstore-production-proof.md`

Added `TestTwoReplicaProductionPath_EndToEnd` — a two-process end-to-end test
proving the durable no-metadata broker correlation path works across distinct
OS processes sharing one PostgreSQL database.

**RED (e1cfa96):** Process B's `correlateToTask` fails (empty local cache,
task only in `a2a_sdk_tasks` not `a2a_tasks`) -> A times out -> TASK_STATE_FAILED.

**GREEN (09a759e):** `FindActiveSDKTaskForAgent` queries `a2a_sdk_tasks` for
unambiguous project+agent match -> B correlates durably -> A receives event ->
TASK_STATE_COMPLETED.

Changes:
- `pgstore.go`: Added `FindActiveSDKTaskForAgent` for durable no-metadata correlation
- `bridge.go`: Rewrote `correlateToTask` no-metadata path (local cache never authorizes alone)
- `testdata/a2a-testserver/main.go`: Added `-mode=production` (ScionExecutor + mock Hub)
- `pgstore_crossprocess_test.go`: Added production helpers + E2E test

Verification: race-clean, 3/3 repeated runs, all 7 cross-process tests pass,
`go build/vet ./...` clean.

### Expanded Production Proof (52c180d)

Addressed 6 EM findings. See `/scion-volumes/scratchpad/projects/ge-a2a/taskstore-production-proof.md`.

1. **Ambiguity fail-closed**: `FindActiveSDKTaskForAgent` uniqueness check always runs
   before accepting any cached nominee. 2 active tasks → rejection, 0 events.
2. **No legacy bypass**: SDK store authoritative when `sdkTaskStore != nil`. Legacy
   `a2a_tasks` only used in plugin mode (`sdkTaskStore == nil`).
3. **Streaming/resubscribe**: SSE `SubscribeToTask` on both replicas. Snapshot
   reflects current state, no replay, no `_bridgeEventID` leak.
4. **Negative streaming ownership**: Wrong caller/project get error -32001,
   no task metadata leakage.
5. **`_bridgeEventID` stripping**: DurableRequestHandler strips from all 5
   user-visible methods (GetTask, ListTasks, CancelTask, SendMessage,
   SendStreamingMessage). Hard wire-level assertions.
6. **Production context trace**: Unit test proves `WithRouteInfo` → `RouteInfoFrom`
   and `WithCallerIdentity` → `buildOwnerKey` → `PostgresTaskStore.Create` stores
   correct columns.

Changes:
- `bridge.go`: Refactored `correlateToTask` into `correlateWithMetadata` /
  `correlateWithoutMetadata` / `validateTopicUser`. No legacy bypass when SDK set.
- `durable_handler.go`: Added `_bridgeEventID` stripping to all user-visible paths.
- `pgstore_crossprocess_test.go`: 6 new tests (ambiguity, legacy bypass, streaming,
  negative streaming, metadata stripping, production context).

Verification: all 13 tests pass (7 original + 6 new), race-clean, `go build/vet`
clean. Clean rebase onto `346b1f7` (origin/main), no semantic conflicts.

### Rebase onto `346b1f7` (2026-09-18)

Conflict-free rebase onto current `origin/main` (`346b1f74ceaf541b2a8838c7b14b6a68ec73baa3`).
Changed-file intersection with main: empty. No semantic conflict resolution needed.

Rebased commit hashes:
- `e1cfa96` — RED test
- `09a759e` — GREEN fix
- `52c180d` — Expanded proof (6 findings)
- `12d063e` — Project log docs (branch tip)

All verification re-run post-rebase:
- 13/13 tests pass (7 cross-process + 6 production proof)
- Race detector: clean
- Full bridge test suite: pass (except pre-existing `TestNotifyAcceleratesDelivery`)
- `go build ./...` and `go vet ./...`: clean
- `git diff --check HEAD~12..HEAD`: clean

### Review Round 2 Findings (2026-09-18)

Addressed 4 EM findings from audit round 2 on `12d063e`.

#### 1. Canonical terminal-state handling (Critical — RESOLVED)

**Problem:** `FindActiveSDKTaskForAgent`, partial index `idx_a2a_sdk_tasks_agent`,
and migration terminalization used lowercase states (`'completed'`, etc.) but SDK
persists uppercase `TASK_STATE_*` via JSON marshal. Terminal tasks were never
excluded from active queries.

**Fix:**
- Introduced `sdkTerminalStates` / `legacyTerminalStates` / `allTerminalStatesSQL()`
  for a single source of truth on terminal state strings.
- `FindActiveSDKTaskForAgent`: uses `allTerminalStatesSQL()` (both cases).
- Migration: terminalization writes `TASK_STATE_FAILED` (canonical uppercase).
  Added normalization step: any legacy lowercase terminal → `TASK_STATE_FAILED`.
- Partial index: DROP + CREATE with canonical `sdkTerminalStatesSQL()`.
  Idempotent under advisory lock.
- `PurgeTasksAndEvents`: uses `allTerminalStatesSQL()` (both cases purgeable).
- `ClaimExecution`/`ReapStaleTasks` query predicates unchanged (already uppercase)
  but CAS guard also uses `allTerminalStatesSQL()` for completeness.

**Tests:** `TestTerminalStateExclusion`, `TestFinishTask1CreateTask2CorrelatesUniquely`,
`TestMigrationIdempotentAndNormalizesIndex`, `TestLegacyLowercaseTerminalExcluded`.

#### 2. Atomic terminal reap + durable event (Critical — RESOLVED)

**Problem:** `ReapStaleTasks` updated `a2a_sdk_tasks` then caller separately
appended events to `a2a_task_events`. Crash between them lost the event.
`Final: true` not set. Startup path emitted no events.

**Fix:**
- `ReapStaleTasks` now does CAS state transition AND `Final=true` event insert
  in a single Postgres transaction per task (`reapOneTask`).
- Dedup key `reap:<taskID>` prevents duplicate events on retry.
- Transaction rollback on any failure — no state-without-event possible.
- `reapStaleSDKExecutions` (janitor caller) simplified — no longer emits events.
- Startup path (main.go) already calls `ReapStaleTasks` directly — now
  atomically includes events. Both paths use the same method.

**Tests:** `TestAtomicReapTransactionRollback`, `TestReapIdempotencyOneEventOnly`,
`TestStartupAndJanitorShareReapPath`, `TestReapedTaskVisibleCrossReplica`.

#### 3. Notifier test isolation (Required — RESOLVED)

**Problem:** `TestNotifyAcceleratesDelivery` used hardcoded task ID `"notify-accel-1"`.
Cleanup only purged events, not the task row. Repeated runs → SQLSTATE 23505.

**Fix:** Unique task ID per run (`time.Now().UnixNano()`). Cleanup deletes both
events and the task row. 3x repeated run verified clean.

#### 4. Nit dispositions (Required — RESOLVED)

**README pool description:** Changed "separate connection pools" to "single shared
`*sql.DB` connection pool" with accurate description.

**ScopedTaskStore.ownership — RESOLVED in Round 3:**
Removed from standalone production path. The in-memory ownership map grew
monotonically without bound in long-lived processes. PostgresTaskStore
enforces `owner_key` at the SQL level on every operation. SDK now receives:
SDK → BarrierTaskStore → PostgresTaskStore (no ScopedTaskStore).

### Review Round 3 (2026-09-18)

Addressed 6 EM findings + security audit + ScopedTaskStore removal.

1. **Migration advisory lock connection scope (Critical):** `migrate()` now uses
   `*sql.Conn` to pin lock/migration/unlock to a single connection. Test:
   `TestConcurrentMigrationSerialization` (5 concurrent, no deadlock).
2. **Legacy terminal mapping (Critical):** `legacyToCanonical` map preserves
   individual mappings (completed→COMPLETED, canceled→CANCELED, etc.). Test:
   `TestLegacyTerminalMappingPreservesSemantics`.
3. **Notifier cleanup scope (Required):** Scoped `DELETE ... WHERE task_id = $1`
   with canary survival verification.
4. **Reap error propagation (Required):** `errors.Join` aggregates failures
   alongside partial successes. Test: `TestReapErrorPropagation`.
5. **One state predicate (Required):** `terminalStatesSQL` computed once at `init()`
   from `sdkTerminalStates`. All runtime predicates use this single source.
6. **Real rollback injection (Required):** CHECK constraint forces transaction
   rollback. Two-phase test proves atomicity.
7. **Security: PurgeTaskEvents cleanup (Required):** Eliminated all broad
   `PurgeTaskEvents` from 12 tests. Each uses scoped DELETE with unique per-run IDs.
8. **ScopedTaskStore removal (Required):** Removed from standalone path. SDK →
   BarrierTaskStore → PostgresTaskStore. No in-memory ownership map.

Verification: 280 tests pass (race clean), `go build/vet` clean.

### Review Round 4 (2026-09-18) — Correction at tip `772fbd1`

Addressed 1 Required finding + 1 Nit from round 4 review.

1. **Unscoped test cleanup (Required — RESOLVED):** Eliminated all bare
   `DELETE FROM a2a_sdk_tasks` and `DELETE FROM a2a_task_events` (19+ sites
   across `pgstore_test.go` and `pgstore_crossprocess_test.go`). Additionally
   fixed 7 static LIKE patterns and 12 static project_id predicates. All
   cleanup now uses `WHERE id = $1`, `WHERE project_id = $1`, or
   `WHERE task_id = $1` with per-run unique predicates generated via
   `time.Now().UnixNano()` or `randomSuffix()`.

2. **Startup reap logging nit (Nit — RESOLVED prior):** `main.go:508-516`
   already uses independent `if` checks (not `if/else if`), matching the
   janitor pattern in `bridge.go`.

3. **Canary assertion added:** `TestPostgresTaskStoreCanarySurvival` creates
   a canary row, performs scoped cleanup of an unrelated row, and verifies
   the canary survived.

4. **Indirect destructive helper scan:** Verified zero `PurgeTaskEvents`
   calls in test cleanup, zero `TRUNCATE`, zero shared cleanup functions
   with broad scope.

Verification: All tests pass against real PostgreSQL (100s), `go build/vet`
clean. Comprehensive `rg` scan confirms zero unpredicated destructive
statements in all test files.

### Manager preflight corrections (2026-09-18) — after round 4 candidate evidence

Precision corrections required by EM preflight before review round 5 consumption.

#### Corrections at `7151dd2`:

1. **state_test.go bare DELETEs → scoped LIKE cleanup**: All bare `DELETE FROM`
   statements replaced with LIKE-pattern cleanup scoped to per-run suffix.
2. **DDL constraint names → per-run unique**: Global constraint names made
   per-run unique with per-task-ID predicates.
3. **PurgeTaskEvents schema isolation**: Test runs in isolated per-run
   PostgreSQL schema (CREATE SCHEMA + search_path). Exact `n == 1` preserved.
4. **PurgeTasksAndEvents schema isolation**: Same approach, both state and
   bridge store tables migrated in isolated schema.
5. **UUID v7 collision**: `a2a.NewTaskID()[:8]` → `randomSuffix()`.
6. **CrashRecoveryKill race**: Timing below janitor threshold + membership check.
7. **LegacyTerminalMapping race**: Insert with `NOW()`, backdate only for purge.

#### Correction at `4f535ad`:

**Ownership-scoped advisory lock check:** Tagged all 5 test-owned migration
connections with a unique per-run `application_name` via `pgx.ParseConfig` +
`stdlib.RegisterConnConfig`. Lock-leak query changed from unscoped
`pg_locks WHERE objid = $1` to `pg_locks JOIN pg_stat_activity ON pid
WHERE application_name = $2 AND objid = $1`. Retry loop eliminated.

#### Correction at current tip:

**Regression test proof flaw:** `TestAdvisoryLockOwnershipScopedQuery` previously
held the unrelated connection on `testLockID+1` (different key), so the `objid`
filter alone explained `seesUnrelated=false`. Corrected to two sequential phases
using the SAME `testLockID` — only the `application_name` ownership filter
distinguishes them:
- Phase 1: owned marker acquires testLockID → query detects it →
  `pg_advisory_unlock` with `QueryRowContext.Scan(&released)` asserts
  `released==true` → conn closed
- Phase 2: unrelated marker acquires SAME testLockID → query with owned marker
  and same lock type/key returns false → `pg_advisory_unlock` with
  `QueryRowContext.Scan(&released)` asserts `released==true` → conn closed
- Both connections proven clean; no deferred silent unlocks

#### Test categorization

**Schema-isolated retention suites** (genuinely isolated per-run schemas):
- `TestPostgresStore/PurgeTaskEvents` — isolated state store
- `TestPostgresTaskStorePurgeTasksAndEvents` — isolated bridge + state stores

**Shared-schema concurrent proofs** (all processes share same database):
- 5-pair concurrent process proof with canary row survival
- `TestConcurrentMigrationSerialization` with ownership-scoped lock check
- `TestAdvisoryLockOwnershipScopedQuery` two-phase same-key regression

Verification at tip `ebcf4d4`: All tests pass against real PostgreSQL.
5-pair concurrent proof with canary byte-for-byte survival. Ownership
regression asserts explicit `pg_advisory_unlock` boolean results (both
`released==true`, no deferred silent unlocks). `go build/vet ./...` clean.

## Residual risks

- Hub side-effect replay: crash after Hub send but before completion record
  leaves the side effect un-replayed; task transitions to `failed` via reaper.
- SSE stream locality: streams are process-local.
- `go.mod`/`go.sum` updated for `grpc-gateway` and OTel version bumps
  (all indirect, no new direct deps).

### Post-acceptance lifecycle correction (2026-09-18)

The #1620 combined topology found three defects in accepted HA tip `23911e5`
(the relevant code is identical at reviewed parent `ebcf4d4`). The temporary
branch was created from that exact tip without advancing
`scion/dev-a2a-taskstore`.

- `919fcb1` is the committed RED for input-required return, continuation cursor
  replay, and caller/project/agent ownership negatives. `fd2f04a` corrects those
  boundaries using authenticated `RouteInfo` + `CallerIdentity` and
  `GetOwnedTaskSnapshotAndCursor`.
- `422b8f3` is the controlled cancel-convergence RED. On exact `23911e5` and
  intermediate `fd2f04a`, the SDK snapshot became canceled while the original
  lease remained, no final bridge event existed, the waiter exceeded the
  semantics-derived three-second propagation bound, and a late reply appended
  two events. Wrapper prototype `75fd9de` proved the mechanism but is
  intentionally superseded because it retained a crash window.
- `e5918d3` moves canceled snapshot/version CAS, lease release, task-scoped
  deduplicated final-event insertion, and cursor advancement into one PostgreSQL
  transaction. `a38768f` injects a real event constraint failure and proves the
  state/version/lease/cursor and event log all roll back. `f503e13` ensures a
  waiter whose lease was atomically released can emit only the matching canceled
  final boundary. Late broker replies are fenced without changing other terminal
  policy.
- Production proofs use two independent bridge processes sharing real
  PostgreSQL. Wrong caller/project/agent cannot cancel, read, or advance cursor;
  the original waiter returns canceled, the lease is released, terminal
  resubscribe is coherent, and the original Hub send is not replayed.

Final-tip verification used task-owned schema `final_correction_20260918`:

```text
lifecycle + atomic cancel suite, count=3: PASS (34.406s)
lifecycle + atomic cancel suite, -race: PASS (12.539s)
accepted cross-replica/cursor/ownership/reaper set: PASS (34.243s)
same accepted regression set, -race: PASS (35.724s)
full module go test ./... at final code tip: PASS (bridge 86.741s)
full module go test -race ./... at final code tip: PASS (bridge 89.027s)
go vet ./internal/bridge: PASS
go build -buildvcs=false ./...: PASS
pre-existing schema canary: preexisting|must-survive
```

The fixture-only closed-SSE-channel observation and 700ms crash-wait timing are
separate from these confirmed product defects and are not counted as product
PASS/FAIL evidence. No live cloud calls were made; external-live remains false.
The correction proves task-scoped cursor/dedup behavior, not global exactly-once
semantics.

### Sixth-cycle upstream review follow-up (2026-09-18)

The follow-up branch `scion/dev-a2a-ha-lifecycle-pr-fixes` was created directly
from immutable correction `70c47cda668c87e7cc6556b63f82c0204d56141c`.
Current `origin/main` was exactly
`21c380344b774fc09a9147f38f9b96be4ca77f34`; it was merged normally in
`80f43eacccf5414b9bb668a805ecca3f0616f82c`. The merge had no textual
conflicts. Its six changed files were unrelated root-module formatting under
`pkg/hub` and `pkg/hubclient`; there was no A2A bridge, nested-module, or
dependency overlap. The unmerged baseline-tidy comparison at `07721e83` was
not imported, and `go.mod`/`go.sum` remain unchanged.

Two bounded corrections were made with committed RED evidence:

- `7fa428e56` reproduces `BarrierTaskStore.Create(ctx, nil)` panicking after
  `PostgresTaskStore.Create` returns wrapped `a2a.ErrInvalidRequest`;
  `f9a23493f` returns the inner result before reading `task.ID`. Public-path
  tracing through `jsonrpcHandler.ServeHTTP` -> `onSendMessage` ->
  `defaultRequestHandler.handleSendMessage` -> `ScionExecutor.Execute` ->
  `a2a.NewSubmittedTask` -> SDK `taskupdate.Manager.saveVersionedTask` shows
  that a public JSON-RPC request cannot supply this nil store argument: malformed
  or nil messages are rejected and the executor yields a non-nil task. The
  correction therefore hardens the internal store contract and prevents a
  direct/internal caller panic; it does not close a demonstrated remotely
  triggerable daemon-crash path.
- `81efa555d` proves string matching misses a wrapped PostgreSQL SQLSTATE 23505
  and falsely accepts both unrelated duplicate-key text and a SQLSTATE 23503
  carrying that text. `555bc3470` classifies through `errors.As` and the
  driver's `SQLState()` contract. The existing real-PostgreSQL duplicate-create
  regression also passes, proving actual driver error mapping to
  `taskstore.ErrTaskAlreadyExists`.

Go 1.26.1 `gofmt` was applied to the two attributed files
`pgstore_crossprocess_test.go` and `testdata/a2a-testserver/main.go` in
`97c9cd187`; `gofmt -l .` then reported no files for the whole bridge module.
No `time.After` change was made because Go 1.26 removes the alleged timer leak
and allocation optimization is outside scope. No Artifact nil check was added:
Artifact is a value struct and ranging nil `Parts` is safe.

Fresh real-PostgreSQL 15.19 verification used a task-local loopback daemon and
an unrelated task/event canary. The canary's before/after payload hashes were
identical (`42064f3eefa2a11c25b35c1e3f52fd23` and
`31082dfd852e84b48454c90f4f755dad`). Results:

```text
nil barrier focused RED: expected panic at barrier_store.go:96
nil barrier focused GREEN: PASS
SQLSTATE focused RED: all three required cases failed under string matching
SQLSTATE + real duplicate-create GREEN: PASS (0.058s)
lifecycle/cancel/rollback/ownership, count=3: PASS (36.497s)
same lifecycle set, -race: PASS (12.939s)
cross-process/cursor/ownership/reaper set: PASS (36.026s)
same accepted regression set, -race: PASS (36.969s)
full bridge/state: PASS (90.350s / 0.429s)
full bridge/state, -race: PASS (91.959s / 1.467s)
go vet ./...: PASS
go build -buildvcs=false ./...: PASS
Go 1.26.1 module-wide format and git diff --check: PASS
```

The original `origin/scion/dev-a2a-taskstore` ref remained exactly
`23911e531c4d3cb8f9d96d8b111a8329db3822e3`. The implementation-and-format tip
before this log update is `97c9cd187e6a01dda4b685b3954523dc0cc3cd5e`;
the pushed final ref is recorded in the durable sixth-cycle report.

### Integration-discovered state migration serialization correction (2026-09-18)

The temporary correction branch `scion/dev-a2a-state-migration-lock-fix` was
created directly from accepted HA tip
`77550f0e3e66397b787e93ffef72e6c9b0c2206b`. The original PR and component refs
were not moved. This is a bounded baseline release-defect correction discovered
by the final two-replica assembly run, not a reopening of the HA architecture.

Committed RED `7952547f5b8822f9e959c7dbd05a134e344e966f` proved that concurrent
`state.NewPostgres` constructors race PostgreSQL catalog creation on an empty
schema. Twenty in-process constructors and two independently synchronized OS
processes both reproduced SQLSTATE 23505 before the production change. A held
schema-specific advisory lock also proved that the old migration ignored the
intended serialization boundary.

Correction `ce5eb34f070239e738ccf3f300f25ecea3781156` changes only
`internal/state.PostgresStore.migrate`. It begins one transaction, resolves
`current_database()` and the actual `current_schema()`, acquires
`pg_advisory_xact_lock(hashtext(database), hashtext(schema))`, and executes
every DDL statement and commit on that transaction. The transaction therefore
pins lock ownership and DDL to one session; commit, rollback, process loss, or
connection loss releases the lock. Equal database/schema pairs serialize,
while distinct schemas remain independent.

Fresh-schema real-PostgreSQL verification covered same-schema contention,
distinct-schema noninterference, two production-constructor processes,
migration failure rollback, killed-process cleanup, no retained advisory
locks, and existing-data idempotence. Results:

```text
focused correction suite, count=5: PASS (6.282s)
focused correction suite, -race count=3: PASS (8.603s)
full internal/state: PASS (1.651s)
full internal/state + internal/bridge: PASS (1.936s / 93.417s)
full internal/state + internal/bridge, -race: PASS (4.225s / 92.810s)
go vet ./...: PASS
go build -buildvcs=false ./...: PASS
Go format and git diff --check: PASS
neighboring final-assembly database canary: must-survive
```

No lease, broker fencing, SSE/crash, authentication, transport, or deduplication
code changed. The correction does not claim exactly-once delivery. Exact pushed
tip and commands are recorded in the durable migration-lock correction report.

### PR #1742 current-main CI composition fix (2026-09-18)

The temporary composition fix branch `scion/dev-ha-ci-composition-fix` was
created directly from accepted HA tip `84f797b1cc1de8e9be3f57b35c25849920552737`.
The original `origin/scion/dev-a2a-taskstore` ref remained untouched at `84f797b1`.
Current main `b5b2590684d9a824e396632bad53d2d31201361d` was merged normally with
a two-parent merge commit (`9893dc19f02482f4ae9ca82178fc8269c689dd1b`).

Merging current main into the HA branch surfaced an A2A bridge test compilation
failure in job `105663452052`: `mockHubClient.Messaging` was declared twice in
`extras/scion-a2a-bridge/internal/bridge/followup_test.go` (at line 163 and line 174).
Main's commit `b5b25906` introduced `Messaging()` at line 163, while the HA
branch had introduced an identical stub at line 174. The duplicate declaration
at line 174 was removed, preserving column alignment and mock behavior matching
current main.

No auth fixture allowlist modifications, conversation-upsert guard alterations,
or module version bumps beyond accepted current main were made. Read-only
inspection of accepted transport candidate `c82fdab076e044b3038b7014320127c152b6c27b`
against current main `b5b25906` confirmed that an identical duplicate mock
declaration occurs upon merging main, with zero A2A module or compat literal issues.

Verification gates against task-local PostgreSQL 15.19 (Debian 15.19-0+deb12u1):
- State migration concurrency suite (`TestConcurrentPostgresStateMigrations`,
  `TestPostgresStateMigrationTwoProductionProcesses`,
  `TestPostgresStateMigrationLockScopesByDatabaseAndSchema`,
  `TestPostgresStateMigrationFailureAndProcessCancellationReleaseLock`,
  `TestPostgresStateMigrationExistingDataIdempotent`):
  - Normal count=1: PASS (1.272s)
  - Repetition count=5: PASS (6.194s)
  - Race count=3: PASS (8.633s)
- Focused lifecycle and ownership regressions: PASS (12.130s)
- Accepted HA cross-process, cursor, and ownership regressions: PASS (37.499s)
- Complete bridge and state suites: PASS (state 1.742s, bridge 96.746s)
- Static analysis: Go formatting (`make fmt-check`), git diff check, `make compat-literals`,
  bridge and root `go vet` and `go build`, scoped lint: PASS
- Test canary (`test_canary.sentinel`): survived byte-for-byte across all suites.
