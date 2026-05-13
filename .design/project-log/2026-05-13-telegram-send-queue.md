# Per-Chat Rate Limiting Send Queue

**Date**: 2026-05-13
**Author**: Developer Agent
**Scope**: `pkg/plugin/telegram/sendqueue.go`, `sendqueue_test.go`, `broker_v2.go`

## Summary

Implemented a per-chat outbound message queue for the Telegram v2 broker to prevent 429 (Too Many Requests) errors from the Telegram Bot API.

## Design

- **SendQueue** manages a map of `chatID -> chatQueue`, each with a dedicated goroutine
- Messages to different chats are sent in parallel; messages to the same chat are serialized with a configurable `minDelay` (default 50ms) between sends
- On 429 errors, the worker respects Telegram's `retry_after` header and applies exponential backoff (doubling on each consecutive 429, capped at 60s)
- Queue overflow (>maxSize) drops the oldest message and notifies the caller with an error
- Idle workers are reaped after 5 minutes of inactivity
- `Send()` is synchronous — the caller enqueues and blocks until the result comes back, preserving existing error handling semantics

## Integration

All outbound send paths in `broker_v2.go` now route through `sendQueue.Send()`:
- `Publish()` — normal messages and reply-to messages
- `publishInputNeeded()` — inline keyboard messages
- `publishStateChangeDM()` — DM state-change notifications
- State-change group notifications

The queue is initialized in `Configure()` and shut down in `Close()`. Fallback to direct API calls is retained when `sendQueue` is nil.

## Config

- `send_queue_size`: max buffered messages per chat (default 100)
- `send_min_delay`: minimum inter-send delay (default "50ms")

## Tests

- Basic send, send with keyboard
- 429 backoff with retry
- Queue overflow (drop oldest)
- Concurrent sends to different chats
- Context cancellation
- Same-chat serialization (ordering guarantee)
- Close behavior
- Default values
