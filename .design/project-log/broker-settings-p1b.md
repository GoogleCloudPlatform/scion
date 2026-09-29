# broker-settings P1b: broker-quota enforcement toggle (ptone/scion#2061)

Base: upstream `GoogleCloudPlatform/scion` main `e1f682eac7d774414cdc50a7f1fa9009f3968596`.
PR: ptone/scion#2270, branch `scion/broker-settings-p1b`, head `7dacb868912f5628b772d20beb12a82d376eca93`.
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

- `go build ./...`, `go vet ./...`: clean.
- `golangci-lint run --new-from-rev=main ./...`: 0 issues.
- `make test-hub-sqlite`: full `pkg/hub` suite passes (828s solo run). A first
  concurrent run (alongside `test-fast` and `golangci-lint` in the same
  sandbox) showed one `pkg/hub` failure; a clean rerun in isolation passed,
  consistent with resource-contention flakiness from three heavy suites
  running at once rather than a real regression — not reproduced.
- `make test-fast` (no_sqlite): one pre-existing, unrelated failure,
  `TestNativeTelemetryPolicyEffectiveChildEnv` in `pkg/sciontool/supervisor`
  (an env-var conflict with this sandbox's `CLAUDE_CODE_ENABLE_TELEMETRY`),
  reproduced identically by stashing this branch's changes and rerunning
  against the unmodified base — not caused by this change.
- `npx tsc --noEmit` (`web/`): clean.

## Scope discipline

Did not touch `pkg/hub/seed.go`, `updateLimitDefinition`
(`pkg/hub/handlers_quota.go`), or `web/src/components/pages/admin-quotas.ts`
(P1a's exclusive files, developed in parallel). No new routes, permissions, or
authz catalog entries were added (design P1-D4: reuses `hub.config.update`).
