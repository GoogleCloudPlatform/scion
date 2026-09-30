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

- **`getUsageSummary` (`handleAdminUsage`) and `getMyUsage` (`handleUsageMe`) were left unchanged.**
  The brief listed both as "as applicable" alongside `getUsageByLimit`. `getUsageSummary` reports one
  aggregate `activeCount` per limit definition across every scope — there is no single "broker" or
  "source" to attach at that granularity (a hub can have many brokers with different overrides under
  the same limit), so the brief's "per-broker effective limit and its source" doesn't map onto that
  response shape. It has the same `QuotaScopeSystem`-only counting gap as `getUsageByLimit` did (its
  `activeCount` for `max_agents_per_broker` is always 0, for the identical reason), but fixing that is
  an aggregate-counting bug, not a "per-broker row" concern, and the brief's own test list only
  describes per-broker source/limit assertions that `getUsageByLimit` (individual reservation rows)
  satisfies. Flagging it here as a real, separate gap for the EM/reviewers to route (possibly P2.3 or
  its own fix) rather than folding an uncalled-for aggregate-counting change into this PR.
  `getMyUsage` resolves a *user's* effective limit at system scope; `max_agents_per_broker` has no
  per-user identity to resolve against a specific broker, so there is no broker-scoped row to enrich
  there either.
- **Visibility rule for the brokers list mapped cleanly onto the providers-listing rule** (brief item
  3's "if it doesn't map cleanly, ask the EM first"): both listings already gate the entire row behind
  a read-capability/permission check before any capacity field is computed, so adding the fields
  inside that existing gate — with no new check — reproduces the providers listing's "no new read
  access" property exactly. No EM escalation was needed.

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

## Note on the upstream-main rebase step

`dev-common-rules.md` asks every branch to rebase onto `GoogleCloudPlatform/scion` `main` before
reporting ready. This branch is stacked on P2.1, which is itself still based on `ptone/scion` `main`
and not yet merged upstream — an independent rebase onto the real upstream here would rewrite P2.1's
commits inside this branch and desync it from the actual `scion/broker-settings-p2-1` branch the EM
tracks. Per the brief's explicit stacking instructions ("rebase with `--onto <new base> 0875d543`"
only when P2.1's base changes), that rebase was deliberately not run independently; flagged to the EM
in the completion message in case an upstream sync is wanted before P2.1 lands.
