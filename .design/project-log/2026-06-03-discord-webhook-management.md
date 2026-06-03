# Discord Webhook Management for Per-Agent Identity

**Date:** 2026-06-03
**Task:** Phase 2, items 1-2 — channel webhook management and outbound webhook routing
**Branch:** discord-chat

## What was done

Implemented per-agent Discord identity via channel webhooks so agent messages
appear with distinct names and RoboHash avatars instead of the generic bot user.

### New files
- `webhooks.go` — `WebhookManager` with lazy webhook creation, in-memory cache
  (map[channelID]*Webhook with RWMutex), auto-discovery of existing webhooks,
  and cache invalidation + retry on 404/Unknown Webhook errors
- `modals.go` — `OpenAskUserModal` and `HandleModalSubmit` for free-text
  ask-user responses (needed for callbacks.go to compile)

### Modified files
- `broker.go` — Added `webhooks *WebhookManager` field, initialized in Configure()
  phase 1, updated Publish() to route agent TypeAssistantReply/TypeInstruction
  messages through webhooks with bot API fallback, added `formatWebhookMessage()`
  that strips agent name header (webhook username displays it)
- `callbacks.go` — Fixed `NewCallbackHandler` constructor to accept
  `deliverInbound` function parameter, added ask-user and notification callback
  handlers

## Design decisions

1. **Webhook routing criteria**: Messages are sent via webhook when
   `sender` starts with `"agent:"` AND type is `TypeAssistantReply` or
   `TypeInstruction`. State changes and input-needed keep bot API identity
   (per design doc Section 9.4).

2. **Fallback on webhook failure**: If `SendAsAgent()` fails, Publish()
   falls back to bot API with the standard `formatMessage()` (includes agent
   name in text since bot identity is generic).

3. **Cache invalidation**: On 404/10015 (Unknown Webhook), the cache entry is
   invalidated and `getOrCreateWebhook()` retries once. This handles the case
   where a guild admin deletes the webhook externally.

4. **Webhook per channel, not per agent**: One "Scion Agent Relay" webhook per
   channel, reused for all agents with different `username`/`avatar_url` params.
   This minimizes webhook count (Discord limits 15 per channel).

## Observations

- The callbacks.go and format.go files had pre-existing uncommitted changes
  from earlier Phase 2 work (embed rendering, ask-user callbacks). These were
  included in the commit since they were needed for compilation.
- `discordgo` handles 429 rate-limit retries internally for `WebhookExecute`,
  so webhook sends don't go through the SendQueue. The SendQueue is only used
  for bot API fallback sends.
