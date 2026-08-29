# WS-2 Phase 8: Read Switch

**Date:** 2026-08-27
**Agent:** dev-phase8-readswitch
**Branch:** scion/ca-msg-em4

## Summary

Implemented Phase 8 of the S4 messaging refactor: the read-switch that gates
message read/query paths on the `ConversationReadSwitch` runtime flag. When the
flag is ON, read paths use `ConversationID` instead of `ThreadID/Channel`. Also
closed the 7th dual-write gap in broker inbound.

## Changes

### 1. Read paths gated on ConversationReadSwitch

Three read paths modified to branch on the runtime flag:

- **handleMessages (user inbox)** — When flag ON and an `agent` query param is
  provided, resolves the DM conversation between user and agent and adds
  `ConversationID` to the filter.

- **handleAgentMessages** — When flag ON, resolves either a thread conversation
  (if `thread_id` param present) or a DM conversation (agent + user), then
  adds `ConversationID` to the filter.

- **handleConversationHistory** — When flag ON, resolves the conversation from
  the thread key (DM keys parse participant IDs; thread keys look up the topic
  for projectID), then queries by `ConversationID` instead of
  `Channel="web" + ThreadID=key`. Falls back to old path if the conversation
  doesn't exist yet (pre-dual-write data).

### 2. Read-only conversation lookup

Added `GetConversationByExternalRef` to the store interface and entadapter
implementation — a read-only lookup by `(surface, external_ref)` that returns
`ErrNotFound` when no matching active conversation exists. This avoids creating
conversations as a side-effect of reads.

Added two helpers to `pkg/messaging/conversation.go`:
- `ResolveThreadConversationForRead` — read-only thread lookup
- `ResolveDMConversationForRead` — read-only DM lookup

Both return nil on miss, matching the non-fatal contract of the write-path
counterparts.

### 3. Broker inbound dual-write (7th call site)

`handleBrokerInbound` now stamps `conversation_id` on inbound messages using
the same resolve-or-create + divergence logging pattern as the other 6 sites:
- Resolves thread or DM conversation (skips broadcasts)
- Stamps `storeMsg.ConversationID`
- Computes and logs divergence
- Runs DEF-3 independent consistency check

### 4. Tests

Eight tests covering all required scenarios:

| Test | Scenario |
|------|----------|
| `TestReadSwitch_FlagOFF_AgentMessages_UsesOldPath` | Flag OFF backward compat — all messages returned |
| `TestReadSwitch_FlagOFF_UserInbox_UsesOldPath` | Flag OFF inbox — old RecipientID path |
| `TestReadSwitch_FlagON_AgentMessages_UsesConversationID` | Flag ON — only conv_id messages returned |
| `TestReadSwitch_FlagON_UserInbox_UsesConversationID` | Flag ON — inbox filters by conv_id |
| `TestReadSwitch_HotReloadToggle` | OFF→ON→OFF toggle without restart |
| `TestReadSwitch_ConversationHistory_FlagOFF` | Thread history — old Channel+ThreadID path |
| `TestReadSwitch_ConversationHistory_FlagON` | Thread history — ConversationID path |
| `TestBrokerInbound_DualWrite_StampsConversationID` | Broker inbound stamps conv_id |

## Test Results

Full suite (`go test -count=1 ./pkg/hub/`) passed twice:
- Run 1: ok (216.620s)
- Run 2: ok (214.511s)

Messaging package tests: ok (0.008s)
Store package tests: ok

## Flag-Flip Question

> What happens to messages written while flag ON if operator flips it OFF
> mid-exercise? Is snapshot restore needed or is the switch clean?

**The switch is clean — no snapshot restore needed.**

When the flag is toggled OFF after being ON:

1. **Write path is unaffected.** Dual-write always runs regardless of the flag.
   Every message gets `conversation_id` stamped whether the read switch is ON
   or OFF. The flag only controls the _read_ path.

2. **Read path reverts instantly.** The old read path (Channel + ThreadID /
   RecipientID + AgentID) still works because the old fields are still populated
   on every message. The conversation model is additive — it adds
   `conversation_id` without removing any existing routing fields.

3. **No data inconsistency.** Messages written while the flag was ON have both
   old-model fields (Channel, ThreadID, Sender, Recipient) AND `conversation_id`.
   When the flag goes OFF, queries use the old fields and these messages are
   still found. The only messages that would be "invisible" in the ON state are
   legacy messages written before dual-write began (they lack `conversation_id`).
   When the flag goes OFF, those messages reappear immediately.

4. **Hot-reload verified.** The `TestReadSwitch_HotReloadToggle` test
   demonstrates OFF→ON→OFF toggle on the same server instance, confirming the
   switch is instantaneous and reversible with no restart.

## Files Modified

- `pkg/store/store.go` — Added `GetConversationByExternalRef` to interface
- `pkg/store/entadapter/conversation_store.go` — Implemented `GetConversationByExternalRef`
- `pkg/messaging/conversation.go` — Added `ConversationReader` interface,
  `ResolveDMConversationForRead`, `ResolveThreadConversationForRead`
- `pkg/messaging/backfill_test.go` — Added `GetConversationByExternalRef` to mock
- `pkg/hub/handlers_messages.go` — Read-switch for `handleMessages` and `handleAgentMessages`
- `pkg/hub/handlers_chat_v2.go` — Read-switch for `handleConversationHistory`
- `pkg/hub/handlers_broker_inbound.go` — 7th dual-write call site
- `pkg/hub/handlers_read_switch_test.go` — New test file (8 tests)
