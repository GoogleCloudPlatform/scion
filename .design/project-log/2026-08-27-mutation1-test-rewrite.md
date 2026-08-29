# Mutation 1 Test Rewrite: Call the Real Handler

**Date:** 2026-08-27
**Author:** dev-def8-hubtest2
**Scope:** `pkg/hub/handlers_read_switch_test.go`

## Problem

`TestDualWrite_UnparseableSenderAddress_NoConversationCreated` replicated
the kind-safe guard logic from `handlers_agent_messaging.go:836` inline
instead of calling `srv.handleAgentMessage`. This made mutation 1 a false
positive: replacing the guard with `senderKind := "user"` in the handler
did not cause the test to fail because the test never invoked the handler.

## Change

Deleted the old test body and wrote a replacement that:

1. Uses `readSwitchWorld(t)` for setup.
2. Creates a dedicated "bob" user to avoid collision with the alice+agent
   DM that `readSwitchWorld` creates.
3. Builds a `MessageRequest` with a `StructuredMessage` whose `Sender` is
   `"bare-name-no-prefix"` (no kind prefix, so `PrincipalKindFromAddress`
   returns false).
4. Calls `srv.handleAgentMessage(rr, req, agent.ID)` directly, with user
   auth context set on the request.
5. Does NOT check the HTTP response status (handler returns 503 because no
   dispatcher is configured, but the dual-write code has already run).
6. Asserts that no conversation row exists for bob+agent (matchCount == 0).
7. Includes a floor assertion (Rule 14): at least 1 direct conversation
   must exist (the alice+agent DM from `readSwitchWorld`).

The test contains zero copies of the code under test: no
`PrincipalKindFromAddress`, no `ResolveOrCreateDMConversation`. It sends
a request and checks the DB.

## Mutation 1 Verification

- **Original code:** test passes.
- **Mutation** (replace guard with `senderKind := "user"`): test fails with
  `expected: 0, actual: 1` on the "unparseable sender address must NOT
  create a conversation row" assertion.
- **Revert:** test passes again.

## All Existing Tests

`go test ./pkg/hub/ -tags sqlite -count=1` passes (213s).
