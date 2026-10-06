# Broker settings P2.2: per-broker effective cap in admin-quotas usage and the brokers list (ptone/scion#2061 P2, ptone/scion#2177)

Base: stacked on PR ptone/scion#2275 (P2.1, branch `scion/broker-settings-p2-1`, approved but not yet
merged), started from its head `0875d543a658cd77e7a9ec92ba42436878aa0839`. Design:
`/scion-volumes/scratchpad/projects/broker-settings/design.md` §5.2, §5.6, §5.7 (P2.2), §5.8 (AC-P2-10),
§5.9, §6. Branch `scion/broker-settings-p2-2`.

## What shipped

- **`handleAdminUsageByLimit` (`getUsageByLimit`, `pkg/hub/handlers_quota.go`)**: for
  `max_agents_per_broker`, each broker-scoped active reservation now carries an additive,
  `omitempty` `brokerAgentLimit`/`brokerAgentLimitSource` pair, populated from the shared
  `brokerCapacity` read model (P2.1's `broker_capacity.go`) — never the raw entitlement binding or
  the hub-wide default (AC-P2-10). `def` (the `LimitDefinition` the handler already fetches) is
  reused directly as the `limitDef` argument, so there is no second lookup; a small in-request cache
  keyed by broker ID avoids recomputing `brokerCapacity` once per reservation when a broker holds
  several.
  - **Found and fixed a real display bug in the process**: the handler previously listed
    reservations with `store.ListActiveReservations(ctx, limitID, store.QuotaScopeSystem, "")`
    unconditionally. `max_agents_per_broker` reservations are always written at
    `store.QuotaScopeBroker` scoped to the specific broker (`broker_quota.go`), never at
    `QuotaScopeSystem`, so that query always returned zero rows for this limit — the admin usage
    detail showed "No active usage reservations" for `max_agents_per_broker` regardless of how many
    agents were actually running. Fixed by enumerating every runtime broker and listing that
    broker's reservations for the limit (`listBrokerScopedActiveReservations`), the same approach
    `ReconcileStaleBrokerQuotaReservations` already uses. Every other limit (still queried at system
    scope) is unaffected — the new path is opt-in by `LimitDefinition.Name`.
- **`listRuntimeBrokers` (`pkg/hub/handlers_runtime_brokers.go`) + `RuntimeBrokerWithCapabilities`
  (`response_types.go`)**: `GET /api/v1/runtime-brokers` gains `agentLimit`/`agentCount`/
  `agentLimitSource`, matching the providers listing's field semantics exactly (nil limit =
  unlimited; ptone/scion#2161) via the existing `lookupAgentLimitDefinition`/`resolveBrokerCapacity`
  helpers — one `limitDef` lookup per listing, reused via `resolveBrokerCapacity` per broker. No new
  visibility check: the fields are set inside the loop's existing `capabilityAllows(caps[i],
  ActionRead)` filter, so a caller sees a broker's capacity exactly when they already see that
  broker row — the same rule the providers listing follows ("this change grants no new read access,
  it only adds fields to an existing, already-authorized response").
- **`getUsageSummary` (`handleAdminUsage`, `pkg/hub/handlers_quota.go`)**: the admin usage summary's
  `activeCount` for `max_agents_per_broker` now sums reservations across every runtime broker via the
  same `listBrokerScopedActiveReservations` helper `getUsageByLimit` uses, instead of the
  `store.QuotaScopeSystem` query that always returned 0 for this limit. (Added in review round 1 —
  see "Review round 1" below.)
- **Web**: `admin-quotas.ts`'s "Active Usage" detail panel shows each broker-scoped reservation's
  effective cap and source next to the reserved count (falls back to "unlimited" when the source is
  reported but the limit is omitted). `brokers.ts` gains an "Agents / Cap" column (table view) and a
  matching stat (grid view, which is the page's default view — the design only mentions a "column",
  but the grid is what most users see first, so it was included as well): `"7 / 30"` /
  `"7 / unlimited"` with the source in a tooltip, `"—"` when the fields are absent. Hand-written TS
  mirrors: `RuntimeBroker` in `web/src/shared/types.ts` (new fields), and the local `UsageReservation`
  interface in `admin-quotas.ts` (kept local, matching that file's existing convention of not sharing
  its quota-page-only types). Scoped strictly to the usage rendering in `admin-quotas.ts` — did not
  touch or look at the P1a system-limit-editing UI landing separately in the same file.

## Scoping decisions (raised here, not treated as blockers)

- **`getMyUsage` (`handleUsageMe`) was left unchanged**, after the EM asked to confirm whether it has
  "the same zero-count bug" as `getUsageByLimit`/`getUsageSummary` did. It does show `current: 0` for
  `max_agents_per_broker` for every user, always — but the root cause is different, not the same bug:
  `getMyUsage` calls `CountActiveReservations(ctx, def.ID, userID, QuotaScopeSystem, "")`, filtering
  on `subjectID = userID`. `max_agents_per_broker` reservations are always created with
  `SubjectID = brokerID` (`broker_quota.go`), never a user ID, so no reservation can ever match this
  query regardless of which scope is queried — summing across brokers "the same way" would not fix a
  scope mismatch here, it would silently replace "this user's own usage" with "the whole system's
  broker usage" attributed to one user, which misrepresents what `/usage/me` (a personal-usage
  endpoint) means. Recommending this be left alone, or that `max_agents_per_broker` be excluded from
  `/usage/me` entirely (it isn't a per-user quota) — the current 0 is at least not misleading in the
  way a borrowed system-wide count would be. Left as-is pending the EM's call; said so explicitly
  rather than applying the literal instruction where the precondition ("the same bug") didn't hold.
  **EM decision (review round 1): leave `getMyUsage` as-is, `0` by construction, no change in this
  PR.** The EM is logging a separate follow-up to consider excluding broker-scoped limits from
  `/usage/me` entirely.
- **Visibility rule for the brokers list mapped cleanly onto the providers-listing rule** (brief item
  3's "if it doesn't map cleanly, ask the EM first"): both listings already gate the entire row behind
  a read-capability/permission check before any capacity field is computed, so adding the fields
  inside that existing gate — with no new check — reproduces the providers listing's "no new read
  access" property exactly. No EM escalation was needed.

## Review round 1

Pre-report exchange with the EM (before formal review): asked to fold in the `getUsageSummary` fix
rather than leave it as a flagged gap (my initial read was that it was aggregate-only and out of the
brief's "per-broker row" scope; the EM's call was that a summary permanently showing 0 active agents is
exactly the display bug P2.2 exists to fix, scope question aside). Fixed by reusing
`listBrokerScopedActiveReservations` for `max_agents_per_broker` in `getUsageSummary` too, with two new
tests (`TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers`,
`TestGetUsageSummary_NonBrokerLimit_Unaffected`). Investigated `getMyUsage` per the EM's conditional
ask and found a related but distinct bug (see above) — left unchanged and reported rather than assumed
"fix it the same way" applied. **EM decision: leave `getMyUsage` as-is** (recorded above).

### Formal review: `broker-settings-rev-p2-2-1`, verdict REQUEST CHANGES

Full report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-1.md`
(reviewed at `1030ec34`; confirmed the server-side read model, limitDef lookups, authz and Go/TS type
parity are all correct). Disposition of every finding:

- **F1 (Required, fixed):** `renderLimitRow` in `admin-quotas.ts` divided the new cross-broker sum by
  the *per-broker* `defaultValue` (e.g. three brokers at 12 agents each with a default of 30 showed
  "36 / 30" at a pegged 100% progress bar — a false breach, since no single broker was near its cap).
  Fixed by rendering just the count for `max_agents_per_broker` (no denominator, no progress bar), plus
  a hint ("across all brokers; cap is per broker"). Localized to `renderLimitRow`; did not touch the
  entitlement/limit-dialog code P1a also edits in this file.
- **F2 (Required, fixed):** added `TestListRuntimeBrokers_AgentCountAgreesWithReserve`
  (`handlers_runtime_brokers_capacity_test.go`), which drives the actual enforcement path
  (`checkAndReserveBrokerQuota`, the same helper `handlers_agents_core.go` calls) rather than a
  manually-inserted reservation row: two reservations admitted, a third rejected at the cap, then the
  cap cleared to unlimited (`maxAgents=0`) and re-asserted — `agentLimit` absent, `agentCount` still
  present at 2, source still `"broker"`. This is the "agree with Reserve" half of AC-P2-10 the brief
  asked for and the round-1 test suite was missing.
- **F3 (Optional, declined by the EM for this PR):** `listBrokerScopedActiveReservations`'s 1+B queries
  and 10,000-broker cap are correct (mirror `ReconcileStaleBrokerQuotaReservations` exactly) but not
  optimal. Added a code comment on the function documenting the bound and the accepted follow-up (a
  single-query store method), per the EM's call — no behavior change.
- **F4 (Nit, fixed in round 1, corrected in round 2 — see below):** the doc comments for
  `AgentCount`/`AgentLimitSource` (`response_types.go`, `handlers_quota.go`) and their TS mirrors
  (`types.ts`, `admin-quotas.ts`) said they were absent "under the same conditions as AgentLimit" —
  wrong, since `AgentLimit` is also nil when the broker is unlimited, and in that case the count and
  source *are* still present. Round 1's fix, however, over-corrected into a second false statement:
  that the source is literally the string `"unlimited"` in that case. It is not — see the round 2
  disposition.
- **F5 (Nit, fixed):** `brokers.ts`'s `renderAgentCapacity` showed the source twice (tooltip + small
  text) — removed the redundant small-text line, keeping the tooltip. `admin-quotas.ts` rendered
  "Broker cap: unlimited (unlimited)" — the parenthetical source is now omitted when the source itself
  is `"unlimited"`. Added an explicit `TemplateResult` return type to `renderAgentCapacity`, which also
  removed the one eslint warning this PR had introduced (45 → 44, back to the `main` baseline).
- **F6, F7, F8 (FYI):** no action, per the EM.

Re-verified after all fixes: `go build`/`go vet` (both build tags), the full
`go test ./pkg/hub/ -run 'Usage|Broker|Provider|Quota'` subset (green, 119.5s, including the new F2
test), `go test -tags no_sqlite ./pkg/hub/...` (green), `golangci-lint run ./pkg/hub/...` (same 11
pre-existing issues, none in changed files — matches the reviewer's own gate exactly),
`hack/check-authz-guards.sh` (clean), `tsc --noEmit` (clean), eslint on the three changed web files back
to the exact `main` baseline (6 errors — all pre-existing — 44 warnings, F5's new warning removed).

## Tests

- `TestGetUsageByLimit_MaxAgentsPerBroker_PerBrokerSourceAndLimit`: three brokers (settings override →
  `broker`; no override → `hub_default`; entitlement binding → `entitlement`) each with one active
  reservation; asserts `brokerAgentLimit`/`brokerAgentLimitSource` per broker and that all three
  reservations are listed (pins the scope-query fix — this would have asserted 0 reservations before
  the fix).
- `TestGetUsageByLimit_NonBrokerLimit_Unaffected`: a system-scoped limit keeps the original query path
  and never carries the new fields (checked against the raw JSON, not just the decoded struct, to
  confirm the keys are actually absent from the wire format).
- `TestGetUsageByLimit_MaxAgentsPerBroker_NoActiveReservations`: 200 with an empty list, not nil/500,
  when no broker holds a reservation.
- `TestListRuntimeBrokers_AgentLimitFromSettingsOverride` / `_AgentLimitHubDefault` /
  `_AgentLimitFromEntitlementBinding`: the three sources, mirroring the providers-listing capacity
  tests.
- `TestListRuntimeBrokers_CapacityFieldsAgreeWithSettingsGET`: cross-checks the brokers list against
  `GET .../settings` for the same broker (AC-P2-10 at this layer).
- `TestListRuntimeBrokers_DeniedUserSeesNoCapacityFields`: a user without `broker.read` for a broker
  doesn't see that broker's row at all, capacity fields included.
- `TestListRuntimeBrokers_OneLimitDefinitionLookupPerListing`: a counting store wrapper asserts
  `GetLimitDefinitionByName` is called exactly once for a three-broker listing.
- `TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers`: two reservations on one broker, one on
  another; the summary's `activeCount` for `max_agents_per_broker` must be 3, not 0.
- `TestGetUsageSummary_NonBrokerLimit_Unaffected`: a system-scoped limit's summary count is unaffected
  by the broker-scoped enumeration.
- `TestListRuntimeBrokers_AgentCountAgreesWithReserve` (review round 1, F2): drives
  `checkAndReserveBrokerQuota` directly (two admitted, a third rejected at the cap), then clears the
  cap to unlimited — asserts the brokers list's `agentCount`/`agentLimit`/`agentLimitSource` agree with
  what Reserve actually enforced at every step, including the unlimited shape.

## Verification

- `make test-hub-sqlite` equivalent (env scrubbed of `SCION_*`/`CLAUDE_*`): full
  `go test -count=1 -timeout 25m -skip '...(the four known pre-existing failures)...' ./pkg/hub/...`
  — all packages `ok` (`pkg/hub` ~970s).
- `go test -tags no_sqlite ./pkg/hub/...` (test-fast equivalent for this package) — `ok`.
- `go build -buildvcs=false ./...` and `go vet -buildvcs=false ./...` — clean.
- `golangci-lint run --new-from-rev=main --concurrency=1 ./pkg/hub/...` — 0 issues.
- `hack/check-authz-guards.sh` — no violations (no new routes; existing routes only gained response
  fields).
- `gofmt -l` on every changed/new Go file — clean.
- `cd web && npx tsc --noEmit` — clean.
- `cd web && npx eslint src/components/pages/admin-quotas.ts src/components/pages/brokers.ts
  src/shared/types.ts` — same pre-existing error/warning count as `main` (6 errors, all pre-existing
  `@typescript-eslint/unbound-method` findings in code this PR didn't touch); the two new
  `prettier/prettier` findings introduced by this PR's own added lines were fixed via `eslint --fix`
  before commit.
- Not run: the Postgres suite (no `pkg/store` changes in this PR) and the full-repo
  `make test-fast`/`make ci-full` (scoped to the touched package/directory instead, consistent with
  sandbox runtime limits for the whole-repo suites).

## Deliverables

- Draft PR ptone/scion#2303 against `ptone/scion` `main` from `scion/broker-settings-p2-2`, stacked on
  P2.1 (ptone/scion#2275) — kept in draft per the brief until P2.1 lands and this is rebased with
  `--onto`.
- This log entry.
- `scion message` to `broker-settings-em` with the PR number, head SHA, and the test evidence above.

## Review round 2

Full report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-2.md`
(reviewed at `78dbd647`; round 1's F1 and F2 fixes independently re-verified as resolved). Verdict
REQUEST CHANGES, comments-only.

- **F1 (Required, fixed):** round 1's F4 fix replaced one false doc statement ("absent under the same
  conditions as AgentLimit") with a *different* false one: that `AgentLimitSource`/`BrokerAgentLimitSource`
  is literally the string `"unlimited"` whenever a broker has no cap. That is wrong.
  `effectiveBrokerLimit` (`broker_capacity.go`, P2.1, unchanged by this PR) only returns the source
  `"unlimited"` when `limitDef == nil || s.quotaService == nil` — a hub-wide "no quota system
  configured" state, not a per-broker "no cap" state. A broker with `settings.maxAgents=0` (or a 0
  binding, or a 0 default) resolves with source `"broker"` (or `"entitlement"`/`"hub_default"`) and
  `Limit` simply absent — exactly what this PR's own `TestListRuntimeBrokers_AgentCountAgreesWithReserve`
  already asserts (`assert.Equal(t, BrokerLimitSourceBroker, view.AgentLimitSource, ...)` after setting
  `maxAgents=0`), which is how the reviewer caught the doc/test mismatch. Reworded all four comments
  (`response_types.go`, `handlers_quota.go`, `shared/types.ts`, `admin-quotas.ts`) to state the real
  invariant: the source names the precedence step that produced the result, not whether that result is
  a cap; `"unlimited"` is reserved for the hub-wide no-quota-service/no-limit-definition case, in which
  all three fields are omitted together. Corrected the round-1 F4 line above accordingly.
  The `admin-quotas.ts` `=== 'unlimited'` branch this false comment had motivated is dead in practice
  (that source value can't reach a row that requires an actual reservation to exist) but harmless if it
  ever did fire — kept per the EM's option, with a comment marking it defensive.
- **F2 (Optional, fixed):** `brokers.ts`'s grid stat re-implemented the same "count / cap-or-unlimited"
  formatting `renderAgentCapacity` already produces for the table cell — a small, easy dedup.
  `renderAgentCapacity`'s span now carries both `mono-cell` (scoped to the table's
  `.resource-table-container`, so a no-op in the grid) and `stat-value` (unscoped, styles the grid
  card), and the grid stat calls it directly instead of duplicating the template.
- **F3, F4 (FYI):** no action — F3 restates round 1's F3 (accepted by the EM); F4 confirms the CI
  reporting-only failures are unrelated (already reported to the EM after round 1's CI run).

Re-verified after both fixes: `go build`/`go vet` (both tags), `tsc --noEmit`, eslint on the three
changed web files (still the `main` baseline, 6 errors/44 warnings, 0 new), `golangci-lint run
./pkg/hub/...` (same 11 pre-existing issues, none in changed files), `hack/check-authz-guards.sh`
(clean), bare-#N grep on commits (empty).

## Review round 3: APPROVE

Full report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-3.md`
(reviewed at `743bdfd6`). No Critical or Required findings; round 2's F1 and F2 both independently
re-verified as resolved, with the fix delta confirmed to be comments/template only (no logic change).
Disposition of the remaining, non-blocking findings:

- **F1 (Optional, addressed):** the PR body had gone stale relative to the shipped diff — it didn't
  mention the `getUsageSummary` `activeCount` fix (always-0 → the cross-broker sum for
  `max_agents_per_broker`, a wire-visible behavior change the EM asked for after the initial submission),
  the matching admin-quotas summary-row rendering change (round-1 F1), or the three tests added for
  those (`TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers`,
  `TestGetUsageSummary_NonBrokerLimit_Unaffected`, `TestListRuntimeBrokers_AgentCountAgreesWithReserve`).
  It also said the UI renders "-" when capacity fields are absent; the code renders an em dash "—".
  **Correction (round 4): this disposition was wrong.** The `gh pr edit` command run at this point
  reported a `GraphQL: ... (repository.pullRequest.projectCards)` error and exited non-zero, but that
  exit status was not checked, and a follow-up `gh pr view --json body | head -5` looked unchanged only
  because the new body's first five lines happen to be identical to the old ones — the actual edit never
  applied. `gh api repos/ptone/scion/pulls/2303 --jq '.updated_at,.body'` still showed the pre-round-3
  body and an unchanged `updated_at`. Caught by the round-4 review; see that section for the real fix.
- **F2 (Nit, fixed):** the `BrokerAgentLimitSource` doc comment (`handlers_quota.go`) gave the wrong
  reason for a correct conclusion — it said `"unlimited"` is unreachable here because `brokerCapacity`
  "returns before counting", but skipping the count has nothing to do with reachability (this view
  ignores `Count` entirely). Reworded to the actual reason: `"unlimited"` needs a nil `limitDef` or a
  nil `quotaService` in `effectiveBrokerLimit`, and neither can occur in `getUsageByLimit` (`def` is
  always this request's non-nil limit definition, and `s.quotaService` is always constructed in
  `NewServer`, `server.go`). *(Note: this bullet originally cited `getUsageByLimit`/`getUsageSummary`;
  round 4's F2 caught that only `getUsageByLimit` builds `usageReservationView`, so the citation is
  corrected here to match the code as it stands after round 4.)*
- **F4 (FYI, cheap fix applied):** `BrokerAgentLimit`'s doc comment listed "unlimited" and "not a broker
  row" as its nil cases but not "resolution failed" (`BrokerCapacity{}` on an outright error). Added.
- **F3 (FYI):** the brokers-list table cell now renders at `font-weight: 500` (via the unscoped
  `.stat-value` class picked up by the F2-round-2 dedup) instead of the default 400; size and color are
  unchanged, and this was already analyzed and accepted as a cosmetic, non-regression side effect of the
  round-2 dedup. No action.
- **F5 (FYI):** confirms there is no separately named "Verify Web Types" CI check on this head — it
  runs inside "Build & Test", which is green — and that the two reporting-only failures remain the same
  pre-existing, unrelated ones already reported after round 1 (`internal/fixturegen`
  `TestFixtureCoverage`, and the 405-Allow-header lint). No action.

Re-verified after F2/F4: `go build ./pkg/hub/...` and `go vet ./pkg/hub/...`. F1 (the PR body refresh)
needs no code verification.

## Review round 4: REQUEST CHANGES

Report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-4.md`.

- **F1 (Required, fixed for real this time):** the round-3 PR body refresh had not actually applied.
  `gh pr edit 2303 -R ptone/scion --body-file <path>` fails on this `gh` CLI (2.23.0) with
  `GraphQL: Projects (classic) is being deprecated ... (repository.pullRequest.projectCards)` and exits
  non-zero — an old CLI querying a field GitHub has since removed from schema for repos where Projects
  Classic is fully sunset. The round-3 attempt did not check the exit code, and the follow-up spot check
  (`gh pr view --json body | head -5`) missed the failure because the new body's first five lines are
  identical to the old ones (the changes are further down). `gh api
  repos/ptone/scion/pulls/2303 --jq '.updated_at,.body'` — a direct, cache-proof read of the REST
  resource — confirmed the body and `updated_at` were both still the pre-round-3 values.
  - Fixed by writing the body to
    `/scion-volumes/scratchpad/projects/broker-settings/notes/p2-2-pr-body.md` and applying it with
    `gh api -X PATCH repos/ptone/scion/pulls/2303 --input <json payload wrapping that file's content>`
    instead of `gh pr edit` — the REST PATCH endpoint doesn't hit the deprecated GraphQL field.
    Confirmed with exit code 0 and a fresh `updated_at`, then re-ran the exact verification grep the EM
    specified; all four required strings (`SumsAcrossBrokers`, `NonBrokerLimit_Unaffected`,
    `AgentCountAgreesWithReserve`, `getUsageSummary`) and the em dash are present in the live PR body.
  - Corrected the false round-3 F1 disposition above rather than quietly rewriting history.
- **F2 (Nit, fixed):** the `BrokerAgentLimitSource` comment's "see getUsageByLimit/getUsageSummary"
  citation was wrong — only `getUsageByLimit` builds `usageReservationView` (the struct this comment is
  on); `getUsageSummary` only sums counts and never touches this type. Changed the citation to
  `getUsageByLimit` alone.

Re-verified: `go build ./pkg/hub/...` and `go vet ./pkg/hub/...`. `gh api ... --jq '.updated_at,.body'`
piped through the EM's exact grep, pasted into the report back to the EM.

## Review round 5: APPROVE

Report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-5.md`. Two
comment-only nits, both fixed:

- **F1:** `brokers.ts`'s `renderAgentCapacity` JSDoc still said `'-'` for the absent case; the code
  renders an em dash `"—"`. Corrected the doc to match.
- **F2:** the round-3 section's F2 bullet above still cited `getUsageByLimit`/`getUsageSummary` for the
  `BrokerAgentLimitSource` comment fix, but round 4 had since corrected the actual code comment to cite
  `getUsageByLimit` alone. Fixed the bullet to match and noted why.

Last review round (6 of 6, including the initial submission). Re-verified: `cd web && npx tsc --noEmit`.

## Review round 6: APPROVE (final, pre-rebase)

Report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-6.md`
(reviewed at `9ed1ea38`, fix delta `a20cdb7e..9ed1ea38`: the `brokers.ts` JSDoc fix and the round-5
project-log entry above). Both round-5 findings independently re-verified as resolved: the JSDoc now
matches the em dash the code renders, and the round-3 F2 bullet's citation is confirmed accurate against
`handlers_quota.go`. One new, non-blocking finding:

- **F1 (Nit, deferred at the time, closed now):** the round-5 entry above said "the round-2 section's F2
  bullet", but the bullet it corrected is in the "Review round 3" section, not round 2. Purely a
  historical-record typo with no effect on code; the round-6 reviewer explicitly deferred the fix to "any
  later touch of the log, e.g. during the pre-merge rebase" rather than spending another round on a
  wording-only nit. That later touch is this entry — see the correction applied to the round-5 F2 bullet
  above (now reads "the round-3 section's F2 bullet").
- **F2 (FYI):** the PR's `updated_at` moved between checks with no body change — traced to the push
  itself updating the timestamp, not a body edit. No action needed.

Gates (detached checkout at `9ed1ea38`): `tsc --noEmit` pass; `eslint` on `brokers.ts` at the same base
count as P2.1's `0875d543` (1 error/13 warnings, 0 new — the delta is JSDoc/log only); `go build ./...`
pass. CI: Build & Test pass (7m32s, includes Verify Web Types), golangci-lint pass, T1 PostgreSQL,
single-node-vm harness, shellcheck and Mergeability Gate all pass; Lint 405 (reporting-only) fails, same
pre-existing issue as every prior round. `pkg/hub SQLite Tests` and the reporting-only Full Test Suite
were still pending when the reviewer finished — neither is a required check, and this delta changes no
Go code. Bare-`#N` greps on both the commit range and the live PR body: empty.

## Note on the upstream-main rebase step (superseded — see below)

`dev-common-rules.md` asks every branch to rebase onto `GoogleCloudPlatform/scion` `main` before
reporting ready. This branch is stacked on P2.1, which is itself still based on `ptone/scion` `main`
and not yet merged upstream — an independent rebase onto the real upstream here would rewrite P2.1's
commits inside this branch and desync it from the actual `scion/broker-settings-p2-1` branch the EM
tracks. Per the brief's explicit stacking instructions ("rebase with `--onto <new base> 0875d543`"
only when P2.1's base changes), that rebase was deliberately not run independently; flagged to the EM
in the completion message in case an upstream sync is wanted before P2.1 lands.

**Update:** this held only while P2.1 was unmerged. P2.1 landed upstream (see below), and the branch has
since been rebased onto real upstream main as the EM instructed.

## Rebase 1: onto P2.1's new approved fork head (19b8064d)

P2.1 gained a new approved head on the fork, `19b8064d81d64b0416d113cd99e698dd7f1361f1` (old:
`0875d543`), still unmerged upstream. Per the EM: `git rebase --onto 19b8064d... 0875d543...`, a pure
rebase (no content changes). Result: clean, no conflicts, 18/18 commits `=` in
`git range-diff 0875d543..<old head 9ed1ea381> 19b8064d..<new head 0e4adb627>`, and
`git diff <old head> <new head>` byte-identical to `git diff 0875d543 19b8064d` (confirmed with `diff`
on both outputs). `go build ./pkg/hub/...` / `go vet ./pkg/hub/...` clean. Pushed with
`--force-with-lease`; reported to the EM. Superseded within the hour by rebase 2 below, before its own
CI watch finished.

## Rebase 2: onto upstream main (P2.1 merged as GoogleCloudPlatform/scion#2126)

P2.1 merged upstream at 2026-09-30 13:06Z as `86fc807b` (after itself being rebased onto upstream main
as `d37a6215` first — the EM's HOLD caught this branch mid-CI-watch on the now-obsolete `19b8064d`
target, so the rebase above was superseded before it was fully verified end-to-end). Per the EM:

```
git fetch https://github.com/GoogleCloudPlatform/scion.git main:upstream-main
git rebase --onto upstream-main 19b8064d81d64b0416d113cd99e698dd7f1361f1   # OLDBASE
```

OLDBASE confirmed via `git merge-base --is-ancestor 19b8064d... HEAD` (the branch's 18 P2.2 commits sat
directly on it). Result: clean, no conflicts — recorded (as "none") in
`/scion-volumes/scratchpad/projects/broker-settings/notes/p2-2-rebase-upstream.md`. All 18 commits show
`=` in `git range-diff 19b8064d..0e4adb627 upstream-main..0bb5c2c97`. New head: `0bb5c2c9703f83a04e304eef4c96349a1782028d`.

Gates: `go build ./...`, `go vet ./pkg/hub/` (+`-tags no_sqlite`), `tsc --noEmit`,
`golangci-lint --new-from-rev=upstream-main ./pkg/hub/...` (0 issues) all clean; bare-`#N` grep on
`upstream-main..HEAD` empty. eslint on the three changed web files reads 7 errors/44 warnings — one more
error than the pre-rebase 6, traced to `admin-quotas.ts` alone: upstream `main` now includes P1a's
merged system-limit-editing UI in this same file, and linting *that* file in isolation at
`upstream-main` (before any P2.2 commit) already shows 6 errors/31 warnings — a pre-existing prettier
finding in P1a's code, not ours. Combined with `brokers.ts`'s unchanged 1 error/13 warnings, 7/44 is the
new correct baseline; 0 new from P2.2's own commits. `make test-hub-sqlite` and CI were run and reported
separately (see the message log to the EM) rather than duplicated here.

Pushed with `--force-with-lease`. PR body updated (metadata only, via `gh api -X PATCH`, not `gh pr
edit` — see the "gh pr edit reliability" note in review round 4's section) to say P2.1 merged upstream
as GoogleCloudPlatform/scion#2126 instead of "stacked on P2.1, do not merge". PR kept in draft, per the
EM's explicit instruction that they will mark it ready themselves after verification.

## Amendment A1: `not_enforced` visible marker

Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md`, "Amendment A1" (added
2026-09-30 13:10Z, after P2.2's own review rounds closed): when the P1b enforcement switch is off, the
effective limit keeps its resolved value but the source becomes `not_enforced`; every surface showing
the limit must show that it is not enforced, visibly — not tooltip-only.

**Scope, one commit on top of the upstream-main rebase, per the EM's addendum:**
- `web/src/components/pages/brokers.ts`: `renderAgentCapacity` (shared by the table cell and the grid
  stat — one change covers both) now appends a `.not-enforced-marker` badge ("not enforced") next to the
  value when `agentLimitSource === 'not_enforced'`. The existing tooltip is unchanged; the badge is the
  new, non-tooltip-only signal.
- `web/src/components/pages/admin-quotas.ts`: the usage-detail reservation card shows the same badge
  next to "Broker cap: N (source)" when `brokerAgentLimitSource === 'not_enforced'`.
- The summary row (`renderLimitRow`) was left alone: for `max_agents_per_broker` it already renders only
  the cross-broker count with no cap/denominator (round-1 F1's fix), so there is no cap shown there for
  the marker to attach to — the EM's own instruction anticipated this ("if it mentions a cap, apply the
  same rule; otherwise leave it").
- `pkg/hub/response_types.go` (`AgentLimitSource`) and `pkg/hub/handlers_quota.go`
  (`BrokerAgentLimitSource`): added a paragraph stating that `not_enforced` means the value is a real,
  resolved cap that is informational only — not currently enforced — and that every caller rendering the
  limit must also render the source, visibly, for that reason. **Deliberately did not** extend the
  TS-side doc comments (`shared/types.ts`, the local `admin-quotas.ts` interface) beyond what the EM
  named — only the two Go locations were requested, and this PR's whole review history is about doc/code
  drift from comments touched beyond what was asked; noted here rather than silently expanding scope.
- The backend does not produce `not_enforced` yet (P1b, ptone/scion#2270, is a separate in-progress PR).
  Per the EM's instruction, the rendering contract is tested ahead of that wiring with a stubbed source,
  using the repo's existing page-component test pattern (Vitest + happy-dom, mounting the real custom
  element and reading `shadowRoot`, the same pattern `project-detail.test.ts` and `admin-users.test.ts`
  already use) rather than a Go handler test — the change is entirely in the rendering layer, so a web
  test exercises the actual code path directly instead of only its data contract:
  - `web/src/components/pages/brokers.test.ts` (new): three tests — grid view and table view both show
    `"7 / 30"` plus a visible `.not-enforced-marker` element containing "not enforced" text when
    `agentLimitSource: 'not_enforced'` is stubbed on the `/api/v1/runtime-brokers` response; a third test
    confirms the marker is absent for an ordinary `hub_default` source.
  - `web/src/components/pages/admin-quotas.test.ts` (new): mounts the page, stubs
    `/api/v1/admin/limits`, `/api/v1/admin/usage`, `/api/v1/admin/limits/{id}/entitlements` and
    `/api/v1/admin/usage/{id}`, clicks the limit row to expand it, and asserts the rendered usage card
    shows the marker for a stubbed `brokerAgentLimitSource: 'not_enforced'` reservation.
  - Both mounted-component tests needed the same `FakeEventSource`/`localStorage` stubbing
    `project-detail.test.ts` already established for happy-dom (which has no native `EventSource`).

Verification: `cd web && npx tsc --noEmit` clean; `npx vitest run
src/components/pages/brokers.test.ts src/components/pages/admin-quotas.test.ts` — 4/4 pass; `npx eslint`
on the two changed page files plus the two new test files — the non-test files stay at the 7/44
baseline (0 new); the two new `.test.ts` files themselves hit a pre-existing, repo-wide parsing error
("TSConfig does not include this file") that every other `*.test.ts` file in the repo also hits (verified
by linting `admin-users.test.ts` in isolation) — not something introduced here, and not part of any
gate this project runs. `go build ./pkg/hub/...` / `go vet ./pkg/hub/...` clean (Go changes are
comments-only).

## Review round 7 (Amendment A1 commit): REQUEST CHANGES, then closed

Report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-2-7.md`.
Scoped to the single Amendment A1 commit (`fd324e4e`). Verdict REQUEST CHANGES on one Required finding;
everything else non-blocking. Disposition, one follow-up commit:

- **F1 (Required, fixed):** the hand-written TS field comments (`shared/types.ts` `agentLimitSource`,
  the local `admin-quotas.ts` `brokerAgentLimitSource`) still enumerated only `"broker" | "entitlement" |
  "hub_default" | "unlimited"` and called the field "the precedence step" — a closed set that excluded
  `not_enforced`, while the same commit's own code already branched on `=== 'not_enforced'` and the Go
  comments already documented it. The project log's A1 section had defended leaving the TS comments
  alone as intentional scope control; the reviewer's point that this specific comment *now contradicts
  the code in the same diff* (not just "incomplete relative to a future feature") is correct — that is
  exactly the doc/code drift class this PR's whole review history exists to catch, so it applies here
  too. Added `"not_enforced"` to both enumerations and a paragraph mirroring the Go wording (resolved
  value kept, informational only, must render visibly).
- **F2 (Optional, fixed):** `admin-quotas.test.ts` only covered the positive (`not_enforced`) case.
  Parameterized the reservation's source, added a `hub_default` case asserting `.not-enforced-marker` is
  absent, and asserted `"Broker cap: 30"` explicitly in both cases (the value-kept half of A1, not just
  that a marker appears).
- **F3 (Nit, fixed):** the usage-detail card rendered the fact twice for a `not_enforced` row —
  `"Broker cap: 30 (not_enforced) not enforced"`. Extended the existing "suppress the raw source token"
  condition (already applied to `unlimited`) to also cover `not_enforced`, so only the pill renders.
- **F4 (Nit, fixed):** the grid's `.not-enforced-marker` sat in a `flex-direction: column` container
  (`.stat`) with the default `align-items: stretch`, blockifying and left-stretching the pill instead of
  letting it hug its text. Added `align-self: flex-start`. In the table cell (not a flex container) the
  property is a no-op, so nothing there changes.
- **F5 (Nit, fixed):** "AgentLimit/BrokerAgentLimit is still the real, resolved cap — not omitted" was
  true only when the resolved value is positive; under A1 a `not_enforced` broker whose resolved limit is
  <= 0 still has the field absent, same as every other source. Reworded to "keeps whatever the precedence
  steps resolved (a cap, or absent when that resolves to unlimited)" in both Go comments, and added
  `"not_enforced"` to the value list at the top of each so the follow-up paragraph doesn't read as an
  afterthought.
- **F6 (Consider, declined):** the 11-line `.not-enforced-marker` CSS rule is duplicated in `brokers.ts`
  and `admin-quotas.ts`. The EM's instruction was to fix it only if there is a shared stylesheet both
  files *already* use — there is not: `brokers.ts` imports `listPageStyles`/`brokerTypeBadgeStyles` from
  `resource-styles.ts`, but `admin-quotas.ts` imports no shared stylesheet at all and defines every rule
  (including its pre-existing `.system-badge`) inline. Introducing a shared import into a file that has
  none, to deduplicate one 11-line rule, is a larger structural change than this fix warrants and cuts
  against "keep it localized" — declined for this PR. `resource-styles.ts`'s existing `.badge`/
  `.badge.sensitive` rules are a reasonable base for that refactor if it's done later, deliberately,
  across both files' badge styling at once rather than as a side effect of this fix.

Re-verified: `tsc --noEmit` clean; `vitest run` on both test files, 5/5 pass (F2 added one); `eslint` on
the two changed page files stays at the stated 7/44 baseline (0 new); `go build ./pkg/hub/...` /
`go vet ./pkg/hub/...` clean (Go changes remain comments-only); bare-`#N` grep empty.

### Local `pkg/hub` SQLite suite FAIL: two tests, both traced to pre-existing upstream-main issues

A local `make test-hub-sqlite`-equivalent run for the upstream-main rebase came back `FAIL` for the
top-level `pkg/hub` package (every subpackage still `ok`). An initial pass at this note assumed the
failure detail was lost to `tail -200` truncation; an untruncated re-run recovered it, and both findings
were then confirmed against CI directly and against a clean upstream-main checkout, not just re-run
locally:

- **`TestCatalogHTTPEntryPoints_LiveMethodCheck`**: fails because `GET`/`PUT
  /api/v1/runtime-brokers/{id}/settings` (P2.1's broker-settings route, merged upstream as
  GoogleCloudPlatform/scion#2126) returns 404 in this authzop catalog live-route-reachability check
  (`operation broker.read` / `quota.update`: "the catalog's declared path does not reach a live route for
  this operation"). **Pre-existing on bare upstream main**: reproduced in a detached worktree at
  `f671d1a8d` (upstream-main's tip, no P2.2 commits at all) running only this test — it fails there
  identically. A gap in P2.1's merged catalog entries versus this test's live-route check, not something
  P2.2 touches or could have caused.
- **`TestHandleAgentMessage_LogCapture_RawContentRedacted`**: fails when run as part of the full
  `pkg/hub` suite (confirmed independently via `gh api` on CI's own "pkg/hub SQLite Tests" job for commit
  `0bb5c2c9`, conclusion `"failure"` — not a local-sandbox artifact), but **passes cleanly in isolation**
  on the same upstream-main worktree. A test-isolation/ordering flake, consistent with upstream-main's
  own tip commit at fetch time being `test(hub): fix races in two flaky hub tests` — evidently not an
  exhaustive fix. Amendment A1's changes are comments and rendering-layer only and touch neither agent
  messaging nor log redaction, so this cannot be a regression from this branch either.

Neither failing test is in a file this PR touches, and both are reproducible independent of this branch
(one on bare upstream main, the other in CI's own job for a commit that predates the Amendment A1
commit). Reported to the EM with the reproduction evidence; no fix attempted here, since the fix belongs
in upstream main / P1b's route registration and in whatever shared state the flaky test needs isolated,
not in `scion/broker-settings-p2-2`.

## Upstream feedback: GoogleCloudPlatform/scion#2141 (gemini-code-assist), items 1-5

Brief: `/scion-volumes/scratchpad/projects/broker-settings/reviews/upstream-2141-2142-brief.md`. Five
gemini-code-assist "medium" threads on the upstream mirror PR (GoogleCloudPlatform/scion#2141) routed to
this branch; items 6-12 (GoogleCloudPlatform/scion#2142) belong to P2.3. Judged each against the actual store contract and the
repo's own convention for the same callee elsewhere in `pkg/hub`, per the brief's instructions — not
accepted or rejected mechanically.

**Pre-fix rebase** onto upstream main (branch was 6 commits behind): old head `2bf227ee2`, new head
`9aa4e56bf`. `git range-diff f671d1a8d..2bf227ee2 upstream-main..9aa4e56bf`: all 21 commits `=`. Diff of
diffs byte-identical. Clean, no conflicts.

1. **DECLINED** — `handlers_quota.go:732` (`for i, res := range reservations`), nil `res` in the
   reservations loop. Contract: every element `ListActiveReservations` returns comes from
   `entUsageReservationToStore` (`pkg/store/entadapter/quota_store.go:71-83`), which unconditionally
   constructs a new `&store.UsageReservation{...}` literal for every input row — it cannot return nil, and
   there is exactly one store implementation. Convention: `broker_quota.go:210-213,222-223`
   (`ReconcileStaleBrokerQuotaReservations`) ranges over the identical `[]*store.UsageReservation` from
   the identical callee and accesses `res.ResourceID` with no nil check. Adding one here would be
   inconsistent with the established pattern for guarding against a case the contract rules out.
2. **DECLINED** — `handlers_quota.go:774` (`for _, broker := range brokers.Items`), nil `brokers` from
   `ListRuntimeBrokers`. Contract: `pkg/store/entadapter/project_store.go:968-1060` — every error path
   returns `(nil, err)` with a non-nil `err`; the success path always constructs and returns a non-nil
   `*store.ListResult`. `listBrokerScopedActiveReservations` already returns early on `err != nil`
   (`:769-771`), so by the time `brokers.Items` is reached, `err == nil` and `brokers` cannot be nil.
   Convention: the same callee is used without a nil check after the error check in
   `broker_quota.go:187-194`, `handlers_runtime_brokers.go:81-86`, and every other `pkg/hub` call site
   (checked all 8 non-test call sites of `ListRuntimeBrokers` in `pkg/hub`).
   - **Also asked:** whether the `ListRuntimeBrokers` `Limit: 10000` silently truncating beyond that many
     brokers is acceptable. Already reviewed and accepted: round-1 F3 (this same log, "Review round 1")
     declined a fix for the identical bound on the identical helper, on the EM's ruling that it mirrors
     `ReconcileStaleBrokerQuotaReservations`'s own established bound exactly and is correct at today's
     scale, with a single-query store method tracked in ptone/scion#2314 ("hub: single-query store
     method for broker-scoped active reservations") rather than a blocker. No new disposition needed;
     restated here since GoogleCloudPlatform/scion#2141 asked about it directly.
3. **FIXED** — `brokers.ts` `renderAgentCapacity`: `broker.agentCount === undefined` →
   `broker.agentCount == null`, `broker.agentLimit !== undefined` → `broker.agentLimit != null`.
4. **FIXED** — `brokers.ts` grid stat guard: `broker.agentCount !== undefined` → `broker.agentCount !=
   null`.
5. **FIXED** — `admin-quotas.ts` usage-detail cap value: `r.brokerAgentLimit !== undefined` →
   `r.brokerAgentLimit != null`.

For 3-5: checked `shared/types.ts` — `agentLimit`/`agentCount`/`agentLimitSource` on `RuntimeBroker`, and
the local `UsageReservation.brokerAgentLimit` in `admin-quotas.ts`, are all optional (`?:`) TS properties,
never typed to allow an explicit `null`. On the Go side every one of these fields is a pointer with
`omitempty`, so the wire format omits the key entirely when nil rather than sending JSON `null` — `null`
cannot occur for these three fields today. `!= null`/`== null` is therefore behaviorally identical to
`!== undefined`/`=== undefined` for every input the API can currently produce (confirmed: both `brokers.test.ts`
and `admin-quotas.test.ts` still pass unchanged, 5/5, with no new test needed for a case that cannot
occur). The fix is accepted anyway because it is cheap, strictly more defensive, and matches an existing
convention in this codebase for the same shape of field — `diagnostics.ts:335-336`
(`health.brokerCount != null` / `health.agentCount != null`) already uses loose nullish checks for
analogous optional wire-sourced counts.

One fix commit on top of the pre-fix rebase. Gates: `go build`/`go vet ./pkg/hub/...` clean (no Go
changes — items 1-2 are declines); `golangci-lint --new-from-rev=upstream-main ./pkg/hub/...` 0 issues;
`tsc --noEmit` clean; `vitest run` on both test files, 5/5 pass; `eslint` on the three web files at the
stated 7-error/44-warning baseline (0 new — the 1 fixable/prettier hit is the pre-existing P1a issue in
`admin-quotas.ts`, unrelated). Bare-`#N` grep on the commit, this log entry and the reply draft: clean
(URLs to GoogleCloudPlatform/scion#2141 discussion threads are the only numeric refs, and are fully qualified
`GoogleCloudPlatform/scion/pull/2141#discussion_r...` URLs, not bare `#N`).

Reply drafts: `/scion-volumes/scratchpad/projects/broker-settings/reviews/upstream-replies-2141.md`.

### Review round up2141-1: APPROVE

Report: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-up2141-1.md`
(reviewed at `513ca071`). All five dispositions confirmed correct — both declines rest on the actual
store contract and a full sweep of the call sites, and the three fixes change no behavior. No Critical or
Required findings; the only problems were stale or over-narrow line citations and comment length in the
reply drafts, none of it code. Disposition:

- **R1 (fixed):** the reply-draft headers for items 3-5 claimed lines had shifted "after an intervening
  rebase" — false; all three threads were posted on the pre-fix head (`9aa4e56b`) itself, and no rebase
  happened between the review and the fix. The thread's `original_line` is simply the last line of a
  multi-line suggestion range. Reworded all three headers to state that plainly, and corrected item 3's
  "current head" lines to `:272`/`:275` (they had drifted again due to N2's comment trim, from the
  `:277`/`:280` the reviewer observed before that trim).
- **R2 (fixed):** item 2's reply text cited `project_store.go:1043-1060` for "`ListRuntimeBrokers` only
  ever returns nil alongside a non-nil error" — too narrow; that range covers only the last of six error
  returns plus the success path. Corrected to `:968-1060` (the whole function), matching what the
  decline-evidence section and this log already had right. Also corrected the log's own copy of this
  citation, which had the identical narrow range and wasn't flagged by the reviewer but had the same
  defect.
- **N1 (fixed):** "tracked as a follow-up" in item 2's reply named no issue. Asked the EM rather than
  guessing, per their instruction — confirmed as ptone/scion#2314. Cited fully qualified in the decline
  evidence and in this log's item 2 entry; the reply text itself now drops the volunteered `Limit: 10000`
  aside entirely (the bot didn't raise it, so the reply stays scoped to the thread) rather than needing
  the issue ref inline. Not added to the code comment in this commit, per the EM.
- **N2 (fixed):** the 7-line explanatory comment on `brokers.ts`'s `== null` check was trimmed to 2 lines
  stating the why (omitempty means the wire never sends `null`; the loose check tolerates one anyway).

Gates re-run after these wording-only fixes: `tsc --noEmit` clean; `vitest run` on both test files, 5/5
pass; `eslint` on the three web files at the stated 7/44 baseline (0 new); bare-`#N` grep on the commit,
this log section and the reply draft: clean (all numeric refs are fully qualified). No Go changes in this
round.
