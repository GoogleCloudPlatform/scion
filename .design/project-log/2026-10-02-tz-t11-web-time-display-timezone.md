# Project Log: tz-refactor task 11 — web `time.ts`, effective zone, "Display timezone" card, 24-hour clock

**Date:** 2026-10-02 (updated after review round 1)
**Branch:** `scion/tz-t11`, rebased onto `scion/tz-t12` (ptone/scion#2533) at `aa4bfc1f`
**Fork issue:** ptone/scion#2504 (closes). Refs ptone/scion#2457. Refs ptone/scion#1056 (narrowed to its display-timezone half; never closed by this issue).
**Design:** `design.md` §2.4 ("[decided, D4] Clock and locale", "Enforcement"), §3 A (a), "Fate of the card"; decisions D1, D4; AC4, AC5, AC17 (partial).

## Sequencing: task 11 now depends on task 12 (merge order)

**Depends on PR ptone/scion#2533 (tz-refactor task 12); that PR merges first.**

Review round 1 (R1-2) found that task 11's original, independently-judged-small duplication of `browserTimeZone`/`isValidTimeZone`/`listTimeZones` understated the real overlap: both branches add a `<scion-timezone-picker>` at the same file path and custom-element tag, with **incompatible APIs** (`zone-change`/`{value}` vs task 12's `timezone-change`/`{timezone}`, `allow-auto`+`auto-label` vs `empty-label`). That's an add/add conflict neither `tsc` nor the build would catch if resolved wrong. tz-em decided task 11 sequences after task 12 instead of the two proceeding independently:

- `scion/tz-t11` was rebased onto `origin/scion/tz-t12` at `aa4bfc1f5fddbaeb128a941415f44308ccc525cf`.
- Task 11's own `timezone-picker.ts`/`timezone-picker.test.ts` were deleted; the Display timezone card now uses task 12's picker (`empty-label="Auto"`, listens for `timezone-change`, reads `e.detail.timezone`).
- Task 11's duplicate `browserTimeZone`/`isValidTimeZone`/`listTimeZones` (and their test coverage) were dropped from `time.ts`/`time.test.ts` in favour of task 12's, which are now the single source — any future change to those three belongs in task 12's lineage.
- A corrected PR body leads with the dependency line above, replacing the earlier (inaccurate) "Overlap with tz-refactor task 12" section, which understated the duplication as limited to three helper functions rather than including the picker itself.
- If task 12 moves again before merge, task 11 rebases again: `git rebase --onto origin/scion/tz-t12 <new-sha>` and force-pushes only `scion/tz-t11` with `--force-with-lease`.

## What changed

- **`web/src/utils/time.ts`** (the validation-gate API for tz-refactor tasks 19-22): `FORMAT_LOCALE` ('en'), an effective-zone store (`setPreferredTimeZone`/`getPreferredTimeZone`/`effectiveTimeZone`/`zoneLabel`, now dispatching `DISPLAY_TIMEZONE_CHANGED_EVENT` on an actual change — review R1-4), `formatInstant` (styles `time`/`time-seconds`/`date`/`datetime`/`datetime-full`, always `hourCycle: 'h23'`), `formatRelative` (past and future; the existing past-only `formatRelativeTime` is kept for its ~30 call sites), and `parseWallClock`/`toWallClockInput` for `datetime-local` inputs. `browserTimeZone`/`isValidTimeZone`/`listTimeZones` are task 12's (see sequencing above).
- **`parseWallClock`'s DST resolution (review R1-1, a High finding):** the original single-seeded-probe algorithm returned the *later* instant in a DST overlap for every zone ahead of UTC (Europe, Australia, NZ) — correct only for zones behind UTC by accident. Replaced with a two-probe (a day before/after the wall-clock instant) bracket-and-validate approach: form a UTC candidate from each bracketing offset, keep whichever reproduces itself, and return the earlier when both do (overlap) or shift forward by the gap when neither does (gap). Also guards `isValidTimeZone(zone)` and rejects out-of-range fields and the two-digit-year `Date.UTC` remap (review R1-7).
- **`web/src/utils/format-number.ts`** (new): `formatNumber(n)` via `Intl.NumberFormat(FORMAT_LOCALE)`, the scan's prescribed escape hatch for the ~13 pre-existing `Number.prototype` locale-string call sites (not migrated by this issue — later P3).
- **Effective-zone wiring**: `User.preferences.timezone` (`web/src/shared/types.ts`); `main.ts` sets the store from `/auth/me` on load (refreshing live, non-blocking, when SSR already supplied the user without `preferences`); `sse-client.ts`'s reconnect auth-check also refreshes it from the same `/auth/me` call it already makes (review R1-6). `invite.ts` is intentionally **not** wired: it redirects into the app shell on success, which loads the zone via `main.ts`; it never itself renders a time.
- **"Display timezone" card** on `profile-settings.ts`: task 12's picker, `empty-label="Auto"`, visible to every signed-in user, PATCHing `preferences.timezone` with a per-key merge and updating the store immediately (no reload). On a failed PATCH (or before the user id has loaded), the picker's displayed value is forced back to the last-saved preference via a `@query` ref (review R1-5): the picker manages its own typed/selected text internally, so rebinding an *unchanged* `.value` through the template is a no-op for Lit's property-binding diff and never reaches the child. The pre-existing "Agent timezone" section is left in place (tz-refactor task 13 deletes it).
- **Zone labels in chat (review R1-3, AC4):** `chat-message.ts`, `chat-system-line.ts` and `chat-interagent-marker.ts`'s time spans now carry a `title` tooltip (full instant + `zoneLabel()`); the shared date-divider row (`chat-date-divider.ts`) appends the zone inline ("Sep 23, 2026 · Asia/Tokyo").
- **Migrated to `time.ts`** (native chat, fully clean of banned tokens): `chat-message.ts`, `chat-date-divider.ts`, `chat-interagent-marker.ts`, `chat-system-line.ts`.
- **Fixed the zone-interpretation bug** in the three `datetime-local` inputs, each labelled "Times in: `<zone>`": `access-boundary-schedule-editor.ts` (also fully clean — its `viewerTimeZone` now reads the effective zone), `scheduled-event-list.ts` and `admin-role-bindings.ts` (both keep their own private relative-time formatters, out of scope, so stay allowlisted; `admin-role-bindings.ts` now stops and shows a form error on a `parseWallClock` failure instead of sending `''`, review R1-9).
- **Format scan**: `web/src/utils/format-scan.test.ts`, banning `toLocaleString(`/`toLocaleDateString(`/`toLocaleTimeString(`/`Intl.DateTimeFormat`/`Intl.RelativeTimeFormat`/`hour12` outside `time.ts`, with a file-granular allowlist (42 files) and a stale-entry check.
- **`web/vitest.config.ts`**: pinned `TZ=UTC` for the test process.

## Review round 1 — disposition

Full review: `gs://scion-xproject-exchange/tz-refactor/out/t11/review-1.md`. Full per-finding response: `gs://scion-xproject-exchange/tz-refactor/out/t11/review-1-response.md`. Summary: R1-1 and R1-2 (High) fixed as described above; R1-3 and R1-4 (High/Medium) fixed; R1-5, R1-6, R1-7 (Medium) fixed; R1-8, R1-9, R1-10 (Low) fixed; R1-12 (Nit) fixed as part of the R1-2 dedupe; R1-11 (Nit, sort performance in task 11's own picker) is moot — that picker no longer exists.

## Test evidence

- `npm run typecheck`: clean.
- `npx vitest run` (full suite): **122 files / 3534 tests passed**, both at ambient TZ and explicitly under `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu` — identical pass counts under all three, confirming the `vitest.config.ts` pin holds.
- `npm run build`: clean.
- No Go files touched; `golangci-lint`/`go test` not applicable.
- `utils/time.test.ts` now also covers: the `DISPLAY_TIMEZONE_CHANGED_EVENT` firing on change / not firing on a no-op or invalid-to-invalid set; DST overlap on `Europe/Berlin`, `Australia/Sydney` and the 30-minute `Australia/Lord_Howe`, plus a `Europe/Berlin` gap; invalid-zone and out-of-range-field/two-digit-year rejection for `parseWallClock`/`toWallClockInput`; the new `datetime-full`/`time-seconds` `formatInstant` styles. `browserTimeZone`/`isValidTimeZone`/`listTimeZones` tests are task 12's, carried over unchanged by the rebase.

## Follow-ups (not fixed here, flagged for the human)

- `scheduled-event-list.ts` and `admin-role-bindings.ts` each still carry a private, file-local past/future relative-time formatter duplicating `Intl.RelativeTimeFormat` logic now available as `formatRelative` in `time.ts`. Out of this issue's explicit scope, but a natural target for the P3 issue that migrates their files off the format-scan allowlist.
- `components/shared/role-binding-utils.ts`'s `formatDateTime` (12-hour, `toLocaleString`) is a second, unrelated absolute-time formatter used by `admin-role-bindings.ts`; also out of scope, also allowlisted.
