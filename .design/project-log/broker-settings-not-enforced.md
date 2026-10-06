# broker-settings not_enforced wiring (ptone/scion#2061 P2, ptone/scion#2177)

PR: ptone/scion#2330 (draft), branch `scion/broker-settings-not-enforced`, branched from upstream `GoogleCloudPlatform/scion`
main (NOT stacked on the now-merged `scion/broker-settings-p2-1`). Base at branch time:
`f671d1a8`, which includes both prerequisite PRs merged upstream:
`GoogleCloudPlatform/scion#2115` (P1b, the `Server.brokerQuotasEnforced()` enforcement switch) and
`GoogleCloudPlatform/scion#2126` (P2.1, per-broker settings with `maxAgents`).
Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md` §4.5, 5.2, 5.4, 5.8
(AC-P2-6, AC-P2-10), 5.9, and **Amendment A1** (binding — see below).

## What this adds

Wires the constant `BrokerLimitSourceNotEnforced` (`pkg/hub/broker_capacity.go`), reserved but
unused since P2.1, into `effectiveBrokerLimit`. Per Amendment A1 (broker-settings-lead,
2026-09-30 13:10Z, EM's option A): when the P1b switch is off, `effectiveBrokerLimit` keeps the
**resolved value** — whichever of the broker override / entitlement binding / hub-wide default
would otherwise apply — but reports the source as `not_enforced` instead of its usual label. This
takes precedence over the broker/entitlement/hub_default labels; when `limitDef` or the quota
service is nil, the result stays `unlimited` regardless of the switch (the switch only relabels a
resolved value, it never manufactures one).

```go
func (s *Server) effectiveBrokerLimit(ctx context.Context, brokerID string, limitDef *store.LimitDefinition) (value int64, source string, err error) {
	if limitDef == nil || s.quotaService == nil {
		return 0, BrokerLimitSourceUnlimited, nil
	}
	// ... resolve value/source via the broker override, else inheritedBrokerLimit ...
	if !s.brokerQuotasEnforced() {
		return value, BrokerLimitSourceNotEnforced, nil
	}
	return value, source, nil
}
```

`Reserve` and `QuotaService.limitOverride` (`brokerSettingLimitOverride`) are **unchanged** — the
switch's effect on enforcement is entirely `QuotaService.enforced`'s concern (`quota.go`, from
P1b). This function only changes what value/source pair every read path reports; it never changes
what Reserve admits or rejects. `inheritedBrokerLimit` (the settings GET's "clear the override"
preview) is also unaffected — Amendment A1 explicitly carves it out, and it keeps reporting its
real step (broker/entitlement/hub_default) even while the switch is off, since an admin deciding
whether to clear an override needs the real fallback, not "not enforced."

Because `brokerCapacity` and `resolveBrokerCapacity` (the one read model shared by enforcement and
every read path, design §5.9) both call `effectiveBrokerLimit` under the hood, this one change
propagates to every surface automatically: the broker settings GET, the providers listing, and
(via the CLI change below) `scion hub projects info`.

## Amendment A1 mapping, per surface

1. **Broker settings GET `effective`** (`pkg/hub/broker_settings_handlers.go`,
   `buildBrokerSettingsResponse`): already built from `brokerCapacity`, so `Effective.MaxAgents`
   automatically reports `{value, source: "not_enforced"}` with the switch off — no handler change
   needed. `Effective.MaxAgents.Count` (from `CountActiveReservations`) and `.Inherited` (from
   `inheritedBrokerLimit`, unaffected) are both retained. Covered by
   `TestBrokerSettings_Get_NotEnforced`.
2. **Broker detail page** (`web/src/components/pages/broker-detail.ts`): already had a
   `not_enforced` case in `sourceLabel` ("not enforced (quota switch is off)") from P2.1, used in
   the "Effective limit: N (from ...)" line alongside the live count — verified the rendering
   already satisfies Amendment A1 (shows the value, the count, and that it's not enforced) with no
   code change required.
3. **Providers API `agentLimitSource`** (`pkg/hub/handlers_env_secrets.go`,
   `resolveBrokerCapacity`/`projectProviderView`): already a thin wrapper over `brokerCapacity`,
   so it automatically reports `not_enforced` too. Updated the `AgentLimitSource` field comment and
   `docs-site/src/content/docs/reference/api.md`'s `GET /:id/providers` entry to document the new
   value and state that `agentLimit` is informational (not enforced on create) when
   `agentLimitSource` is `not_enforced`. Covered by `TestListProjectProviders_NotEnforced`.
4. **`scion hub projects info`** (`cmd/hub.go`, `pkg/hubclient/types.go`): `hubclient.ProjectProvider`
   did not decode `agentLimitSource` at all before this change — added it (`omitempty`).
   `formatProviderCapacity` now appends `" (not enforced)"` after the count/limit when
   `AgentLimitSource == "not_enforced"`, e.g. `"7/30 (not enforced)"` (the brief's example).
   `providerCapacityIndicator` wraps it unchanged, so the labeled suffix (`" (agents: ...)"`)
   carries the note through automatically. Covered by new cases in `TestFormatProviderCapacity`
   and `TestProviderCapacityIndicator`.
5. **TS types** (`web/src/shared/types.ts`): checked for "reserved/not yet wired" wording per the
   brief — `EffectiveSetting.source`'s comment already listed `not_enforced` as a valid value since
   P2.1 (it documented the wire shape ahead of the value actually being produced), so no stale
   wording was found there and no edit was needed.
   Removed the actual stale wording, which was only on the Go side
   (`pkg/hub/broker_capacity.go`'s constants block and `effectiveBrokerLimit`'s doc comment: "is
   reserved for PR ptone/scion#2270... not yet wired").

## Surfaces NOT on this branch (P2.2 gap)

Per the brief, the brokers list "Agents / Cap" column (`web/src/components/pages/brokers.ts`) and
the admin-quotas usage detail (`web/src/components/pages/admin-quotas.ts`) do not exist yet —
confirmed via grep that neither file currently renders any per-broker `agentLimit`/`agentLimitSource`
at all. Both are P2.2 (`ptone/scion#2303`, an open PR being rebased onto upstream main in
parallel), explicitly out of scope here; this branch does not pull P2.2 in.

**Gap to route to P2.2, recorded for the EM:** once P2.2 lands, per the brief's own note, it
renders `source` in a tooltip, so a not_enforced broker would show "Source: not_enforced" there.
Whoever finishes P2.2 should confirm that tooltip text reads sensibly for `not_enforced`
specifically (Amendment A1 requires it be unambiguous that the value is not enforced, not just an
opaque enum string) — the same clarity bar this branch applied to `sourceLabel` in
`broker-detail.ts` and to the CLI's `" (not enforced)"` suffix. Not fixed here.

## Tests (AC-P2-6)

All in `pkg/hub/broker_settings_handlers_test.go` unless noted:

- `TestEffectiveBrokerLimit_NotEnforced_KeepsValueChangesSourceOnly`: exercises all three
  precedence steps (hub_default, entitlement, broker override) with the switch on (real sources),
  off (value unchanged, source becomes `not_enforced` in every case), and back on (real sources
  return immediately, no rebuild needed — matching P1b's "counting never stopped" design). Also
  pins that `inheritedBrokerLimit` keeps reporting its real step throughout.
- `TestEffectiveBrokerLimit_NotEnforced_NilLimitDefStaysUnlimited`: the explicit Amendment A1
  carve-out — nil `limitDef`/quota service stays `unlimited` regardless of the switch.
- `TestBrokerSettings_Get_NotEnforced`: the settings GET surface — value/count retained, source
  `not_enforced`, `Inherited` unaffected.
- `TestListProjectProviders_NotEnforced`: the providers-listing surface — `agentLimit` retained,
  `agentLimitSource` is `not_enforced`.
- `TestBrokerQuotaSwitch_OffAllowsOverCapWithBrokerSettingOverride` (in
  `pkg/hub/broker_settings_handlers_test.go`, alongside the existing P1b tests in
  `pkg/hub/broker_quota_enforcement_switch_test.go`): AC-P2-6's Reserve leg specifically for a
  per-broker settings override (P1b's own tests only cover the hub-default step via
  `setBrokerAgentCeiling`) — with the switch off, a broker at its per-broker cap can still
  `Reserve` (201, reservation row created), confirming Reserve/`limitOverride` are unaffected when
  the effective limit comes from a broker override specifically.
- `TestFormatProviderCapacity` / `TestProviderCapacityIndicator` (`cmd/hub_test.go`): new cases for
  the `" (not enforced)"` suffix, including the unlimited-but-not-enforced case.
- The switch being scoped to `max_agents_per_broker` only, and `max_agents_per_project` staying
  enforced with the switch off, is already covered by P1b's own
  `TestBrokerQuotaSwitch_OffProjectCapStillEnforced` — not duplicated here.
- Existing `TestEffectiveBrokerLimit_Precedence`, `TestBrokerSettings_ClearOverrideFallsBackToHubDefault`,
  and `TestBrokerSettings_EndToEndEnforcement` (all pre-existing, switch left at its unset/enforced
  default) continue to pass unchanged, confirming the switch-on path is untouched.

## Verification

- `go build ./...` — clean.
- `go vet ./pkg/hub/ ./cmd/ ./pkg/hubclient/`, plus `-tags no_sqlite` — both clean.
- `go test ./pkg/hub/... -run 'NotEnforced|BrokerQuotaSwitch|EffectiveBrokerLimit|BrokerSettings'` —
  all pass.
- `go test ./cmd/ -run 'Hub|Provider'` — all pass.
- `make test-hub-sqlite` (SCION_*/CLAUDE_* scrubbed) — full pass.
- `cd web && npx tsc --noEmit` — clean (no web files changed this branch; verification only).
- `golangci-lint run --new-from-rev=upstream-main ./cmd/... ./pkg/hub/... ./pkg/hubclient/...` — 0
  new issues.
- `hack/check-authz-guards.sh` — no violations (no new routes; this branch adds no API surface,
  only a new source label on the existing settings/providers responses).
- Bare-`#N` audit (`git log --format=%B upstream-main..HEAD | grep -nE '(^|[^/A-Za-z0-9])#[0-9]+'`)
  — prints nothing.

## Review rounds

**Round 1** (`reviews/broker-settings-rev-not-enforced-1.md`, reviewed head `6baa498e`):
REQUEST CHANGES, one Required finding; the implementation itself was confirmed correct per
Amendment A1.
- **Required — the switch-off assertions didn't match the test's own doc comment.** The doc
  comment claimed coverage of "all three precedence steps," but the switch-off section only
  asserted the broker-override and hub_default cases: by the time it ran, the one broker used for
  the entitlement check earlier in the test had already been given an override, so the entitlement
  step was never checked with the switch off, and a resolved-zero/unlimited cap was never checked
  at all. Fixed by adding two more test brokers — one carrying only an entitlement binding, one
  overridden to 0 — each checked under the switch on and off, plus a direct `brokerCapacity` check
  that a resolved value of 0 still yields `Limit == nil` while `Source == not_enforced`.
- **Nit — a confusing comment** claimed a hub-default-only broker's source became not_enforced
  "once the entitlement binding is superseded," when nothing is superseded; that broker simply
  never had a binding or an override. Reworded.
- **Nit — a CLI test case's name didn't match its input** ("empty source" but
  `AgentLimitSource: "broker"`). Renamed to match, and added a genuine empty-source case alongside
  it.
- **Optional, accepted — nested parentheses in the broker-detail rendering.** `sourceLabel`'s
  `not_enforced` text carried its own parenthetical inside the shared `"(from ...)"` wrapper every
  source uses, so the page read `"(from not enforced (quota switch is off))"`. Reworded to a plain
  string with no embedded parentheses. No vitest assertion added: there is no existing test harness
  for this card.
- **Nit — a stale precedence-step number.** `inheritedBrokerLimit`'s comment still called the
  broker override "step 2," left over from before `effectiveBrokerLimit`'s doc above it was
  renumbered to call the override "step 1" — the two comments read as contradicting each other.
  Pointed it at the override step by name instead of a number.
- Fixed in commit `19ab4f8a`, one commit, no rebase.

**Round 2** (`reviews/broker-settings-rev-not-enforced-2.md`, reviewed head `19ab4f8a`): APPROVED,
one nit — several of round 1's fixes had left comments that narrated the review itself (citing
"R1," "N3," "review round 1 on ptone/scion#2330," etc.) rather than explaining what the code does
and why, which would read oddly to an upstream reviewer with no access to this project's review
history. Removed all such narration from `broker_settings_handlers_test.go` and
`broker-detail.ts`, keeping the underlying explanation (why a separate broker is needed for the
entitlement/zero cases; why the `not_enforced` label avoids parentheses) reworded on its own terms.
Also declined, per the EM: widening the switch-back-on leg to re-check the other three brokers
(entitlement, hub_default, zero) in addition to the broker-override one it already checks — the
switch-off section already exercises every precedence outcome, and the on-leg only needs to prove
the switch itself restores the real source, which the existing single check already does.

**Round 3** (`reviews/broker-settings-rev-not-enforced-3.md`, reviewed head `7a66cca9`): APPROVE,
comment-only delta.

## Deliverables

- Fork PR ptone/scion#2330 (draft) against `ptone/scion` `main` from
  `scion/broker-settings-not-enforced`.
- This log entry.
- `/scion-volumes/scratchpad/projects/broker-settings/notes/` — no additional notes file was
  required by this brief beyond this log entry and the PR body.
- `scion message` to `broker-settings-em` with the PR number, head SHA, gate results, CI status,
  and the P2.2 gap note above.
