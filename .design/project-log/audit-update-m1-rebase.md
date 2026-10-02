# Audit update milestone 1 upstream-main rebase

## Verdict

The milestone-1 series is semantically rebased and validated against the
authoritative `GoogleCloudPlatform/scion` main fetched on 2026-10-02. It is
ready for an independent M1 review. This does not authorize milestone 2 work or
merge-queue notification.

## Provenance

- Required and observed old fork head:
  `ff62f8cb2ed95877e8e3598049e6408c579ed18a`.
- Original M1 merge base:
  `a5f96db9cb3bcbe511ada4dd84a078c60319db62`.
- Minimum permitted upstream base:
  `cda49f0b092e70bb3181c11d825c600f3a404d2e`.
- Authoritative upstream `main` fetched at `2026-10-02T13:51:42Z`:
  `509bd856f2f0855842ac1847fa8977c871ad4f2c`.
- The minimum base is an ancestor of the selected upstream head, so upstream
  had advanced and the newer exact head was selected.
- Rebased code head before this report-only commit:
  `a2863ba3bb7ae25960c970b8c3735bc650a057e8`.
- Old compare evidence:
  `https://github.com/GoogleCloudPlatform/scion/compare/a5f96db9cb3bcbe511ada4dd84a078c60319db62...ptone:scion:scion/audit-update-m1`.
- Previously accepted validation and PostgreSQL evidence is recorded in
  `.design/project-log/audit-update-m1-validation.md`,
  `.design/project-log/audit-update-m1-2404.md`, and the other tracked
  `audit-update-m1-*` project logs.

The workspace was `clone-per-agent`, Git-backed, on
`scion/audit-update-m1`, with a clean tree and matching local, tracking, and
independent remote old heads before the rebase. The clone was shallow at its
initial M1 head; targeted history deepening restored the ancestry and merge
base without fetching or mutating milestone 2.

## Rebase and conflict disposition

The exact operation replayed all 36 commits with:

```text
git rebase --onto 509bd856f2f0855842ac1847fa8977c871ad4f2c \
  a5f96db9cb3bcbe511ada4dd84a078c60319db62 \
  scion/audit-update-m1
```

There was one conflict, in generated `pkg/ent/client.go`, while replaying old
commit `38ac1a9ac502b47b641d2febf1f500451d7506c1` (bounded access-constraint
history store):

- Upstream intent: retain the new `UserTerminalWorkspace` entity in the Ent
  hook and interceptor client lists.
- M1 intent: add `AccessConstraintHistory` to those same lists.
- Resolution: additive union of both entities. Neither side's schema contract
  was discarded.
- Generated-artifact proof: the canonical `go generate ./pkg/ent` command
  succeeded. It changed only layout in the manually reconciled list and
  preserved both entities. Commit
  `8052c47614431e27baf2947de018f93f104e7a7d` records that exact generator
  output. No generated artifact was hand-edited after regeneration.

No other conflict occurred and no old commit was skipped.

## Dropped-change and semantic audit

`git range-diff` maps all 36 old commits to the same ordered 36 replayed
commits. Thirty-five are patch-identical. The sole changed replay is the Ent
history commit described above, whose range-diff delta is exactly the additive
upstream `UserTerminalWorkspace` preservation plus generated layout. The old
and rebased M1 ranges have the same 78-path changed-file set; there are no
reordered, squashed, skipped, or no-op old commits.

The rebased integration also exposed one upstream scanner drift:
`TestMutationClassificationBidirectional` found the create mutation at
`createAccessConstraintWithAudit`, while the old catalog still classified the
former `CommitBoundaryChange` location. Commit
`a2863ba3bb7ae25960c970b8c3735bc650a057e8` moves only that classification to
the actual transactional helper; the existing bidirectional guard fails
without the repair and passes with it.

Manual and test-backed contract inspection confirmed:

- the typed, allowlisted, privacy-safe audit envelope and literal catalog are
  retained;
- create plus its purpose-specific history row still use one `Store.WithTx`
  transaction and the exact same event identity;
- the per-constraint cap remains 1,000 rows behind the transaction-required,
  row-locking (`FOR UPDATE` where supported) append path;
- structured logging occurs only after the live row/history transaction
  commits, and sink failure cannot rewrite the committed result;
- the audit endpoint first resolves the live constraint, authorizes
  `hub.audit.read` on that resource, and preserves non-enumerating 404 behavior;
- cursor decoding remains bounded/fail-closed and pagination remains strict
  `(occurred_at DESC, event_id DESC)` tuple pagination;
- the timeline retains loading, error, empty, 404/stale-resource clearing,
  pagination, and typed rendering states;
- Ent schema input, generated migration table, cascade edge, and tuple indexes
  agree after canonical regeneration;
- the M1 delta contains no milestone-2-only path, commit, decision-audit, or
  authorization-decision behavior.

The previously accepted PostgreSQL 16.15 overlap/lock/cap evidence remains
applicable because the production cap and locking implementation did not
change during this rebase.

## Validation

Full `make ci` and `make ci-full` were intentionally not run, as required by
the rebase brief.

Passed gates:

- `go test -count=1 -p 2 ./pkg/credentialmeta ./pkg/hub/auditevent
  ./pkg/hub/permissions ./pkg/hub/authzop ./pkg/store/entadapter`.
- Focused `go test -count=1 -p 2 ./pkg/hub -run ...` covering governance
  create audit, history endpoint/cursors, unified and broker auth
  normalization, privacy behavior, route metadata, credential carriage, and
  catalog reconciliation.
- Focused regression
  `go test -count=1 -p 2 ./pkg/hub/authzop -run
  '^TestMutationClassificationBidirectional$'`.
- `go test -race -count=1 -p 2` for `pkg/credentialmeta` and
  `pkg/hub/auditevent`.
- Focused Hub race slice for governance create audit, history endpoint, and
  unified/broker auth normalization (`157.676s`).
- Scoped `go vet -p 2` for the changed backend package families.
- One scoped `go build -buildvcs=false -p 2` for those package families.
- Bounded `GOGC=40 golangci-lint ... --concurrency=1
  --new-from-rev=509bd856...` over the changed backend families: `0 issues`.
- Four focused Vitest files: 4 files and 70 tests passed.
- Web `npm run typecheck`.
- Scoped production ESLint: zero errors and 28 existing
  `explicit-function-return-type` warnings.
- Scoped Prettier check, web production build, canonical Ent generation,
  `git diff --check`, and changed-Go formatting inspection.

Inconclusive/limited gates:

- The combined race invocation passed `pkg/credentialmeta` and
  `pkg/hub/auditevent`, but the `pkg/store/entadapter` portion remained active
  without diagnostics near the 10-minute hard bound and ended with exit 130.
  It was not rerun to warm caches. Its complete non-race package tests passed,
  and unchanged production cap code retains the accepted external PostgreSQL
  proof noted above.
- Typed ESLint excludes `*.test.ts` from its configured tsconfig. Directly
  including the four test files therefore reports parser configuration errors;
  those test files instead passed Vitest and Prettier. The changed production
  files passed scoped ESLint with zero errors.
- `npm ci` reported the lockfile-existing three advisories (one low, two high);
  this rebase changed neither manifest nor lockfile.

## Residual risk and M2 handoff

The only residual local-validation gap is the inconclusive Ent adapter race
portion described above. There is no known semantic defect and no failed
required gate remaining. A fresh independent M1 review is required before any
further milestone action.

Milestone 2 remains frozen and untouched. A future, separately authorized M2
integrator must begin only after M1 review/acceptance, independently verify the
then-current remote M1 and M2 heads, fetch authoritative upstream `main`, and
rebase/revalidate M2 under a new exact lease. None of the M1 lease, local refs,
or validation results in this report authorizes or substitutes for that work.
