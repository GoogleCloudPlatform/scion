# tz-refactor task 18b: applied-config-tz-cleanup can re-run

**Date:** 2026-10-04
**Branch:** `scion/tz-t18b`
**Fork issue:** ptone/scion#2847 (part of ptone/scion#2457)

## Problem

The `applied-config-tz-cleanup` description promises "safe to re-run; a
second run converts 0", but `executeMigration` returned `409` for any
completed migration whose key is not in `rerunnableMigrations`.

## Fix

- Added `applied-config-tz-cleanup` to `rerunnableMigrations`
  (`pkg/hub/utc_timestamp_normalize.go`), with a comment on why each listed
  key is idempotent.
- Idempotency check: `adoptLegacyTZ` strips `TZ` from both applied env
  copies, so a second run skips every handled agent
  (`appliedConfigHasEnvTZ` is false), makes no `UpdateAgent` call and
  only updates the operation row. `TestAppliedConfigTZCleanupIsIdempotent`
  already covered this at executor level.
- New test `TestAppliedConfigTZCleanupRerunsThroughExecuteMigration` runs
  the migration twice through the admin run handler: both runs return 200
  and complete, the second reports 0 adopted and 0 stripped, and agent
  state versions do not change.
- `.design/server-routine-maintenance.md` §2, §3.3 and §3.5 describe the
  409 rule, the exemption list and that a listed migration must be
  idempotent.

## Verification

- `go build`, `go vet`, `gofmt -l`, `golangci-lint --new-from-rev`: clean.
- Under `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`: `pkg/store/entadapter`
  and the targeted `pkg/hub` run
  (`AppliedConfigTZCleanup|Maintenance|Rerunnable|ExecuteMigration`) pass.

## Not changed

The Admin → Maintenance page offers **Run** only for pending or failed
migrations, so a completed rerunnable migration is re-run through
`POST /api/v1/admin/maintenance/migrations/<key>/run`.

## Review round 1 fixes

- **Dry run of a completed migration.** `executeMigration` used to reset a
  successful dry run to `pending`, so a dry run of a completed rerunnable
  migration erased its completion. It now captures the status, completion
  time, result, start time and user before the run. When the migration was
  completed, it restores them after a successful dry run and writes the
  dry-run output to the hub log. A dry run of a pending migration is
  unchanged. This also covers `utc-timestamp-normalize`. New test:
  `TestExecuteMigrationDryRunKeepsCompletedRecord` (fails without the
  restore).
- **Re-run replaces the result.** §2, §3.3 and §3.5 of
  `.design/server-routine-maintenance.md`, and the seeded operation
  description, say that a re-run replaces the stored result, including the
  `ADOPT` lines. Adopted pins stay identifiable by timezone source `legacy`.
- **Key constant.** `entadapter.AppliedConfigTZCleanupKey` replaces the
  string literal in the seed, `resolveMaintenanceExecutor`,
  `rerunnableMigrations` and the tests.

Not changed: a failed dry run of a completed migration, or an executor
panic during one, still marks it `failed`. That is the pre-existing failure
path.

## Review round 2 fixes

- **Dry run of a completed migration is rejected.** The round 1 restore is
  removed. `executeMigration` now returns `409 Conflict` ("Migration already
  completed; a re-run is idempotent, so run it without dryRun") for a dry run
  of a completed migration. A failed or panicking dry run, or a hub restart
  during one, can no longer overwrite the completed record, and no dry-run
  output is left reachable only through the hub log.
  `TestExecuteMigrationDryRunKeepsCompletedRecord` asserts the 409 and an
  unchanged record (it fails without the check). §2 and §3.5 are updated.
- **Concurrent runs.** The check-then-set race on the running guard predates
  this change and is not fixed here. Follow-up: ptone/scion#2953.

## Review round 3 fix

- §3.5 now gives each 409 message where it applies. An unlisted completed
  migration gets the plain "Migration already completed" from the earlier
  guard. Only a listed one gets the dry-run message.

## Upstream merge

- Merged upstream main 833426e9 as merge commit 829b8bfb. No conflicts, and the remerge-diff is empty. Build and vet are clean. The targeted pkg/hub run passes 53/53 under TZ=Asia/Tokyo and TZ=Asia/Kathmandu.
