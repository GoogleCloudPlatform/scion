# DEF-15 Phase 1+2: DeriveConversationKey Foundation

**Date:** 2026-08-27
**Branch:** scion/ca-msg-em6
**Scope:** pkg/messaging

## Summary

Implemented the canonical `DeriveConversationKey` function that collapses five
separate conversation key derivation paths into one. This is the foundation for
the DEF-15 security fix that prevents a malformed dm: key from falling through
to the thread-key path and creating a defective row.

## Changes

### New: `pkg/messaging/derive_key.go`

- `KeyInputs` struct holding all derivation inputs
- `DeriveConversationKey(KeyInputs)` — single canonical function for all
  external_ref construction, with three branches:
  - Case 1: `dm:`-prefixed ThreadID — parse, re-derive, verify canonicality,
    return verbatim (never normalize)
  - Case 2: Non-dm ThreadID — validate ProjectID, return `thread:<proj>:<tid>`
  - Case 3: Empty ThreadID — derive from principal pair via DMConversationKey
- `ResolveOrCreateConversationByKey` — shared upsert step used by delegating
  conversation.go functions

### New: `pkg/messaging/derive_key_test.go`

Golden vector table test covering all 3 success branches and 10 error branches:
- Malformed dm key, unknown kind, invalid UUID, non-canonical UUID (uppercase),
  non-canonical token order, unhyphenated UUID, empty projectID, empty senderID,
  unknown senderKind, invalid senderID UUID
- Verbatim return assertion for case 1

### Modified: `pkg/messaging/conversation.go`

- `ResolveOrCreateThreadConversation` now delegates to `DeriveConversationKey`
  with distinct refusal log text ("conversation key derivation refused") per
  Change 5
- `ResolveThreadConversationForRead` delegates to `DeriveConversationKey`;
  removed `projectID == ""` from early return so dm:-prefixed ThreadIDs work
  without a projectID

### Modified: `pkg/messaging/conversation_test.go`

- AC-DEF15-5 write-then-read tests for both dm: and non-dm ThreadIDs
- Distinct log text assertion for Change 5
- DM key with empty projectID read-path test

## Acceptance Criteria Met

- AC-DEF15-2: Golden vector table covers all 3 success + all error branches
- AC-DEF15-5: Write-then-read test passes (same inputs produce same row)
- All existing tests pass (`go test ./pkg/messaging/... ./pkg/hub/... -count=1`)
- Canonicality comment present in derive_key.go
- Both thread functions delegate to DeriveConversationKey
- Distinct log text for refusal vs resolution failure
