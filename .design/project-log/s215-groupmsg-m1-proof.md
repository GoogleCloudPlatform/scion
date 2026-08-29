# S215 Group Message M1 Proof Test

**Date:** 2026-08-27
**Tracking:** AC-S215-M1
**Author:** dev-groupmsg-m1-test agent

## Summary

Added `TestHandleGroupMessage_ThreadID_NotPropagated` in
`pkg/hub/handlers_read_switch_test.go` to prove that `handleGroupMessage`
does not propagate a caller-supplied `ThreadID` into persisted message rows
and does not use it for conversation key derivation.

## Background

The security auditor flagged M1 (Medium): `handleGroupMessage` bypasses
`DeriveConversationKey` and uses `ResolveOrCreateDMConversation` directly.
The architect's response was: "'Not exploitable, no ThreadID in group
messages' is a claim about code -- prove it with a test."

## What the test proves

1. A `StructuredMessage` with `ThreadID: "thread:proj:some-thread-id"` is
   sent through the inbound handler (`handleAgentMessage`) with a
   `group[agent:target-agent,agent:anchor-agent]` recipient.
2. After the handler persists the messages, **none** of the stored
   `store.Message` rows carry a non-empty `ThreadID`.
3. The `GroupID` assigned to the messages is a fresh UUID, not derived
   from the injected `ThreadID`.

This confirms that `handleGroupMessage` constructs `store.Message` structs
directly (lines ~967-984 and ~1062-1078) without copying the
`StructuredMessage.ThreadID` field, making the ThreadID injection path
inert.

## Files changed

- `pkg/hub/handlers_read_switch_test.go` (new) -- the proof test
