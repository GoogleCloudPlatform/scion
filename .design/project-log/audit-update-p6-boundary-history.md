# Audit update P6: boundary history remainder (#2404 remainder, #2405 reconciliation)

Tracker #2369. Governing design: plan sections "P6 rulings and implementation handoff" and the "P6-R addendum" (the addendum wins where they conflict), plus architect ruling R16 on the three developer questions. Base `8b95af1`.

## What changed

Milestone 1 made CREATE write its `access_constraint_history` row in the same transaction as the mutation. UPDATE, DELETE and the recovery operations still went through `BoundaryAuditWriter`: an in-process slice with no production reader, written after a separate auto-committed mutation, with best-effort compensation when the write failed. P6 replaces it.

- **Store.** `UpdateAccessConstraint` now runs on the caller's transaction when one is open: it has the same `ent.ErrTxStarted` branch as `CreateAccessConstraint`. That lets UPDATE and recovery run inside `Store.WithTx`.
- **UPDATE.** `updateAccessConstraintWithAudit` runs the mutation and exactly one validated history row in one `WithTx`, mirroring CREATE. The per-constraint 1,000-row cap and row lock are unchanged.
  - The row is built from a history-only `access_boundary/update` envelope (catalog destination: history; it is never passed to a sink).
  - `auditId` is the row's `event_id`, a UUID. It replaces the old `aud_<hex>` format. A grep of web/, cmd/ and pkg/hubclient found no consumer of that prefix.
  - The before revision is after−1: the store increments the revision by exactly one under the row it reads in the same transaction.
- **Recovery** (`RecoveryService`; still unwired, still tests only). `DisableAll` and `RecoverAll` take the acting `PrincipalContext`.
  - Each affected constraint runs in its own transaction: its mutation plus one row, `recovery_disable` (classification relax) or `recovery_enable` (tighten), processed sequentially.
  - The revision fields hold the revisions read in that transaction before and after the mutation. `DisableAccessConstraint` does not bump the revision, so a `recovery_disable` row truthfully shows before == after.
  - Results carry `AuditIDs`, aligned with the constraint IDs. The old aggregate `"all"` entry, with its counts in revision fields, is gone.
- **DELETE.** After the delete commits, exactly one `access_boundary/delete` envelope goes out (commit/succeeded, info, structured_log only).
  - It carries principal, resource (`access_constraint`, ID and scope as for CREATE), classification and `before_revision`. It has no name, subject, purpose or email.
  - In production, `initBoundaryServices` routes it through a sink over the server's shared asynchronous audit writer (`s.auditWriter`, the same writer under the decision log; not experiment-gated). CREATE keeps its own synchronous sink. A build or dispatch failure is logged with `failure_code=audit_sink_emit_failed` and never fails or compensates the delete.
  - `auditId` is the envelope's `event_id`.
  - The existing operational log lines, the cascade and the non-enumerating 404 are unchanged.
- **Correlation.** UPDATE, DELETE and recovery rows take correlation from `auditevent.OperationFromContext`. That is installed by `withAccessBoundaryAuditOperation`: the request ID, else one fresh operation ID per `CommitBoundaryChange` / `DisableAll` / `RecoverAll` call, shared by every row that call writes. Before P6 these paths read the never-set `correlationIDKey`, so the value was always empty.
- **Removed:** `BoundaryAuditWriter` with its slice, `AuditRequest`, `BoundaryAuditEntry`, `ImpactCounts`, `generateAuditID`, `validateAuditEntry`, the package-level `computeStateFingerprint`, `computeImpactCounts`, `compensateAuditFailure`, and the recovery rollbacks. The authzop mutation catalog is updated to match. `BoundaryMetrics` stays.
- **Fingerprint naming.** `(*PreviewService).computeStateFingerprint` is renamed `computeConstraintStateFingerprint`. Its mismatch message and the `CommitBoundaryChange` step comments no longer claim role bindings or memberships, because it hashes constraint state only. No logic change.

## History versus log: durability

- **UPDATE, recovery and CREATE rows are persisted.** They are committed with the mutation and readable through `GET …/access-constraints/{id}/audit` while the constraint exists and the row is within the cap.
- **The DELETE envelope is a structured log event, not a persisted record.** It is async, loss-counted and best-effort. Its `auditId` identifies a log event only: it is not retrievable through any API and is not backed by history, because the constraint's history is cascaded with the constraint. **After P6 a delete still has no durable persisted record.**

## Partial recovery semantics

The first failure stops the run and returns `*RecoveryPartialError` with `Committed` (k−1), the committed IDs and their audit IDs, the failed ID, and the cause (`errors.Is` works). Constraints committed before the failure stay committed, each with its row. Nothing is rolled back across constraints, and the failed constraint has neither its change nor a row. A rerun processes only the remaining constraints. `recovery.completed` is published only when the whole run succeeds (unchanged); there is no event for a partial run.

## Pending owner decision

**P6-OD1 (durable delete record)** is pending ptone's retention decision and was not made in P6. The options are: keep a tombstone or history row past the cascade, write a `mutation_audits` row for deletes, or accept log-only.

## Follow-ups (not done here)

- Q-C / D6: amend the #2405 AC2 text to the approved non-enumerating 404 at closure.
- Structured-log conversion of UPDATE and recovery stays the later boundary-family decision. Today they write history only.
- At the single forward merge, coordinate the `auditevent/catalog.go` and `auditevent_test.go` catalog regions with P4 (ptone#5711 / GCP#4009).
- The web audit timeline renders `recovery_disable` / `recovery_enable` as raw operation strings (cosmetic).
- `correlationIDFromContext` / `correlationIDKey` survive only as the fallback in `withAccessBoundaryAuditOperation`. No production code sets the key, so the fallback can be removed.
- `RecoveryLock` is per-process. If recovery is ever wired, multi-replica exclusion needs a database lock, and the caller must supply an operation context.
