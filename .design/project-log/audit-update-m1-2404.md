# Audit update milestone 1 / #2404

## Store checkpoint

This checkpoint adds the purpose-specific `access_constraint_history` read-model schema and store transaction primitives from the approved #2378 design. The table uses the audit event ID as its primary key, retains the stable constraint/time/operation/actor/correlation/revision/classification/preview/draft fields, and stores only the two typed composite payload leaves as JSON text. It does not store a rendered generic event envelope.

`constraint_id` is a foreign key to the live access constraint with `ON DELETE CASCADE`. The chronological index is `(constraint_id, occurred_at DESC, event_id DESC)`, with a separate occurred-at index. Store methods append a row, prune rows outside the fixed newest-1,000 window using the same deterministic tuple order, and list retained rows newest-first. Access-constraint creation now detects an ambient Ent transaction so a later governance writer can use `Store.WithTx` without opening a nested transaction.

Focused tests prove that row 1,001 removes the oldest tuple while leaving another constraint untouched, deleting a live constraint cascades its timeline, a history insert failure rolls back an access-constraint create performed in the same `WithTx`, and committed history remains visible through both a second store instance and a reopened SQLite database.

## Blocked create-path integration

Governance wiring and post-commit dispatch are intentionally not part of this checkpoint because two blockers belong to the approved #2401 interface owner:

1. `auditevent` exposes `Sink` and `CaptureSink` but no production structured-logging sink. #2404 must dispatch the same in-memory envelope only after `WithTx` returns successfully; it must not invent a second renderer or logging contract.
2. The approved design makes `resource.project_id` optional for non-project-scoped resources, but the #2401 `access_boundary/create` catalog currently requires it. Existing access constraints support system scope and therefore have no project ID. The builder correctly rejects that event, so wiring it now would roll back every system-scoped create.

Once #2401 supplies the production sink and corrects the catalog requirement, the remaining #2404 integration point is `GovernanceService.CommitBoundaryChange`'s `create` branch: build one envelope inside `Store.WithTx`, create the constraint, map that exact envelope into `AccessConstraintHistory`, append and prune through the transaction-scoped store, return on commit, then emit that same envelope through the supplied sink before publishing the existing invalidation event. Insert/build/prune failures must return from the callback so creation rolls back; sink failure remains post-commit and cannot roll back database state.

#2405 remains blocked from query/API/view work until that create-path integration is reviewed clean. Its eventual query should use `ListConstraintHistory` semantics as a starting point but replace the checkpoint's unpaged list with the approved tuple cursor contract.

## Verification

- `go test -count=1 -p 2 ./pkg/store/entadapter -run 'TestConstraintHistory|TestCreateAccessConstraint_SetsRevision1'`
- `git diff --check`

Local `make ci` and `make ci-full` were not run because the campaign broker-workload rule prohibits them.
