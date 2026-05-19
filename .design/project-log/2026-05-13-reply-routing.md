# Conversation-Scoped Reply Routing for Telegram v2

**Date**: 2026-05-13
**Author**: Developer agent
**Commit**: e85af266

## Summary

Implemented reply-to routing in `handleGroupMessage()` so users can reply to a bot message in a Telegram group to route their message to the correct agent, without needing an @-mention.

## Changes

1. **`mentions.go`**: Added `extractAgentFromBotMessage()` with precompiled regex patterns to parse the `FormatMessageV2` output formats:
   - Standard: `🤖 agent-slug` (with optional `[URGENT]`/`[Broadcast]` prefixes and `[status]` suffix)
   - Agent-to-agent observer: `👀 🤖 sender → 🤖 recipient 👀` (extracts recipient)

2. **`broker_v2.go`**: Added two fallback tiers in `handleGroupMessage()` after `resolveTargetAgents()` returns empty:
   - Tier 1: Parse agent slug from the replied-to bot message text
   - Tier 2: Look up the user's most recent `ConversationContext` for the project

3. **`store.go`**: Added `GetLatestConversationContext(ctx, telegramUserID, projectID)` to the Store interface with SQLite implementation (ORDER BY last_message_at DESC LIMIT 1).

4. **Tests**: 16 new test cases across mentions_test.go, broker_v2_test.go, and store_test.go.

## Design Decisions

- Reply routing is strictly a fallback — @-mentions always take priority to avoid surprising behavior changes.
- Both fallbacks require the replied-to message to be from the bot (checked via `botInfo.ID`) to avoid false routing on arbitrary replies.
- The agent slug is validated against the project's known agents list before routing.

## Pre-existing Test Failures

The `TestFormatMessageV2` and several `TestFormatMessage_*` tests were failing before this change. They expect an older bracket format (`[coder]`) that doesn't match the current `FormatMessageV2` output (`🤖 coder`). Not related to this change.
