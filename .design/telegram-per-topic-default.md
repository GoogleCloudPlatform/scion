# Design: Per-Topic Default Agent in Telegram

**Date:** 2026-06-01
**Branch:** chat-channels
**Status:** Exploration

## Current Behavior

The `/default` command sets a single default agent per Telegram group chat. The flow:

1. User types `/default` in a group → `handleDefault()` (`commands.go:194`) fetches the `GroupLink` by `chat_id` and presents an inline keyboard of agents.
2. User taps an agent button → `handleDefaultCallback()` (`callbacks.go:314`) writes `link.DefaultAgent = agentSlug` and calls `SaveGroupLink()`.
3. On inbound messages, `handleGroupMessage()` (`broker_v2.go:1607-1615`) falls back to `link.DefaultAgent` when no @-mention, reply-to, or conversation context resolves a target.

**Storage:** The `group_links` table has `chat_id INTEGER PRIMARY KEY` and a single `default_agent TEXT` column. One default per chat.

## Thread/Topic Context in Telegram

Telegram forum-mode groups have named topics. Each topic has a `message_thread_id` (int64). The General topic is thread ID 1 (or 0 in some API versions). The plugin already captures this:

- **Inbound:** `TGMessage.MessageThreadID` is populated by the Telegram Bot API. At `broker_v2.go:1757-1759`, it's stored as `msg.ThreadID`.
- **Outbound:** `Publish()` reads `msg.ThreadID` and passes it back as `SendOption{MessageThreadID: tid}` so replies land in the correct topic.
- **Commands:** When `/default` is typed inside a topic, `msg.MessageThreadID` carries the thread ID. The `CallbackQuery.Message` also has `MessageThreadID`, so the callback knows which topic the button was pressed in.

**Key insight:** All the thread context is already flowing through the system — it's just not used for default-agent scoping.

## Proposed Changes

### 1. Storage: New `topic_defaults` Table

Add a new table rather than modifying `group_links`:

```sql
CREATE TABLE IF NOT EXISTS topic_defaults (
    chat_id    INTEGER NOT NULL,
    thread_id  INTEGER NOT NULL,
    agent_slug TEXT NOT NULL,
    PRIMARY KEY (chat_id, thread_id)
);
```

The existing `group_links.default_agent` stays as the chat-level fallback. This avoids a schema migration and keeps the two concepts cleanly separated.

**Store methods to add:**
- `GetTopicDefault(ctx, chatID, threadID) (string, error)`
- `SetTopicDefault(ctx, chatID, threadID, agentSlug) error`
- `DeleteTopicDefault(ctx, chatID, threadID) error`

### 2. Command Handling Changes

**`handleDefault()` in `commands.go`:**
- Read `msg.MessageThreadID`.
- If nonzero (i.e., inside a topic), pass it through to the keyboard builder and callback data.
- The keyboard prompt changes to: "Select the default agent for this topic:" (vs. the current "for @-mentions:").
- If zero (General topic or non-forum group), behave as today (chat-level default).

**Callback data format:**
- Current: `dflt:<agentSlug>` (e.g., `dflt:coder`)
- New: `dflt:<agentSlug>:<threadID>` (e.g., `dflt:coder:42`)
- When `threadID` is empty or "0", it's a chat-level default (backward-compatible).

**`handleDefaultCallback()` in `callbacks.go`:**
- Parse the optional `threadID` from callback data parts.
- If present and nonzero: call `SetTopicDefault(ctx, chatID, threadID, agentSlug)`.
- If `__none__`: call `DeleteTopicDefault(ctx, chatID, threadID)`.
- Otherwise: set chat-level default as today.

### 3. Routing Logic Changes

**`handleGroupMessage()` in `broker_v2.go` (around line 1607):**

Replace the current fallback:
```go
if len(targets) == 0 && link.DefaultAgent != "" {
```

With a two-tier lookup:
```go
if len(targets) == 0 {
    defaultAgent := ""
    if tgMsg.MessageThreadID != 0 {
        topicDefault, _ := b.store.GetTopicDefault(ctx, chatID, tgMsg.MessageThreadID)
        if topicDefault != "" {
            defaultAgent = topicDefault
        }
    }
    if defaultAgent == "" {
        defaultAgent = link.DefaultAgent
    }
    if defaultAgent != "" {
        // existing routing logic
    }
}
```

Fallback chain: **topic default → chat default → no default**.

### 4. UX for Querying/Clearing

**Showing current default:** When `/default` is invoked in a topic, the keyboard should show the topic-level default (if set) with a checkmark, falling back to showing the chat-level default with a "(chat default)" label.

**Clearing a topic default:** The "No default agent" button in topic context removes the topic override, reverting to the chat-level fallback. It does NOT clear the chat-level default.

**Showing all topic defaults:** Consider adding a `/defaults` command (or a flag like `/default list`) that shows all topic-specific overrides for the group. This is a nice-to-have, not required for v1.

## UX Flows

### Setting a per-topic default
1. User navigates to the "Backend" topic in a forum group.
2. Types `/default`.
3. Sees keyboard: "Select the default agent for this topic:" with agent buttons.
4. Taps "coder" → "Default agent for this topic set to @coder."

### Clearing a per-topic default
1. In the same topic, types `/default`.
2. Keyboard shows "✓ coder (current)" and "No default agent (use chat default)".
3. Taps "No default agent" → "Topic default removed. Messages will use the chat default (@designer)."

### Non-forum group (no change)
1. `/default` works exactly as today.
2. `MessageThreadID` is 0, so all code paths hit the chat-level branch.

## Complexity Assessment

**This is a small, contained change.** Estimated at 1-2 days of implementation + testing.

| Area | Scope |
|------|-------|
| New table + 3 store methods | ~40 lines |
| `handleDefault()` thread-awareness | ~10 lines changed |
| Callback data + `handleDefaultCallback()` | ~15 lines changed |
| Routing fallback in `handleGroupMessage()` | ~10 lines changed |
| Keyboard label tweaks in `cards.go` | ~10 lines changed |
| **Total new/changed code** | **~85 lines** |

No changes needed to:
- The outbound message flow (thread routing already works)
- The `GroupLink` struct or `group_links` table
- The Telegram Bot API client
- Any other command handlers

## Risks and Edge Cases

1. **General topic ambiguity:** Telegram uses thread ID 1 for the General topic in forum groups, but non-forum groups have thread ID 0. The code should treat `MessageThreadID == 0` as "no topic" (chat-level default). Thread ID 1 (General) should be a valid topic for per-topic defaults.

2. **Topic deletion:** If a topic is deleted, its `topic_defaults` row becomes orphaned but harmless — no messages will arrive with that thread ID. Could add periodic cleanup, but not necessary.

3. **Callback data length:** Telegram limits callback data to 64 bytes. Current format `dflt:agentSlug` uses ~15-25 bytes. Adding `:threadID` (max 20 digits) stays well within limits. If using the `callback_lookups` short-ID system already in the codebase, this is a non-issue.

4. **Race between topic and chat defaults:** A user might set a chat default expecting it to apply everywhere, not realizing a topic has an override. The `/default` command should clearly indicate when a topic override exists.

5. **Forum mode toggled off:** If a group admin disables forum mode, all topics collapse. Topic defaults become inert (messages arrive with thread ID 0). The chat-level default takes over naturally. No data loss; if forum mode is re-enabled, the topic defaults resume working.
