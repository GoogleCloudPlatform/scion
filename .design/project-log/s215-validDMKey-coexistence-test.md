# S215: validDMKey coexistence test

**Date:** 2026-08-27
**Trace:** AC-S215-COEXISTENCE

## Summary

Added `TestValidDMKey_CoexistenceWithDeriveConversationKey` to
`pkg/hub/handlers_read_switch_test.go`. The test proves two properties of the
outbound message handler's DM key guards:

1. **validDMKey rejects malformed `dm:` keys and prevents message persistence.**
   A ThreadID like `dm:bot:<uuid>:user:<uuid>` (where "bot" is not "user" or
   "agent") fails the `dmKeyRegexp`, returns HTTP 400, and no message or
   conversation row is written to the store.

2. **DeriveConversationKey guards only the conversation row, not the message row.**
   A non-canonical key `dm:user:<userUUID>:agent:<agentUUID>` passes the
   `validDMKey` regex but fails `DeriveConversationKey`'s canonicality check
   (the canonical form puts agent first: `dm:agent:...:user:...`). The message
   IS persisted with the non-canonical ThreadID, but no conversation row is
   created. This confirms the two guards protect different sinks.

## Side fix

Fixed pre-existing build failure in `handlers_agent_messaging_test.go` where
three references to `messaging.DirectMessageExternalRef` (unexported after
DEF-8 refactoring) were replaced with a local `testDirectMessageExternalRef`
helper that reproduces the legacy external ref format needed for divergence
tests.

## Files changed

- `pkg/hub/handlers_read_switch_test.go` -- added coexistence test
- `pkg/hub/handlers_agent_messaging_test.go` -- fixed build (DirectMessageExternalRef)

## Verification

```
go test ./pkg/hub/... -run TestValidDMKey_CoexistenceWithDeriveConversationKey -v -count=1
--- PASS (both sub-tests)

go test ./pkg/hub/... -run "TestDEF11_PreResolved|TestDEF11.*Genuine" -v -count=1
--- PASS (all DEF11 divergence tests still pass)
```
