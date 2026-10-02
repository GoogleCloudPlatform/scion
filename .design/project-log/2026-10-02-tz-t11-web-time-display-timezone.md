# Project Log: tz-refactor task 11 — web `time.ts`, effective zone, "Display timezone" card, 24-hour clock

**Date:** 2026-10-02
**Branch:** `scion/tz-t11`
**Fork issue:** ptone/scion#2504 (closes). Refs ptone/scion#2457. Refs ptone/scion#1056 (narrowed to its display-timezone half; never closed by this issue).
**Design:** `design.md` §2.4 ("[decided, D4] Clock and locale", "Enforcement"), §3 A (a), "Fate of the card"; decisions D1, D4; AC4, AC5, AC17 (partial).

## What changed

- **`web/src/utils/time.ts`** (the validation-gate API for tz-refactor tasks 19-22): added `FORMAT_LOCALE` ('en'), `browserTimeZone`, `isValidTimeZone`, `listTimeZones`, an effective-zone store (`setPreferredTimeZone`/`getPreferredTimeZone`/`effectiveTimeZone`/`zoneLabel`), `formatInstant` (styles `time`/`date`/`datetime`, always `hourCycle: 'h23'`), `formatRelative` (past and future, unlike the existing past-only `formatRelativeTime`, which is kept for its ~30 existing call sites), and `parseWallClock`/`toWallClockInput` for `datetime-local` inputs. `parseWallClock` resolves DST gap/overlap with Temporal's "compatible" rule (gap moves forward by the gap length; overlap takes the earlier offset), via a three-probe offset-fixpoint technique, not a one-shot guess.
- **`web/src/utils/format-number.ts`** (new): `formatNumber(n)` via `Intl.NumberFormat(FORMAT_LOCALE)`, the scan's prescribed escape hatch for the ~13 pre-existing `Number.prototype` locale-string call sites (not migrated by this issue — that's a later P3 issue per the task's scope guard).
- **`web/src/components/shared/timezone-picker.ts`** (new): `<scion-timezone-picker>`, a searchable `listTimeZones()` picker with an optional "Auto" entry, for reuse by tz-refactor task 12 (admin `default_timezone`) and task 17 (configure-page pin).
- **Effective-zone wiring**: `User.preferences.timezone` (`web/src/shared/types.ts`); `main.ts` sets the store from `/auth/me` on load (and, when SSR already supplied the user without `preferences` — deliberately never session-cached — refreshes it live, non-blocking).
- **"Display timezone" card** on `profile-settings.ts`: Auto + searchable zone list, visible to every signed-in user, PATCHing `preferences.timezone` with a per-key merge and updating the store immediately (no reload). The pre-existing "Agent timezone" section is left in place (tz-refactor task 13 deletes it).
- **Migrated to `time.ts`** (native chat, now fully clean of banned tokens): `chat-message.ts`, `chat-date-divider.ts`, `chat-interagent-marker.ts`, `chat-system-line.ts`.
- **Migrated the zone-interpretation bug** in the three `datetime-local` inputs, each now labelled "Times in: `<zone>`": `access-boundary-schedule-editor.ts` (also: its `viewerTimeZone` now reads the effective zone instead of a raw `Intl.DateTimeFormat` call — now fully clean), `scheduled-event-list.ts` and `admin-role-bindings.ts` (both keep their own private relative-time formatters, out of this issue's scope, and so stay on the allowlist).
- **Format scan**: `web/src/utils/format-scan.test.ts`, a vitest source scan (CI runs `npm test`, not `npm run lint`) banning `toLocaleString(`/`toLocaleDateString(`/`toLocaleTimeString(`/`Intl.DateTimeFormat`/`Intl.RelativeTimeFormat`/`hour12` outside `time.ts`, with a file-granular allowlist (42 files, generated at this SHA) and a stale-entry check (fails if a listed file no longer matches).
- **`web/vitest.config.ts`**: pinned `TZ=UTC` for the test process, so ambient-TZ differences can't make tests flaky; `time.ts` tests pass zones explicitly instead.

## Overlap with tz-refactor task 12 (ptone/scion#2533)

`browserTimeZone`, `isValidTimeZone` and `listTimeZones` in `web/src/utils/time.ts` are duplicated, with identical names, signatures and semantics (including the IANA-shape check), in task 12's branch (`scion/tz-t12`). Per the brief, this duplication was judged small (three pure functions, not the whole timezone-picker), so task 11 proceeded rather than waiting on task 12. Whichever of the two PRs merges second should drop its copy of these three functions in favour of the other's — a trivial conflict, called out in both PR bodies.

## Test evidence

- `npm run typecheck`: clean.
- `npx vitest run` (full suite): **122 files / 3483 tests passed**, both at the ambient (ran via `npm test`, which inherits the shell's `TZ`) and explicitly under `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu` — identical pass counts under all three, confirming the `vitest.config.ts` pin holds regardless of ambient `TZ`.
- `npm run build`: clean.
- No Go files touched; `golangci-lint`/`go test` not applicable to this issue.
- New/changed test files: `utils/time.test.ts` (zone helpers, effective-zone store, `formatInstant` incl. midnight-as-00:00, `formatRelative` past/future, `parseWallClock`/`toWallClockInput` round trip, explicit DST gap/overlap cases on `America/New_York` 2026-03-08 and 2026-11-01, and explicit Asia/Tokyo and Asia/Kathmandu (+05:45) preference-zone cases), `utils/format-number.test.ts`, `utils/format-scan.test.ts`, `components/shared/timezone-picker.test.ts`, and additions to `components/pages/profile-settings.test.ts` for the new card.

## Follow-ups (not fixed here, flagged for the human)

- `scheduled-event-list.ts` and `admin-role-bindings.ts` each still carry a private, file-local past/future relative-time formatter duplicating `Intl.RelativeTimeFormat` logic now available as `formatRelative` in `time.ts`. Out of this issue's explicit scope (brief's "Scope guard"), but a natural target for the P3 issue that migrates their files off the format-scan allowlist.
- `components/shared/role-binding-utils.ts`'s `formatDateTime` (12-hour, `toLocaleString`) is a second, unrelated absolute-time formatter used by `admin-role-bindings.ts`; also out of scope here, also allowlisted.
