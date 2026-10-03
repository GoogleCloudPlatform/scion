# tz-refactor task 20: admin and access-boundary views format through time.ts

**Issue:** ptone/scion#2513 (part of ptone/scion#2457, design Option A, §2.4, AC17).

## What changed

The admin pages, the access-boundary views and the role-binding views now format every time through `web/src/utils/time.ts`, and their numeric `toLocaleString()` sites go through `formatNumber`:

- Absolute times use `formatInstantWithZone` (or `formatInstant` plus one `zoneLabel()` for the boundary list's two-bound schedule column). They render in the effective display zone, with a 24-hour clock and a zone label. Each component that renders one now has a `DisplayZoneController`, so it re-renders when the display zone changes.
- Ten private copies of the relative-time helper were removed or reduced to a guard around `formatRelative`. The guards stay: 'Never' on the users and scheduler pages, '' on the maintenance page, and 'now' for a scheduler next-run that is already due.
- `role-binding-utils.formatDateTime` was removed, along with its `shared/index.ts` re-export. Its three callers (`admin-role-bindings.ts`, `admin-role-detail.ts`, `effective-role-provenance.ts`) call `formatInstantWithZone` directly.
- `admin-scheduler.ts` (tick count) and `metrics-dashboard.ts` (small counts) use `formatNumber`.
- 17 files were removed from the format-scan allowlist.

## Why

AC17: one formatter, the user's display zone, 24-hour time everywhere. Before this change, these views used the browser zone with mixed 12-hour and 24-hour output.

## Test evidence

- New `admin-time-format.test.ts`, plus render tests in `admin-experiments.test.ts`, `admin-role-bindings.test.ts` and `admin-role-detail.test.ts`. They set an Asia/Tokyo display preference while vitest pins the browser zone to UTC, and check that midnight renders as `00:00`. Two of them also check a live re-render after a zone change.
- `vitest run` over all touched admin, access-boundary and role-binding test files, `format-scan.test.ts` and `time.test.ts`: 19 files and 404 tests pass. The same run under host `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu` also passes; vitest pins `TZ=UTC` regardless of the host zone.
- `tsc --noEmit` is clean. ESLint on the changed source files adds no new errors compared with upstream main.

## Follow-ups (not done here)

- `access-boundary-preview.ts` renders a "from X until Y" window where each bound carries its own zone label. It is correct, but the label is repeated.
- `admin-maintenance.ts`, `admin-quotas.ts`, `admin-roles.ts` and `admin-server-config.ts` are already prettier-dirty on upstream main. CI does not run prettier, so this change leaves those files as they are.
