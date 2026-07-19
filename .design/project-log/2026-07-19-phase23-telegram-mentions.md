# Phase 2+3: Position-Aware Mention Classification for Telegram Broker

**Date**: 2026-07-19
**Branch**: `scion/ca-mgr-mention`
**Author**: Developer Agent

## Summary

Implemented position-aware mention classification (`classifyMentions()`) and integrated it into the Telegram broker's `handleGroupMessage()` pipeline. This enables distinguishing between "start mentions" (primary recipients at the beginning of a message) and "body mentions" (inline references to agents within the message body).

## Changes

### pkg/messages/types.go
- Added `TypeMention = "mention"` to the message type enum
- Added `NewMention(sender, recipient, msg, mentionSource string)` constructor
- Updated `validTypes` map and `ValidateType` error message

### extras/scion-telegram/internal/telegram/mentions.go
- Added `Mention` struct with Name, Kind ("agent"/"user"/"unknown"), and Identity fields
- Added `ClassifiedMentions` struct with StartMentions, BodyMentions, and StrippedBody
- Implemented `classifyMentions()` function with:
  - Whitespace tokenization and consecutive leading @-mention scanning
  - Case-insensitive agent matching preserving original case from knownAgents
  - User resolution via pluggable `userResolver` callback
  - @all detection (returns empty result to let broadcast logic handle it)
  - Bot username skipping
  - Body mention cap at 5
  - Start-to-body deduplication (agent in start won't appear in body)
  - Original spacing preservation for StrippedBody using byte positions

### extras/scion-telegram/internal/telegram/mentions_test.go
- Added 16 test cases covering:
  - Start-only mentions, body-only mentions, mixed start+body
  - User mention resolution (resolved and unresolvable)
  - Self-dedup (same agent in start and body)
  - No mentions, empty text
  - @all broadcast bypass
  - Bot name skipping
  - Body mention cap (7 mentions, only 5 returned)
  - Case-insensitive matching
  - Original spacing preservation
  - Unknown body mentions skipped
  - Trailing punctuation handling

### extras/scion-telegram/internal/telegram/broker_v2.go
- Integrated `classifyMentions()` call after target resolution in `handleGroupMessage()`
- Changed `stripMentions()` to only strip start-mention agent slugs (body mentions remain in text)
- Added body mention delivery loop after main delivery loop:
  - Creates `TypeMention` messages via `messages.NewMention()`
  - Sets mention_source metadata to primary recipient identity
  - Deduplicates against primary targets
  - Copies channel/thread metadata from original message
  - Delivers via `deliverInboundWithFeedback()`
- Skips mention classification for @all broadcasts

## Design Decisions

1. **Body mentions use StrippedBody**: Body mention recipients see the message with start mentions removed but body mentions preserved, giving them context about who else was mentioned.
2. **User resolver stub**: The user resolver currently returns not-found for all users. Per the design doc, unresolvable @chatUser mentions are dropped, not delivered as messages.
3. **Dedup at two levels**: classifyMentions() deduplicates body vs start mentions, and the broker integration also checks against the resolved targets set (which may differ from classified start mentions due to fallback routing).
4. **Existing behavior preserved**: @all broadcasts, reply-to-bot fallback, conversation context fallback, and default agent routing are all unchanged.

## Verification

- `go build ./...` passes (root module)
- `go build ./...` passes (scion-telegram module)
- `go test ./pkg/messages/...` passes
- `go test ./...` passes (scion-telegram module, all 16 new + existing tests)
