# Phase 2: ConversationStore Interface + Ent Adapter

**Date:** 2026-08-27
**Branch:** dev-store (based on dev-schema)
**Agent:** dev-store

## Summary

Implemented the store layer for the messaging refactor (Phase 2), building on
the ent schemas delivered in Phase 1 (dev-schema).

## Deliverables

### 1. Store Models (`pkg/store/models.go`)
- `Conversation` — conversation container with project association, kind/surface,
  external reference, default agent, drift state, and soft-delete timestamps
- `ConversationParticipant` — through-table linking principals to conversations
  with role and temporal metadata
- `MessageAddressee` — per-message addressing record with delivery tracking
- `ConversationFilter` — query filter for listing conversations

### 2. ConversationStore Interface (`pkg/store/store.go`)
- Full CRUD: Create, Get, Update, Delete (soft), List with pagination
- `UpsertConversationByExternalRef` — idempotent create-or-update keyed on
  (surface, external_ref); race-safe via the partial unique index
- Participant lifecycle: Add, Remove (soft), List, GetConversationsForPrincipal
- Addressee management: Add, List, UpdateDeliveryState
- Embedded in the top-level `Store` interface

### 3. Ent Adapter (`pkg/store/entadapter/conversation_store.go`)
- Follows message_store.go patterns: dedicated struct, conversion helpers,
  consistent error mapping
- DefaultAgentID validation rejects non-UUID values (slugs not accepted)
- Soft-delete: DeleteConversation sets deleted_at; Get and List exclude
  soft-deleted rows
- Cursor-based pagination using the same encoding as message_store
- UpsertConversationByExternalRef uses query-then-create/update pattern
  (SQLite does not support ON CONFLICT with partial unique indexes); a
  constraint violation on insert triggers a retry that updates the existing row

### 4. CompositeStore Wiring (`pkg/store/entadapter/composite.go`)
- Added `*ConversationStore` field and `NewConversationStore(client)` to
  `NewCompositeStore`

### 5. Tests (`pkg/store/entadapter/conversation_store_test.go`)
24 test cases covering:
- Basic CRUD: create, get, update, soft-delete, list
- Soft-deleted conversations excluded from Get and List
- List filters: kind, surface, project_id
- Cursor-based pagination
- DefaultAgentID validation (non-UUID rejected)
- UpsertConversationByExternalRef: create-if-not-exists, update-if-exists
- Concurrent upsert: 5 goroutines converge on one conversation
- Different external_refs on same surface produce distinct conversations
- Partial unique index: soft-deleted row allows external_ref reuse
- Participant add/remove/list, duplicate rejection, default role
- GetConversationsForPrincipal (excludes left participants and soft-deleted convs)
- Addressee add/list/update-delivery-state, duplicate rejection
- Nil ProjectID (direct conversations)

## Verification
- `go build ./...` — clean
- `go test ./pkg/store/entadapter/ -v` — all tests pass (existing + new)
