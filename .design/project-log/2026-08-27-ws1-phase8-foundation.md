# WS-1: Phase 8 Foundation (S4 Messaging Refactor)

**Date**: 2026-08-27  
**Agent**: ca-msg-em4  
**Branch**: scion/ca-msg-em4  
**Base**: scion/messaging-v2  

## Summary

Implemented three foundation deliverables required before the Phase 8 read-switch can be built.

## Deliverables

### DEF-1: Participant-level auth on conversation resolution

**Problem**: `resolveConvByID` checked project isolation (AC-30) but did NOT check whether the sender was a participant. Any principal in the same project could resolve any conversation — a HIGH finding from S1's security audit.

**Solution**: Added post-resolution authorisation in the `Resolve()` function that runs after every resolution branch returns. Rules vary by conversation kind:

- **Direct conversations** (`Kind == "direct"`): sender MUST be a participant. This applies regardless of grammar (conv:<id>, @<agent>, @<email>).
- **Group conversations** (`Kind == "group"`): project membership authorises access (already enforced). No participant check — agents must be able to post in rooms they've never spoken in.
- **Global DMs** (nil ProjectID): participant check is the ONLY auth gate.

**Files**: `pkg/messaging/resolve.go` — added `checkPostResolutionAuth()`, `requireParticipant()` functions.

**Rule 10 Tests** (3 tests in `resolve_test.go`):
1. `TestResolveConvByID_RejectsNonParticipant` — non-participant rejected on direct conv via conv:<id>
2. `TestResolve_GroupConv_AcceptsNonParticipantProjectMember` — project member accepted on group conv without participation
3. `TestResolve_DirectConv_RejectionGrammarIndependent` — proves auth check is grammar-independent (conv:<id> vs @<agent>)

All three fail when the participant check is removed (mutation verified).

### DEF-3: Independent divergence source of truth

**Problem**: `ComputeDivergenceMatch` compared old-model routing keys against the conversation's external_ref, but both were derived from the same input fields. They CANNOT disagree — a clean report meant "resolution did not fail", not "the new model routes where the old model routed".

**Solution**: Added `CheckConversationConsistency()` — an independent check that queries prior persisted messages and compares their `conversation_id` against the newly resolved one.

**Files**:
- `pkg/store/models.go` — added `ConversationID` field to `MessageFilter`
- `pkg/store/entadapter/message_store.go` — wired `ConversationID` into ent query builder
- `pkg/messaging/divergence.go` — added `MessageQueryStore` interface and `CheckConversationConsistency()` function
- `pkg/hub/handlers_agent_messaging.go` — added calls at 4 dual-write sites
- `pkg/hub/messagebroker.go` — added calls at 2 dual-write sites

**Rule 10 Test**: `TestCheckConversationConsistency_DetectsMismatch` — stores a prior message with conv-A, checks a new message with conv-B for the same thread, asserts mismatch. Removing the comparison makes the function return true → test fails.

### D3: Runtime flag + live divergence endpoint

**Part A — MessagingSettings opsettings section**:
- Added `MessagingSettings` struct with `ConversationReadSwitch *bool` field
- Registered as DB-only section (no KoanfPaths, like maintenance)
- Added hand-written JSON schema
- Added `ConversationReadSwitch()` accessor on `OperationalSettings` — hot-reloadable via DB-backed cache

**Part B — Divergence HTTP endpoint**:
- `GET /api/v1/admin/messaging/divergence` — admin-authenticated
- Returns `{ matches, mismatches, total, read_switch_enabled }`
- Reads from `messaging.DivergenceMetrics` and `MessagingSettings`

**Files**:
- `pkg/config/opsettings/sections.go` — `MessagingSettings` struct
- `pkg/config/opsettings/registry.go` — section + schema registration
- `pkg/config/opsettings/opsettings_test.go` — updated section expectations
- `pkg/hub/operational_settings.go` — `ConversationReadSwitch()` accessor
- `pkg/hub/admin_messaging_divergence.go` — new handler file
- `pkg/hub/server.go` — route registration

## Test Results

- `go test ./pkg/messaging/...` — all green
- `go test ./pkg/store/...` — all green
- `go test ./pkg/config/opsettings/...` — all green
- `go test ./pkg/hub/` — all green (full suite)

## Rule 10 Mutation Verification

All Rule 10 tests verified: removing the guarded check causes each test to fail as expected.
