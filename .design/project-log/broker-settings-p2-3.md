# Broker settings P2.3: binding migration, createEntitlement 400, docs (ptone/scion#2061 P2-D4, ptone/scion#2063 items 2 and 3)

Base: started stacked on P2.1 (ptone/scion#2275, branch `scion/broker-settings-p2-1`) at its head
`0875d543a658cd77e7a9ec92ba42436878aa0839`, rebased onto P2.1's later fork head `19b8064d81` when
P2.1 picked up its own review fixes, then rebased onto `upstream-main` after P2.1 merged upstream as
GoogleCloudPlatform/scion#2126 (`86fc807b1`) — see "Upstream rebase" below. Design:
`/scion-volumes/scratchpad/projects/broker-settings/design.md` §5.5, §5.7 (P2.3), §5.8 (AC-P2-5), §6.
Branch `scion/broker-settings-p2-3`. PR: ptone/scion#2306 (draft).

## What shipped

- **One-shot boot data migration `broker_quota_bindings_to_settings`**
  (`cmd/boot_broker_quota_bindings_to_settings.go`, marker in `cmd/migration_markers.go`, wired into
  `runBootDataMigrations` in `cmd/boot_data_migrations.go`). For every broker B with
  `max_agents_per_broker` entitlement bindings where `scopeType=broker, scopeId=B` — covering both
  historical workarounds identically (the user-subject hack and a `system_default` row with a
  non-empty subject; the migration groups purely on scope, not subject shape) — it writes the value
  the entitlement engine's "most generous wins" rule would have produced (0 if any binding is 0,
  otherwise the max) into that broker's `broker_settings` row via `store.BrokerSettingStore`, with
  `updatedBy: "migration:ptone/scion#2061"`. A broker that already has a `maxAgents` setting (even a
  future field other than `maxAgents`, since the write preserves the rest of the document) is left
  untouched. Bindings are never deleted — only shadowed by the new setting under the P2-D2
  precedence rule (broker setting beats bindings), and their IDs are logged. A binding whose broker
  no longer exists is skipped, logged, and counted in the marker's residual field — a deterministic,
  permanent outcome, not a run-level failure. Follows the M-1' marker pattern exactly as the other
  migrations in `cmd/boot_data_migrations.go` (e.g. `runBrokerOwnershipBackfill`): any run-level
  failure (list/read/write error other than "not found") aborts the whole pass without writing the
  marker, so the next boot retries from scratch; this migration does not do per-broker partial
  progress tracking (unlike the message backfill) because there's no expectation of a large enough
  row count to need a time budget.

- **`createEntitlement` / `updateEntitlement` reject the deprecated shape**
  (`pkg/hub/handlers_quota.go`): for `limit == max_agents_per_broker && scopeType == broker`, both
  now return 400 `"per-broker agent caps are set via PUT /api/v1/runtime-brokers/{id}/settings"`.
  `updateEntitlement` needed the identical check — the brief specifically asked to check the update
  path, and without it, PUT could reshape an existing binding into the exact rejected shape,
  bypassing the POST-time check. System-scoped bindings for this limit, and broker-scoped bindings
  on any other limit, are unaffected — confirmed with tests for both.

- **UI check, no change made**: the brief asked to check whether the web admin-quotas entitlement
  form offers broker scope for this limit and, if so, make the 400 surface cleanly. It doesn't —
  `web/src/components/pages/admin-quotas.ts`'s entitlement dialog's Scope Type `<sl-select>` only
  offers `system` and `project` options (no `broker` option exists at all), so no admin-quotas.ts
  edit was needed. (admin-quotas.ts is P2.2's file to edit in parallel per the brief, so this was
  worth confirming rather than assuming.)

- **Docs** (`docs-site/src/content/docs/reference/api.md`,
  `docs-site/src/content/docs/hosted/ha/multi-broker.md`): added a "Broker Settings" section to
  api.md documenting `GET`/`PUT /api/v1/runtime-brokers/{id}/settings` — request/response shape
  (`settings`, `effective.maxAgents.{value,source,count,inherited}`, `revision`, `updatedBy`,
  `updated`, `_capabilities`), status codes (400/403/404/409), and `quota.update` as the write
  permission — plus the precedence rule (broker setting > entitlement bindings, most generous wins >
  hub-wide default; 0 = unlimited), the migration's behavior, and the new 400. Replaced the sentence
  in both files that told operators to use a `system_default`/`scope=broker` entitlement binding for
  a per-broker override (the exact thing ptone/scion#2063 items 2/3 tracked as broken) with a
  pointer to the new settings API. Left the seed-default-number sentences (stating **12**) completely
  untouched in both files, per the brief — that text is P1a's (ptone/scion#2268), kept mechanical for
  its later rebase. Confirmed via `grep -rl max_agents_per_broker docs-site/` that no other page
  references the broken path; the only other hit is a dated release-notes entry, out of scope by
  instruction.

## Tests

- `cmd` package (migration), SQLite only — see "what I ran" below for why no Postgres run exists:
  `TestBrokerQuotaBindingsToSettingsMigration_UserHackBindingBecomesSetting`,
  `_TwoBindingsGiveMax`, `_AnyZeroGivesZero`, `_ExistingSettingUntouched`, `_Idempotent`,
  `_MissingBrokerSkipped`, `_NoLimitDefined` — all seven scenarios design §5.7 lists for the
  migration, plus a no-limit-defined edge case not in the list.
- `pkg/hub` (handler): `TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected`,
  `_MaxAgentsPerBrokerSystemScoped_StillWorks`, `_BrokerScopedOtherLimit_Unaffected`, and the three
  `UpdateEntitlement` equivalents.
- `pkg/hub` end-to-end: `TestBrokerQuota_MigratedSettingEnforced` writes the exact
  `store.BrokerSettings` document the migration produces for a broker, then proves `Reserve`
  enforces it — over a hub-wide default 100x larger — through the real `POST /agents/:id/start` HTTP
  path. It does not literally invoke the `cmd`-package migration function (`pkg/hub` cannot import
  `cmd` without a cycle); the migration's own grouping/write logic is covered by the `cmd`-package
  tests above, and this test covers the integration point between "migration wrote a setting" and
  "Reserve enforces it," which P2.1 wired.

**What I ran, and why no Postgres run for the migration**: `make test-hub-sqlite` and `make
test-fast`, both with `SCION_*`/`CLAUDE_*` env vars scrubbed (`unset $(env | grep -E
'^(SCION_|CLAUDE_)' | cut -d= -f1)`) — both green. Without scrubbing, `make test-fast` fails
`TestNativeTelemetryPolicyEffectiveChildEnv` in `pkg/sciontool/supervisor` on ambient
`CLAUDE_CODE_ENABLE_TELEMETRY`; confirmed environment-only, not a regression. `go test -count=1
./cmd/...` has three failures (`TestHubAllOrOneActions`, `TestRequireImageRegistryForBroker_Settings`,
`TestInitPluginManager_MigratesConfigFileOnlyPlugin`) that I confirmed pre-exist on the P2.1 base
commit (`git stash -u` + re-run), unrelated to this PR. `go vet ./cmd/... ./pkg/hub/...` and `go vet
-tags no_sqlite` both clean. `golangci-lint run ./cmd/... ./pkg/hub/...` reports 15 pre-existing
issues, none in any file this PR touches. `tsc --noEmit` not run — web is untouched by this PR. The
brief asked to run the migration tests "on SQLite and on Postgres if the existing migration tests
support it": the precedent this migration follows (`boot_broker_ownership_backfill_test.go` and
every sibling in `cmd/`) is SQLite-only (`//go:build !no_sqlite`, `entc.OpenSQLite` directly) with
no Postgres harness in the `cmd` package; I did not build one for this PR and flagged that choice
explicitly to the EM rather than silently only running SQLite.

## Surprising / worth flagging

- `pkg/hub/broker_capacity.go`'s `brokerSettingLimitOverride`/`effectiveBrokerLimit` (from P2.1)
  already wires broker settings ahead of bindings in `Reserve` — this PR's migration only had to get
  the right value into the store; the enforcement side needed no changes at all. Worth confirming a
  reviewer doesn't expect a `Reserve`/`quota.go` diff in this PR — there isn't one, deliberately.
- The design doc's illustrative migration description didn't specify what "no maxAgents setting yet"
  means when a `broker_settings` row exists but its `maxAgents` field is nil (as opposed to no row at
  all). I treated "has a setting" as "row exists AND `MaxAgents != nil`," preserving any other stored
  fields on write — there are none today, but this keeps the migration correct if a second
  broker-settings key is added later. This paid off in review round 1 (F4): a dedicated test
  (`_NilMaxAgentsCASUpdate`) now proves the CAS-update branch this choice implies.

## Review round 1 disposition (broker-settings-rev-p2-3-1, head `50e19fb10` after the F1 reword — see round 2 below)

Full review: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-3-1.md`.
Verdict was REQUEST CHANGES on a single Required item (F1); everything else was Optional/Nit/FYI.
All were addressed:

- **F1 (Required, fixed):** the PR body had a bare `#2275` ("do not merge until #2275 has landed").
  Fixed to `ptone/scion#2275` via `gh api repos/ptone/scion/pulls/2306 -X PATCH -f body=...` (`gh pr
  edit` itself hit an unrelated "Projects (classic)" GraphQL deprecation error on this repo — the
  REST PATCH avoids that code path entirely). Confirmed no other bare `#N` remains in the body.
- **F2 (fixed):** the migration's doc comment wrongly claimed its grouping matched exactly what the
  quota engine enforced. Rewritten to state accurately that `effectiveBrokerLimit` resolves with
  `subjectID=brokerID`, so the engine only ever enforced two of the historical shapes, and the
  migration deliberately takes every broker-scoped binding anyway (design §5.5), including ones the
  engine silently ignored (the actual ptone/scion#2063 item-3 bug).
- **F3 (fixed):** `_UserHackBindingBecomesSetting` now seeds `subjectId = broker.ID` (the real hack
  shape) instead of an arbitrary user ID, so it proves AC-P2-5 literally. Added
  `_NeverEnforcedSubjectStillMigrated` for the never-enforced-subject case.
- **F4 (fixed):** added `_NilMaxAgentsCASUpdate` (existing row, `maxAgents` nil → CAS update, not a
  failed create) and `_NegativeSelection` (system-scoped binding, a different limit's broker-scoped
  binding, and an unrelated broker each correctly produce or affect nothing).
- **F5-F7 (fixed, docs):** api.md now says clearing a migrated setting re-exposes any leftover shadowed
  bindings (the precedence rule falls through to them); added `unlimited` to the `source` enum;
  corrected the old-binding-shape wording (subject, not scopeId, is what distinguished the hack) and
  the 400's scope (editing an existing broker-scoped binding is covered too, not just creating a new
  one).
- **F8 (fixed, docs, FYI-severity):** added a sentence that upgrading can newly impose or tighten a
  broker's cap, by design — both because the migrated setting no longer merges with a system-scoped
  binding via "most generous wins," and because of F2's never-enforced-subject rows. The reviewer is
  raising the release-note version of this with the lead separately; not this PR's job.
- **F9-F12 (FYI, no action):** `Residuals` counting brokers rather than binding rows (F9), the
  expected one-line P1a rebase conflict at the two untouched seed-default sentences (F10), and the
  Postgres-gap / race / failure-mode confirmations (F11, F12) needed no changes.

Re-ran after the fixes: the full `TestBrokerQuotaBindingsToSettingsMigration_*` suite in `cmd`
(10 tests, all passing, including the two new ones), the targeted `pkg/hub` tests
(`-run 'Quota|Entitlement|BrokerSetting|Migrated'`), `go build ./...`, `gofmt -l`, and
`golangci-lint run ./cmd/...` (clean on the touched files). Originally pushed as `a11493c77`; that
commit's message itself introduced a fresh bare `#2275` (caught in round 2, see below), so it no
longer exists on the branch — it was reworded in place to `50e19fb10` (tree-identical, verified via
`git diff a11493c77 50e19fb10` being empty) and force-pushed with lease.

## Review round 2 disposition (broker-settings-rev-p2-3-2, head `62976cd0e`)

Full review: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-3-2.md`.
Verdict: **APPROVE** at head `62976cd0e9f61a2a6999ea73b4243a9c3bbb8a00`, conditional only on CI
(pending at review time; confirmed green afterward — see below). The reviewer independently verified
the message-only reword (`git rev-parse 62976cd0^{tree} a7d85e94^{tree}` — same tree) before applying
the verdict to the new head. Three non-blocking findings remained; the EM asked me to close them as
test/docs/log-only changes with no further review round, since the EM would verify the delta
directly:

- **F2 (Optional, fixed):** round 1's `_NegativeSelection` test asserted outcomes that were also true
  without the scope-type filter (a system-scoped binding's empty `scopeId` happens to resolve to
  `ErrNotFound` either way), so it didn't actually prove the filter mattered. The reviewer confirmed
  this by mutation (temporarily disabling the filter — all 10 tests still passed). I did the same
  check myself before committing the fix:
  1. Replaced the filter body with `if false { continue }` in
     `cmd/boot_broker_quota_bindings_to_settings.go`.
  2. Ran `go test ./cmd/... -run 'TestBrokerQuotaBindingsToSettingsMigration' -v`: 9 of 10 passed;
     `_NegativeSelection` failed, with the log showing `brokers_scanned=2 missing_broker=1` and the
     target broker's migrated value pulled from the wrong binding (`max_agents=777` instead of `5`) —
     the system-scoped binding's empty-`scopeId` bucket got treated as its own broker group, and a new
     project-scoped binding I added (scoped to the target broker's own ID) leaked its value in.
  3. Reverted the mutation (`git diff cmd/boot_broker_quota_bindings_to_settings.go` empty afterward)
     and reran the full suite: 10/10 pass.
  The committed fix: capture slog and assert `brokers_scanned=1`/`missing_broker=0`, and add the
  project-scoped-binding-with-broker's-ID case the reviewer suggested, so the test now fails under the
  exact mutation that was silently passing before.
- **F3 (Nit, fixed, docs):** api.md's migration paragraph said clearing a migrated setting makes "any
  leftover broker-scoped bindings" live again — overstated, since the never-enforced shapes (F2 from
  round 1) never become live regardless. Reworded to name only the shapes the entitlement engine
  actually matches (user binding with `subjectId` = broker ID, or `system_default` with an empty
  subject).
- **F4 (FYI, fixed, log-only):** this log's round-1 section cited the pre-reword head `a11493c77`,
  which no longer exists on the branch after the F1 fix. Corrected above to `50e19fb10`, and this
  section records the actual current head.

No production code changed for round 2 — F2 is test-only (plus the mutate/revert cycle above, which
left the production file byte-identical to before), F3 and F4 are docs/log only.

Re-ran after the fixes: the full `TestBrokerQuotaBindingsToSettingsMigration_*` suite (10/10 pass),
`go build ./...`, `gofmt -l` on the changed files, and confirmed
`git diff cmd/boot_broker_quota_bindings_to_settings.go` against the pre-mutation state is empty.
Checked `git log --format=%B 0875d543..HEAD | grep -nE '(^|[^/A-Za-z0-9])#[0-9]+'` prints nothing.

## Upstream rebase (P2.1 merged as GoogleCloudPlatform/scion#2126)

Old base: `19b8064d81d64b0416d113cd99e698dd7f1361f1` (P2.1's second fork head). Old head:
`3c782655a74c17e08f715dac19dfc87e8b052bc8`. New base: `upstream-main` (fetched from
`https://github.com/GoogleCloudPlatform/scion.git`), which contains P2.1 merged as
GoogleCloudPlatform/scion#2126 at `86fc807b1`. New head: `d067b7b188887833262058b81b3f767468ff2f1c`.

`git rebase --onto upstream-main 19b8064d81 ...`. Range-diff (`19b8064d81..3c782655a`
`upstream-main..d067b7b18`): **6 of 7 commits `=`** (byte-identical), **1 `!`** — the docs commit
(`docs: document the per-broker settings API and its migration`).

The `!` commit conflicted in exactly the shape round-1 finding F10 predicted: P1a's own upstream
rebase had, in the meantime, bumped the seed-default number from 12 to 100 and rewritten the same
paragraph P2.3 had rewritten to describe the Broker Settings override, in both
`docs-site/src/content/docs/hosted/ha/multi-broker.md` and `.../reference/api.md`. Resolution kept
every unrelated P1a addition verbatim — the new default of 100, the Cloud Run sizing clause in
multi-broker.md, and the entire new `PUT /limits/:id` mechanics paragraph in api.md (full-replace
semantics, the 403 on name/resourceType/unit changes, the Cloud Run operator-docs pointer) — and
replaced only the now-false "not supported yet / coming in P2" clause in each with P2.3's accurate
pointer to the Broker Settings API. No other content changed. Full detail, including the exact
before/after text for both hunks:
`/scion-volumes/scratchpad/projects/broker-settings/notes/p2-3-rebase-upstream.md`.

Gates: `go build ./...`, `go vet ./pkg/hub/` (with and without `-tags no_sqlite`), `cd web && npx tsc
--noEmit`, and `golangci-lint --new-from-rev=upstream-main` all clean/0 new issues. Commit-message
grep over `upstream-main..HEAD` clean. `make test-hub-sqlite` is covered under "pkg/hub SQLite
tests" below rather than repeated here.

## Review round 3 (rev-p2-3-3): REQUEST CHANGES

Full review: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-3-3.md`.
The rebase conflict resolution itself was found correct and needed no rework. Two Required findings
and one Nit, all closed in one follow-up commit `f19e87bb2` (no further rebase) plus one PR-body PATCH:

- **R1 (Required, fixed in `f19e87bb2`):** a third copy of the "per-broker values are coming in P2"
  claim survived in `docs-site/src/content/docs/hosted/single-node/hub-setup-cloudrun.md`. It came
  from P1a, on a line the rebase's conflict resolution never touched (not a conflict hunk), so rounds
  1 and 2 could not have seen it — P1a was not yet in their base at the time. Reworded to point at
  Broker Settings, matching the other two pages; left the surrounding Cloud Run scaling guidance
  unchanged. Grepped the whole docs tree for any other "coming in"/"not supported yet" per-broker
  claims: none found (one unrelated hit in a deploy runbook, about an unrelated GKE project field).
- **Nit (fixed in `f19e87bb2`):** the resolved `multi-broker.md` sentence kept the words "admin
  limits API" but had dropped the link the upstream (P1a) sentence carried. Restored it.
- **R2 (Required, fixed via PR-body PATCH, no commit):** the PR body's test-plan line claimed
  `make test-hub-sqlite` was green post-rebase. It wasn't — updated to name both failures
  (`TestCatalogHTTPEntryPoints_LiveMethodCheck`, `TestHandleAgentMessage_LogCapture_RawContentRedacted`)
  and state that both reproduce on bare upstream main, independent of this PR. Applied with
  `gh api -X PATCH repos/ptone/scion/pulls/2306 --input <json>`, verified with one `GET`.

## Review round 4 (rev-p2-3-4): APPROVE

Approved at head `f19e87bb2`. One remaining nit, PR-body only, no commit: item 3 ("Docs") in the
summary still said the seed-default sentences were "left untouched... kept mechanical for its later
rebase" — stale after the upstream rebase actually happened. Updated to say the docs keep P1a's
merged default of 100 and point the per-broker override at Broker Settings everywhere, including the
third hub-setup-cloudrun.md sentence R1 fixed, and added that file to the list of docs touched.
Applied with one `gh api -X PATCH`, verified with one `GET` (confirmed the new text present, the old
text gone, and the bare-`#N` grep on the body still clean). No push was needed for this fix — PR-body
metadata only.

## pkg/hub SQLite: two failures, both pre-existing on upstream main

Both `TestCatalogHTTPEntryPoints_LiveMethodCheck` and `TestHandleAgentMessage_LogCapture_RawContentRedacted`
fail when running the full `make test-hub-sqlite` target on this branch's post-rebase head. Both were
independently reproduced on bare `upstream-main` (`f671d1a8d`, the commit this branch is rebased onto)
by the round-3 reviewer and, before that, by checking out `upstream-main` into an isolated `git
worktree` with zero P2.3 commits. Neither is caused by, or fixable within, this PR:
- `TestCatalogHTTPEntryPoints_LiveMethodCheck` fails identically on both — `GET`/`PUT
  /api/v1/runtime-brokers/{id}/settings` return 404 in the authzop live-inventory check, a P2.1
  route-wiring gap. A separate fix is in progress (per the EM); the route/catalog owner for P2.1
  should track it, not this PR.
- `TestHandleAgentMessage_LogCapture_RawContentRedacted` fails only under the full `pkg/hub` run and
  passes in isolation on both upstream main and this branch — an order-dependent flake (a shared
  log-capture buffer racing with other tests), the same class of issue `f671d1a8d test(hub): fix
  races in two flaky hub tests` already addresses elsewhere on `main`.

The PR body's test plan and this log both now say so accurately; CI's "pkg/hub SQLite Tests" job is
expected to go red on the branch for the same, pre-existing reason and should not block merge review
on that basis.

## Upstream feedback (GoogleCloudPlatform/scion#2142), items 6-12

Second rebase before starting: upstream main had advanced (`f671d1a8d` -> `84aecd566`, 6 commits:
GoogleCloudPlatform/scion#2121, GoogleCloudPlatform/scion#2119, GoogleCloudPlatform/scion#2076,
GoogleCloudPlatform/scion#2132, GoogleCloudPlatform/scion#2136, GoogleCloudPlatform/scion#2129, none
touching `handlers_quota.go` or the
`cmd/boot_broker_quota_bindings_to_settings.go` family). `git rebase upstream-main` from old head
`705157a86` produced a pure rebase — range-diff (`f671d1a8d..705157a86` `upstream-main..4e5c8b4ab`):
all 9 commits `=`. New head `4e5c8b4ab`, pushed before starting the review-feedback work, per the
EM's instruction to report the rebase before touching anything else.

Full brief: `/scion-volumes/scratchpad/projects/broker-settings/reviews/upstream-2141-2142-brief.md`.
Reply drafts (URL, disposition, reply text, evidence per thread):
`/scion-volumes/scratchpad/projects/broker-settings/reviews/upstream-replies-2142.md`.

Gemini-code-assist raised 7 "medium" defensive-nil-check comments against this PR's diff (items 6-12
of the combined brief covering both P2.2 and P2.3). Each was checked against the real contract —
`pkg/store/entadapter/` is the only production `store.Store` implementation — and the established
convention for the same callee elsewhere in the repo, rather than accepted or rejected mechanically.
**All seven were DECLINED; no production code changed.**

| # | Location | Disposition | Why |
|---|---|---|---|
| 6 | `handlers_quota.go:472-486`, nil `limitDef` in `createEntitlement` | DECLINED | `GetLimitDefinition`'s only implementation (`quota_store.go:115-125`) never returns `(nil, nil)` — `entLimitDefinitionToStore` (`:42-54`) always allocates. Four pre-existing call sites in this same file use the result with no guard beyond the same `err != nil` check: `getLimitDefinition` (`:314-323`, passed straight to `writeJSON`, no field access at all), `updateLimitDefinition`'s `existing` (`:359-373`), `deleteLimitDefinition`'s `def` (`:415-426`), `getUsageByLimit`'s `def` (`:668-688`, also passed straight through with no field access); `quota.go:307-315` does the same for the sibling call. |
| 7 | `handlers_quota.go:584-593`, nil `limitDef` in `updateEntitlement` | DECLINED | Same contract and convention as item 6 — the second of the two call sites this PR added. |
| 8 | `boot_broker_quota_bindings_to_settings.go:103`, nil `limitDef` from `GetLimitDefinitionByName` | DECLINED | Same non-nil guarantee via `entLimitDefinitionToStore` (`quota_store.go:128-136,42-54`). `broker_quota.go:179-184` uses the identical call with no guard. (Noted honestly: `quota.go`'s `Reserve` (`:103-112`) and `Release` (`:346-355`) *do* add a defensive guard there, treating it as "no limit configured" — the exception, not the rule, and not the right semantics for a migration that only reaches this line after a successful lookup.) |
| 9 | `boot_broker_quota_bindings_to_settings.go:111`, nil `b` in the grouping loop | DECLINED | `ListEntitlementBindings`'s only implementation (`quota_store.go:231-248`) builds every element via `entEntitlementBindingToStore` (`:56-68`, always non-nil) from ent's own `.All(ctx)`, which never contains nil elements. `quota.go`'s five binding-iteration loops (`:265-269,285-289,298-302,320-324,328-332`) never guard nil elements either. |
| 10 | `boot_broker_quota_bindings_to_settings.go:154`, nil `existing` with `err == nil` | DECLINED | `GetBrokerSettings`'s only implementation (`brokersetting_store.go:71-79`) always returns a non-nil `entBrokerSettingToStore(row)` (`:56-68`) on success. P2.1's own `broker_capacity.go:111-121` dereferences the identical call's result (`rec.Settings.MaxAgents` at `:118`) with no guard. |
| 11 | `maxAgentsFromBindings`, nil `b` | DECLINED | Same contract/convention as item 9, plus a local proof: the grouping loop at `boot_broker_quota_bindings_to_settings.go:111-112` already dereferences every element's `ScopeType`/`ScopeID` before any of them reach `byBroker` — the source of this function's argument — so a nil guard here would be unreachable dead code. |
| 12 | `entitlementBindingIDs`, nil `b` | DECLINED | Same contract/convention and the same local proof as item 11. |

No fixes were needed, so no code commit was made for this round — the project-log update(s) are the
only changes, plus the reply-draft file above (outside the repo). Gates run: `git log --format=%B
upstream-main..HEAD | grep -nE '(^|[^/A-Za-z0-9])#[0-9]+'` (clean) and the same grep over this log
file and the reply-draft file. The first version of this section failed that last check — it named
two internal table cross-references using the literal digraphs, which the grep pattern matches inside
backticks even though GitHub does not autolink code spans; reworded them to "item 6"/"item 9" so the
grep is actually clean, not just "clean except for the two things this sentence used to point at". No
`gh` calls beyond the one batched fetch of the seven review-comment bodies by ID, and no CI watch, per
the EM's instruction to keep `gh` usage minimal.

### Round 1 citation fixes (broker-settings-rev-up2142-1)

Independent review agreed with all seven dispositions on substance (7/7 AGREE) but found several line
citations had drifted from the numbers first noted and returned REQUEST CHANGES limited to text. Full
review: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-up2142-1.md`.
Every citation below was re-checked directly against the branch head, not copied from the reviewer's
numbers:

- **R1 (fixed):** item 6's `handlers_quota.go` citations were off by a small, consistent drift
  (`363/418/672` instead of `359/415/668`), and the claim that all four sites "dereference the result
  immediately" was inaccurate — `getLimitDefinition` and `getUsageByLimit` never dereference a field
  at all (they pass the pointer straight into `writeJSON`/a response struct); only
  `updateLimitDefinition` and `deleteLimitDefinition` read a field (`.System`), and only a few lines
  after the fetch, not immediately. Reworded to state this precisely; corrected to `314-323, 359-373,
  415-426, 668-688`.
- **R2 (fixed):** `quota.go`'s binding-loop citations for items 9, 11, 12 were wrong (`230,250,264,320`
  instead of the actual `265,285,298,320,328`); `brokersetting_store.go`'s citations for item 10 were
  wrong (`77-85`/`62-74` instead of `71-79`/`56-68`); item 8's `GetLimitDefinitionByName` range was
  off by two lines (`126-134` instead of `128-136`). All corrected in both this table and the reply
  drafts.
- **R3 (fixed):** this table now cites the same lines as `upstream-replies-2142.md` — both were wrong
  in the same way before, so both needed the same fix, made together in this commit.
- **Optional, done:** items 11 and 12 now also cite the stronger, local argument the reviewer
  suggested — the grouping loop dereferences every element before either helper is ever called, so a
  guard inside them would be unreachable.

No production code changed in this fixup either — same as the first pass, this is text-only.
