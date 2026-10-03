# tz-refactor task 21 (part a): log viewers, chat exports and the rest through time.ts

**Date:** 2026-10-03
**Branch:** scion/tz-t21
**Issue:** ptone/scion#2514 (part of ptone/scion#2457; design §2.4, AC17, decision D4)

## What changed

- **time.ts additions (additive).**
  - `formatInstant` reuses one `Intl.DateTimeFormat` per style and zone (a module `Map`).
    The file browser formats one value per row, and per-row construction cost about 160 ms
    on a 1000-row listing (ptone/scion#2382). A zone change selects the new zone's formatter.
  - With no display preference, `effectiveTimeZone()` holds `browserTimeZone()` (which itself
    builds a formatter) for the current synchronous task and clears it in a microtask. One
    render resolves the zone once; the next task sees an operating-system zone change.
    `browserTimeZone()` is unchanged.
  - New style `'time-millis'` (`HH:mm:ss.SSS`, `h23`) for the log viewers.
- **Migrated files** (removed from the format-scan allowlist): `client/chat-palette-data.ts`,
  `components/shared/agent-log-viewer.ts`, `agent-message-viewer.ts`, `unified-log-viewer.ts`,
  `file-browser.ts`, `chat/chat-members.ts`, `chat/chat-search.ts`, `chat/chat-space-rail.ts`,
  `chat/chat-thread.ts`.
  - Log viewers: row times in the display zone, 24-hour (milliseconds kept where they were
    shown). Date dividers name the zone; the unified viewer's time tooltip carries the full
    date and zone. Its expanded detail panel shows the display-zone instant with milliseconds
    and the zone, then the raw ISO value labelled UTC (for Cloud Logging correlation); an
    unparsable timestamp is shown as given instead of throwing.
  - Chat exports (markdown, print, copy, and the rail's thread export) use
    `formatInstantWithZone`. The export filename date is in the display zone.
  - Chat members: ages under a week keep their compact text; older dates use
    `formatInstantWithZone(iso, 'date')` (shown in a tooltip).
  - Chat search: same ladder, but older dates show the compact `formatInstant(iso, 'date')`
    because the time slot does not shrink; every result time carries the full
    `formatInstantWithZone(iso, 'datetime-full')` as its title.
  - Palette attachment date: `formatInstant(..., 'date')`, so it now includes the year.
    `PaletteCandidate` has no title field, so no tooltip was added.
  - File browser: the "Modified" column uses `formatInstant(..., 'datetime-full')`, and the header
    names the zone. Counts use `formatNumber`.
- **Skill-registry pages** (`admin-skill-registries.ts`, `admin-skill-registry-detail.ts`): the
  hand-rolled "3h ago" helpers now call `formatRelative`. Unparsable input still shows an em dash.

## Tests

`npx vitest run --maxWorkers=2` on `utils/time.test.ts`, `utils/format-scan.test.ts`,
`client/chat-palette-data.test.ts`, the new `log-viewer-timezone.test.ts`,
`chat-relative-dates.test.ts`, `admin-skill-registries-relative-time.test.ts`,
`file-browser*.test.ts` and all of `components/shared/chat/` (37 files, 1013 tests) passed, plus
`npm run typecheck`. vitest pins `TZ=UTC`, so the component tests set a different display zone
(Asia/Tokyo, America/New_York, Asia/Kathmandu). They cover midnight shown as `00:00` and
re-rendering on a zone change. A run with `TZ=Asia/Tokyo` and with `TZ=Asia/Kathmandu` in the shell
also passed.

## Follow-ups

- Part (b) of tz-refactor task 21 deletes the allowlist and adds the AC17 check that only
  `time.ts` holds banned tokens. It starts once tz-refactor tasks 13, 19, 20 and 21a are on
  upstream main.
- Part (b) also switches chat members/search to `formatRelative(iso, { style: 'narrow' })` once
  tz-refactor task 19 provides that option on upstream main.
