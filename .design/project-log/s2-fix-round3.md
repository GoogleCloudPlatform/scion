# S2 Fix Round 3: Non-tautological divergence, thread dual-write, global DM ProjectID

**Date**: 2026-08-27
**Branch**: `scion/dev-fix-round3`
**Author**: dev-fix-round3

## Summary

Addressed three architect findings from S2 round 2 review:

### C-3: DM conversations must not have ProjectID

- Removed the `if projectID != "" { conv.ProjectID = &projectID }` block from `ResolveOrCreateDMConversation`
- Removed `projectID` from the function signature entirely
- DM conversations are now always created with `ProjectID = nil` (global per design 2.4.1)
- This prevents project-isolation bugs where DMs stamped with project X become inaccessible from project Y

### C-1: Non-tautological ComputeDivergenceMatch

- Added `ConversationResult` type carrying both `ConversationID` and `ExternalRef` from the database
- Changed `ResolveOrCreateDMConversation` to return `*ConversationResult` (nil on failure) instead of a string
- Rewrote `ComputeDivergenceMatch` signature to `(oldRouting, actualExternalRef, convID string)` — the comparison now uses the actual `ExternalRef` from the DB, not a value reconstructed from inputs
- Supports both DM comparison (`sender-recipient:` vs `dm:`) and thread comparison (`thread:{threadID}` vs `thread:{projectID}:{threadID}`)
- Added mandatory genuine disagreement test that constructs different sender/recipient pairs and asserts `match==false`
- Added thread disagreement test and routing-type-mismatch test

### C-2: Thread conversation resolution in dual-write

- Added `ResolveOrCreateThreadConversation` function for thread-based conversations with external ref format `thread:{projectID}:{threadID}`
- Thread conversations are project-scoped (ProjectID is set, unlike DMs)
- Updated all 6 call sites in `handlers_agent_messaging.go` and `messagebroker.go` to:
  - Route to `ResolveOrCreateThreadConversation` when `threadID != ""`
  - Route to `ResolveOrCreateDMConversation` when sender/recipient are present
  - Use `ConversationResult` for both conversation ID and actual external ref
  - Pass actual external ref to `ComputeDivergenceMatch`
- Preserved `!msg.Broadcasted` guards on broker call sites

## Files Changed

| File | Change |
|------|--------|
| `pkg/messaging/conversation.go` | Added `ConversationResult`, `ResolveOrCreateThreadConversation`; removed projectID from DM resolution; changed return type to `*ConversationResult` |
| `pkg/messaging/divergence.go` | Rewrote `ComputeDivergenceMatch` for non-tautological comparison with DM and thread support |
| `pkg/hub/handlers_agent_messaging.go` | Updated 4 call sites for new signatures and thread routing |
| `pkg/hub/messagebroker.go` | Updated 2 call sites for new signatures and thread routing |
| `pkg/messaging/conversation_test.go` | Updated DM tests for new signature; added thread conversation tests |
| `pkg/messaging/divergence_test.go` | Updated for new signature; added genuine disagreement, thread disagreement, routing-type-mismatch tests |

## Verification

- `go build ./...` passes
- `go test ./pkg/messaging/...` passes (55 tests, 0 failures)
