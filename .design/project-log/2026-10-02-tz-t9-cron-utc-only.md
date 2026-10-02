# tz-refactor task 9: recurring schedules are UTC-only

**Date:** 2026-10-02
**Branch:** scion/tz-t9
**Issue:** ptone/scion#2502 (part of ptone/scion#2457; design Option A, decision D2)

## What changed

- **One parser.** `parseScheduleCron` in `pkg/hub/schedule_cron.go` replaces the five
  `cron.NewParser(...).Parse` sites (create, update, enable-via-update, resume, and
  `executeSchedule`). It rejects a case-sensitive `CRON_TZ=`/`TZ=` prefix (the same check
  robfig/cron v3.0.1 makes) with `errCronZonePrefix`, and pins the parsed `SpecSchedule` to
  `time.UTC`. robfig captures `time.Local` for prefix-free specs and then evaluates in the zone
  of the time passed to `Next`, so the pin makes UTC explicit, independent of the process pin
  from tz-refactor task 1.
- **HTTP.** Create and update with a prefix return 400 with
  `cron expressions are evaluated in UTC; zone prefixes (CRON_TZ=, TZ=) are not supported — convert the time to UTC`.
  Enable (update with `status: active`) and resume of a stored prefixed row return the same 400
  instead of a 500. A metadata-only update, including resending the unchanged expression, still works.
- **Startup pass.** `pauseZonePrefixedSchedules` runs once in `StartBackgroundServices`, before
  the scheduler's first tick. It pages through all active schedules (page size 200) to the end,
  pauses each prefixed row and logs one warning with ID, project and expression.
- **Backstop.** `executeSchedule` pauses a prefixed row and returns, so a row written after start
  (direct DB edit, older replica) is paused at its first tick instead of erroring every tick.
- **Store fix (prerequisite).** `ListSchedules` returned a `NextCursor` but never read
  `opts.Cursor`, so the REST list repeated page one past 50 rows and the startup pass could not
  page. It now uses a keyset on `(created DESC, id DESC)` with the opaque `encodeCursor` token the
  message store uses. No cursor-row lookup, so paging survives the boundary row being deleted or
  paused. A malformed cursor (including a bare ID from before) wraps `store.ErrInvalidInput` and
  is a 400 on REST.
- **Web.** `schedule-list.ts` shows a "Zone prefix not supported — edit to UTC" badge on rows and
  in the detail dialog. The "(UTC)" help text stays.
- **CLI, docs, skill.** `create-recurring` help, `hosted/user/scheduling.md` and the
  `scion-scheduler` platform skill say UTC only. None of them mentioned the prefix before.

## Descriptors (AC wording correction)

AC15 and the task's test plan assumed `@every`/descriptors were accepted ("still work"). They never
were at the hub parse sites: the parser has no `cron.Descriptor` flag. That behaviour is unchanged
(ruled by the tz-refactor EM); tests check that `@every 1h` and `@daily` are still rejected with
the existing parser error, not the zone-prefix message.

## Test evidence

- Store: `TestListSchedules_*` (next page, equal-`created` ties, mixed ties across a boundary,
  boundary row paused under the active filter, boundary row hard-deleted, default `created`,
  malformed and bare-UUID cursor). Run on SQLite locally under TZ=UTC, Asia/Tokyo and
  Asia/Kathmandu. No Postgres in the dev container: the tests are added to the
  `test-launch-store-postgres` `-run` list, so the CI Postgres job runs them.
- Hub: `TestParseScheduleCron`, `TestSchedule_*` (including the new zone-prefix, descriptor,
  enable/resume and REST cursor tests), `TestPauseZonePrefixedSchedules_*` (seeded row with
  idempotence; 450 rows with prefixed rows at list positions 200 and 400),
  `TestExecuteSchedule_ZonePrefixBackstop`, and the existing `TestScheduler*`. 63 pass under each
  of TZ=UTC, Asia/Tokyo and Asia/Kathmandu with the leaked `SCION_*` env stripped.
- Web: `schedule-list.test.ts` (4 tests) under TZ=Asia/Tokyo and Asia/Kathmandu; `npm run typecheck`.

## Follow-ups (not done here)

- The web schedule list fetches only the first page (50) and has no edit dialog, so the badge
  says "edit to UTC" but the edit happens through the API or by re-creating the schedule.
- Next-run in the viewer's display zone belongs to tz-refactor task 19 (needs `time.ts`).
