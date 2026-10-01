# tz-refactor task 12: admin control for `agent_defaults.default_timezone`

**Date:** 2026-10-01
**Branch:** `scion/tz-t12`
**Fork issue:** ptone/scion#2505 (part of ptone/scion#2457, design Option A, decision D3)

## Problem

The hub already has a fully working `agent_defaults.default_timezone` setting
end to end on the backend (storage, live-read provider, the resolver, and a
server-side 422 for an invalid IANA name), but the web admin UI never grew a
form control for it. Admins could only set it by hand-editing
`settings.yaml` or writing directly to the settings DB.

## Solution

Added a "Default Timezone" field to the Agent Defaults card in
`web/src/components/pages/admin-server-config.ts`, next to "Default Runtime
Broker":

- loads/saves `default_timezone` on both the DB-mode and file-mode PUT paths;
- uses a new shared `<scion-timezone-picker>` component with an "UTC" empty
  entry (selecting it emits `''`, which the backend treats as "clear/use
  UTC");
- shows an inline message (not a save-blocker) when the typed value fails
  `isValidTimeZone`, so the authoritative rejection is still the server's
  422 (`admin_settings_db.go`'s existing IANA check), per tz-lead's wording
  on ptone/scion#1878 (the project-page inherited-value hint going stale in
  file mode until restart is a separate, out-of-scope bug; agent creation
  itself reads the live value in both modes).

**Clearing in file mode needed one deliberate deviation** from the sibling
`default_runtime_broker`/`default_max_agent_role` fields: those use
`|| undefined` in the file-mode payload builder, which omits the key
entirely when cleared. `admin_settings.go`'s `DefaultTimezone *string` field
treats an *omitted* key as "no change" and only an *explicit* `""` as
"clear". So `default_timezone`'s file-mode payload always sends the current
value, matching the existing `default_max_turns`/etc. "send zero/empty
explicitly" precedent documented inline for ptone/scion#860. (Flagging this
as a possible latent gap in the sibling fields' clear-in-file-mode behavior
for tz-em/a future issue — out of scope here.)

### Shared zone helpers (built for tz-refactor task 11 to reuse)

This task has a soft dependency on tz-refactor task 11 and landed first, so
per the brief it adds the shared pieces task 11's scope describes, using the
exact names and shapes design §2.4 and the task 11 section specify, so task
11 does not need to rename anything:

- `web/src/utils/time.ts` gained three exports: `browserTimeZone()`,
  `isValidTimeZone(zone)` (wraps `new Intl.DateTimeFormat('en', {timeZone})`
  in try/catch — the only place in the web app allowed to do that directly,
  per the eventual format-scan test design §2.4 describes), and
  `listTimeZones()` (`Intl.supportedValuesOf('timeZone')` plus `'UTC'`,
  since this runtime's ICU build omits the bare `'UTC'` identifier from that
  list even though `Intl.DateTimeFormat` accepts it).
- `web/src/components/shared/timezone-picker.ts` — new component
  `<scion-timezone-picker>` (export `ScionTimezonePicker`), a local-search
  combobox over `listTimeZones()` modelled on `scion-project-picker`'s
  network-search pattern. Props: `label`, `placeholder`, `disabled`,
  `value`, and `empty-label` (unset omits the "no explicit zone" entry;
  task 11's effective-zone picker would set it to `'Auto'`, this task sets
  it to `'UTC'`). Emits `timezone-change` with `{ timezone }` (empty string
  for the empty entry).

### Note on ICU canonicalization

Node's ICU build (and apparently browsers using the same CLDR data) returns
`"Asia/Katmandu"` (no "h") as the canonical identifier from
`Intl.supportedValuesOf('timeZone')`; `"Asia/Kathmandu"` is a valid alias
accepted by `Intl.DateTimeFormat` but is not itself in that list. Both
spellings validate via `isValidTimeZone`; only the canonical one appears in
`listTimeZones()`. Documented in `time.test.ts`.

## Files changed

| File | Change |
|------|--------|
| `web/src/utils/time.ts` | Added `browserTimeZone`, `isValidTimeZone`, `listTimeZones` |
| `web/src/utils/time.test.ts` | New: unit tests for the three helpers above |
| `web/src/components/shared/timezone-picker.ts` | New: `<scion-timezone-picker>` shared component |
| `web/src/components/shared/timezone-picker.test.ts` | New: unit tests for the picker |
| `web/src/components/pages/admin-server-config.ts` | Added the Default Timezone field (load, save both modes, render, client-side validation hint) |
| `web/src/components/pages/admin-server-config.test.ts` | New tests: load, DB-mode save, file-mode save (incl. the explicit-`""`-on-clear regression guard), env-override read-only rendering, inline validation message |

## Test evidence

- `npm run typecheck` — clean.
- `npx vitest run` (full web suite) — 112 files / 3183 tests passed.
- `npm run build` — clean production build.
- `GOFLAGS="-buildvcs=false -p=2" make ci` — green, after stripping two
  leaked-from-this-container env vars that are unrelated to this change:
  `CLAUDE_CODE_ENABLE_TELEMETRY=1` (this agent's own harness variable,
  which broke `pkg/harness`'s `TestNativeTelemetryProvisionedChildEnv`) and
  the container's `SCION_*` vars (which broke `cmd`'s
  `TestHubAllOrOneActions`/`TestReincarnateHandoffTemplate_WorksAnywhere`
  and `pkg/config`'s `TestLoadSettingsKoanfV1LegacyEnvNeverAdopted`). All
  three were re-confirmed green individually with the leaked vars stripped;
  none of the affected packages were touched by this change.
- No Go files were touched (the backend half of `default_timezone` already
  existed), so the dev-common both-TZ (`Asia/Tokyo` / `Asia/Kathmandu`)
  store/wire-boundary requirement does not apply to this task; the one
  TZ-shaped thing added, `isValidTimeZone`/`listTimeZones`, is covered
  directly by `time.test.ts`'s Kathmandu/Katmandu cases instead of a TZ-env
  test run.

## Deferred / out of scope

- The `ptone/scion#1878` file-mode stale-hint bug (resolved-settings
  endpoint showing the old `agent_defaults` until hub restart) — named in
  the task brief as out of scope.
- The possible `default_runtime_broker`/`default_max_agent_role` file-mode
  "can't clear via the UI" gap noted above — not this task's field, flagged
  for tz-em.
