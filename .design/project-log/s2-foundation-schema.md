# S2 Foundation: Add conversation_id to Message Schema

**Date:** 2026-08-27
**Author:** dev-foundation
**Phase:** S2 – Messaging Conversation Model Refactor

## Summary

Added the `conversation_id` field to the Message ent schema, store model, and
ent adapter. This is the foundational schema change that links existing messages
to Conversation records (created in S1). The field is optional and nullable,
allowing incremental population via backfill (Phase 4) and dual-write (Phase 5).

## Changes

### 1. Ent Schema (`pkg/ent/schema/message.go`)
- Added `conversation_id` as an optional, nillable UUID field after `thread_id`.
- Added an index on `conversation_id` for efficient lookups.

### 2. Ent Code Generation
- Ran `go generate ./pkg/ent/` — all generated files updated to include
  `ConversationID *uuid.UUID`, setters (`SetConversationID`,
  `SetNillableConversationID`, `ClearConversationID`), and query predicates.

### 3. Store Model (`pkg/store/models.go`)
- Added `ConversationID string` field to `Message` struct with
  `json:"conversationId,omitempty"` tag, placed after `ThreadID`.

### 4. Ent Adapter (`pkg/store/entadapter/message_store.go`)
- **`entMessageToStore()`**: Maps `*uuid.UUID` → `string` (empty string when nil).
- **`CreateMessage()`**: Parses and sets `conversation_id` when
  `msg.ConversationID` is non-empty.
- **`SetMessageConversationID()`**: New method that updates `conversation_id` on
  an existing message row. Used by Phase 4 backfill. Returns `ErrNotFound` if
  the message does not exist.

### 5. Store Interface (`pkg/store/store.go`)
- Added `SetMessageConversationID(ctx, messageID, conversationID string) error`
  to `MessageStore` interface.

## Verification

- `go build ./...` — passes
- `go test ./pkg/store/entadapter/... -count=1` — passes (5.3s)

## Notes

- The field is intentionally optional/nillable: existing rows will have
  `NULL` until the Phase 4 backfill populates them.
- No migration file is generated here — ent auto-migration handles schema
  changes at startup.
