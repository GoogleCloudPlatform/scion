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
- **Startup pass.** `pauseZonePrefixedSchedules` runs once, through `startScheduler`, in
  `StartBackgroundServices` before the scheduler's first tick. It pauses each active prefixed row
  and logs one warning with ID, project and expression. It deliberately does **not** page through
  `ListSchedules`, although the task text suggested that. That keyset is on `created`, and on SQLite
  legacy rows written in a non-UTC zone before timestamps were normalized make it skip rows (east
  of UTC) or never advance (west of UTC). The pass would then miss prefixed rows or hang hub
  startup, before an operator could run the fix. Instead, the store query
  `ListActiveZonePrefixedSchedules` (active, `cron_expr` LIKE `CRON_TZ=%` or `TZ=%`, ordered by ID,
  with an exclude list) feeds a fetch-then-pause loop (batches of 200). Pausing a row removes it
  from the next fetch. Rows that are fetched but not paused (the pause failed, or SQLite's
  case-insensitive LIKE matched a non-prefix) are sent back as excluded IDs. Every handled ID,
  paused ones included, is recorded locally and handled at most once. A batch with no new ID (for
  example a pause that reported success without taking effect) stops the loop with an error naming
  `utc-timestamp-normalize`, so the loop always ends.
- **Backstop.** `executeSchedule` pauses a prefixed row and returns, so a row written after start
  (direct DB edit, older replica) is paused at its first tick instead of erroring every tick.
- **Store fix (prerequisite).** `ListSchedules` returned a `NextCursor` but never read
  `opts.Cursor`, so the REST list repeated page one past 50 rows and the startup pass could not
  page. It now uses a keyset on `(created DESC, id DESC)` with the opaque `encodeCursor` token the
  message store uses. No cursor-row lookup, so paging survives the boundary row being deleted or
  paused. A malformed cursor (including a bare ID from before) wraps `store.ErrInvalidInput` and
  is a 400 on REST. A no-progress guard returns no `NextCursor` unless it is strictly after the input
  cursor, so no client can page forever. On SQLite hubs upgraded from a non-UTC zone, list paging
  is exact only after the `utc-timestamp-normalize` maintenance operation (tz-refactor task 6) has
  run: before that, legacy east-zone rows can be skipped, and in the west zone the listing ends
  after repeating one row.
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
  Asia/Kathmandu. No Postgres in the dev container: the tests (and
  `TestListActiveZonePrefixedSchedules`) are added to the `test-launch-store-postgres` `-run`
  list, so the CI Postgres job runs them. `TestListSchedulesLegacyText_*` (SQLite only) rewrite
  `created` to Asia/Tokyo and America/New_York `Time.String()` text with raw SQL and assert that
  paging terminates, documenting the east-zone skip and the west-zone repeat, and that it is exact
  after the rows are rewritten to UTC text.
- Hub: `TestParseScheduleCron`, `TestSchedule_*` (including the new zone-prefix, descriptor,
  enable/resume and REST cursor tests), `TestPauseZonePrefixedSchedules_*` (seeded row with
  idempotence; 450 rows over four batches; legacy east- and west-zone `created` text over several
  batches; a failing pause; loose store matches), `TestStartScheduler_PausesBeforeFirstTick` (the
  evaluator's first tick already sees the row paused; no event materialized),
  `TestExecuteSchedule_ZonePrefixBackstop`, and the existing `TestScheduler*`. All pass under each
  of TZ=UTC, Asia/Tokyo and Asia/Kathmandu with the leaked `SCION_*` env stripped.
- Web: `schedule-list.test.ts` (4 tests) under TZ=Asia/Tokyo and Asia/Kathmandu; `npm run typecheck`.

## Follow-ups (not done here)

- The web schedule list fetches only the first page (50) and has no edit dialog, so the badge
  says "edit to UTC" but the edit happens through the API or by re-creating the schedule.
- Next-run in the viewer's display zone belongs to tz-refactor task 19 (needs `time.ts`).
