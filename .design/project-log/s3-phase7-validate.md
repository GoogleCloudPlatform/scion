# Phase 7: Validation Choke Point

**Date:** 2026-08-27
**Agent:** dev-validate
**Branch:** scion/ca-msg-em3
**Status:** Complete

## Summary

Implemented a single validation choke point for the new message envelope types
and wired it into all three inbound paths. This closes the defect where
`StructuredMessage.Validate()` was never called on hub inbound paths (findings
section 6).

## Deliverables

### 1. `pkg/messaging/validate.go`
- `ValidateMessage(msg *Message) error` — checks ConversationID, From, Kind,
  intent/event mutual exclusivity, body size limits, attachment count,
  visibility, and reply_to_id.
- `ValidateAddressees(addrs []Addressee, msg *Message) error` — checks
  PrincipalKind, PrincipalID, Via, DeliveryState, and duplicate detection
  using a deterministic `kind:id` composite key.
- `ValidateCrossProjectAddressees(ctx, agentStore, addrs) error` (AC-33 / DEF-2)
  — ensures all agent addressees belong to the same project. User addressees
  are exempt. Names conflicting projects in the error message per design §2.4.1.
- `AgentProjectLookup` interface — minimal store dependency for the cross-project
  check.

### 2. `pkg/messaging/validate_compat.go`
- `ValidateLegacyMessage(msg *StructuredMessage) error` — the adapter that makes
  old envelopes go through the new validation choke point. Checks legacy-specific
  invariants (thread_id requires channel, type enum, channel length) then
  converts via `MapLegacyEnvelope` and runs `ValidateMessage` and
  `ValidateAddressees`. Sets a synthetic ConversationID since legacy messages
  don't have one yet.

### 3. Wiring into inbound paths (AC-8)
- **Hub handler** (`handlers_agent_messaging.go`): validation after message
  assembly, before mentions cap.
- **Broker inbound** (`handlers_broker_inbound.go`): validation after "!"
  interrupt processing, before dispatch.
- **CLI** (`cmd/message.go`): validation after `buildStructuredMessage` in the
  primary send and broadcast paths.

### 4. Tests
- `validate_test.go`: 30 tests covering every ValidateMessage rule, every
  ValidateAddressees rule, and AC-33 cross-project check (including the
  Rule 10 load-bearing test).
- `validate_compat_test.go`: 12 tests covering the legacy adapter, including the
  Teams regression (channel="" + thread_id=set).
- `handlers_validation_integration_test.go`: 3 integration tests proving AC-8
  (invalid messages rejected at hub handler and broker inbound paths; valid
  messages pass through).

## Acceptance Criteria Status

| AC | Description | Status |
|----|-------------|--------|
| AC-8 | Validate() invoked on all three inbound paths | ✓ |
| AC-33 | No single message has agent addressees in >1 project | ✓ |
| Teams regression | channel="" + thread_id=set rejected at hub boundary | ✓ |
| Rule 10 | Every validation rule has a failing-when-removed test | ✓ |
| Build | `go build ./...` passes | ✓ |
| Tests | `go test ./pkg/messaging/... ./cmd/...` passes | ✓ |

## Design Decisions

1. **Synthetic ConversationID for legacy messages**: Legacy StructuredMessages
   don't carry a ConversationID (that's resolved by Phase 4/5 conversation
   attribution). The compat adapter sets `"legacy-pending"` to pass the
   required-field check without weakening the validator.

2. **Duplicate key is `kind:id`**: The addressee deduplication key uses
   `PrincipalKind + ":" + PrincipalID` — a single constructor, so two code
   paths cannot produce the same key differently.

3. **Agent sender in broker integration tests**: The broker inbound handler
   checks user identity (requires store lookup) before reaching validation.
   Integration tests use `agent:` senders to bypass this, testing validation
   in isolation from user identity resolution.
