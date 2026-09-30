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

## Note on the upstream-main rebase step

`dev-common-rules.md` asks every branch to rebase onto `GoogleCloudPlatform/scion` `main` before
reporting ready. This branch is stacked on P2.1, which is itself still based on `ptone/scion` `main`
and not yet merged upstream — an independent rebase onto the real upstream here would rewrite P2.1's
commits inside this branch and desync it from the actual `scion/broker-settings-p2-1` branch the EM
tracks. Per the brief's explicit stacking instructions ("rebase with `--onto <new base> 0875d543`"
only when P2.1's base changes), that rebase was deliberately not run independently; flagged to the EM
in the completion message in case an upstream sync is wanted before P2.1 lands.
