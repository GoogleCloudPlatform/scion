# broker-settings P1b: broker-quota enforcement toggle (ptone/scion#2061)

PR: ptone/scion#2270, branch `scion/broker-settings-p1b`.
Base (as of the round-4 fixes below): upstream `GoogleCloudPlatform/scion` main
`e56b87b106269931f1b0ff126eb16a68f8264c61` (rebased three times during review
to pick up GoogleCloudPlatform/scion#2101, GoogleCloudPlatform/scion#2103 and
GoogleCloudPlatform/scion#2105; see "Review rounds" below for the history of
heads and bases).
Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md` §4.4, 4.5, 4.7 (P1b), 4.8 (AC4, AC5).

## What this adds

A new Layer-1 opsettings section `quotas` with one key, `enforce_broker_quotas`
(bool, default `true`), following the exact shape of the existing
`auto_expose_ports` section: `pkg/config/opsettings/{sections,registry,koanf}.go`,
the `VersionedSettings.Quotas` file-mode field in `pkg/config/settings_v1.go`,
and the `GlobalConfig.EnforceBrokerQuotas` top-level-key parsing in
`pkg/config/hub_config.go`'s `loadServerFromSettingsFile` (mirroring
`project_defaults`).

`pkg/hub/operational_settings.go` carries it through `Layer1Snapshot`,
`buildSnapshotFromKoanf` (postgres), `BuildLayer1SnapshotFromFile` (file mode),
and `ApplySnapshot`. `pkg/hub/server.go` adds `ServerConfig.EnforceBrokerQuotas`
and `(*Server).brokerQuotasEnforced()` (nil → enforced, fail-safe default), and
wires `QuotaService.enforced` at construction:
`limitName != store.LimitMaxAgentsPerBroker || s.brokerQuotasEnforced()`.

`pkg/hub/quota.go`: `QuotaService` gains the `enforced func(limitName string) bool`
field (nil-safe via `isEnforced`, defaulting to enforced). In `Reserve`: when
`count >= effectiveLimit` and the limit is not enforced, it logs at DEBUG and
creates the reservation anyway (`created=true`) instead of returning
`ErrQuotaExceeded`; on lock contention while not enforced, it returns
`(false, nil)` instead of `ErrQuotaLockContention`. Everything else — counting,
idempotency, release, reconcile, backfill — is unchanged. Because every
enforcement site (create, lifecycle start/restart/resume, DM wake in
`wake_dm.go`) calls `Reserve`, the switch applies everywhere with no per-site
code, matching design P1-D5's rationale for "keep counting."

The admin UI (`web/src/components/pages/admin-server-config.ts`) gets a new
"Quotas" card with an `<sl-switch>` labelled "Enforce broker agent quotas",
the exact help text from the brief, and a link to Admin → Quotas. Wired into
the type, label map, load, and both DB-mode/file-mode payload builders, the
same way `auto_expose_ports.enabled` is.

`pkg/hub/admin_settings.go` and `admin_settings_db.go` needed the same
`Quotas`/`QuotaSettings` field added to `ServerConfigResponse` and
`ServerConfigUpdateRequest`, and a `"quotas"` case in `extractKoanfKeysFromRequest`
/ `buildSingleSectionDoc` — these files are not named in the brief's file list,
but are the necessary counterpart of the `admin-server-config.ts` UI wiring:
the generic `/admin/server-config` endpoint is not a fully reflective
pass-through, every section has hand-written Go-side plumbing for its typed
field, same as `auto_expose_ports` and `project_defaults` before it.

## A file-mode gap found and worked around (not fixed, out of scope)

`BuildLayer1SnapshotFromFile` intentionally does *not* populate
`AutoExposePortsEnabled` from `GlobalConfig` (see its own comment), which means
`auto_expose_ports` may not actually take live effect on a file-mode reload
today. For `quotas`, design explicitly requires "takes effect without restart"
in file mode (test 8), so `BuildLayer1SnapshotFromFile` **does** copy
`gc.EnforceBrokerQuotas` — unlike its `auto_expose_ports` sibling.

Separately, `loadServerFromSettingsFile` (and therefore the live-reload path
`LoadGlobalConfig` → `loadGlobalConfigFromSettings`) only parses a
`settings.yaml` as "versioned settings" if it has a top-level `server:` key;
otherwise it silently falls through to the legacy `server.yaml` path, which
never touches `quotas` (or `project_defaults`, or any other top-level Layer-1
key) at all. This is pre-existing and affects every such key equally, not just
quotas. A real deployed hub always has a `server` section, so this only
surfaces in a test that PUTs `quotas` alone against an empty settings.yaml.
The test in `admin_settings_test.go` includes a minimal `server.hub.port` in
its PUT body to reflect a realistic settings.yaml. Flagged here rather than
fixed, since fixing the general gap is shared infrastructure outside this
issue's scope — mentioned to `broker-settings-em` if it becomes relevant to
another phase.

## Tests (design 4.7 P1b, items 1-9)

All nine covered:
1. switch unset → enforced: `TestBrokerQuotaSwitch_UnsetIsEnforced`.
2. switch off, over-cap create → 201 and a reservation row exists:
   `TestBrokerQuotaSwitch_OffAllowsOverCapAndCounts`.
3. switch off then on, next over-cap create → 429 immediately:
   `TestBrokerQuotaSwitch_OffThenOnRejectsImmediately`.
4. switch off + lock contention → proceeds:
   `TestBrokerQuotaSwitch_OffLockContentionProceeds` (direct `QuotaService`
   test using the existing `lockingStoreWrapper` pattern).
5. DM wake over cap with switch off succeeds:
   `TestBrokerQuotaSwitch_OffAllowsWakeOverCap` (mirrors the existing
   `TestBrokerQuota_WakeAtCapRejected` fixture).
6. `max_agents_per_project` still enforced with switch off:
   `TestBrokerQuotaSwitch_OffProjectCapStillEnforced`.
7. opsettings validation rejects non-boolean: extended
   `TestValidateInvalidDoc` (opsettings) plus
   `TestPutServerConfigDB_Quotas_NonBooleanRejected` (HTTP level, rejected at
   JSON decode since the field is `*bool`).
8. file mode PUT writes settings.yaml and takes effect without restart:
   `TestHandlePutServerConfig_EnforceBrokerQuotas_PersistedAndAppliedWithoutRestart`.
9. `tsc --noEmit` passes (`web/`).

All new tests live in `pkg/hub/broker_quota_enforcement_switch_test.go`
(`//go:build !no_sqlite`), plus additions to `pkg/hub/admin_settings_test.go`,
`pkg/hub/admin_settings_db_test.go`, `pkg/config/opsettings/opsettings_test.go`,
and `pkg/config/hub_config_test.go`.

## Verification

As of the round-2 fixes (base `1526431232616f0b`):

- `go build ./...`, `go vet ./...`, `go vet -tags no_sqlite ./...`: clean.
- `golangci-lint run --new-from-rev=<base> ./...`: 0 issues.
- `go test -race ./pkg/hub/ -run 'TestBrokerQuotaSwitch|TestBrokerQuotasEnforced|Quotas|EnforceBrokerQuotas|TestResetSection|TestExtractKoanfKeys'`:
  all pass, race-clean.
- `make test-hub-sqlite`: `pkg/hub` itself passes cleanly (no `-skip` needed
  for anything in this feature). The only failure anywhere in the module is
  `pkg/hub/authzop.TestMutationClassificationBidirectional`, pre-existing on
  the base and tracked upstream as GoogleCloudPlatform/scion#2105 — not
  caused by this change, confirmed by reproducing it with this branch's
  changes stashed out.
- `make test-fast` (no_sqlite): same authzop failure as above; with a
  scrubbed environment (`env -i PATH HOME GO*`) no other package fails —
  ambient `SCION_*`/`CLAUDE_*` env vars in this sandbox cause unrelated
  failures in unscrubbed runs (`pkg/config`, `pkg/sciontool/supervisor`,
  etc.), not this PR.
- `npx tsc --noEmit` (`web/`): clean.

Two intermediate CI blockers surfaced and were resolved by rebasing during
review, both pre-existing on upstream main and unrelated to this PR's diff:
`roundTripFunc` undefined under `no_sqlite` (fixed by
GoogleCloudPlatform/scion#2101) and a stale test premise in
`TestEvaluateSAAssignment_CeilingDelegatorLacksPermission` (fixed by
GoogleCloudPlatform/scion#2103).

## Review rounds

**Round 1** (reviewer `broker-settings-rev-p1b-1`, head `b0e59348`, base upstream
`e1f682ea`): REQUEST CHANGES. Findings F1-F5, all addressed at head
`6e0a8017` (base rebased onto upstream `1526431232616f0b`, which brought in
GoogleCloudPlatform/scion#2101 and GoogleCloudPlatform/scion#2103, both needed to unblock CI on
unrelated pre-existing breaks):

- **F1 (required, data race).** `brokerQuotasEnforced()` read
  `s.config.EnforceBrokerQuotas` with no lock while `ApplySnapshot` writes it
  under `s.mu.Lock()`. Fixed by reading under `s.mu.RLock()`, matching the
  project convention (`defaultProjectSharedDirs`, `hubAutoExposeDefault`).
  Added a concurrent regression test that fails under `-race` without the fix.
- **F2 (required, auto-close).** The PR body's "Closes ptone/scion#2061"
  would have auto-closed the tracking issue on merge, while P1a and P2 are
  still open. Changed to "Part of ptone/scion#2061".
- **F3 (required, fail-open on clear).** `ApplySnapshot` skipped the
  assignment when the snapshot's `EnforceBrokerQuotas` was nil, treating nil
  as "no change" like every sibling `*bool` field. But for this key nil is a
  real, meaningful value — the fail-safe "enforced" default — so DELETE
  `/sections/quotas` or `PUT {"quotas":{}}` left the live hub fail-open
  (still `false` in memory) while GET and the UI both reported "enforced".
  Fixed by assigning unconditionally, with a `boolPtrEqual` helper for
  correct applied-change tracking. This is the one place in this feature
  where nil isn't "leave it alone" — worth remembering if a future *bool
  Layer-1 setting needs the same fail-safe-on-clear semantics.
- **F4 (optional).** The DB-mode test asserted only the snapshot/GET, not
  the live `brokerQuotasEnforced()` value, because `ops.server` was never
  wired in the test harness (so `Update()`'s self-apply was a no-op). Wired
  it and asserted the live value; added a simulated cross-replica test (two
  `OperationalSettings` sharing one fake store, one calling
  `refreshAndApply` — the same call the LISTEN/NOTIFY subscription and the
  60s poll backstop make) since no live Postgres was available to test AC5
  directly.
- **F5 (optional).** Added an end-to-end lock-contention variant through
  the real server wiring (`srv.quotaService`, the real
  `store.LimitMaxAgentsPerBroker` limit, a real HTTP create), alongside the
  original direct `QuotaService` unit test.

**Round 2** (reviewer `broker-settings-rev-p1b-2`, fresh/independent, head
`6e0a8017`): APPROVE. Verified F1-F5 by mutation-testing each fix (reverting
it and confirming the corresponding regression test fails) — all five held.
Three optional nits, all addressed:

- **N1.** The PR body's CI paragraph blamed the wrong (already-fixed) test.
  Updated to name the actual current red check
  (`authzop.TestMutationClassificationBidirectional`, pre-existing on
  upstream main, tracked as GoogleCloudPlatform/scion#2105) instead of the
  SA-assignment-ceiling test that GoogleCloudPlatform/scion#2103 had already
  fixed by that point.
- **N2.** This project log (you're reading the fix).
- **N3.** Added an explicit DB-mode end-to-end test for
  `PUT {"quotas":{}}` (as opposed to DELETE `/sections/quotas`) resetting
  the live `brokerQuotasEnforced()` value back to `true` —
  `TestPutServerConfigDB_Quotas_EmptyPutResetsEnforcementToTrue`.

Both reviews independently confirmed: `pkg/hub` itself is green in
`make test-hub-sqlite`; every CI failure encountered throughout review
(`roundTripFunc` under `no_sqlite`, the SA-assignment-ceiling test, and
`authzop.TestMutationClassificationBidirectional`) was a pre-existing break
on `GoogleCloudPlatform/scion main` / `ptone/scion main`, unrelated to this
PR's diff, and reproduced identically with this branch's changes stashed out.

**Round 3** (reviewer `broker-settings-rev-p1b-3`, fresh/independent, head
`e77b8245`): REQUEST CHANGES, one required, one-line doc fix:

- **R1 (required).** Two bare `#2103` issue refs in this log (this file,
  base section and the round-2 paragraph above) resolved to
  ptone/scion#2103, an unrelated closed issue, instead of
  GoogleCloudPlatform/scion#2103 — a violation of the project's
  fully-qualified-refs rule. Fixed by qualifying both.
- **O1 (optional, but treated as in scope by the EM per design 4.4's
  "settings-v1 schema" bullet).** `pkg/config/schemas/settings-v1.schema.json`
  had no `quotas` entry, and its root `additionalProperties: false` rejected
  a `settings.yaml` as soon as an admin saved the switch in file mode, so
  `scion config validate` reported a hub-loadable file as invalid. Added a
  `quotas` object (`enforce_broker_quotas: boolean`, `additionalProperties:
  false`, `x-scope: global`, matching the `server` object's "global-only"
  pattern) and three `ValidateSettings` tests (valid, non-boolean rejected,
  unknown nested field rejected). The identical, pre-existing gap for
  `auto_expose_ports` and `project_defaults` is deliberately left alone —
  the EM will track it as a separate follow-up, not this PR's scope.
- **O2 (optional).** Refreshed the PR body's CI paragraph again — see
  round 4 below for the version current as of that rebase.
- **FYI-2 (disclosed, not fixed).** In file mode, a `settings.yaml` with no
  top-level `server:` key (e.g. a fresh `embeds/default_settings.yaml`) never
  reaches the top-level `quotas` parse in `loadServerFromSettingsFile`, so a
  raw-API `PUT` of the switch against such a file returns 200 and GET/the UI
  show it set, but the hub stays enforced. This fails in the safe direction,
  is pre-existing (shared by `telemetry` and `default_scratchpad`), and the
  web UI's file-mode save always sends `server` alongside `quotas`, so the UI
  itself can never trigger it. Noted in the PR body as a known limitation.

**Round 4** (reviewer `broker-settings-rev-p1b-4`, fresh/independent, head
`efa6398b`): REQUEST CHANGES, one required, metadata-only fix:

- **F1 (required).** The round-3 fix commit's own message repeated the bare
  `#2103` problem it was fixing, in its subject and body — not the file
  content (already correct), the message text. This mattered more than a
  doc typo: ptone/scion squash-merges using commit messages
  (`squash_merge_commit_message = COMMIT_MESSAGES`), so an unqualified ref in
  any commit message lands in `main`'s history and would have linked to the
  same wrong ptone/scion#2103 issue on merge. Fixed by amending that commit's
  message (not its content) to qualify both refs, and by auditing every
  commit on the branch with
  `git log --format=%B upstream-main..HEAD | grep -nE '(^|[^/A-Za-z0-9])#[0-9]+'`,
  which now returns nothing.
- **O1 (optional).** This round-3/round-4 write-up (the paragraphs you're
  reading) closes the "no round-3 entry" gap the reviewer noted.

At the same time as the round-4 fix, rebased onto a fresh upstream main pull
to pick up GoogleCloudPlatform/scion#2105 (merged: the `authzop`
`TestMutationClassificationBidirectional` failure that had recurred in every
round through round 4 is now resolved upstream). The one remaining
pre-existing, unrelated CI break at this point is GoogleCloudPlatform/scion#2088's
`authzTestSetup` `no_sqlite` vet failure, which every round from 3 onward
found and disclosed in the PR body.

## Scope discipline

Did not touch `pkg/hub/seed.go`, `updateLimitDefinition`
(`pkg/hub/handlers_quota.go`), or `web/src/components/pages/admin-quotas.ts`
(P1a's exclusive files, developed in parallel). No new routes, permissions, or
authz catalog entries were added (design P1-D4: reuses `hub.config.update`).
