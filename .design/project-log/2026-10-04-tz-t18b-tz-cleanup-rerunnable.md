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
