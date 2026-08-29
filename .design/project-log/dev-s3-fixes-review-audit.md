# S3 Envelope Review/Audit Fix Round

**Date:** 2026-08-27
**Agent:** dev-s3-fixes
**Branch:** scion/ca-msg-em3

## Summary

Applied six fixes from code review and security audit of S3 Envelope (phases 6, 7, 9).

## Changes

### Fix 1: Compose validators (Review R1+R2)
- `ValidateMessage()` now delegates to `msg.Validate()` for structural checks (ID, From, Kind, Visibility, kind/intent/event), then adds domain-level checks (ConversationID, body size, attachments, reply_to_id).
- Eliminates duplicated switch block between `Message.Validate()` and `ValidateMessage()`.
- Added `TestValidateMessage_MissingID` test.

### Fix 2: Metadata + channel validation in ValidateLegacyMessage (Audit M1)
- Added metadata entry count, key size, and value size limits to `ValidateLegacyMessage`.
- Added channel regex check (`IsValidChannel`) to `ValidateLegacyMessage`.
- Added `IsValidChannel()` helper to `pkg/messages/types.go` (only change to that file).
- Added five tests: channel invalid/valid chars, metadata entry count, key size, value size.

### Fix 3: Wire ValidateCrossProjectAddressees (Audit HIGH / AC-33)
- Added `ValidateMessageAddressees()` composition function in `validate.go`.
- Wired cross-project check into `handleAgentMessage` for the mentions path: before dispatch, resolved mention agents are checked to ensure they all belong to the same project as the primary recipient.
- Added tests for `ValidateMessageAddressees` (valid + cross-project rejected).

### Fix 4: Sanitize project IDs from error (Audit M3)
- Removed project ID values from `ValidateCrossProjectAddressees` error message.
- Updated test to assert project IDs are NOT disclosed.

### Fix 5: Validation on outbound/broadcast handlers (Audit M2)
- Added `ValidateLegacyMessage` call in `handleAgentOutboundMessage` after building the StructuredMessage.
- Added `ValidateLegacyMessage` call in `handleProjectBroadcast` after assembling the request.

### Fix 6: PrincipalRef length limit (Audit L1)
- Added `MaxPrincipalRefLength = 512` constant and length check to `ValidatePrincipalRef`.
- Added tests for over-limit and at-limit PrincipalRef values.

### Pre-existing bug fix: buildPrincipalRef raw UUID handling
- `buildPrincipalRef` in `envelope_compat.go` was using raw UUID SenderIDs directly as PrincipalRefs, which fail validation (no colon prefix). Fixed to derive the kind prefix from the Sender name field when the ID is a raw identifier.
- This fixed pre-existing test failures in `handleAgentMessage` tests and enables the new outbound validation.

### Test adaptations for new validation
- `TestOutboundMessage_UnknownTypeIsChargedAsAgentTraffic` — updated to expect 400 rejection (previously unknown types were silently accepted on the outbound path).
- `TestOutboundMessage_AttachmentsLinkedToMessage` — removed ThreadID from test (not relevant to the attachment linking being tested; thread_id without channel is now caught by validation).

## Files Modified

- `pkg/messaging/validate.go` — composed validators, added ValidateMessageAddressees, sanitized error
- `pkg/messaging/validate_test.go` — added MissingID, ValidateMessageAddressees, updated project-id assertion
- `pkg/messaging/validate_compat.go` — added metadata/channel checks
- `pkg/messaging/validate_compat_test.go` — added metadata/channel tests
- `pkg/messaging/envelope.go` — added PrincipalRef length limit
- `pkg/messaging/envelope_test.go` — added PrincipalRef length tests
- `pkg/messaging/envelope_compat.go` — fixed buildPrincipalRef for raw UUID IDs
- `pkg/messaging/envelope_compat_test.go` — added buildPrincipalRef tests
- `pkg/messages/types.go` — added IsValidChannel helper
- `pkg/hub/handlers_agent_messaging.go` — wired ValidateLegacyMessage into outbound/broadcast, wired cross-project mention check
- `pkg/hub/handlers_agent_messaging_test.go` — updated unknown type test
- `pkg/hub/attachments_agent_test.go` — removed ThreadID from attachment test

## Verification

- `go build ./...` — passes
- `go test ./pkg/messaging/...` — passes
- `go test ./cmd/...` — passes
- `go test ./pkg/messages/...` — passes
- `go test ./pkg/hub/...` — passes
