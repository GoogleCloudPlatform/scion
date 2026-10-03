# tz-refactor task 18: applied-config-tz-cleanup maintenance migration

Issue: ptone/scion#2511 (part of ptone/scion#2457).

## What changed
- New optional migration `applied-config-tz-cleanup`
  (`pkg/hub/applied_config_tz_cleanup.go`). It runs `adoptLegacyTZ`
  unchanged over every agent row, including soft-deleted rows as the env
  cleanup does. It logs the number of agents whose saved TZ became a legacy
  pin (source `legacy`) and the number whose env TZ was only stripped.
  Supports `dryRun`. Writes use the env cleanup's optimistic-lock retry
  pattern. Values are never logged.
- Seeded as a migration-category operation in
  `pkg/store/entadapter/maintenance_store.go`, so it never runs
  automatically. Wired in `resolveMaintenanceExecutor`.
- The description states the order interaction with
  `applied-config-env-cleanup`. Both orders are safe. If the env cleanup runs
  first, it drops saved TZ values that match no live plain source, so those
  agents follow the resolver. Values that match InlineConfig or a storage var
  are kept and adopted.

## Why
Lazy adoption already happens on every TZ read or write. The migration
classifies everything in one pass and makes adopted pins countable (design
§3 A "Legacy classification").

## Tests
`pkg/hub/applied_config_tz_cleanup_test.go`: counts and source `legacy`,
idempotence (a second run adopts 0 and writes nothing), dry run, both orders
with the env cleanup, and registration. Targeted pkg/hub run plus the env
cleanup and adoptLegacyTZ tests pass under TZ=UTC, Asia/Tokyo and
Asia/Kathmandu. Scoped golangci-lint reports 0 issues.

## Follow-ups (not done)
- Reincarnation snapshots are not swept. Reincarnate already adopts on the
  old config before it builds the new one.
- `.design/server-routine-maintenance.md` §3.3 lists only the original seeded
  migration (the env cleanup is missing too).
