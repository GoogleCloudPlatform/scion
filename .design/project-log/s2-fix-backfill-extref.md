# Fix: Unify DM external_ref and Require ProjectID in Backfill

**Date:** 2026-08-27
**Author:** dev-fix-backfill
**Branch:** `scion/dev-fix-backfill`
**Commit:** `fix(messaging): unify DM external_ref and require ProjectID in backfill`

## Problem

Two defects in the backfill service:

1. **Duplicate DM conversations:** The backfill and dual-write paths produced
   different `external_ref` values for the same logical DM conversation. Backfill
   used `direct:{projectID}:{kindA}:{idA}:{kindB}:{idB}` while dual-write used
   `dm:{sorted(idA,idB)}`. Since conversations have a UNIQUE constraint on
   `(surface, external_ref)`, the same DM would get two rows.

2. **Missing project isolation:** `BackfillService.Run()` accepted an empty
   `ProjectID`, which could group messages across project boundaries into a
   single thread conversation, violating the project isolation invariant.

## Changes

### `pkg/messaging/backfill.go`

- **`groupForMessage`**: Replaced the project-scoped `direct:...` key
  construction with a call to `DirectMessageExternalRef(senderID, recipientID)`.
  This produces `dm:{sorted(idA,idB)}` — the same format the dual-write path
  uses — so both paths resolve to the same conversation row. Thread keys
  (`thread:{projectID}:{threadID}`) are unchanged since threads are
  project-scoped.

- **`Run`**: Added early validation that `cfg.ProjectID != ""`, returning an
  error if empty. This prevents cross-project thread grouping.

### `pkg/messaging/backfill_test.go`

- Updated `TestBackfill_ConversationExternalRef` to assert the exact
  `DirectMessageExternalRef` output instead of checking for the old `direct:`
  prefix.

- Added `TestBackfill_EmptyProjectID` to verify that `Run()` returns an error
  when `ProjectID` is empty.

## Verification

- `go build -buildvcs=false ./...` — passes
- `go test ./pkg/messaging/...` — all tests pass (21 backfill + messaging tests)
- `go vet ./pkg/messaging/...` — clean
- `gofmt` — clean
