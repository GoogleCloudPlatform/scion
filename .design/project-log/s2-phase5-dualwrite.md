# S2 Phase 5: Dual-Write Conversation Resolution and Divergence Logging

**Date:** 2026-08-27
**Agent:** dev-dualwrite
**Branch:** scion/ca-msg-em2

## Summary

Implemented Phase 5 of the messaging conversation model refactor (S2). All
message send paths in the Hub server now resolve-or-create a Conversation
record and stamp `conversation_id` on the `store.Message` before persisting it.
Read paths remain unchanged per S2 contract.

## Changes

### New Files

- **`pkg/messaging/divergence.go`** — `DivergenceEntry` struct, `LogDivergence`
  function (INFO for matches, WARN for divergences), `DivergenceCounter` with
  atomic metrics, and helper functions `DirectMessageExternalRef` and
  `OldRoutingFromMessage` for building deterministic routing keys.

- **`pkg/messaging/divergence_test.go`** — Unit tests for external ref
  determinism, old-routing computation, divergence logging at correct levels,
  and concurrent counter safety.

- **`pkg/messaging/conversation.go`** — `ResolveOrCreateDMConversation` helper
  that calls `UpsertConversationByExternalRef` with a deterministic
  `dm:{sorted(senderID,recipientID)}` external_ref. Failures are logged and
  return empty string (non-fatal contract).

### Modified Files

- **`pkg/hub/handlers_agent_messaging.go`**
  - `handleAgentOutboundMessage` (agent→user): stamps `conversation_id` on
    `storeMsg` before persist.
  - `handleAgentMessage` (user/agent→agent): stamps `conversation_id` on
    `storeMsg` before persist.
  - `handleGroupMessage` (fan-out): stamps `conversation_id` on each
    per-recipient `storeMsg` for both agent and user recipients.
  - Broadcasts (`broadcastDirect`): **skipped** — broadcasts are ephemeral and
    do not belong to a conversation.
  - Added `messaging` package import.

- **`pkg/hub/messagebroker.go`**
  - `deliverToUser` (broker callback): stamps `conversation_id` on user-bound
    messages (skips broadcasts).
  - `deliverToAgent` (broker callback): stamps `conversation_id` on
    agent-bound messages (skips broadcasts).
  - Added `messaging` package import.

## Design Decisions

1. **Deterministic external_ref** — `dm:{sorted(idA,idB)}` ensures the same
   conversation is resolved regardless of send direction. This is idempotent
   via the `UpsertConversationByExternalRef` upsert pattern.

2. **Non-fatal resolution** — All conversation resolution failures log an error
   and continue without setting `conversation_id`. Message delivery is never
   broken by a conversation resolution failure.

3. **Broadcasts skipped** — Per the design brief, broadcast messages are
   ephemeral and do not receive conversation assignment.

4. **Divergence logging** — Every stamped message emits a divergence log entry
   comparing old-model routing (thread_id or sender/recipient pair) with the
   new conversation_id. This is the gate for S4 (read-switch). Currently all
   entries log as `Match: true` since the new model is write-only and there is
   no read path to compare against yet.

## Verification

- `go build ./...` — passes
- `go test ./pkg/messaging/... -count=1` — passes (all tests including new divergence tests)
- `go test ./pkg/hub/... -count=1` — 15 pre-existing failures (verified by
  running without changes); no new failures introduced
