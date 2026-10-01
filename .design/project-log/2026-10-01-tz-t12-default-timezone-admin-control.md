# tz-refactor task 12: admin control for `agent_defaults.default_timezone`

**Date:** 2026-10-01
**Branch:** `scion/tz-t12`
**Fork issue:** ptone/scion#2505 (part of ptone/scion#2457, design Option A, decision D3)

## Problem

Round 1 of the plan (web control only) assumed the backend half of
`agent_defaults.default_timezone` worked end to end in both settings modes,
because `admin_settings_db.go` already had storage, a live-read provider,
the resolver, and a 422 for an invalid IANA name. **Round-1 review
(`gs://scion-xproject-exchange/tz-refactor/out/t12/review-1.md`) found that
premise was wrong for file mode**, which is the default for a non-Postgres
hub: a file-mode `default_timezone` PUT persisted to `settings.yaml` and
returned 200, but the value never reached `hubAgentDefaults()` — the
accessor agent create actually reads — so the control did nothing, and
there was no server-side validation in file mode at all (an invalid name
was written to disk silently). tz-lead confirmed the backend fix is in
scope for this task, because task 16's resolver depends on
`hubAgentDefaults()` being correct in file mode.

This log now covers both rounds: the original web-only work, and round 1's
backend fix plus the rest of its findings.

## Solution

### The web control (round 0)

Added a "Default Timezone" field to the Agent Defaults card in
`web/src/components/pages/admin-server-config.ts`, next to "Default Runtime
Broker": loads/saves `default_timezone` on both the DB-mode and file-mode
PUT paths, using a new shared `<scion-timezone-picker>` component with a
"UTC" empty entry (selecting it emits `''`), and shows a non-blocking inline
hint when the typed value fails `isValidTimeZone`.

**Clearing in file mode needed one deliberate deviation** from the sibling
`default_runtime_broker`/`default_max_agent_role` fields: those use
`|| undefined` in the file-mode payload builder, which omits the key
entirely when cleared. `admin_settings.go`'s `DefaultTimezone *string` field
treats an *omitted* key as "no change" and only an *explicit* `""` as
"clear". So `default_timezone`'s file-mode payload always sends the current
value, matching the existing `default_max_turns`/etc. "send zero/empty
explicitly" precedent documented inline for ptone/scion#860. (Flagged as a
possible latent gap in the sibling fields' clear-in-file-mode behavior —
tz-em is raising that separately, along with the related finding below that
those same fields' values never reach `hubAgentDefaults()` in file mode
either. Neither is touched by this task.)

#### Shared zone helpers (built for tz-refactor task 11 to reuse)

This task has a soft dependency on tz-refactor task 11 and landed first, so
per the brief it adds the shared pieces task 11's scope describes, using the
exact names and shapes design §2.4 and the task 11 section specify, so task
11 does not need to rename anything:

- `web/src/utils/time.ts` gained three exports: `browserTimeZone()`,
  `isValidTimeZone(zone)`, and `listTimeZones()`
  (`Intl.supportedValuesOf('timeZone')` plus `'UTC'`, since this runtime's
  ICU build omits the bare `'UTC'` identifier from that list even though
  `Intl.DateTimeFormat` accepts it).
- `web/src/components/shared/timezone-picker.ts` — new component
  `<scion-timezone-picker>` (export `ScionTimezonePicker`), a local-search
  combobox over `listTimeZones()` modelled on `scion-project-picker`'s
  network-search pattern. Props: `label`, `placeholder`, `disabled`,
  `value`, and `empty-label` (unset omits the "no explicit zone" entry;
  task 11's effective-zone picker would set it to `'Auto'`, this task sets
  it to `'UTC'`). Emits `timezone-change` with `{ timezone }` (empty string
  for the empty entry).

### Round 1 review fixes

**R1-1 [High] — file-mode `default_timezone` never reached agent create.**
Fixed by adding `GlobalConfig.DefaultTimezone` (`pkg/config/hub_config.go`,
`koanf:"-"`, filled from the raw top-level `default_timezone` key in
`loadServerFromSettingsFile`, mirroring `DefaultHarnessConfig`), and setting
`snap.DefaultTimezone = gc.DefaultTimezone` in
`BuildLayer1SnapshotFromFile` (`pkg/hub/operational_settings.go`) — the
half that fed into `ApplySnapshot` was already unconditional and correct.
Three stale doc comments that enumerated the file-mode-populated
agent-defaults fields (missing `DefaultTimezone`) were corrected, including
`hub_agent_defaults.go`'s `hubAgentDefaults()` comment, which incorrectly
claimed file mode "always returns the zero value" (already false before
this PR, for `DefaultHarnessConfig` and the GCP identity fields).

**R1-2 [High] — file mode had no server-side validation.** Added
`validateDefaultTimezone` (`pkg/hub/admin_settings.go`), called from
`handlePutServerConfig` before any write with the same 422 the DB handler
already returned; the DB handler (`admin_settings_db.go`) now calls the
same helper instead of a bare `time.LoadLocation`, so the two can't drift.

**R1-2 addendum — adjacent client-side bug, fixed with tz-em's sign-off.**
Writing the web test for R1-2 surfaced a pre-existing bug: `handleSaveError`
in `admin-server-config.ts` expected error bodies shaped
`{error: "<string>", ...}`, but the real `writeError()` helper
(`pkg/hub/errors.go`) — which backs every plain field-validation 400/422 on
this page, including `default_timezone`'s, `agent_endpoint`'s and
`default_user_role`'s — sends `{error: {code, message, details}}`. Since
`body.error` is an object there, no `switch` case matched and
`body.message` was undefined (nested at `body.error.message` instead), so
the real message was silently replaced by the generic "An unexpected error
occurred" fallback. Confirmed with tz-em before fixing, since it's outside
R1-2's named scope but required for this task's own AC ("an invalid name
shows the server's 422 message") to hold end to end. Fix is additive, in
the `default` branch only: also read `body.error.message` when `body.error`
is an object.

**R1-3 [Medium] — `isValidTimeZone` accepted names the server rejects.**
`Intl.DateTimeFormat` is looser than Go's `time.LoadLocation` two ways:
case-insensitive matching (`asia/tokyo`, `utc`) and numeric offset IDs
(`+05:30`, which resolve to themselves, so a case check alone wouldn't
catch them). `isValidTimeZone` now rejects both, while still accepting a
genuine alias that resolves to a *different* string regardless of case
(`Asia/Kathmandu` → `Asia/Katmandu`). tz-em's addendum: a hub-default
denylist for `Local`/`localtime`/`posixrules`/`Factory` (same rule task 10
uses for the per-user preference) — Go's `time.LoadLocation` accepts all
four (its embedded tzdata ships those files), so the server needed an
explicit `nonPortableTimezoneNames` denylist; the client needed none, since
`Intl.DateTimeFormat` already throws for all four.

**R1-4 [Medium] — picker couldn't find current IANA names; a stored alias
looked unmatched.** `listTimeZones()`'s underlying
`Intl.supportedValuesOf('timeZone')` returns ICU's *canonical* picks
(`Asia/Katmandu`, `Asia/Calcutta`, `Europe/Kiev`, `Asia/Saigon`), not the
aliases admins are more likely to search for (`Kathmandu`, `Kolkata`,
`Kyiv`, `Ho_Chi_Minh`) — so searching those terms, or focusing a field with
a stored alias value, found nothing. Fixed in
`web/src/components/shared/timezone-picker.ts`: the candidate list always
includes the current `value` even when it's an alias missing from the
canonical list; typing a full valid alias not already offered (e.g.
`Asia/Kolkata`) surfaces it directly via `isValidTimeZone`; and a small
named `SEARCH_ALIAS_HINTS` table surfaces the canonical zone for the four
bare alternate-name searches the review called out.

**R1-5 [Low] — duplicate "UTC" rows.** `listTimeZones()` always includes
the real `"UTC"`, which collided with the admin control's
`empty-label="UTC"`, showing two identical rows. Fixed by excluding a
zone equal to `emptyLabel` from the plain list before prepending the single
`emptyLabel` row.

**R1-6 [Low] — PR body and this log overstated what already worked.**
This rewrite.

**R1-7 [Nit] — stale comment; an ineffective flag.** Reworded a
`time.test.ts` comment that claimed to bound a loop it didn't. Removed
`timezone-picker.ts`'s unconditional `this.selectedViaDropdown = false` at
the end of `willUpdate`, which ran before the parent's `.value` prop
round-tripped back down and so never actually gated anything there (it only
ever mattered for the blur handler, where it's still cleared, now only by
`handleSearchInput`/`sl-clear`).

### Note on ICU canonicalization

Node's ICU build (and browsers using the same CLDR data) returns
`"Asia/Katmandu"` (no "h") as the canonical identifier from
`Intl.supportedValuesOf('timeZone')`; `"Asia/Kathmandu"` is a valid alias
accepted by `Intl.DateTimeFormat` but is not itself in that list — likewise
`Asia/Calcutta`/`Asia/Kolkata`, `Europe/Kiev`/`Europe/Kyiv`, and
`Asia/Saigon`/`Asia/Ho_Chi_Minh`. Every alias validates via
`isValidTimeZone`; only the canonical spelling appears in `listTimeZones()`.
Documented in `time.test.ts` and exercised by the picker's R1-4 tests.

## Files changed

| File | Change |
|------|--------|
| `pkg/config/hub_config.go` | Added `GlobalConfig.DefaultTimezone`, filled from raw settings.yaml (R1-1) |
| `pkg/hub/operational_settings.go` | `BuildLayer1SnapshotFromFile` now sets `DefaultTimezone`; doc comments corrected (R1-1) |
| `pkg/hub/hub_agent_defaults.go` | Corrected `hubAgentDefaults()`'s stale file-mode doc comment (R1-1) |
| `pkg/hub/admin_settings.go` | Added `validateDefaultTimezone` + file-mode 422 call site (R1-2); fixed `handleSaveError`'s nested-error-shape bug (R1-2 addendum) |
| `pkg/hub/admin_settings_db.go` | DB-mode 422 now calls the shared `validateDefaultTimezone` (R1-2, "On Local") |
| `pkg/hub/admin_settings_test.go` | New file-mode tests: persisted+applied, clear round-trip, invalid-rejected (incl. denylist), valid-persisted (incl. aliases) |
| `pkg/hub/admin_settings_db_test.go` | New: `TestPutServerConfigDB_DefaultTimezone_NonPortableNamesRejected` |
| `pkg/hub/httpdispatcher_test.go` | New: `TestHTTPAgentDispatcher_TZInjection_HubDefault_FileMode`, the dispatcher-level R1-1 proof |
| `web/src/utils/time.ts` | Added `browserTimeZone`, `isValidTimeZone`, `listTimeZones`; `isValidTimeZone` tightened (R1-3) |
| `web/src/utils/time.test.ts` | Unit tests for the three helpers, incl. R1-3's case/offset/alias/denylist cases; R1-7 comment fix |
| `web/src/components/shared/timezone-picker.ts` | New `<scion-timezone-picker>` shared component; R1-4/R1-5/R1-7 fixes |
| `web/src/components/shared/timezone-picker.test.ts` | Unit tests for the picker, incl. R1-4/R1-5/R1-7 cases |
| `web/src/components/pages/admin-server-config.ts` | Added the Default Timezone field; fixed `handleSaveError`'s nested-error-shape bug (R1-2 addendum) |
| `web/src/components/pages/admin-server-config.test.ts` | New tests: load, DB/file-mode save (incl. explicit-`""`-on-clear), env-override read-only, inline validation hint, nested-error-shape 422 rendering |
| `.design/project-log/2026-10-01-tz-t12-default-timezone-admin-control.md` | This file (R1-6) |

## Test evidence

- `npm run typecheck` — clean.
- `npx vitest run` (full web suite) — 112 files / 3193 tests passed.
- `TZ=Asia/Kathmandu npx vitest run src/utils/time.test.ts src/components/shared/timezone-picker.test.ts src/components/pages/admin-server-config.test.ts` — 86 passed.
- `npm run build` — clean production build.
- `npx prettier --check` on all nine changed web files — clean.
- `go build -buildvcs=false -p 2 ./...` — clean. `gofmt -l .` — clean.
- `GOFLAGS="-buildvcs=false -p=2" make ci` — green, after stripping two
  leaked-from-this-container env vars unrelated to this change:
  `CLAUDE_CODE_ENABLE_TELEMETRY=1` (this agent's own harness variable) and
  the container's `SCION_*` vars. Both re-confirmed individually green with
  the leaked vars stripped; neither affected package (`pkg/harness`, `cmd`,
  `pkg/config`'s unrelated test) is touched by this change.
- `go test ./pkg/hub/... ./pkg/config/...` under both `TZ=Asia/Tokyo` and
  `TZ=Asia/Kathmandu`: all pass **except**
  `TestHTTPAgentDispatcher_TZInjection_HubDefault_FileMode` (new, R1-1) and
  the pre-existing `TestHTTPAgentDispatcher_TZInjection_HubDefault` /
  `..._Precedence_ProfileEnvTZ`, which fail under Kathmandu only, with
  exactly the documented baseline error
  (`createTestStore`'s migration: `empty agent role backfill: sql: Scan
  error on column index 6, name "create_time"`) — confirmed this is the
  same failure on `main` at e572e72 for the pre-existing sibling test,
  task #2's scope, not this task's.

## Deferred / out of scope

- The `ptone/scion#1878` file-mode stale-hint bug (resolved-settings
  endpoint showing the old `agent_defaults` until hub restart) — named in
  the task brief as out of scope.
- The sibling fields' (`default_runtime_broker`, `default_max_agent_role`,
  etc.) `|| undefined`-in-file-mode "can't clear via this UI" gap — tz-em is
  raising separately.
- The same root cause as R1-1, but for every other `agent_defaults` field
  besides `default_timezone`/`default_harness_config`/the GCP identity
  pair: `BuildLayer1SnapshotFromFile` still leaves them at zero in file
  mode. Pre-existing, tz-em is raising separately.
