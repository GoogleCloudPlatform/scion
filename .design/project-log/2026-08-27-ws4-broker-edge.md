# WS-4: Broker Edge Conversation Resolution (Phase 11)

**Date**: 2026-08-27
**Agent**: dev-phase11-broker
**Branch**: scion/ca-msg-em4 (based on scion/messaging-v2)

## Summary

Implemented conversation resolution at the broker edge for all 5 broker
plugins. Each plugin now sends `surface`, `external_ref`, and `parent_ref`
with its inbound messages so the hub can resolve (or create) a conversation
before dispatching.

## Changes

### Hub (first commit)

- Extended `inboundMessageRequest` in `pkg/hub/handlers_broker_inbound.go`
  with `Surface`, `ExternalRef`, `ParentRef` fields.
- After message validation, the handler calls
  `store.UpsertConversationByExternalRef` when both Surface and ExternalRef
  are present. ExternalRef without Surface is rejected (AC-8 regression
  guard).
- Extended `MessageRequest` in `pkg/hub/handlers_agent_messaging.go` with
  the same fields for the Google Chat SDK path
  (`POST /api/v1/agents/{id}/message`).
- Added `SendStructuredMessageWithConv` to `hubclient.AgentService`
  interface and implementation.
- Updated `mockAgentService` in the A2A bridge test to satisfy the new
  interface method.

### Discord

- Populated conversation fields in `deliverInbound`: Surface = `"discord"`,
  ExternalRef = channel/thread snowflake, ParentRef = guild ID.
- Tests: payload verification + AC-8 regression (non-discord channel skips
  fields).

### Slack

- Populated conversation fields in `deliverInbound`: Surface = `"slack"`,
  ExternalRef = `"channelID:threadTS"` (or bare channelID for top-level),
  ParentRef = channel ID.
- Tests: threaded message, top-level message, and AC-8 regression.

### Telegram

- Added `telegramConvFields` helper shared by V1 and V2 brokers.
  Surface = `"telegram"`, ExternalRef = forum topic ID (or chat ID),
  ParentRef = chat ID.
- Updated all three payload construction sites (V1 `deliverInbound`, V2
  `deliverInbound`, V2 `deliverInboundWithFeedback`).
- Tests: forum topic, non-forum, nil cases, V1 delivery, AC-8 regression.

### Google Chat

- Added `gchatConvFields` helper. Surface = `"gchat"`,
  ExternalRef = thread path (`spaces/X/threads/Y`), ParentRef = space path.
- Updated all three `SendStructuredMessage` call sites to use
  `SendStructuredMessageWithConv`.
- Added `SendStructuredMessageWithConv` to test stub.
- Fixed pre-existing `UploadMedia` mock in `sendqueue_test.go`.
- Tests: threaded event, space-only, empty/nil events.

### Teams

- Added `teamsConvFields` helper. Surface = `"teams"`,
  ExternalRef = reply-to activity ID (or conversation ID),
  ParentRef = conversation ID.
- Updated `DeliverInbound` to populate conversation fields.
- Tests: thread reply, top-level, non-teams channel, nil cases.
- **AC-8 regression test**: explicitly verifies that `channel=""` with
  `thread_id` set does NOT produce conversation fields.

## Test Results

- `go test -count=1 ./pkg/hub/` (with -tags sqlite): PASS
- Discord plugin tests: PASS
- Slack plugin tests: PASS
- Telegram plugin tests: PASS
- Google Chat plugin tests: PASS
- Teams plugin tests: PASS

## Design Decisions

1. **Hub-side resolution over plugin-side**: Plugins send platform
   identifiers; the hub calls `UpsertConversationByExternalRef`. This keeps
   the store dependency in the hub and makes plugins simpler.

2. **Two handler paths**: The broker/inbound handler reads conversation
   fields from `inboundMessageRequest`. The agent/message handler (used by
   Google Chat) reads from `MessageRequest`. Both call the same store
   method.

3. **AC-8 guard**: ExternalRef without Surface is rejected at the hub
   boundary. Each plugin also guards against producing bare ExternalRef
   (e.g. Teams checks `msg.Channel == "teams"` before deriving fields).

4. **Non-fatal resolution**: If `UpsertConversationByExternalRef` fails,
   the hub logs the error and proceeds with dispatch. Message delivery is
   not blocked by conversation resolution failures.
