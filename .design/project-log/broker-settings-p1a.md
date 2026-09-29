# broker-settings P1a: seed max_agents_per_broker at 100, admin PUT for system limits

Tracking: `ptone/scion#2061` (design AGREED rev 2, `.design` on the scratchpad volume,
not this repo). Fixes item 1 of `ptone/scion#2063`. PR: `ptone/scion#2268`
(branch `scion/broker-settings-p1a`, head `2b93d14f2`).

## What changed

- `pkg/hub/seed.go`: the `max_agents_per_broker` seed default moves from 12 to
  100, per ptone's 2026-09-29 ruling to keep one global default for now (no
  per-broker tuning until P2 / `ptone/scion#2177`). The comment above
  `seedLimitDefinitions` was rewritten to state the ruling, the Cloud Run
  single-node crash risk (100 is above the observed ~17-18 agent idle ceiling
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

## Coordination

The design's coordination note calls out PR `ptone/scion#2168`
(`TestListProjectProviders_AgentLimitDefaultNoBindings`, small-issues-lead-2)
as a test that reads the seeded value rather than hard-coding 12, so it and
this PR can land in either order. As of this branch (rebased onto upstream
`e1f682eac`), that PR has not merged into `ptone/scion` — there is no
`resolveBrokerCapacity` or `TestListProjectProviders_AgentLimitDefaultNoBindings`
in this tree yet, and a repo-wide grep found no other test hard-coding 12 for
this seed value (`pkg/hub/agent_ceiling_gate_test.go`'s reference was a
comment only, now corrected). No further action was needed here; if
`ptone/scion#2168` lands with the hard-coded value still in place, whichever
PR merges second should update the assertion to read `DefaultValue` from the
seeded definition, per the design.

## Verification

- `go build ./...` — clean.
- `go test -tags no_sqlite ./pkg/hub/...` (targeted) and `make test-hub-sqlite`
  (covers the `//go:build !no_sqlite` quota/broker-quota suite) — pass.
- `make test-fast` — the two failing packages (`pkg/runtimebroker`,
  `pkg/sciontool/supervisor`) are pre-existing and environment-induced (the
  supervisor failure is a literal conflict with this container's own
  `CLAUDE_CODE_ENABLE_TELEMETRY=1`); confirmed identical failures on a clean
  `upstream-main` worktree with no broker-settings changes applied, so
  neither is attributable to this PR.
- `golangci-lint run --new-from-rev=upstream-main ./pkg/hub/...` — 0 issues
  (originally flagged 2 unchecked `s.Close()` errors in the new test file,
  fixed).
- `go vet -tags no_sqlite ./...` (`make lint`) — clean.
- `cd web && npx tsc --noEmit` — clean. No `web/src/shared/types.ts` changes
  were needed: `LimitDefinition.system` is a local interface field in
  `admin-quotas.ts` already present before this change, and the JSON shape
  of the PUT request/response is unchanged.

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
