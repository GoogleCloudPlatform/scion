# S2 Phase 4: Backfill Service for Conversation Attribution

**Date:** 2026-08-27
**Author:** dev-backfill
**Branch:** scion/ca-msg-em2

## Summary

Implemented the Phase 4 backfill service that scans existing messages and groups
them into Conversation records. The service stamps each message with the
appropriate `conversation_id`, enabling the transition from legacy flat messages
to the structured conversation model.

## Files Created

- `pkg/messaging/backfill.go` — BackfillService, BackfillConfig, BackfillResult
- `pkg/messaging/backfill_test.go` — 19 test cases covering all acceptance criteria

## Design Decisions

### Conversation Key Strategy
- **Direct conversations:** Canonical key from sorted `(kind:id, kind:id)` pairs
  within a project. Sorting ensures `(A→B)` and `(B→A)` map to the same conversation.
- **Thread conversations:** Key from `(projectID, threadID)`. Kind is "group".
- Keys are stored as `ExternalRef` on the Conversation record, enabling
  idempotent upserts via `UpsertConversationByExternalRef`.

### Hazard (a): Email-Based DM Keys
Legacy messages with non-UUID sender/recipient IDs (e.g. `alice@example.com`)
are flagged as "inferred". A conversation is still created and stamped, but the
result tracks `HazardAEmailCount` for reporting.

### Hazard (b): DefaultAgent Slug-or-UUID Union
Agent references are resolved exclusively through `NormalizeAgentRef` (binding
decision D2). Three outcomes:
- UUID → used directly as DefaultAgentID
- Slug resolved → UUID used, hazard-B count incremented
- Slug not found (deleted agent) → drift state set to "orphaned"
- Invalid ref (neither UUID nor slug) → treated as inferred

### Three Operating Modes
1. **Dry-run:** Computes statistics without any writes (no conversations created,
   no messages stamped).
2. **Idempotent:** Skips messages that already have a `conversation_id`.
3. **Resumable:** Accepts a checkpoint message ID; only processes messages created
   after the checkpoint's `created_at` timestamp.

## Test Coverage

| Test | What it verifies |
|------|-----------------|
| NormalDirectMessages | Two-direction pair grouping, separate conversations |
| ThreadBasedGrouping | Thread ID creates group conversation |
| BroadcastedMessagesSkipped | Broadcast exclusion |
| HazardA_EmailBasedDMKeys | Non-UUID sender flagged as inferred |
| HazardA_BothSidesEmail | Both sides non-UUID |
| HazardB_SlugResolution | Slug resolved via NormalizeAgentRef |
| HazardB_DeletedAgent | Deleted agent → orphaned drift state |
| HazardB_InvalidAgentRef | Invalid ref → inferred |
| DryRun | No side effects, statistics only |
| Idempotent | Second run skips already-stamped messages |
| Resumable | Checkpoint skips earlier messages |
| ConversationParticipants | Both sender and recipient added |
| ConversationExternalRef | Upsert key set correctly |
| MixedDirectAndThread | Same participants, different key types |
| CheckpointNotFound | Error on invalid checkpoint |
| LastCheckpointTracked | Result tracks last processed ID |
| DefaultBatchSize | Empty project handled gracefully |
| ParsePrincipal | Helper unit tests |
| IsValidUUID | Helper unit tests |

## Verification

- `go build ./...` — passes
- `go test ./pkg/messaging/... -count=1` — all tests pass (0.005s)
