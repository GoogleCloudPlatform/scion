# Audit update P6: boundary history remainder (#2404 remainder, #2405 reconciliation)

Tracker #2369. Governing design: plan sections "P6 rulings and implementation handoff" and the "P6-R addendum" (the addendum wins where they conflict), plus architect ruling R16 on the three developer questions. Base `8b95af1`.

## What changed

Milestone 1 made CREATE write its `access_constraint_history` row in the same transaction as the mutation. UPDATE, DELETE and the recovery operations still went through `BoundaryAuditWriter`: an in-process slice with no production reader, written after a separate auto-committed mutation, with best-effort compensation when the write failed. P6 replaces it.

- **Store.** `UpdateAccessConstraint` now runs on the caller's transaction when one is open: it has the same `ent.ErrTxStarted` branch as `CreateAccessConstraint`. That lets UPDATE and recovery run inside `Store.WithTx`.
- **UPDATE.** `updateAccessConstraintWithAudit` runs the mutation and exactly one validated history row in one `WithTx`, mirroring CREATE. The per-constraint 1,000-row cap and row lock are unchanged.
  - The row is built from a history-only `access_boundary/update` envelope (catalog destination: history; it is never passed to a sink).
  - `auditId` is the row's `event_id`, a UUID. It replaces the old `aud_<hex>` format. A grep of web/, cmd/ and pkg/hubclient found no consumer of that prefix.
  - The before revision is read in the same transaction before the mutation. If the store's result is not exactly that revision plus one, the update is refused as a revision conflict, so the recorded revisions are always the true pair.
- **Recovery** (`RecoveryService`; still unwired, still tests only). `DisableAll` and `RecoverAll` take the acting `PrincipalContext`.
  - Each affected constraint runs in its own transaction: its mutation plus one row, `recovery_disable` (classification relax) or `recovery_enable` (tighten), processed sequentially.
  - The revision fields hold the revisions read in that transaction before and after the mutation. `DisableAccessConstraint` does not bump the revision, so a `recovery_disable` row truthfully shows before == after.
  - Each transaction re-reads its constraint first. A constraint that no longer exists, or whose disabled state already matches the target (it changed after the list was taken), is skipped: no mutation, no row, not counted.
  - Results carry `AuditIDs`, aligned with the constraint IDs. The old aggregate `"all"` entry, with its counts in revision fields, is gone.
- **DELETE.** After the delete commits, exactly one `access_boundary/delete` envelope goes out (commit/succeeded, info, structured_log only).
  - It carries principal, resource (`access_constraint`, ID and scope as for CREATE), the request ref (ID, method, route `/api/v1/admin/access-constraints/{id}`, surface; as for CREATE), classification and `before_revision`. It has no boundary name, subject, purpose or email. Credential decoration (token name and labels, when a user access token was used) follows the CREATE envelope contract; in practice a delete needs an interactive session, so it is usually absent.
  - In production, `initBoundaryServices` routes it through a sink over the server's shared asynchronous audit writer (`s.auditWriter`, the same writer under the decision log; not experiment-gated). CREATE keeps its own synchronous sink. A dispatch failure is logged with `failure_code=audit_sink_emit_failed` and a build failure with `failure_code=audit_event_build_failed` (then `auditId` is empty); neither ever fails or compensates the delete.
  - `auditId` is the envelope's `event_id`.
  - The handler and governance operational log lines ("access constraint updated/deleted via preview", "boundary change committed") are unchanged apart from the `audit_id` value, which is now a UUID. The cascade and the non-enumerating 404 are unchanged.
  - The two recovery operational lines ("all boundaries disabled", "all boundaries recovered") did change: they lost `audit_id` (there is no single ID any more) and now fire on every successful run (before, only when a writer was set). RecoveryService is unwired, so this has no production effect.
- **Correlation.** UPDATE, DELETE and recovery rows take correlation from `auditevent.OperationFromContext`. That is installed by `withAccessBoundaryAuditOperation`: the request ID, else one fresh operation ID per `CommitBoundaryChange` / `DisableAll` / `RecoverAll` call, shared by every row that call writes. Before P6 these paths read the never-set `correlationIDKey`, so the value was always empty.
- **Removed:** `BoundaryAuditWriter` with its slice, `AuditRequest`, `BoundaryAuditEntry`, `ImpactCounts`, `generateAuditID`, `validateAuditEntry`, the package-level `computeStateFingerprint`, `computeImpactCounts`, `compensateAuditFailure`, and the recovery rollbacks. The authzop mutation catalog is updated to match. `BoundaryMetrics` stays.
- **Fingerprint naming.** `(*PreviewService).computeStateFingerprint` is renamed `computeConstraintStateFingerprint`. Its mismatch message and the `CommitBoundaryChange` step comments no longer claim role bindings or memberships, because it hashes constraint state only. No logic change.

- **User docs.** `reference/permissions-policy.md` and `hosted/ha/permissions.md` no longer claim a "tamper-evident" "secure audit ledger" or a mutation audit log for boundaries. They now say: CREATE/UPDATE history is written in the same transaction and readable while the boundary exists. The newest 1,000 entries are kept, so the creation entry can be pruned. A delete deletes the history and emits only a best-effort log event, with no persisted record.

## History versus log: durability

- **UPDATE, recovery and CREATE rows are persisted.** They are committed with the mutation and readable through `GET …/access-constraints/{id}/audit` while the constraint exists and the row is within the cap.
- **The DELETE envelope is a structured log event, not a persisted record.** It is async and best-effort. Queue-full, oversize and closed-writer rejections are counted by the writer, but a record is silently not emitted (and not counted) when the process log level is above info. Its `auditId` identifies a log event only: it is not retrievable through any API and is not backed by history, because the constraint's history is cascaded with the constraint. **After P6 a delete still has no durable persisted record.**

## Partial recovery semantics

The first failure stops the run and returns `*RecoveryPartialError` with `Committed` (k−1), the committed IDs and their audit IDs, the failed ID, and the cause (`errors.Is` works). Constraints committed before the failure stay committed, each with its row. Nothing is rolled back across constraints, and the failed constraint has neither its change nor a row. A rerun processes only the remaining constraints. `recovery.completed` is published only when the whole run succeeds (unchanged); there is no event for a partial run.

## Pending owner decision

**P6-OD1 (durable delete record)** is pending ptone's retention decision and was not made in P6. The options are: keep a tombstone or history row past the cascade, write a `mutation_audits` row for deletes, or accept log-only.

## Follow-ups (not done here)

Deferred from review round 1 (fix-round-1.md):
- **FU-1:** have the boundary audit sink bypass log-level gating, or count `!Enabled` as a rejection. This is an owner decision, and it applies to the CREATE and decision logs too.
- **FU-2:** fold CREATE onto `accessConstraintAuditIdentities` (mechanical; deferred because CREATE is unchanged in P6).
- **FU-3:** enforce catalog destinations at `Emit` and at the history mapping (defense in depth).
- **FU-4:** make the DELETE revision check and the delete atomic (WithTx plus a row lock, or a conditional delete). This changes pre-existing domain behaviour.
- **FU-5:** add `DisableAccessConstraint` to `authzop.SecurityMutationSymbols` and classify `DisableAll`. Extended in review round 2: the recovery skip re-read is not a locking read, so add the row lock (or a conditional update) to it. Both are needed before any recovery wiring.
- **FU-7 (review round 2):** the offline recover-authz docs (outside this diff) call a best-effort write a "permanent" audit record. Fix that wording.
- **FU-6:** replace `user.Email()` and the boundary name in the handler operational lines (a boundary-family decision); map `RecoveryPartialError` to a generic message if recovery is ever wired; remove the `correlationIDKey` fallback.

Earlier follow-ups:

- Q-C / D6: amend the #2405 AC2 text to the approved non-enumerating 404 at closure.
- Structured-log conversion of UPDATE and recovery stays the later boundary-family decision. Today they write history only.
- At the single forward merge, coordinate the `auditevent/catalog.go` and `auditevent_test.go` catalog regions with P4 (ptone#5711 / GCP#4009).
- The web audit timeline renders `recovery_disable` / `recovery_enable` as raw operation strings (cosmetic).
- `correlationIDFromContext` / `correlationIDKey` survive only as the fallback in `withAccessBoundaryAuditOperation`. No production code sets the key, so the fallback can be removed.
- `RecoveryLock` is per-process. If recovery is ever wired, multi-replica exclusion needs a database lock, and the caller must supply an operation context.
