# DEF-15 Phase 6: Migration Sweep for thread:%:dm:% Rows

**Date:** 2026-08-27
**Agent:** dev-migration-sweep
**Branch:** scion/ca-msg-em6
**Commit:** 05b37e89

## Summary

Extended `pkg/messaging/dm_migration.go` with `RepairDEF15Artifacts`, a new
migration function that finds and repairs DEF-15 artifact conversation rows —
rows whose `external_ref` matches `thread:<projectID>:dm:<rest>` due to a
dm:-prefixed ThreadID being incorrectly wrapped in a `thread:` key prefix.

## What Was Built

### New classification: `convClassDEF15Artifact`

Added to the existing `convClass` enum for rows matching the
`thread:<projectID>:dm:<rest>` pattern.

### `RepairDEF15Artifacts` method

Scans all conversations (no kind filter) for the DEF-15 pattern, then for each:

1. **Extracts** the dm: key suffix via `SplitN(":", 3)`
2. **Validates** with `messages.ParseDMKey` — invalid keys are unrepairable
3. **Checks canonicality** by re-deriving with `messages.DMConversationKey` — non-canonical keys are unrepairable
4. **Repairs repairable rows:**
   - If a correct row already exists: merges (messages re-stamped, DEF-15 row soft-deleted)
   - If no correct row: updates in place (external_ref → dm: key, kind → "direct", project_id → NULL)
   - Rebuilds participants from the key (key wins)
5. **Reports** structured `DEF15RepairResult` with per-row detail

### Tests (AC-MIGRATE-1)

- **Three-row fixture test**: repairable, unrepairable, and merge-conflict scenarios
- **Non-canonical key test**: verifies uppercase-UUID keys are left byte-identical
- **Idempotency test**: second run finds zero artifacts
- **`isDEF15Artifact` unit test**: pattern matcher edge cases

## Files Modified

- `pkg/messaging/dm_migration.go` — new types + methods (~240 lines)
- `pkg/messaging/dm_migration_test.go` — 4 new test functions (~240 lines)

## Verification

```
go test ./pkg/messaging/... -count=1  → PASS
go test ./pkg/messages/... -count=1   → PASS
```
