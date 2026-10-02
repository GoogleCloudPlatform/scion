# Audit update milestone 1 upstream-main rebase

## Verdict

The milestone-1 series is semantically rebased and validated against the
authoritative `GoogleCloudPlatform/scion` main fetched on 2026-10-02. It is
ready for an independent M1 review. This does not authorize milestone 2 work or
merge-queue notification.

## Superseding second rebase cycle

Upstream `main` advanced after the first lease-safe publication, leaving PR
#2292 conflicting. The issue owner explicitly authorized the same exclusive
integrator to perform a second cycle. This section supersedes the first-cycle
selected-head and residual-risk statements below while retaining them as
historical evidence.

- Prior published M1 head and exact second lease expectation:
  `44d21648bd40426ea0a6a98b100e52e1b844e283`.
- Second authoritative upstream fetch timestamp:
  `2026-10-02T14:28:41Z`.
- Required second minimum and selected upstream `main` (exact match):
  `28d83ede4e7c5969da4817a144d12adb672de324`.
- The selected head is a descendant of the first-cycle selected head
  `509bd856f2f0855842ac1847fa8977c871ad4f2c` and adds five upstream commits.
- Second-cycle rebased code/evidence head before this report update:
  `dda55b53f037539ad36aaa0353beb5d5e13bb361`.

The second exact replay used the prior selected upstream head as the old base
and replayed all 39 delivered commits onto `28d83ede...`. One conflict occurred
in `pkg/hub/authz.go` while replaying credential metadata compatibility:

- New upstream intent: preserve the internal-only `hub_delivery` credential
  kind and its constructor/gate restrictions.
- M1 intent: alias the six shared externally carried credential kinds to the
  canonical dependency-neutral `credentialmeta` constants.
- Resolution: retain the six canonical aliases plus upstream's separately
  documented typed `CredentialKindHubDelivery = "hub_delivery"` constant.
  This does not add `hub_delivery` to serialized audit credential metadata or
  make it request-carriable.

Second-cycle range-diff maps all 39 prior delivered commits to 39 replayed
commits in the same order. Thirty-eight are patch-identical; only the credential
compatibility commit differs by the additive `hub_delivery` preservation. The
changed-path set is identical, there are no skipped/squashed/no-op commits, and
the M2/decision-audit delta remains empty. Canonical `go generate ./pkg/ent`
again produced no delta.

Second-cycle validation passed:

- the same core and focused Hub normal tests under `-p 2`;
- credential metadata, audit envelope, focused Ent history/cap/transaction,
  and focused Hub governance/history/auth race tests under `-race -p 2`;
- the focused upstream `HubDelivery`/`DeliveryGate` suite plus canonical
  credential-kind parity;
- scoped vet and bounded single-concurrency lint (`0 issues`);
- focused web tests (4 files / 70 tests), typecheck, production ESLint (zero
  errors; the same 28 warnings), and scoped Prettier;
- gofmt inspection, `git diff --check`, generated-Ent inspection, and the
  repeated semantic contract audit.

The first-cycle scoped Go and production web builds passed. They were not run a
second time because the task brief permits at most one heavy build; the
second-cycle normal/race tests compiled the affected packages against the new
base. The earlier Ent race limitation is closed by the correctly scoped
`^TestConstraintHistory_` race run, which passed in 17.205s.

## Superseding third rebase cycle

The mandatory cycle-2 pre-push fetch found another upstream advance, so the
issue owner authorized a third cycle under a refined merge-tree publication
rule. This section supersedes the earlier selected-head status while retaining
both earlier cycles as provenance.

- Third fetch timestamp: `2026-10-02T14:46:50Z`.
- Required third minimum and selected authoritative upstream `main`:
  `ed14d2cc93539a7b5bdb983722b9057fa9bfbfe5` (exact match).
- The selected head is a descendant of cycle 2's `28d83ede...` and adds two
  upstream commits: request-local authorization-input memoization and
  configure-page CreateInputs recording.
- Prior local candidate: `54d97aaae52bc617a01514ac97d3db4a8d6f0cca`.
- Third-cycle rebased code/evidence head before this report update:
  `c4c6cdd6b4f331026df44b6e83806dbc39a9c2a2`.

All 40 prior local commits replayed noninteractively with no conflict. Although
the upstream delta and M1 both touch `pkg/hub/authz.go` and
`pkg/store/models.go`, Git retained each side without manual resolution.
Range-diff maps all 40 commits exactly, in order. The path set is identical,
there are no skipped/squashed/reordered/no-op commits, the M2 delta remains
empty, and canonical Ent generation again produces no delta.

Cycle-3 validation repeated and passed the same bounded gates:

- core backend and focused Hub normal tests (`-p 2`);
- credential metadata, audit envelope, focused Ent history, and focused Hub
  race tests (`-race -p 2`; Ent 17.597s, Hub 150.964s);
- scoped vet and bounded single-concurrency lint (`0 issues`);
- focused web tests (4 files / 70 tests), typecheck, production ESLint (zero
  errors; the same 28 warnings), and scoped Prettier;
- gofmt inspection, `git diff --check`, generated-Ent inspection, and semantic
  checks for transaction/cap/logging/live-resource/404/cursor/timeline behavior.

The one-build task cap remains satisfied: the successful cycle-1 Go and web
production builds were not repeated. Cycle-3 normal and race tests compiled the
affected packages against `ed14d2cc...`.

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

## Post-publication CI correction

GitHub Actions run `37024276146`, job `110894820013`, failed its `Run Tests`
step after all preceding web, format, vet, compatibility, authorization, and
security-marker gates passed. The exact failure was deterministic rather than
environmental:

```text
TestRegisteredRoutesHavePermissionClassification
registered routes missing permission classification:
[GET /api/v1/admin/access-constraints/{id}/audit]
```

The same failure reproduced locally with the bounded focused command
`go test -count=1 -p 2 ./pkg/hub -run
'^TestRegisteredRoutesHavePermissionClassification$'`. With explicit owner
authorization, the exact method-aware route was added to the existing
test-owned permission classification table as `policy:audit`. A focused
regression now also pins its existing production metadata to `RoutePolicy` and
the already-approved `hub.audit.read` permission. No production route, auth
behavior, audit schema, or timing changed.

Correction validation passed:

- the exact no-SQLite classification test and focused regression;
- focused route-metadata, history endpoint/cursor, unified auth, broker auth,
  privacy, and legacy B7 endpoint tests;
- `go vet -p 2 ./pkg/hub`;
- bounded single-concurrency `golangci-lint` for `./pkg/hub/...`: `0 issues`;
- gofmt inspection and `git diff --check`.

The replacement run's reporting-only full-suite job then exposed a second
deterministic M1 omission: `internal/fixturegen.TestFixtureCoverage` found the
new `access_constraint_history` domain table but no representative row and
still expected 65 rather than 66 tables. The bounded focused local test
reproduced the exact failure. With explicit owner authorization, the fixture
now contains one deterministic create-history row linked by `constraint_id` to
the existing constraint fixture, and the fixture-owned table count is 66. No
production or audit behavior changed.

The exact fixture coverage, loadability, and determinism tests pass. Canonical
Ent generation produces no additional delta; the focused
`TestConstraintHistory_` store/migration slice passes; scoped vet, bounded
single-concurrency lint (`0 issues`), gofmt, and `git diff --check` pass.

## Residual risk and M2 handoff

The first-cycle Ent adapter race limitation described above was closed in the
superseding second cycle by the passing focused history race gate. There is no
known semantic defect and no failed required gate remaining. A fresh
independent M1 review is required before any further milestone action.

Milestone 2 remains frozen and untouched. A future, separately authorized M2
integrator must begin only after M1 review/acceptance, independently verify the
then-current remote M1 and M2 heads, fetch authoritative upstream `main`, and
rebase/revalidate M2 under a new exact lease. None of the M1 lease, local refs,
or validation results in this report authorizes or substitutes for that work.

## Independent review round 1 fixture event-ID correction

Review round 1 found that the representative `access_constraint_history`
fixture used the descriptive string `fixture-access-constraint-created` for
`event_id`, while the production audit-envelope validator requires a canonical,
non-nil UUID. The focused regression first reproduced the defect through
`auditevent.Validate` with the exact error `invalid audit event field event_id:
must be a canonical UUID`. The fixture now uses the stable canonical UUID
`ae100000-0000-4000-8000-000000000001`. Its constraint foreign key and every
other history payload field are unchanged.

The following bounded checks pass:

- fixture coverage, loadability/migration, determinism, and the new canonical
  event-ID invariant;
- focused access-constraint history endpoint, route-classification, and store
  transaction/cap tests under `-p 2`;
- the complete `pkg/hub/auditevent` package;
- scoped `go vet -p 2`, single-concurrency golangci-lint (`0 issues`), gofmt,
  and `git diff --check`.

No production, authorization, timing, audit-schema, or M2 behavior changed.
Publication and final evidence remain subject to an immediate authoritative
upstream fetch, the established deterministic merge-tree gate if upstream has
advanced, and an exact force-with-lease against
`06dce97dfe2e42aae4e06dbce01cd1e34f7b606f`.

The immediate pre-push fetch at `2026-10-02T16:49:06Z` resolved authoritative
main unchanged at `ed14d2cc93539a7b5bdb983722b9057fa9bfbfe5`, so no
merge-tree fallback was needed. Independent remote reads confirmed the exact
M1 lease remained `06dce97dfe2e42aae4e06dbce01cd1e34f7b606f` and M2 remained
`708455edafd6e86f6bccb7befc9a9773be0ad2c8`.
