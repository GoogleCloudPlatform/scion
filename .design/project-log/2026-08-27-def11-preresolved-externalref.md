# DEF-11: Pre-resolved ConversationID leaves ExternalRef empty

**Date:** 2026-08-27
**Author:** dev-def11
**Branch:** `scion/ca-msg-em7` (from `scion/messaging-v2`)

## Problem

When the CLI pre-resolves a `ConversationID` on a `StructuredMessage`, the
handler in `handleAgentMessage` (pkg/hub/handlers_agent_messaging.go) constructed
a `ConversationResult` with only `ConversationID` set, leaving `ExternalRef` as
an empty string. This empty ref was then fed into `ComputeDivergenceMatch`, which
compared e.g. `oldRouting="sender-recipient:A:B"` against `actualExternalRef=""`
and produced a false `routing-type-mismatch: old=sender-recipient:A:B new=` error.

The two routing models actually agreed; the comparison was being fed a blank.

## Fix

Three changes across `pkg/messaging/divergence.go` and `pkg/hub/handlers_agent_messaging.go`:

1. **Populate ExternalRef from the store:** After building the `ConversationResult`
   for a pre-resolved `ConversationID`, call
   `s.store.GetConversation(ctx, structuredMsg.ConversationID)` and set
   `convResult.ExternalRef = conv.ExternalRef` on success. On failure, set an
   explicit `lookupFailed` boolean (scoped to the pre-resolved branch only —
   empty ExternalRefs from thread conversations or unmigrated rows still flow
   through `ComputeDivergenceMatch` as intended).

2. **Fallback field on DivergenceEntry:** Added `Fallback bool` to
   `DivergenceEntry`. When set, `LogDivergence` routes the event to
   `IncFallback()` instead of `Inc(match)`, ensuring one event increments
   exactly one counter. This prevents lookup failures from blocking the
   read-switch gate that requires zero mismatches.

3. **Fallback handling for lookup failures:** When `lookupFailed` is true,
   log a `DivergenceEntry` with `Reason: "conv-lookup-failed"` and
   `Fallback: true` through the standard `LogDivergence` path. This skips
   `ComputeDivergenceMatch` entirely, avoids the ambiguous empty-ref artifact,
   and produces the same parseable record shape as all other divergence entries.

## Tests Added

Four new tests in `pkg/hub/handlers_agent_messaging_test.go`, all using the
existing `testServer(t)` / `doRequest()` harness with delta-based assertions on
the process-global `DivergenceMetrics` singleton:

| Test | Verifies |
|------|----------|
| `TestDEF11_PreResolvedConversation_PopulatesExternalRef` | ExternalRef is populated from store; dm-routing-agreement match occurs |
| `TestDEF11_PreResolvedConversation_DivergenceMatch` | Pre-resolved send produces Matches delta >= 1, Mismatches delta == 0 |
| `TestDEF11_PreResolvedConversation_LookupFailure` | Non-existent ConversationID records Fallbacks delta >= 1, Mismatches delta == 0 |
| `TestDEF11_PreResolvedConversation_GenuineDisagreement` | Wrong ExternalRef produces Mismatches delta >= 1 (comparison is active) |

**Mutation proof:** Removing the `GetConversation` call causes
`TestDEF11_PreResolvedConversation_DivergenceMatch` to fail because ExternalRef
reverts to empty, producing `routing-type-mismatch` instead of
`dm-routing-agreement`.

## Files Changed

- `pkg/messaging/divergence.go` -- added `Fallback` field to `DivergenceEntry`, updated `LogDivergence`
- `pkg/messaging/divergence_test.go` -- added `TestLogDivergence_Fallback`
- `pkg/hub/handlers_agent_messaging.go` -- the fix + fallback handling with `lookupFailed` flag
- `pkg/hub/handlers_agent_messaging_test.go` -- four new tests
