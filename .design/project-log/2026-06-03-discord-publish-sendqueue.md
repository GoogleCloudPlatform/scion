# Discord Publish() SendQueue routing

**Date:** 2026-06-03
**Branch:** discord-chat
**Commit:** fix: route Publish() through SendQueue for rate limiting

## Summary

The `DiscordBroker.Publish()` method was sending outbound messages by calling
`session.ChannelMessageSend()` directly, bypassing the rate-limited `SendQueue`
that was already initialized during `Configure()`.

## Changes

- Captured `b.sendQueue` in the read-lock section at the top of `Publish()`
- Updated the "send to each target channel" loop to route through
  `sendQueue.Send(ctx, channelID, text, nil, nil)` instead of direct session call
- Added nil guard: falls back to direct `session.ChannelMessageSend()` if
  `sendQueue` is not initialized (defensive, since Configure always sets it)

## Observations

- The `SendQueue.Send()` method accepts embeds and components parameters — passing
  `nil` for both matches the current plain-text-only behavior. When rich embeds are
  added (Phase 2 per the code comments), those parameters can be populated.
- The SendQueue enforces per-channel serialization with configurable minimum delay
  (default 50ms), which prevents Discord API 429 rate-limit errors on burst sends.
