# tz-refactor task 13 (P2c): retire the runtime-profile timezone

**Date:** 2026-10-03
**Branch:** `scion/tz-t13`
**Fork issue:** ptone/scion#2506 (part of ptone/scion#2457, design Option A, decision D3)

## What changed

- **Removed** `V1ProfileConfig.Timezone`, the `profiles` schema property,
  `SettingsOverlay.ProfileTimezone`, `Server.profileTimezone`, and the
  dispatcher's `profileTimezoneProvider` field, setter and wiring. tz-refactor
  task 16 had already moved dispatcher `TZ` injection into `resolveAgentTZ`,
  so the profile injection branch named in the task text no longer existed;
  the resolver has no profile rung (`TestResolveAgentTZ_ProfileSettingsAreNotARung`).
- **422.** `handlePutServerConfigDB` and `handlePutServerConfig` both read the
  raw body and reject any `profiles.<name>.timezone` key (any value, including
  `""` and `null`). The old IANA check for profile zones is gone.
- **Migration** (`hub.RetireProfileTimezones`), run in `initOperationalSettings`
  after Refresh and before the snapshot is applied (so before the dispatcher
  is wired):
  - DB tier (Postgres `hub_settings`): the five-case table from design §3 A.
    It writes `agent_defaults` first (copy case only), then strips
    `profiles`, through `OperationalSettings.Update` with each row's revision
    and `updatedBy = system:profile-timezone-retire`. It retries on a revision
    conflict and logs one warning per value.
  - File tier: `settings.yaml` is never rewritten. A raw map walk
    (`config.ScanSettingsFileProfileTimezones`, same parser as the loader, so
    dotted profile names work) finds the keys, and the hub logs one warning
    per value. In the copy case the warning includes the line to add.
  - Seed material: the legacy keys are deleted from the bootstrap koanf, so the
    every-boot seed sync cannot write them back into a seeded `profiles` row.
- **Web.** Deleted the "Agent timezone" section from `profile-settings.ts`.
  The page makes no admin server-config request. `profile-settings.ts` is off
  the format-scan allowlist. It was the last entry, so the allowlist is now
  empty; the list and the stale-entry check stay for tz-refactor task 21 to
  delete.
- **Docs.** `orchestrator-settings.md` drops the profile `timezone` row,
  says where the hub default lives in `settings.yaml`, and adds a "Removed:
  profile `timezone`" paragraph. `admin-settings.md` notes that runtime
  profiles no longer set a timezone.

## Deviation from the task text

The file-tier hint names the top-level `default_timezone: <zone>` line, not a
literal `agent_defaults.default_timezone: <zone>` line. `settings.yaml` stores
the hub default as the top-level `default_timezone` key, and nothing reads a
nested `agent_defaults:` block from the file, so the literal line would have
no effect. The warning names both forms.

## Tests

- New: `pkg/hub/profile_timezone_retire_test.go` (table, no-op, crash between
  writes, seeded/managed rows, file tier, 422 on both handlers for `""` and
  `Asia/Tokyo`), `pkg/config/profile_timezone_legacy_test.go`,
  `pkg/config/opsettings/profile_timezone_seed_test.go` (schema property gone,
  seed strip), `cmd/server_foreground_profile_timezone_test.go` (two boots).
- Removed the profile-timezone cases listed in the task. The listed cases in
  `hub_gcp_identity_default_settings_test.go` and
  `cmd/server_foreground_settings_init_test.go` were already gone at this base.
- Run under TZ=UTC, Asia/Tokyo and Asia/Kathmandu; see the PR for commands.

## Follow-ups

- tz-refactor task 21 deletes the (now empty) format-scan allowlist and its test.
- tz-refactor task 23 (docs) covers the full timezone order.
