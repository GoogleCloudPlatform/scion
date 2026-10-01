# Audit update milestone 1 / #2404

## Store checkpoint

This checkpoint adds the purpose-specific `access_constraint_history` read-model schema and store transaction primitives from the approved #2378 design. The table uses the audit event ID as its primary key, retains the stable constraint/time/operation/actor/correlation/revision/classification/preview/draft fields, and stores only the two typed composite payload leaves as JSON text. It does not store a rendered generic event envelope.

`constraint_id` is a foreign key to the live access constraint with `ON DELETE CASCADE`. The chronological index is `(constraint_id, occurred_at DESC, event_id DESC)`, with a separate occurred-at index. The append method requires the store supplied by `Store.WithTx`, locks the owning constraint row on PostgreSQL to serialize concurrent writers, inserts the row, and prunes outside the fixed newest-1,000 window using the same deterministic tuple order before the transaction can commit. The store lists retained rows newest-first. Access-constraint creation now detects an ambient Ent transaction so a later governance writer can use `Store.WithTx` without opening a nested transaction.

Focused tests prove that row 1,001 removes the oldest tuple while leaving another constraint untouched, direct append outside `WithTx` fails closed, deleting a live constraint cascades its timeline, history insert and prune-delete failures roll back an access-constraint create performed in the same `WithTx`, and committed history remains visible through both a second store instance and a reopened SQLite database. `TestConstraintHistory_ConcurrentCapPostgres` exercises two independent PostgreSQL clients racing from 999 equal-timestamp rows and requires the deterministic final 1,000-row tuple window.

## Blocked create-path integration

Governance wiring and post-commit dispatch are intentionally not part of this checkpoint because two blockers belong to the approved #2401 interface owner:

1. `auditevent` exposes `Sink` and `CaptureSink` but no production structured-logging sink. #2404 must dispatch the same in-memory envelope only after `WithTx` returns successfully; it must not invent a second renderer or logging contract.
2. The approved design makes `resource.project_id` optional for non-project-scoped resources, but the #2401 `access_boundary/create` catalog currently requires it. Existing access constraints support system scope and therefore have no project ID. The builder correctly rejects that event, so wiring it now would roll back every system-scoped create.

Once #2401 supplies the production sink and corrects the catalog requirement, the remaining #2404 integration point is `GovernanceService.CommitBoundaryChange`'s `create` branch: build one envelope inside `Store.WithTx`, create the constraint, map that exact envelope into `AccessConstraintHistory`, append through the transaction-scoped store (which enforces retention atomically), return on commit, then emit that same envelope through the supplied sink before publishing the existing invalidation event. Insert/build/prune failures must return from the callback so creation rolls back; sink failure remains post-commit and cannot roll back database state.

#2405 remains blocked from query/API/view work until that create-path integration is reviewed clean. Its eventual query should use `ListConstraintHistory` semantics as a starting point but replace the checkpoint's unpaged list with the approved tuple cursor contract.

## Verification

- `go test -count=1 -p 2 ./pkg/store/entadapter -run 'TestConstraintHistory|TestCreateAccessConstraint_SetsRevision1'`
- `git diff --check`

The PostgreSQL-specific concurrency regression is intentionally handed to the ii2 execution route and uses:

- `go test -tags integration -count=1 -timeout 10m -v -p 2 -run '^(TestConstraintHistory_ConcurrentCapPostgres)$' ./pkg/store/entadapter`

The ii2 external gate passed against exact code SHA `cc3db9dee634d0d62527035c6ad7144cd28fc820` using Go 1.26.1 and PostgreSQL 16.15. The command above exited 0; `TestConstraintHistory_ConcurrentCapPostgres` passed in 3.59 seconds (package time 4.195 seconds). Both concurrent appends succeeded after seeding 999 equal-timestamp rows. The final timeline contained exactly 1,000 rows: `event-1000` was newest, `event-0001` was oldest, and `event-0000` was evicted. The run had no deadlock or flake, and its harness database, container, volume, scratch data, and password were fully torn down.

Local `make ci` and `make ci-full` were not run because the campaign broker-workload rule prohibits them.
