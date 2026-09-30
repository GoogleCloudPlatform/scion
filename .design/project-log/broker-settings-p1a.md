# broker-settings P1a: seed max_agents_per_broker at 100, admin PUT for system limits

Tracking: `ptone/scion#2061` (design AGREED rev 2, `.design` on the scratchpad volume,
not this repo). Fixes item 1 of `ptone/scion#2063`. PR: `ptone/scion#2268`
(branch `scion/broker-settings-p1a`). This PR went through three review
rounds, each rebasing the branch onto upstream main, which rewrites every
commit's hash — see the PR page or `git log` on the branch for the current
head rather than a SHA pinned in this file, which the next rebase would
invalidate.

## What changed

- `pkg/hub/seed.go`: the `max_agents_per_broker` seed default moves from 12 to
  100, per ptone's 2026-09-29 ruling to keep one global default for now (no
  per-broker tuning until P2 / `ptone/scion#2177`). The comment above
  `seedLimitDefinitions` was rewritten to state the ruling, the Cloud Run
  single-node crash risk (100 is above the observed ~19-20 idle-agent ceiling
  on 4 CPU/8 GiB), and the mitigation (set it via Admin → Quotas or `PUT
  /api/v1/admin/limits/{id}`). Seeding remains strictly insert-only — a hub
  that already has a row (12, 30, or any other value) is never touched on
  reseed.
- `pkg/hub/handlers_quota.go` `updateLimitDefinition`: system-seeded limit
  definitions can now have `default_value` and `description` changed via
  `PUT`; a request that also changes `name`, `resource_type`, or `unit`
  returns `403` with `"system limit definitions: only default_value and
  description can be changed"`. This is the supported admin path the design
  calls for (P1-D3) — previously any PUT on a system limit was unconditionally
  403. Non-negative validation, the DELETE-forbidden rule, and the
  `quota.update` permission requirement are all unchanged.
- `web/src/components/pages/admin-quotas.ts`: the Edit action now shows for
  system limits (previously hidden entirely), with name/resource
  type/unit rendered read-only in the edit dialog when `limit.system` is
  true, so the UI can't produce a request the server will 403.
- `pkg/hub/handlers_quota.go` (added after upstream review, see Round 5
  below): `createLimitDefinition` and the non-system branch of
  `updateLimitDefinition` now trim `resource_type` and reject it with `400`
  if empty, matching the existing `name` validation. `unit` is trimmed on
  update but deliberately **not** required on either path — `unit` isn't
  read by quota resolution, and requiring it on update while create never
  required it would have made pre-existing empty-unit rows permanently
  uneditable. System-row identity-field handling is unchanged.
- Docs: `docs-site/.../reference/api.md` and
  `docs-site/.../hosted/ha/multi-broker.md` no longer describe the
  `system_default`/`scope=broker` entitlement override as a working way to
  set a per-broker cap (`ptone/scion#2063` item 2/3 — that path is
  documented-but-broken and is fixed properly in P2). They now document the
  `PUT` path for the hub-wide default and say per-broker values are "coming
  in `ptone/scion#2061` P2". Added Cloud Run single-node operator guidance in
  `docs-site/.../hosted/single-node/hub-setup-cloudrun.md` (new "Set the
  agent cap after deploying" subsection under Sizing) and
  `.design/hosted/cloud-run-single-node.md` §9.1, both recommending ~16 via
  Admin → Quotas right after deploying a single-node Cloud Run hub.

## Tests added

- `pkg/hub/seed_limits_test.go`: fresh DB seeds `max_agents_per_broker` at
  100; a store already holding 12 or 30 is untouched by a reseed call.
- `pkg/hub/handlers_quota_test.go`: new cases alongside the existing
  `TestQuotaAPI_UpdateLimitDefinition_SystemSeeded` (which already covered
  the combined name+value-change 403) — a `default_value`+`description`-only
  change on a system limit returns 200 and persists; isolated
  name/resource_type/unit-only changes each return 403; a caller without
  `quota.update` gets 403 on a system-limit PUT via the guarded handler path
  (not `doRequest`'s default super-admin token).
- `pkg/hub/seed_limits_e2e_test.go`: end-to-end HTTP test — `PUT` the seeded
  `max_agents_per_broker` definition to 2 through the admin API, create two
  agents on a broker (201 each), and confirm the third returns `429` with
  error code `quota_exceeded`. Deliberately does not use the
  `setBrokerAgentCeiling` test helper (which writes the store directly),
  per the design's test-3 requirement, to close the gap where only
  `updateLimitDefinition`'s behavior — not the full HTTP round trip — was
  exercised.
- `pkg/hub/agent_ceiling_gate_test.go`: updated a stale comment referencing
  the old default of 12 to 100.
- `pkg/hub/handlers_quota_test.go` (Round 5): `TestQuotaAPI_CreateLimitDefinition_EmptyResourceType`/`_WhitespaceResourceType`;
  `TestQuotaAPI_UpdateLimitDefinition_EmptyResourceType` (kept from the
  initial upstream fix); `TestQuotaAPI_UpdateLimitDefinition_EmptyUnitFromCreateStillEditable`
  (a limit created with an empty unit stays editable — the round-5 F1
  regression test); `TestQuotaAPI_UpdateLimitDefinition_TrimsResourceTypeAndUnit`
  (padded whitespace is trimmed and the trimmed value persists);
  `TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_EmptyResourceTypeAndUnitForbidden`
  (an empty/whitespace resource_type or unit sent to a system row takes the
  403 identity-mismatch path, not the non-system 400 validation).

## Coordination

The design's coordination note calls out PR `ptone/scion#2168`
(`TestListProjectProviders_AgentLimitDefaultNoBindings`, small-issues-lead-2)
as a test that reads the seeded value rather than hard-coding 12, so it and
this PR can land in either order. As of this PR's initial commit (base
`e1f682eac`, before any of the rebases in the Review rounds section below),
that PR had not merged into `ptone/scion` — there was no `resolveBrokerCapacity`
or `TestListProjectProviders_AgentLimitDefaultNoBindings` in the tree yet, and
a repo-wide grep found no other test hard-coding 12 for
this seed value (`pkg/hub/agent_ceiling_gate_test.go`'s reference was a
comment only, now corrected). No further action was needed here; if
`ptone/scion#2168` lands with the hard-coded value still in place, whichever
PR merges second should update the assertion to read `DefaultValue` from the
seeded definition, per the design.

## Verification

- `go build ./...` — clean.
- `go test -tags no_sqlite ./pkg/hub/...` (targeted) and `make test-hub-sqlite`
  (covers the `//go:build !no_sqlite` quota/broker-quota suite) — pass.
  `pkg/hub/authzop` `TestMutationClassificationBidirectional` (a stale
  mutation-classification registry entry for a rename in
  `pkg/hub/useraccesstoken.go`, unrelated to this PR) failed on earlier,
  older bases during this PR's review; it was fixed upstream by
  `GoogleCloudPlatform/scion#2105`, which has since merged, and `authzop`
  passes as of the current base.
- `make test-fast` initially also showed failures in `pkg/runtimebroker` and
  `pkg/sciontool/supervisor`. The round-2 independent reviewer
  (`broker-settings-rev-p1a-2`) re-ran those packages, plus `pkg/agent`,
  `pkg/config`, `pkg/harness`, `cmd`, and `pkg/runtime`, with a scrubbed
  environment (`env -i` keeping only `HOME`/`PATH`/`USER`/`TMPDIR`/Go vars)
  and all passed on both base and this PR's head. The failures were caused
  by ambient `SCION_*`/`CLAUDE_*` environment variables leaking into local
  `make test-fast` runs in the review containers, not a base-branch bug and
  not caused by this PR, and not something that showed up in CI.
- `golangci-lint run --new-from-rev=upstream-main ./pkg/hub/...` — 0 issues
  (originally flagged 2 unchecked `s.Close()` errors in the new test file,
  fixed).
- `go vet -tags no_sqlite ./...` (`make lint`) — clean on this PR's own
  files. On the current base, this same command fails repo-wide on
  `pkg/hub/authz_relationship_{characterization,policy,rules}_test.go`
  (`undefined: authzTestSetup`/`newGoldenFixture`), from
  `GoogleCloudPlatform/scion#2088` — not this PR, and not yet fixed
  upstream as of this note. This is what currently makes CI's "Build &
  Test" check red on this PR (at the "Vet Code" / `make lint` step),
  independent of anything in this diff.
- `cd web && npx tsc --noEmit` — clean. No `web/src/shared/types.ts` changes
  were needed: `LimitDefinition.system` is a local interface field in
  `admin-quotas.ts` already present before this change, and the JSON shape
  of the PUT request/response is unchanged.
- CI on `ptone/scion#2268`: golangci-lint, shellcheck, T1 Postgres,
  single-node-vm harness, and Mergeability Gate all passed. "pkg/hub SQLite
  Tests" passes on the current base (it only failed on older bases, before
  `GoogleCloudPlatform/scion#2105` merged). "Build & Test" fails solely on
  the unrelated `GoogleCloudPlatform/scion#2088` vet break described above.
  "Lint 405 Allow header" (reporting-only) and reporting-only
  `internal/fixturegen TestFixtureCoverage` both fail pre-existing and
  repo-wide, unrelated to this PR.
- After `GoogleCloudPlatform/scion#2110` merged (fixing the
  `GoogleCloudPlatform/scion#2088` vet break) and a rebase, CI on
  `ptone/scion#2268` went fully green on every required check: Build & Test
  (including Verify Web Types, Vet Code, Run Tests), golangci-lint, pkg/hub
  SQLite Tests, T1 Postgres, shellcheck, single-node-vm harness,
  Mergeability Gate. Only the pre-existing reporting-only "Lint 405" stayed
  red.
- Round 5 fix (`resource_type`/`unit` validation symmetry, see "Upstream
  mirror" below): `go build ./...` clean; `go vet ./pkg/hub/` and
  `go vet -tags no_sqlite ./pkg/hub/` clean;
  `go test ./pkg/hub/ -run 'Limit|Quota|Seed' -count=1` pass; `make
  test-hub-sqlite` pass; `golangci-lint run --new-from-rev=upstream-main
  ./pkg/hub/...` 0 issues. No web files touched this round, so `tsc
  --noEmit` was not re-run.

## Surprises / notes for reviewers

- The design's `seed.go:1274-1288` line reference for the comment to rewrite
  didn't line up exactly with the comment's actual location in the current
  tree (`seedLimitDefinitions`'s doc comment starts around line 1234); the
  content was rewritten in place at its real location rather than at the
  stale line numbers.
- Chose to keep the existing `TestQuotaAPI_UpdateLimitDefinition_SystemSeeded`
  test as-is (it changes name+value together and still expects 403, which
  remains correct under the new rule) rather than folding it into the new
  tests, to avoid rewriting a pre-existing regression test unnecessarily.

## Review rounds

- **Round 1** (`broker-settings-rev-p1a-1`): REQUEST CHANGES on one Required
  doc finding (F1: the documented `PUT` recipe for the Cloud Run mitigation
  erased the seeded `description`, since `PUT` replaces the whole row) plus
  three Nits (F2: admin-quotas resource-type select was missing `group`;
  F3: duplicated field assignments in `updateLimitDefinition`; F4: a test
  helper switch simplified to `fmt.Sprintf`). All four fixed.
- **Round 2** (`broker-settings-rev-p1a-2`, fresh/independent): APPROVE, with
  five non-blocking findings, all addressed:
  - F1: the Default Value field coerced an empty input to `0` (=
    unlimited), so clearing it on `max_agents_per_broker` would have
    silently disabled the crash ceiling. `saveLimitDefinition` now rejects
    an empty/non-numeric value as a validation error; an explicit `0` is
    still accepted, with `help-text` on the field spelling out "0 =
    unlimited".
  - F2: worded the 8 CPU/32 GiB sizing guidance in
    `hub-setup-cloudrun.md` so it no longer contradicts the "sizing to the
    ceiling is not the safe choice" guidance earlier in the same doc.
  - F3: corrected the `seed.go` comment's Cloud Run crash-point figures to
    match `.design/hosted/cloud-run-single-node.md` §9.1 (~19-20 idle on
    4 CPU/8 GiB, not ~17-18).
  - F4: updated the stale `LimitDefinition.System` field comment in
    `pkg/store/models.go` to describe the new default_value/description-only
    editability. Left the similarly-worded `RoleDefinition.System` comment
    at a different line untouched — role definitions did not change in this
    PR and remain fully immutable, so that comment is still accurate.
  - F5: this file — corrected the head SHA and the `make test-fast`
    attribution (see Verification above).
- **Round 3** (`broker-settings-rev-p1a-3`, fresh/independent): APPROVE, with
  four non-blocking findings, all addressed:
  - F1: the new "Set the agent cap after deploying" subsection in
    `hub-setup-cloudrun.md` had been inserted above the pre-existing
    "To change the Instance size" `deploy.sh` snippet, so that snippet
    rendered under the new heading. Moved the new subsection below it.
  - F2: this file still said "~17-18" in one place after the seed.go fix
    changed to "~19-20", and cited a base SHA (`e1f682eac`) that a later
    rebase had already moved past. Fixed the figure and reworded the SHA
    reference to not go stale on the next rebase (see the top of this
    file).
  - F3: the PR body's test plan repeated the outdated
    `pkg/runtimebroker`/`pkg/sciontool/supervisor` env-leakage
    misattribution and didn't mention the two known upstream CI breaks.
    Edited directly on the PR (`ptone/scion#2268`), not in this file.
  - F4: a curl comment in `hub-setup-cloudrun.md` said omitting `name` is
    "rejected" without specifying it's `400` (not `403` like
    resourceType/unit), and cited `ptone/scion#2063` inexactly. Fixed the
    comment.
  - The reviewer also independently found and reported a second pre-existing
    base-branch break, `GoogleCloudPlatform/scion#2088`'s `no_sqlite` vet
    failure (`authzTestSetup` undefined), on top of the already-known
    `GoogleCloudPlatform/scion#2105`. Both are listed in the PR body now.
- **Round 4** (`broker-settings-rev-p1a-4`, fresh/independent): APPROVE,
  metadata-only findings, all addressed with no code changes:
  - F1: by round 4, `GoogleCloudPlatform/scion#2105` had merged and
    `pkg/hub/authzop` passed again on the current base, but the PR body and
    this file's Verification section still listed it as a current failure.
    Reworded both (see Verification above) to say it's fixed upstream, and
    that the sole remaining "Build & Test" failure is
    `GoogleCloudPlatform/scion#2088`'s vet break.
  - F2: commit `206af3143`'s body had a bare `#2168` (should be
    `ptone/scion#2168`, the fully-qualified form every other ref in this PR
    uses). Reworded via `git filter-branch --msg-filter` at the next rebase
    (this round), which also changes that commit's hash.
  - F3: the PR body said the `pkg/runtimebroker`/`pkg/sciontool/supervisor`
    env-leakage failures were seen "in earlier CI runs"; they were actually
    local `make test-fast` runs, never a CI failure. Reworded.

## Upstream mirror: GoogleCloudPlatform/scion#2114

ptone opened `GoogleCloudPlatform/scion#2114` from this branch, mirroring
`ptone/scion#2268`. gemini-code-assist reviewed it and flagged (medium)
that `updateLimitDefinition`'s non-system branch wrote `resource_type`
straight through with no validation, so an empty or whitespace-only value
would silently corrupt the row. Fixed in commit `44deb4a43` by trimming and
requiring `resource_type`, and — per broker-settings-em's instruction to
check `name` and `unit` for the same hole — also trimming and requiring
`unit` (an overreach corrected in Round 5 below) and confirming `name`
already had this validation.

- **Round 5** (`broker-settings-rev-p1a-5`, fresh/independent, reviewing the
  `44deb4a43` upstream fix): REQUEST CHANGES.
  - F1 (Required): requiring `unit` on update was a regression.
    `createLimitDefinition` never required `unit` (the admin UI treats it
    as optional, rendering `—`), so a row created with an empty unit could
    no longer be edited at all — a plain `defaultValue`-only PUT now failed
    with `400 "unit is required"`. broker-settings-em's decision: keep
    trimming `unit` on update, but drop the requirement. Added
    `TestQuotaAPI_UpdateLimitDefinition_EmptyUnitFromCreateStillEditable`.
  - F2: the same `resource_type` hole the upstream thread flagged for
    update was also reachable through `createLimitDefinition` (accepted
    `"   "` with `201`; accepted `""` and 500'd from an ent validator).
    Fixed: `createLimitDefinition` now trims and requires `resource_type`
    too, without touching `unit` on create. Added
    `TestQuotaAPI_CreateLimitDefinition_EmptyResourceType`/`_WhitespaceResourceType`.
  - F3: `..._SystemSeeded_EmptyFieldsUnaffected` was misleadingly named — it
    sent no empty fields, and mostly duplicated
    `..._SystemSeeded_DefaultValueAllowed`. Replaced with
    `..._SystemSeeded_EmptyResourceTypeAndUnitForbidden`, which actually
    sends an empty/whitespace `resource_type` and `unit` to a system row
    and asserts the real outcome: `403` (the pre-existing identity-mismatch
    path), not the new non-system `400` validation, since the checks live
    only in the non-system branch.
  - F4: added `TestQuotaAPI_UpdateLimitDefinition_TrimsResourceTypeAndUnit`
    to assert that a non-system PUT's trimmed `resource_type`/`unit` values
    are what gets persisted (previously verified only by the reviewer's
    manual probe, not by a committed test).
  - Fix commit: see the PR/branch history for the current SHA (this file
    avoids pinning a SHA that a rebase would invalidate, per Round 3 F2/F5
    above).
