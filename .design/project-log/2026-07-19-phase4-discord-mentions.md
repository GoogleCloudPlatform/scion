# Phase 4: Discord Broker Position-Aware Mention Classification

**Date:** 2026-07-19
**Author:** developer agent
**Branch:** scion/ca-mgr-mention

## Summary

Implemented position-aware mention classification for the Discord broker, mirroring the architecture planned for the Telegram broker. This allows body mentions (e.g., "cc @agent-b") to generate `TypeMention` notification messages, distinct from start mentions which continue to route as primary `TypeInstruction` (or `TypeGroupSet`) messages.

## Changes

### `extras/scion-discord/internal/discord/mentions.go`
- Added `Mention` struct (Name, Kind, Identity) and `ClassifiedMentions` struct (StartMentions, BodyMentions, StrippedBody)
- Added `classifyMentions()` function that:
  - Tokenizes text by whitespace and scans leading `@` tokens as start mentions
  - Skips Discord bot mentions (`<@ID>` format) — they are handled separately
  - Returns empty result for `@all` (delegates to existing broadcast logic)
  - Classifies mentions as "agent", "user" (via userResolver callback), or "unknown"
  - Caps body mentions at 5 to prevent spam
  - Deduplicates: skips body mentions for agents already in StartMentions

### `extras/scion-discord/internal/discord/mentions_test.go`
- Added 11 test cases covering:
  - Start mentions only, body mentions only, mixed start+body
  - Self-dedup (same agent at start and in body)
  - No mentions, `@all` handling
  - Body mention cap (7 mentions, only first 5 captured)
  - Discord bot mention skipping (`<@BOT_ID>`)
  - Empty text, user resolver, unknown body mentions skipped

### `extras/scion-discord/internal/discord/broker.go`
- Integrated `classifyMentions()` into `handleIncomingMessage()`:
  - Called before `stripMentions()` to determine start vs body mentions
  - `stripMentions()` now only strips start-mention agents (body mentions stay in text)
  - Added `TypeGroupSet` support: multi-agent routing (len(targets) > 1, not `@all`) sets type to `TypeGroupSet` with `Recipients` populated via `FormatGroupRecipients()`
  - After the primary delivery loop, iterates body mentions of kind "agent", skips agents already in targets, and delivers `TypeMention` messages via `messages.NewMention()`
  - Mention messages carry full channel/thread metadata (discord_channel_id, discord_message_id, discord_guild_id, project_id)
  - `mentionSource` is set to the primary recipient identity (single agent) or group format (multiple agents)

## Verification

- `go test ./extras/scion-discord/...` passes (all existing + 11 new tests)
- `go build ./...` succeeds
- Existing behavior preserved: single agent routing, reply-to-webhook fallback, default agent fallback, `@all` broadcast
