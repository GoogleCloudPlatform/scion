# DEF-15 Phases 3-5: Key Derivation Consolidation

**Date**: 2026-08-27
**Branch**: scion/ca-msg-em6
**Author**: dev-handler-fixes

## Summary

Completed Phases 3, 4, and 5 of §2.15 "One key derivation, not five". This
consolidates all conversation key construction to flow through
`DeriveConversationKey`, eliminating five separate key-construction paths in
favor of one canonical function.

## Changes

### Phase 3: Handler Repoints

- **Outbound handler** (`handleAgentOutboundMessage`): Replaced the two-branch
  ThreadID/DM conversation resolution with a single `DeriveConversationKey` call.
- **Inbound handler** (`handleAgentMessage`): Replaced the ThreadID and DM
  branches with a single `DeriveConversationKey` call. The `ConversationID`
  pre-resolved branch (DEF-11) is unchanged.
- Broker delegation path confirmed working: dm:-keyed ThreadID through
  `ResolveOrCreateThreadConversation` correctly produces `kind=direct`.

### Phase 4: Backfill Repoint

- **`groupForMessage`** in `backfill.go`: Now uses `DeriveConversationKey`
  instead of `fmt.Sprintf("thread:...")` and `DirectMessageExternalRef`.
  Returns nil on key derivation failure; caller skips with error record.
- **`DirectMessageExternalRef`** unexported → `directMessageExternalRef`.
  Confined to divergence tests that need the legacy shape.
- Updated backfill test assertions: DM external_ref now matches
  `DMConversationKey` format (kind-prefixed). Hazard-A email tests updated
  to expect derivation failure (email IDs fail UUID validation).

### Phase 5: DEF-16 Ordering Fix

- Moved `ValidateLegacyMessage` BEFORE conversation resolution in the outbound
  handler. A rejected request now never creates a conversation row.
- The inbound handler already had correct ordering (validate at :615,
  dual-write at :857).

## Tests Added

| Test | File | Assertion |
|------|------|-----------|
| AC-DEF15-4 | handlers_read_switch_test.go | Invalid dm: key as ThreadID → 0 conversation rows |
| AC-DEF15-6 | backfill_test.go | dm:-prefixed ThreadID backfill → kind=direct |
| AC-DEF15-1 | key_consolidation_test.go | Source-reading: thread key and legacy DM ref confined to expected files |
| AC-DEF16-1 (outbound) | handlers_read_switch_test.go | thread_id without channel → 400 + 0 conversations |
| AC-DEF16-1 (inbound) | handlers_read_switch_test.go | thread_id without channel → 400 + 0 conversations |
| Broker delegation | handlers_read_switch_test.go | dm: ThreadID through thread delegation → kind=direct |

## Mutation Test Result

Moving `ValidateLegacyMessage` below dual-write causes
`TestDEF16_ValidationRejectsBeforeConversationCreated_Outbound` to fail
(conversation row count goes from 0 to 1). Restoring the fix makes it pass.

## Known Issues

- `TestHandleAdminServerConfig_Get` fails on the base branch (pre-existing,
  unrelated to this work).
