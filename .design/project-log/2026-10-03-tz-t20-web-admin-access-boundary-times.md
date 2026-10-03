# tz-refactor task 20: admin and access-boundary views format through time.ts

**Issue:** ptone/scion#2513 (part of ptone/scion#2457, design Option A, §2.4, AC17).

## What changed

The admin pages, the access-boundary views and the role-binding views now format every time through `web/src/utils/time.ts`, and their numeric `toLocaleString()` sites go through `formatNumber`:

- Absolute times use `formatInstantWithZone` (or `formatInstant` plus one `zoneLabel()` for the boundary list's two-bound schedule column). They render in the effective display zone, with a 24-hour clock and a zone label. Each component that renders one now has a `DisplayZoneController`, so it re-renders when the display zone changes.
- Ten private copies of the relative-time helper were removed or reduced to a guard around `formatRelative`. The guards stay: 'Never' on the users and scheduler pages, '' on the maintenance page, and 'now' for a scheduler next-run that is already due.
- `role-binding-utils.formatDateTime` was removed, along with its `shared/index.ts` re-export. Its three callers (`admin-role-bindings.ts`, `admin-role-detail.ts`, `effective-role-provenance.ts`) call `formatInstantWithZone` directly.
- `admin-scheduler.ts` (tick count) and `metrics-dashboard.ts` (small counts) use `formatNumber`.
- 17 files were removed from the format-scan allowlist.
- Review round 1 fixes:
  - The role-binding create form's lifecycle checks (the past-expiry warning, the ordering warning and the `createFormValid` ordering check) now read the `datetime-local` values in the display zone through `parseWallClock`, as the submit path already did. This bug predates this change.
  - The boundary list's schedule column shows the year (`'datetime-full'`) and adds the zone label only when a bound was actually converted.
- Review round 4 fix: the server-config Build Time is formatted in the display zone, with the raw value as its `title`. It used to show the raw server string. A sweep of all migrated files for other raw server timestamps found none.

## Why

AC17: one formatter, the user's display zone, 24-hour time everywhere. Before this change, these views used the browser zone with mixed 12-hour and 24-hour output.

## Test evidence

- New `admin-time-format.test.ts` and `effective-role-provenance-times.test.ts`, plus render tests in `admin-experiments.test.ts`, `admin-role-bindings.test.ts`, `admin-role-detail.test.ts` and `admin-server-config.test.ts`. They set an Asia/Tokyo display preference while vitest pins the browser zone to UTC, and check that midnight renders as `00:00`. The server-config test also covers the build time and an `unknown` build time. Four of them also check a live re-render after a zone change: the experiments attribution, the role-binding lifecycle details, the server-config section meta, and the effective-role Expires/Activates times.
- `vitest run` over all touched admin, access-boundary and role-binding test files, `format-scan.test.ts` and `time.test.ts`: 20 files and 411 tests pass after review round 4. Two of the tests fail with the role-binding fix reverted: an expiry that is past in Tokyo but future in UTC must show the warning, and an activation in the America/New_York spring-forward gap that falls after its expiry must both block submit and show the ordering warning. Reverting only the warning's activation parse also fails the DST-gap test. A Tokyo-only ordering test cannot catch this, because Tokyo has no DST. The same run under host `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu` also passes; vitest pins `TZ=UTC` regardless of the host zone.
- `tsc --noEmit` is clean. ESLint on the changed source files adds no new errors compared with upstream main.

## Follow-ups (not done here)

- `access-boundary-preview.ts` renders a "from X until Y" window where each bound carries its own zone label. It is correct, but the label is repeated.
- `admin-maintenance.ts`, `admin-quotas.ts`, `admin-roles.ts` and `admin-server-config.ts` are already prettier-dirty on upstream main. CI does not run prettier, so this change leaves those files as they are.
