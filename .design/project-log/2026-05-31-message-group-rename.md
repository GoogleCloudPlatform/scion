# Rename messaging "set" concept to "message group"

**Date:** 2026-05-31
**Issue:** #94
**Branch:** scion/cleanup-message-group-rename

## Summary

Renamed the internal "set" vocabulary for multi-recipient messaging to "message group"
to improve clarity and self-documentation. The user-facing `set[...]` CLI wire-format
syntax is preserved for backward compatibility.

## Key Renames

| Old | New |
|-----|-----|
| `SetRecipient` | `GroupRecipient` (type alias kept) |
| `IsSetRecipient()` | `IsGroupRecipient()` (wrapper kept) |
| `ParseSetRecipient()` | `ParseGroupRecipient()` (wrapper kept) |
| `FormatSetRecipients()` | `FormatGroupRecipients()` (wrapper kept) |
| `MaxSetRecipients` | `MaxGroupRecipients` (const alias kept) |
| `sendSetMessageViaHub()` | `sendGroupMessageViaHub()` |
| `SetMessageRecipientResult` | `GroupMessageRecipientResult` |
| `SetMessageResponse` | `GroupMessageResponse` |
| `handleSetMessage()` | `handleGroupMessage()` |
| `PublishToSet()` | `PublishToGroup()` |
| `set.go` / `set_test.go` | `message_group.go` / `message_group_test.go` |

## Design Decisions

1. **"Broadcast" retained as-is.** Broadcast is semantically distinct from message groups
   (targets *all* agents vs. a *named* subset). No rename needed.

2. **User-Group vs. message-group distinction.** The Hub has a separate "Group" concept
   for user permissions. The new naming uses "GroupRecipient" / "message group" to
   keep these clearly distinct — the messaging code does not operate on permission groups.

3. **Wire format unchanged.** The `set[...]` syntax is preserved in CLI and API for
   backward compatibility. `SetPrefix` and `SetSuffix` constants are unchanged.

## Files Changed

- `pkg/messages/set.go` → `pkg/messages/message_group.go`
- `pkg/messages/set_test.go` → `pkg/messages/message_group_test.go`
- `cmd/message.go`, `cmd/message_test.go`
- `pkg/hub/handlers.go`, `pkg/hub/messagebroker.go`, `pkg/hub/messagebroker_test.go`
- `extras/scion-telegram/internal/telegram/broker_v2.go`

## Coordination Notes

- `pkg/broker` was not touched. If `pkg/broker` → `pkg/eventbus` rename proceeds
  separately, no conflict expected.
