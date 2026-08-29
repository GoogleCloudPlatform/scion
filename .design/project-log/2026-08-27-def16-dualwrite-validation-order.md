# DEF-16: dual-write runs before validation in outbound handler

## Date
2026-08-27

## Discovery
During DEF-15 investigation, the EM observed that the dual-write conversation
creation at line 244 runs BEFORE ValidateLegacyMessage at line 288 in
handleAgentOutboundMessage. The inbound handler (handleAgentMessage) orders
these correctly: validation at line 615, dual-write at line 848.

## Impact
When ValidateLegacyMessage rejects a message (e.g., "thread_id requires channel
to be set"), the conversation row created by the dual-write persists as an
orphan — no message is attached to it, but the row exists in the database.

## Ordering comparison
- handleAgentOutboundMessage: dual-write (:245) -> validate (:288) — WRONG
- handleAgentMessage: validate (:615) -> dual-write (:848) — CORRECT

## Status
Logged as DEF-16. Not assigned. Fix is to move the dual-write after validation
in the outbound handler, matching the inbound handler's order.
