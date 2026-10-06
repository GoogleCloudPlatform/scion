# Audit update milestone 1 / #2404

## Store checkpoint

This checkpoint adds the purpose-specific `access_constraint_history` read-model schema and store transaction primitives from the approved #2378 design. The table uses the audit event ID as its primary key, retains the stable constraint/time/operation/actor/correlation/revision/classification/preview/draft fields, and stores only the two typed composite payload leaves as JSON text. It does not store a rendered generic event envelope.

`constraint_id` is a foreign key to the live access constraint with `ON DELETE CASCADE`. The chronological index is `(constraint_id, occurred_at DESC, event_id DESC)`, with a separate occurred-at index. The append method requires the store supplied by `Store.WithTx`, locks the owning constraint row on PostgreSQL to serialize concurrent writers, inserts the row, and prunes outside the fixed newest-1,000 window using the same deterministic tuple order before the transaction can commit. The store lists retained rows newest-first. Access-constraint creation now detects an ambient Ent transaction so a later governance writer can use `Store.WithTx` without opening a nested transaction.

Focused tests prove that row 1,001 removes the oldest tuple while leaving another constraint untouched, direct append outside `WithTx` fails closed, deleting a live constraint cascades its timeline, history insert and prune-delete failures roll back an access-constraint create performed in the same `WithTx`, and committed history remains visible through both a second store instance and a reopened SQLite database. `TestConstraintHistory_ConcurrentCapPostgres` exercises two independent PostgreSQL clients racing from 999 equal-timestamp rows and requires the deterministic final 1,000-row tuple window.

## Create-path governance integration

The approved shared interface now supplies `SlogSink` and permits a system-scoped resource to omit `project_id`. At implementation checkpoint `f777aed`, `GovernanceService.CommitBoundaryChange`'s create branch was moved onto the required one-envelope transaction boundary:

- The service establishes one operation correlation at ingress, preserving an existing audit operation or trusted request ID and otherwise generating it once before the builder runs.
- Inside `Store.WithTx`, the transaction-scoped store creates the live constraint, builds and validates exactly one typed `access_boundary/create` envelope, maps the typed envelope directly to `AccessConstraintHistory`, and calls `AppendConstraintHistoryTx` for the atomic insert/cap operation. The history writer never parses rendered JSON.
- The envelope's event ID is the returned `AuditID`. Its event ID, occurred-at, principal, correlation/causation, revisions, classification, preview ID, draft hash, and defined composite payload leaves are the source of the purpose-specific history row.
- System constraints use explicit system scope and omit project ID. Project constraints use explicit project scope and require their project ID. The HTTP create handler adds only trusted request metadata from request-log middleware; principal, canonical credential metadata, and deferred executor attribution come from existing server-side contexts.
- Only after `WithTx` returns successfully does the service pass that same built envelope to its constructor-installed `auditevent.SlogSink`. A synchronous sink error is logged with the committed event and constraint IDs; it does not roll back or return a false mutation failure. Existing invalidation publication remains post-commit.
- CREATE no longer writes or compensates through the legacy in-memory `BoundaryAuditWriter`. UPDATE and DELETE retain their existing behavior and are outside this integration slice.

Focused failure tests prove that builder validation failure, history insert failure, and an injected post-insert/prune failure all return from the transaction callback, leaving no live constraint and making no sink call. The post-insert case first writes the history row and then returns an injected error, proving the transaction removes both it and the live row. A sink that verifies the database during `Emit` proves live state and the matching history event are already committed before dispatch. An injected sink error is called exactly once while the live row and single matching history row remain committed and the command still succeeds. Successful system- and project-scoped cases compare the committed history and structured record to the same event ID, time, correlation, resource, revision, and classification; the system structured record has no `project_id` key.

The existing store cap, restart/second-store visibility, and deterministic PostgreSQL concurrency evidence below remain the persistence proof beneath this integrated create path. A transaction commit failure was not separately injected because the Ent transaction wrapper has no safe test seam for a pre-commit failure after the callback; the production code retains the envelope only when `WithTx` returns nil and therefore cannot dispatch on any returned commit error.

#2405 remains the next handoff and is untouched here. It should add the approved tuple-cursor history query/API/view behavior on top of `ListConstraintHistory`; it must not change this create transaction or post-commit dispatch boundary.

## Verification

- `go test -count=1 -p 2 ./pkg/store/entadapter -run 'TestConstraintHistory|TestCreateAccessConstraint_SetsRevision1'`
- `git diff --check`

The PostgreSQL-specific concurrency regression is intentionally handed to the ii2 execution route and uses:

- `go test -tags integration -count=1 -timeout 10m -v -p 2 -run '^(TestConstraintHistory_ConcurrentCapPostgres)$' ./pkg/store/entadapter`

The ii2 external gate passed against exact code SHA `cc3db9dee634d0d62527035c6ad7144cd28fc820` using Go 1.26.1 and PostgreSQL 16.15. The command above exited 0; `TestConstraintHistory_ConcurrentCapPostgres` passed in 3.59 seconds (package time 4.195 seconds). Both concurrent appends succeeded after seeding 999 equal-timestamp rows. The final timeline contained exactly 1,000 rows: `event-1000` was newest, `event-0001` was oldest, and `event-0000` was evicted. The run had no deadlock or flake, and its harness database, container, volume, scratch data, and password were fully torn down.

Round-2 review determined that this first external run used a schedule-only goroutine release and did not prove the transactions overlapped. The strengthened regression now holds a PostgreSQL `FOR NO KEY UPDATE` lock in transaction A after inserting `event-0999`, identifies both transaction backend PIDs, and uses `pg_stat_activity` plus `pg_blocking_pids` to prove transaction B is waiting in the production `FOR UPDATE` query before A may commit. Completion before that observed lock wait fails the test, so removing the production `ForUpdate` call is detected deterministically. The strengthened ii2 external gate passed at exact code SHA `d7a51219a47bc3dbdd0443d77ff6acb461a29126` using Go 1.26.1 and PostgreSQL 16.15. The command above exited 0; the test passed in 3.13 seconds (package time 3.345 seconds). Before transaction A was released, the observer proved that transaction B was in a lock wait, `pg_blocking_pids(B)` contained A's backend PID, and B's active query contained `FOR UPDATE`. The atomic final 1,000-row cap held. The harness database, container, volume, scratch data, and password were fully torn down with no dangling resources.

Round-3 review found one test-only `errcheck` issue: the PostgreSQL backend-PID helper did not explicitly handle `Rows.Close`'s returned error. The cleanup now uses the adapter's established explicit-discard closure. `GOGC=40 golangci-lint run --enable-only=errcheck --concurrency=1 ./pkg/store/entadapter/...` reports `0 issues`, and `go test -count=1 -p 2 ./pkg/store/entadapter -run 'TestConstraintHistory|TestCreateAccessConstraint_SetsRevision1'` passes (package time 1.867 seconds). The external PostgreSQL test was not rerun because this cleanup changes neither production code nor concurrency-test behavior; the accepted PostgreSQL 16.15 evidence above remains unchanged.

The create-path governance integration passed these targeted gates:

- `go test -p 2 ./pkg/hub -run '^TestGovernanceCreateAudit_' -count=1` — PASS (package 2.427s).
- `go test -p 2 ./pkg/hub -run '^(TestGovernance_|TestGovernanceCreateAudit_|TestAudit_)' -count=1` — PASS (package 10.406s).
- `go test -p 2 ./pkg/store/entadapter -run '^TestConstraintHistory_' -count=1` — PASS (package 1.255s).
- `go test -race -p 2 ./pkg/hub -run '^TestGovernanceCreateAudit_' -count=1` — PASS (tests 46.899s).
- `go vet -p 2 ./pkg/hub ./pkg/store/entadapter` — exit 0.
- `go build -buildvcs=false -p 2 ./pkg/hub ./pkg/store/entadapter` — exit 0.
- `GOGC=40 golangci-lint run --new-from-rev=c610586d9f25f34a768529808b93bed53c2af90c --concurrency=1 ./pkg/hub/...` — `0 issues`.
- `git diff --check` — exit 0.

## Complete-integration review round 1

Review round 1 found two required integration defects at `4372ae9343cd2cf1c0cc3810f4edda5475f10e00`, both fixed at implementation checkpoint `fa9c217`:

1. A system-scoped constraint with a non-empty `ScopeID` could be written inside the create transaction before audit scope mapping silently omitted that ID. `accessConstraintAuditScope` now rejects this contradictory pair with a typed, stable `invalid_request` governance error that never echoes the rejected value. The project-scope mirror still requires a non-empty project ID, and unsupported scope types now also return a bounded typed error rather than reflecting the input. The regression observes the attempted live-row ID inside `Store.WithTx`, proves the error escapes the callback, and then proves the live row and history row are both absent and no envelope survives for sink dispatch.
2. Post-commit sink-failure telemetry previously attached the raw sink `error`, which could call and serialize unrestricted `Error()` text. The failure path now emits only stable `failure_code=audit_sink_emit_failed` plus the already-safe event and constraint IDs. A capture-handler regression injects a unique secret canary in the sink error and proves it is absent from every captured message, attribute key/value, and JSON-rendered record while the bounded failure code is present. The sink is called once, the command still succeeds, and exactly one live row and matching history row remain committed.

Round-1 targeted verification:

- `go test -p 2 ./pkg/hub -run '^(TestGovernanceCreateAudit_RejectsContradictorySystemScope|TestGovernanceCreateAudit_SinkFailurePreservesCommit)$' -count=1` — PASS (package 1.002s).
- `go test -p 2 ./pkg/hub -run '^TestGovernanceCreateAudit_' -count=1` — PASS (package 2.726s).
- `go test -race -p 2 ./pkg/hub -run '^TestGovernanceCreateAudit_' -count=1` — PASS (tests 60.557s).
- `go vet -p 2 ./pkg/hub` — exit 0.
- `go build -buildvcs=false -p 2 ./pkg/hub` — exit 0.
- `GOGC=40 golangci-lint run --new-from-rev=4372ae9343cd2cf1c0cc3810f4edda5475f10e00 --concurrency=1 ./pkg/hub/...` — `0 issues`.
- `gofmt` and `git diff --check` — clean.

Store/schema/cap code was unchanged, so the accepted focused store and PostgreSQL 16.15 evidence above was not rerun. #2405 remains gated and untouched.

Local `make ci` and `make ci-full` were not run because the campaign broker-workload rule prohibits them.
