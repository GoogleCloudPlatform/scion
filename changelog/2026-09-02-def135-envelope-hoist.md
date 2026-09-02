# DEF-135 — Delivery envelope now carries conversation id on broker inbound

**Tranche:** G
**Branch:** `scion/ca-msg-fix3`
**Date:** 2026-09-02

## Summary

The delivery envelope rendered for broker-inbound messages (Discord, Telegram,
and any plugin that does not supply `surface` + `external_ref`) now carries the
resolved conversation id. Previously the conversation was resolved *after* the
envelope was rendered and dispatched, so the agent received an envelope with no
`conversation` key despite the message being persisted with a conversation_id.

The fix hoists sender resolution and Phase 5 conversation resolution above the
delivery-envelope render. A single `effectiveConv` value, computed once, is used
for both the envelope and the persisted `storeMsg.ConversationID`.

## Tracked Drift

### 1. Write-deny 409 now fires before dispatch (behaviour change)

Previously, a conversation-resolution failure under write-deny returned 409
*after* `dispatchWithBrokerRetry` had already delivered the message — the agent
had the message and the caller had a failure, making client retries unsafe
(double-delivery).

After the hoist, a 409 means nothing was delivered and a retry is safe. This is
fail-closed, consistent with the standing rule that messaging switches fall back
to refusal (*under-granting is recoverable, over-granting is not*).

**Impact:** Messages that today are delivered-then-409'd will instead be refused
outright. The direction is strictly safer.

### 2. Dispatch failure can leave a conversation row with no messages

Resolution writes a conversation row. Hoisting it above dispatch means a
conversation row may be created for a message that is subsequently never
delivered (dispatch timeout, 502, etc.) and never persisted.

This is accepted because DM and thread conversations are **resolved by
deterministic key** — the next message from the same user to the same agent
resolves the identical conversation and uses it. An empty conversation is inert
and self-healing, not an orphan requiring cleanup.

### 3. Broadcasts with surface + external_ref no longer carry a conversation in the envelope

Base rendered the Phase 11 conversation into the broadcast envelope while
persisting no conversation_id on the row — exactly the envelope/row disagreement
this defect exists to remove. The fix unifies on no-conversation-for-broadcasts,
matching the documented invariant at `handlers_agent_messaging.go:1898`
("broadcasts deliberately skip conversation resolution"). Phase 11 still creates
the conversation row; it is simply not stamped on the broadcast.

### 4. Tripwire: Phase 11 unification changes which conversation messages persist into

When both Phase 11 (explicit `surface` + `external_ref`) and Phase 5 (inferred
DM/thread) produce a result, the precedence rule selects Phase 11. Phase 11
produces a **group** conversation keyed on the external ref; Phase 5 produces a
**direct** conversation keyed on the sender/agent pair.

No live caller sets `surface` + `external_ref` on this endpoint today, so
nothing changes in practice. **The day anyone enables Phase 11 on a plugin that
also resolves DM conversations, Discord messages will move from DM conversations
into group conversations.** This is Alternative B from the design, which was
explicitly rejected because it splits every user's history at the deploy
boundary. It must not happen as a side effect of enabling Phase 11; it requires a
deliberate product decision and migration plan.
