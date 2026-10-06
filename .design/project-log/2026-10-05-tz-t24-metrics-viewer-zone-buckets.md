# Project Log: tz-refactor task 24 — metrics dashboard buckets days in the viewer's zone

**Date:** 2026-10-05
**Branch:** `scion/tz-t24`, based on upstream main `5c4ac36` (GoogleCloudPlatform/scion).
**Fork issue:** ptone/scion#3370 (closes; mirror of miller79/scion#165). Refs ptone/scion#2457.
**Design:** tz design §2.3 ("Server-side calendar bucketing"), which made viewer-zone bucketing a follow-up to the UTC-day policy. GoogleCloudPlatform/scion#2308 added the "(UTC)" labels this change generalises.

## What changed

- **Hub (`pkg/hub/metrics_dashboard.go`).**
  - `resolveDashboardTimeZone(tz)` validates the `tz` query parameter before `time.LoadLocation` sees it, because LoadLocation opens the name as a path under the zoneinfo directory. Rejected: anything longer than 64 bytes, `..`, a leading `/`, a backslash, anything that is not one to three letter-led segments of `[A-Za-z0-9_+-]`, the non-portable names (`Local`, `localtime`, `posixrules`, `Factory`), and `right/` or `posix/` prefixes. Every rejected or unknown value resolves to UTC; the request never fails because of `tz`. `UTC`-equivalent names (`Etc/UTC` and others) collapse to `time.UTC` and share the UTC cache entries.
  - All three bucketing sites (`queryDailyTimeSeries`, `queryGroupedTimeSeries`, `queryDailyUniqueCount`) key days with `t.In(loc).Format("2006-01-02")`. `queryDailyUniqueCount` also skips nil-interval and out-of-window points now, as the other two sites already did.
  - `metricsQueryWindowFor` starts "the last N days" at local midnight N−1 days before today, using local-calendar dates rather than 24h steps. DST days are therefore 23 or 25 hours long. `startOfLocalDay` handles zones whose DST change skips midnight (America/Santiago on 2026-09-06, where `time.Date` resolves the missing midnight to 23:00 the day before).
  - **Behaviour change:** the window used to be a rolling N×24h ending now, which yielded N+1 partial UTC days. It is now N calendar days in the zone, today included. The summary view uses the same window, so its totals match the charts.
  - **Cache:** the resolved zone name is part of every dashboard cache key (`<view>:<period>:tz=<zone>[:<project>]`). `setZonedCache` drops expired entries on each write, which also stops the map growing without bound. At most `maxCachedZones` (16) distinct non-UTC zones have live entries; a further zone is computed but not cached until older zones expire. UTC is always cached.
  - Every view's response carries the resolved zone as `timeZone`.
  - **Time-literals gate:** every zone-local day format goes through `dayKey`, the one deliberate `Format` without `.UTC()`. It has a justified entry in `hack/time-literals-allowlist.txt`: the key is a calendar day in the viewer's zone, never stored or sent as an instant.
- **Bucketing stays in Go.** This path has no SQL: the dashboard reads raw Cloud Monitoring points (`ListTimeSeries` with no aligner, because of the cumulative-math fix) and already aggregated them in Go. There was no SQLite or Postgres `GROUP BY` date to make zone-correct.
- **Web (`metrics-dashboard.ts`, `utils/time.ts`).** The page sends `effectiveTimeZone()` (the user's display-zone preference, else the browser zone) as `tz`. tz-em and tz-lead chose this reading of AC1 for consistency with the rest of the UI, and AC1 in ptone/scion#3370 was updated to match. Headings and the x-axis title name the zone the hub reports (`Day (America/Chicago)`), never the requested one, using the new `dayBucketZoneLabel`/`dayBucketAxisTitle` helpers. "(UTC)" is kept for UTC and for an older hub that omits `timeZone`. The page refetches on `DISPLAY_TIMEZONE_CHANGED_EVENT`, because the buckets are computed by the hub and a re-render alone would not be enough.
- **Docs.** New "Metrics dashboard day buckets" section in `reference/times-and-timezones.md`, plus cross-links from `workstation/dashboard.md` and `hosted/single-node/metrics.md`.

## Tests

- Go (`metrics_dashboard_timezone_test.go`): tz resolution table (valid zones, path-shaped input, garbage, `Local`, non-portable names, the length cap); local-midnight window starts for Chicago, Kathmandu and the Santiago midnight gap; 23h and 25h Chicago DST day lengths; a Chicago evening point bucketed at all three sites (local date in Chicago, next date in UTC); DST transition days at all three sites; Asia/Kathmandu +05:45 at all three sites; the cache key distinguishing zones (including an invalid zone sharing the UTC entry, and a repeat zone being a cache hit); the zone cap and expiry pruning; and the handler echoing `timeZone` for all four views, including bad `tz` values that must still return 200.
- Existing tests updated for the new window semantics and cache-key format.
- Web: `metrics-dashboard.test.ts` covers `tz` with the preference unset (the browser zone, stubbed to America/Chicago) and set; headings and axis title from the reported zone on all three tabs; reported-UTC fallback; an older hub without `timeZone`; refetch on a preference change; and no listener after disconnect. `time.test.ts` covers the label helpers.

## Review round 1 — disposition

All four findings were fixed; none declined.
- **R1-1 (Medium):** tab labels came from a page-global zone set by whichever response arrived last. They now come from each view's own `timeZone`. Stale responses for the same view are dropped by request sequence number, and an existing chart's axis title is refreshed with its data.
- **R1-2 (Low):** the resolver now reuses `validateIANATimezone` for the portability rules.
- **R1-3 (Nit):** every exactly-UTC tzdata name maps to `time.UTC`.
- **R1-4 (Nit):** the orphaned class comment is reattached.

## Follow-ups noticed (not done)

- `QueryProjectSummary` ("Last 24 hours") stays a rolling 24h window with no buckets, so the zone does not affect it.
- The dashboard cache has no global entry cap beyond TTL pruning and the zone cap. Project-scoped keys still scale with the number of projects viewed in a 5-minute window, which is bounded by real usage.
