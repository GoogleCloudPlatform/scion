# E-1: Native Chat Validation Bypass Fix

**Date:** 2026-08-27
**Branch:** scion/ca-msg-em3
**Task:** Wire ValidateLegacyMessage into native chat inbound path

## Summary

The native chat send path (`handlers_chat_v2.go:sendAgentRouted`) constructed
and dispatched StructuredMessages without calling `ValidateLegacyMessage`. This
was a gap in the AC-8 validation choke point — all four inbound surfaces must
validate through the same choke point.

## Changes

### 1. Validation Wired into sendAgentRouted (`pkg/hub/handlers_chat_v2.go`)

Added `messaging.ValidateLegacyMessage(msg)` call after the StructuredMessage is
fully constructed (including attachment metadata and mention metadata) but before
persist and dispatch. Uses `writeError(w, 400, "VALIDATION_ERROR", ...)` error
pattern consistent with the native chat handler.

### 2. Rule 10 Integration Test (`pkg/hub/handlers_validation_integration_test.go`)

Added `TestNativeChatPath_RejectsInvalidMessage` which proves the validation call
is load-bearing. The test sends a message with empty content but valid attachment
through the chat endpoint. The handler allows this (attachments present), but the
constructed StructuredMessage has Msg="" which ValidateLegacyMessage rejects.

**Rule 10 proof:** When the ValidateLegacyMessage call is removed, the test fails
(empty body reaches the store which returns 500). With the call present, the test
passes (400 with "msg field is required").

### 3. Validation Exemptions Doc (`pkg/messaging/VALIDATION_EXEMPTIONS.md`)

Documented three server-generated emitters exempt from ValidateLegacyMessage:
- Mention fan-out (sendAgentRouted) — derived from already-validated primary message
- Notification dispatch (notifications.go) — server-internal lifecycle signals
- Scheduler message (server.go) — previously-validated scheduled event payloads

## Files Changed

- `pkg/hub/handlers_chat_v2.go` — added messaging import + ValidateLegacyMessage call
- `pkg/hub/handlers_validation_integration_test.go` — added TestNativeChatPath_RejectsInvalidMessage
- `pkg/messaging/VALIDATION_EXEMPTIONS.md` — new file documenting exemptions
- `.design/project-log/e1-native-chat-validation.md` — this log entry

## Verification

- `go build ./...` passes
- `go test ./pkg/messaging/...` passes
- `go test ./pkg/hub/ -run TestNativeChatPath_RejectsInvalidMessage` passes
- `go test ./cmd/...` passes
- Rule 10 verified: test fails when validation call is removed
