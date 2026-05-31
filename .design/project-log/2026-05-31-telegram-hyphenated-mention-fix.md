# Fix Telegram mention parsing for hyphenated agent names

**Date:** 2026-05-31
**Author:** dev-telegram-mention agent

## Problem

When a user mentions an agent with a hyphen in its name (e.g., `@deploy-agent`),
Telegram's entity system may create a mention entity covering only the portion
before the hyphen (e.g., `@deploy`), because hyphens are not valid characters in
Telegram usernames. This caused two issues:

1. **`hasNonBotUserMention`** incorrectly classified partial entities (like `@deploy`
   from `@deploy-agent`) as non-bot user mentions, potentially blocking fallback
   routing to the default agent.

2. **`resolveUserMentions`** could corrupt message text by replacing only the
   entity-covered portion with a scion identity, leaving the hyphenated suffix
   dangling (e.g., `user:email-agent` instead of `@deploy-agent`).

The text-based mention extraction (`extractAgentMentions`) already handled hyphens
correctly via `strings.Fields` tokenization and explicit `-` exclusion in
`TrimRightFunc`.

## Solution

Added `isPartialMentionEntity()` helper that detects when a Telegram mention entity
covers only part of a longer token by checking if the character immediately after
the entity boundary is a valid agent-slug character (letter, digit, hyphen, or
underscore).

Applied the check in:
- `hasNonBotUserMention()` — skip partial entities so they don't block routing
- `resolveUserMentions()` — skip partial entities to prevent text corruption

## Files Changed

- `extras/scion-telegram/internal/telegram/mentions.go` — added `isPartialMentionEntity`, fixed `hasNonBotUserMention`
- `extras/scion-telegram/internal/telegram/broker_v2.go` — fixed `resolveUserMentions`
- `extras/scion-telegram/internal/telegram/mentions_test.go` — 26 new tests

## Testing

All existing tests continue to pass. New tests cover:
- `isPartialMentionEntity` with hyphens, underscores, letters, digits, spaces, end-of-string, commas, emoji
- `hasNonBotUserMention` with hyphenated agent entities
- `resolveTargetAgents` with hyphenated agents (with and without Telegram entities)
- `extractAgentMentions` with multiple hyphens, trailing punctuation, dots, multiple agents
- `stripMentions` with hyphenated agents
- `extractUnresolvedMentions` with hyphenated known/unknown agents
