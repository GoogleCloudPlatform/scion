# DEF-12: Store Layer + Startup Detection

**Date:** 2026-08-27
**Branch:** `scion/ca-msg-em6-def12`
**Base:** `scion/messaging-v2` at `14b3ba7c`

## Summary

Added `CountUnbackfilledMessages` to the `MessageStore` interface and implemented
the startup detection warning that alerts operators when unbackfilled messages
exist.

## Changes

### Store interface (`pkg/store/store.go`)
- Added `CountUnbackfilledMessages(ctx, projectID) (int, error)` to `MessageStore`

### Entadapter implementation (`pkg/store/entadapter/message_store.go`)
- Implemented using `message.ConversationIDIsNil()` predicate (since
  `conversation_id` is `Optional().Nillable()` in the ent schema, NULL means
  unbackfilled)
- Optional `projectID` scoping via `message.ProjectIDEQ(pid)`

### Mock store (`pkg/messaging/backfill_test.go`)
- Added `CountUnbackfilledMessages` to `mockMessageStore` — filters in-memory
  messages with empty `ConversationID`, with optional project scoping

### Startup detection (`cmd/server_foreground.go`)
- Added `maybeWarnUnbackfilledMessages(ctx, store.Store)` — calls
  `CountUnbackfilledMessages("" /* all projects */)` and logs a `slog.Warn`
  with count and remediation guidance when count > 0
- Errors are non-fatal (logged but don't block boot)
- Call site: after `migrateStore` succeeds, before `Ping`, inside `initStore`

### Tests (`cmd/server_foreground_backfill_test.go`)
- **AC-12-1 positive:** count=42 → warning logged with count and command
- **AC-12-1 negative:** count=0 → no warning logged
- **AC-12-7 mutation guard:** proves count=0 produces no warning, so a
  mutation forcing `CountUnbackfilledMessages` to return 0 causes the positive
  test to fail
- **Error handling:** store error → graceful warning, no panic

## Design Decisions

- Used `ConversationIDIsNil()` rather than `ConversationIDEQ(uuid.Nil)` because
  the ent schema declares `conversation_id` as `Optional().Nillable()` — NULL
  is the sentinel for "not yet backfilled"
- Placed the warning after migration but before Ping — migration must succeed
  first (schema might not exist yet), and Ping is the last gate before returning
  the store
- Used a stub store (embedding nil `store.Store`) rather than a full mock since
  `maybeWarnUnbackfilledMessages` only touches `CountUnbackfilledMessages`
