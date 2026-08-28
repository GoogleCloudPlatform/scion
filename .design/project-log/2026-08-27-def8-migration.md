# DEF-8: DM Migration — Listing Rebuild + Merge/Re-key

**Date:** 2026-08-27  
**Task:** WS-2  
**Branch:** `scion/ca-msg-em6`

## What was migrated and why

Direct message conversations historically used three different external_ref
formats. The new kind-encoded format (`dm:<kind>:<uuid>:<kind>:<uuid>`) was
introduced as part of DEF-8 to support key-based auth and immutable participant
guards. This migration normalizes all historical rows to the new format.

## Three categories of old rows

1. **Kind-encoded rows missing participants:** Created by the new code path but
   lacking entries in the participants table (listing-index). The migration
   verifies each principal exists in its claimed table (user or agent), then
   adds both as participants (all-or-nothing per row).

2. **Empty external_ref rows:** Created by the old resolver path (now deleted).
   The migration reads participants to compute the kind-encoded key. If a row
   with that key already exists, messages are re-stamped to the target and the
   old row is soft-deleted (merge). Otherwise, the external_ref is set in place
   and ProjectID is cleared (DMs are global, fixing DEF-10).

3. **Old-format `dm:{sorted(id1,id2)}` rows:** Created by the old dual-write
   path. The migration extracts the two UUIDs, resolves each to a kind by
   looking up both user and agent tables. Ambiguous IDs (found in both or
   neither) are skipped. Unambiguous pairs get re-keyed or merged with an
   existing kind-encoded row.

## Files modified

- `pkg/messaging/dm_migration.go` — Migration service with DMMigrationStore
  interface, DMMigrationConfig, DMMigrationResult, and three-step migration
  logic (listing rebuild, empty-ref merge/re-key, old-format re-key).
- `pkg/messaging/dm_migration_test.go` — Unit tests covering all migration
  steps, dry-run mode, ambiguous IDs, merge scenarios, and three permanent
  guard tests (A: no empty-ref direct rows, B: every dm: row has 2
  participants, C: all dm: keys are parseable by ParseDMKey).

## Guard tests (permanent)

- **Guard A:** Zero non-deleted direct conversations with empty external_ref
- **Guard B:** Every non-deleted dm: row has exactly 2 participants
- **Guard C:** Every non-deleted dm: row with participants has a ParseDMKey-valid key

All guards enforce rule 14 floors (minimum rows examined to prevent vacuous passes).
