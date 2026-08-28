# B6+B7+B9: D-1 Guard Extraction, EnsureParticipant, nil-pe Guard

**Date:** 2026-08-28
**Branch:** ca-msg-em6-b6b7b9

## Summary

Three coupled fixes addressing the interaction between conversation resolution,
participant registration, and the DM immutability guard.

## Changes

### Deliverable 1: Shared D-1 guard predicate

Extracted the direct-conversation immutability guard from `AddParticipant` into
a shared function `checkDMParticipantKey(conv, principalKind, principalID)`.
Both `AddParticipant` and `EnsureParticipant` now route through this single
predicate, eliminating the risk of guard divergence (the exact defect class
that DEF-31 demonstrated).

**File:** `pkg/store/entadapter/conversation_store.go`

### Deliverable 2: EnsureParticipant (B6 un-leaving + B9 self-repair)

**Problem:** `ResolveOrCreateDMConversation` called `AddParticipant` on every
resolve. `AddParticipant` queries for `LeftAtNotNil()` rows and clears
`left_at`, silently overwriting a user's listing preference when they had
left/hidden a DM conversation (B6). The self-repair contract (retry on next
message) is what causes B6 (B9).

**Solution:** Split the intent:

- **AddParticipant** keeps its `ClearLeftAt` behavior for explicit re-join.
- **EnsureParticipant** (new) does insert-if-absent. If any row exists (active
  or soft-removed), it is left completely untouched including `left_at`.

`ResolveOrCreateDMConversation` now takes `ParticipantEnsurer` instead of
`ParticipantAdder` and calls `EnsureParticipant`.

**Files:**
- `pkg/store/entadapter/conversation_store.go` — `EnsureParticipant` implementation
- `pkg/store/store.go` — interface addition
- `pkg/messaging/conversation.go` — `ParticipantEnsurer` interface, signature change

### Deliverable 3: nil-pe guard (B7)

Added a nil check on the `ParticipantEnsurer` parameter before the participant
registration loop. A nil `pe` logs a warning and returns the `ConversationResult`
normally, honouring the function's non-fatal contract for participant registration.

**File:** `pkg/messaging/conversation.go`

## Test Coverage

1. **B7 nil-pe test** — `TestResolveOrCreateDMConversation_NilParticipantEnsurer`:
   Passes nil pe, asserts non-nil ConversationResult and no panic.

2. **B6 resolve-after-leave** — `TestEnsureParticipant_ResolveAfterLeave`:
   Full scenario: create DM, add both, soft-remove one, EnsureParticipant,
   assert left_at timestamp is exactly preserved.

3. **EnsureParticipant does not clear left_at** — `TestEnsureParticipant_DoesNotClearLeftAt`:
   Focused: pins the exact left_at timestamp before and after EnsureParticipant.

4. **D-1 guard shared predicate** — `TestEnsureParticipant_DM_ThirdPartyRejection`:
   Exercises BOTH AddParticipant and EnsureParticipant with a third party not
   named in the DM key. Both must reject with the same error.

5. **EnsureParticipant insert-if-absent** — `TestEnsureParticipant_InsertIfAbsent`:
   Creates row via EnsureParticipant, verifies it exists, calls again to verify
   idempotency.

6. **Mock updates** — Added `EnsureParticipant` to all three test mocks
   (`mockConversationUpserter`, `mockConversationStore`, `mockResolutionStore`).
   Updated existing tests that call `ResolveOrCreateDMConversation`.

## Design Rationale

The key insight is that `AddParticipant` conflates two intents: "join this
conversation" (explicit, should clear left_at) and "ensure the row exists for
listing" (implicit, must not touch existing state). Splitting these into
separate methods makes each intent explicit and prevents one from breaking the
other.
