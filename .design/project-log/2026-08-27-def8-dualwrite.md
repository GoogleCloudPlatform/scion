# DEF-8 Dual-Write Path Convergence

**Date:** 2026-08-27
**Agent:** dev-def8-dualwrite
**Branch:** scion/ca-msg-em6

## Summary

Updated the dual-write path (`ResolveOrCreateDMConversation`, `ResolveDMConversationForRead`)
to use `messages.DMConversationKey` instead of the old `DirectMessageExternalRef`. This
converges both write paths (resolver and dual-write) onto the same kind-encoded DM key
format (`dm:<kind>:<uuid>:<kind>:<uuid>`), fixing the core DEF-8 defect where both paths
created different rows for the same principal pair.

## Changes

### Core function updates (pkg/messaging/conversation.go)
- `ResolveOrCreateDMConversation` signature changed: now accepts `senderKind, senderID,
  recipientKind, recipientID` instead of just `senderID, recipientID`.
- `ResolveDMConversationForRead` signature changed: now accepts `idAKind, idA, idBKind, idB`
  instead of just `idA, idB`.
- Both now use `messages.DMConversationKey()` to build the external_ref, producing
  kind-encoded keys that match the production regex.

### New helper (pkg/messages/dm_key.go)
- `PrincipalKindFromAddress()` — extracts "user" or "agent" from an address string like
  `"user:alice"` or `"agent:my-bot"`. Used by hub call sites where the kind must be
  derived from the Sender/Recipient field.

### Hub call site updates (10 total)
- **7 ResolveOrCreateDMConversation** call sites updated across:
  - `handlers_agent_messaging.go` (4 sites)
  - `handlers_broker_inbound.go` (1 site)
  - `messagebroker.go` (2 sites)
- **3 ResolveDMConversationForRead** call sites updated across:
  - `handlers_messages.go` (2 sites)
  - `handlers_chat_v2.go` (1 site)

### Tests
- **Conformance test** (`TestDMConversationKey_MatchesProductionRegex`): verifies all keys
  match the production regex from `handlers_chat_v2.go:391`.
- **Golden test vectors** (`TestDMConversationKey_GoldenVectors`): 5 hardcoded deterministic
  vectors for cross-language conformance (Go + TS).
- **True cross-path AC-DEF8-1 test** (`TestAC_DEF8_1_CrossPath_DualWriteAndResolverConverge`):
  exercises BOTH `ResolveOrCreateDMConversation` and `Resolve(@agent)`, asserts same
  conversation ID and exactly one row.
- Updated existing `conversation_test.go` and `handlers_read_switch_test.go` for new signatures.

### What was NOT changed (per instructions)
- `DirectMessageExternalRef` in `divergence.go` — still used by divergence comparison.
- Divergence comparison logic — untouched.
- Hub helper functions (`validDMKey`, `parseAgentDMKey`, etc.) — assessed but not refactored.

## Hub Helper Refactoring Size Assessment (Deliverable 5)

Five hub helpers independently string-split DM keys instead of using `messages.ParseDMKey`:

| Helper                  | Production call sites | Files                          |
|-------------------------|-----------------------|--------------------------------|
| `validDMKey`            | 6                     | handlers_chat_v2.go            |
| `parseAgentDMKey`       | 3                     | handlers_chat_v2.go            |
| `dmUserParticipants`    | 6                     | handlers_chat_v2.go, events.go |
| `resolveDMPeer`         | 1                     | handlers_chat_v2.go            |
| `resolveProjectFromDMKey` | 2                   | handlers_chat_v2.go            |
| **Total**               | **18**                |                                |

**Assessment:** Medium-sized refactoring. Each helper would call `messages.ParseDMKey` instead
of `strings.Split`, then use the returned `kindA, idA, kindB, idB` fields. The 18 call sites
are mechanical — no logic changes needed at the call sites themselves. The main risk is
that `ParseDMKey` validates strictly (rejects malformed keys), while the current helpers
are lenient. Callers that pass pre-validated keys (via `validDMKey` guard) are safe; callers
that skip validation need error handling added. Estimated ~100 lines changed, ~3 test files
updated.

## Verification

- `go build ./...` — passes
- `go test ./pkg/messages/...` — passes
- `go test ./pkg/messaging/...` — passes
- `go test ./pkg/hub/... -run TestUpsert|TestReadSwitch|TestBrokerInbound` — passes
