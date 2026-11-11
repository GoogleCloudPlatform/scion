# tz-refactor task 19 (P3a): web formatter fan-out, lists and relative times

**Date:** 2026-10-02
**Branch:** `scion/tz-t19`
**Fork issue:** ptone/scion#2512 (part of ptone/scion#2457, design Option A, D4)

## What changed

- 15 web files now format times only through `web/src/utils/time.ts` and
  numbers through `formatNumber`. Each one left the format-scan allowlist
  (42 entries before, 27 after):
  - pages: `agent-detail.ts`, `project-detail.ts`, `brokers.ts`,
    `broker-detail.ts`, `home.ts`, `project-settings.ts`;
  - shared lists: `env-var-list.ts`, `gcp-service-account-list.ts`,
    `pre-start-hook-list.ts`, `project-template-list.ts`, `schedule-list.ts`,
    `scheduled-event-list.ts`, `secret-list.ts`, `subscription-manager.ts`,
    `token-list.ts`.
- Each private `Intl.RelativeTimeFormat` copy in those files was replaced
  with `formatRelative`. The local guards stay: an em dash for a zero or
  unparsable time, and "now" for an overdue next run or fire time.
- Hand-rolled compact ladders ("5m ago", "2d ago") were also migrated.
  They contain no banned token, so the format scan cannot see them:
  `agents.ts`, `project-detail.ts`, `skills.ts`, `skill-detail.ts`,
  `health-dashboard.ts`, `notification-tray.ts`, `inbox-tray.ts` and
  `home.ts`. They call `formatRelative(iso, { style: 'narrow' })`, which
  keeps the compact look. Each keeps its guards: an em dash ("unknown" on the
  health dashboard) for an unparsable time, and "just now" for a future
  instant. The home page still shows a labelled date for items older than
  30 days. `formatRelative` gained one optional argument, `{ style }`, which
  selects the `Intl.RelativeTimeFormat` style (`'long'` is the default, the
  others are `'short'` and `'narrow'`).
- Behaviour changes from the shared ladder: values round to the nearest
  unit instead of truncating, a future next run or fire time more than a day
  out reads "in 3 days" instead of "in 72 hours", times under a minute read
  "30s ago" or "now" instead of "just now", and one day reads "yesterday".
- Absolute times use `formatInstantWithZone`, which gives the effective zone,
  24-hour time and a zone label. Date-only cells (template and hook created
  dates, the home page's fallback for items older than 30 days) use the
  `'date'` style, which keeps the label. Token expiry is an exact instant, so
  it uses `'datetime-full'`.
- Components that render an absolute time use `DisplayZoneController`, so a
  change to the display zone re-renders them without a reload.
- AC15: each active row in the schedule list shows its next run twice, as a
  relative time and as the absolute time in the display zone with its label.
  The detail dialog does the same. The cron column header and the dialog's
  cron row now read "Cron (UTC)", so the UTC cron expression and the zoned
  next run sit next to each other with distinct labels. The tooltip on a
  scheduled event's fire time shows the zoned absolute instant.

## Out of scope (left on the allowlist)

- Admin, access-boundary and role-binding views, including
  `role-binding-utils.ts`, `admin-experiments.ts`, `admin-server-config.ts`
  and `metrics-dashboard.ts`. These belong to tz-refactor task 20, which
  also owns the compact ladders in `admin-skill-registries.ts` and
  `admin-skill-registry-detail.ts`. The ladders in
  `chat-members.ts` and `chat-search.ts` belong to tz-refactor task 21.
- Log viewers, chat, `file-browser.ts` and `chat-palette-data.ts`. These
  belong to tz-refactor task 21.
- `profile-settings.ts`, which belongs to tz-refactor task 13.
- Schedule-list paging and the edit dialog (ptone/scion#2643).

## Tests

- Vitest pins `TZ=UTC`. Each new case sets the display preference to
  `Asia/Tokyo` and uses `2026-10-01T15:00:00Z`, which is midnight in Tokyo,
  so the expected output is `00:00`.
  - `schedule-list.test.ts`: the zoned next run in the row and in the dialog,
    a re-render when the zone changes, no next run for a paused schedule,
    and the "Cron (UTC)" header.
  - `scheduled-event-list.test.ts`: the zoned tooltip on the fire time.
  - `list-time-zone.test.ts`: dates on the broker and project detail pages,
    token expiry, template and hook created dates, and the Auto fallback to
    the browser zone.
  - `agent-detail.test.ts`: `formatDate` at midnight, and the em dash for a
    zero time.
  - `list-time-zone.test.ts` also covers the project settings "Last Token
    Mint" and the home page's fallback for items older than 30 days.
  - `compact-relative-time.test.ts`: each migrated compact helper, covering
    seconds, minutes, hours, days, the future guard and the invalid guard.
  - `time.test.ts`: `formatRelative` with seconds, "now", days past and
    future, and each style option.
  - `schedule-list.test.ts` and `scheduled-event-list.test.ts`: a run three
    days out reads "in 3 days", and an overdue one reads "now".
- `npm run typecheck` passes. The touched component tests and the format
  scan pass with `--maxWorkers=2`. No Go code changed.
