# Design: Telegram Bot Plugin v2 for Scion Hub

**Status**: Draft (updated with owner feedback)
**Author**: Design Agent
**Date**: 2026-05-13
**Updated**: 2026-05-13 — revised bot topology, @-mention routing model,
1:1 group→project constraint, @all broadcast, agent-to-agent visibility option

## 1. Problem Statement

The current Telegram plugin (`pkg/plugin/telegram/`) provides basic 1:1 routing
between a Telegram group chat and a scion agent. Configuration is entirely
static: chat-to-topic routes are declared in YAML, user identity mapping is a
flat email form, and there is no interactive UI beyond plain text messages.

This limits the plugin in several ways:

1. **Static routing**: Adding a new group requires editing YAML and restarting
   the plugin. There is no way for a user to link a group to a project from
   within Telegram.

2. **No structured interaction**: When an agent needs user input
   (`TypeInputNeeded`), the plugin renders it as plain text with
   "Please reply in this chat to respond." There are no buttons, no inline
   keyboards, no way to present choices or confirmation dialogs.

3. **Weak identity verification**: Registration is a simple email form on an
   HTTP endpoint. There is no proof that the email owner is the person
   registering — anyone with the token URL can claim any email.

4. **Single-bot, single-project assumption**: The architecture assumes one bot
   token maps to one project's worth of routing. Multi-project hubs require
   multiple bot instances.

5. **No notification management**: Users cannot control which agent events they
   receive in Telegram. All messages for a linked group are broadcast
   unconditionally.

**Goal**: Redesign the Telegram plugin as a rich, hub-integrated bot that
supports dynamic group linking, interactive inline-keyboard UIs, secure
hub-verified registration, and multi-project routing — all without requiring
YAML edits for day-to-day operations.

## 2. Current Architecture Summary

### 2.1 Plugin Structure

| Component | File | Role |
|-----------|------|------|
| `TelegramBroker` | `telegram.go` | Main broker: polling, routing, publish/subscribe |
| `TelegramAPIClient` | `api.go` | Telegram Bot API wrapper (getMe, getUpdates, sendMessage) |
| `FormatMessage` | `format.go` | StructuredMessage → Telegram text (4096 char limit) |
| `registrationServer` | `register.go` | HTTP server for email-based user registration |

### 2.2 Routing Model

**Inbound** (Telegram → Hub):
1. Long-poll `getUpdates` receives `TGMessage`
2. Look up chat ID in `chatRoutes` map → topic string
3. Build `StructuredMessage` with sender identity from `userMappings`
4. POST to hub `/api/v1/broker/inbound` with HMAC auth

**Outbound** (Hub → Telegram):
1. `Publish(topic, msg)` called by hub via FanOutBroker
2. Route via: (a) `telegram_chat_id` in metadata, (b) exact topic match in
   `topicChats`, (c) NATS-style pattern match, (d) reverse lookup on `chatRoutes`
3. Format message, send via `sendMessage`, deduplicate on content hash

### 2.3 Current Telegram API Surface

Only three API methods are implemented:
- `getMe` — bot identity validation
- `getUpdates` — long-polling for inbound messages
- `sendMessage` — outbound text delivery (plain or MarkdownV2)

### 2.4 Registration Flow

1. User sends `/register` in Telegram
2. Bot generates 10-minute token, returns HTTP link
3. User opens link in browser, enters email in HTML form
4. Plugin stores `telegramUserID → email` mapping in JSON file
5. No hub verification — email is self-asserted

### 2.5 Configuration

All routing is static YAML:
```yaml
plugins:
  broker:
    telegram:
      config:
        bot_token: "..."
        chat_routes: '{"123456789": "scion.project.myproj.agent.coder.messages"}'
        outbound_routes: '{"scion.project.*.user.*.messages": "-5242408331"}'
        user_mappings: '{"98765": "alice@example.com"}'
```

## 3. Proposed Design

### 3.1 Bot Topology: One Bot Per Hub

Each hub gets its own bot token. This is a 1:1 association, driven by two
independent constraints:

1. **Scion plugin model**: Plugins are hub-scoped spokes of a FanOutBroker,
   authenticated via that hub's HMAC credentials.
2. **Telegram API**: Only one consumer can call `getUpdates` (long-polling)
   per bot token. Sharing a token across multiple hub plugin instances would
   require a gateway/multiplexer process — complexity that isn't justified
   given that BotFather makes creating a new bot trivial.

```
    ┌──────────┐         ┌──────────────┐
    │  Hub A   │────────▶│ Bot A        │
    │          │         │ (@AlphaBot)  │
    └──────────┘         └──────────────┘

    ┌──────────┐         ┌──────────────┐
    │  Hub B   │────────▶│ Bot B        │
    │          │         │ (@BetaBot)   │
    └──────────┘         └──────────────┘
```

**Edge case: same user across hubs.** A Telegram user interacts with each
hub's bot independently. They register separately with each (Section 3.4).
Each bot manages its own group links and routing.

**Edge case: multiple bots in same group.** If an admin adds both Bot A and
Bot B to a Telegram group, each bot independently processes @-mentions of
its own project's agents. Since the bots have different usernames, there's
no ambiguity. The bots run with privacy mode OFF to see all messages (needed
for @-mention parsing — see Section 3.2).

### 3.2 Group ↔ Project Linking and @-Mention Routing

#### Design Principles

1. **One group, one project.** A Telegram group links to exactly one scion
   project. This keeps routing unambiguous — every agent @-mention in the group
   resolves to that project's agent namespace. Re-running `/setup` replaces the
   existing link.

2. **One project, many groups.** A project can be linked from multiple groups
   (e.g., a "dev team" group and a "stakeholders" group). This is supported but
   expected to be uncommon.

3. **Explicit addressing required.** No message is forwarded to any agent
   unless it explicitly addresses one — either via the bot @-mention (routed
   to the group's default agent) or a direct agent @-mention.

4. **Bot @-mention = default agent.** Mentioning the bot itself
   (`@ScionHubBot`) routes to the group's default agent. This leverages
   Telegram's native autocomplete — bot usernames autocomplete in all clients,
   giving users a frictionless path for the most common interaction.

5. **Direct agent @-mentions for specific agents.** Users can bypass the
   default and address any agent by name (`@coder`, `@reviewer`). These don't
   autocomplete natively (Telegram limitation), but the `/agents` command
   lists what's available.

6. **Agents @-mention users.** Outbound agent replies mention the user so
   recipients are clear in a busy group.

#### Dynamic `/setup` Command

Replace static `chat_routes` YAML with a command-driven flow:

```
User types: /setup
Bot replies: Select a project to link this group to:
  [Project Alpha]  [Project Beta]  [Project Gamma]
User taps: [Project Alpha]
Bot replies: Select a default agent (messages to @ScionHubBot go here):
  [coder]  [reviewer]  [ops]
User taps: [coder]
Bot replies: Project Alpha linked to this group!
             Default agent: @coder (mention @ScionHubBot to talk to it)
             Other agents: @reviewer, @ops (mention by name)
             Use @all to message every agent.
```

**Implementation:**

1. Bot receives `/setup` command in a group chat
2. Bot verifies sender is a registered user with admin role on at least one
   project (checked via hub API: `GET /api/v1/groves?member={userID}`)
3. Bot sends an inline keyboard with project names as buttons
4. User taps a project → callback query fires
5. Bot queries agents for that project (`GET /api/v1/groves/{id}/agents`)
6. Bot sends second inline keyboard for default agent selection
7. User taps an agent → bot stores `groupChatID → projectID` mapping with
   the selected default agent (replacing any existing link)
8. Mapping stored in persistent DB (see Section 3.5)

If the group already has a link, `/setup` shows the current project and offers
`[Change project]` or `[Keep current]` buttons before proceeding.

The default agent can be changed without re-linking via `/default`:
```
User types: /default
Bot replies: Select new default agent for @ScionHubBot:
  [coder] (current)  [reviewer]  [ops]
```

#### @-Mention Routing (Inbound)

When a message arrives in a linked group, the bot resolves the target agent(s)
via a three-tier mention system:

```
"@ScionHubBot deploy the migration"    → routed to DEFAULT agent (e.g., coder)
"@coder deploy the new migration"      → routed to agent:coder (explicit)
"@reviewer check PR #42"               → routed to agent:reviewer (explicit)
"@coder @reviewer sync on the API"     → routed to BOTH agents
"@all we're cutting a release today"   → routed to ALL agents in the project
"hey team, lunch at noon?"             → NOT routed (no mention)
```

**Tier 1 — Bot @-mention (autocompletes natively):**
When a user @-mentions the bot itself (e.g., `@ScionHubBot`), the message is
routed to the group's configured default agent. This is the primary UX path:
Telegram natively autocompletes bot usernames in all clients, so users get a
frictionless, discoverable way to talk to an agent without remembering names.

**Tier 2 — Direct agent @-mention (explicit targeting):**
Users can mention a specific agent by name (`@coder`, `@reviewer`). These
names don't autocomplete natively in Telegram (only real users/bots do), but
they allow power users to address any agent directly. The `/agents` command
lists available names.

**Tier 3 — `@all` broadcast:**
The special mention `@all` expands to every agent in the linked project's
agent list. The message is delivered to each agent independently.

**Combined mentions:** A message can mention both the bot and specific agents:
`@ScionHubBot @reviewer` routes to both the default agent AND reviewer.
Deduplication ensures the default agent doesn't receive the message twice if
it's also explicitly named.

**Mention parsing:**

The bot maintains a `ProjectAgents` cache for the linked project (refreshed
periodically from `GET /api/v1/groves/{id}/agents`). Inbound message text is
scanned for mentions. Telegram provides structured `entities` data for bot
@-mentions (entity type `mention`), which the bot checks against its own
username. For agent names, the bot scans the raw text for `@<name>` tokens
matching the `ProjectAgents` cache.

```go
func (b *TelegramBrokerV2) resolveTargetAgents(msg *TGMessage, link *GroupLink) []string {
    agents := b.getProjectAgents(link.ProjectID)
    targets := make(map[string]bool)

    // Tier 1: bot @-mention → default agent
    if b.isBotMentioned(msg) {
        targets[link.DefaultAgent] = true
    }

    // Tier 2 & 3: agent @-mentions and @all
    for _, token := range strings.Fields(msg.Text) {
        if !strings.HasPrefix(token, "@") {
            continue
        }
        name := strings.TrimPrefix(token, "@")
        name = strings.TrimRight(name, ".,!?;:")
        if name == "all" {
            // @all: return all agents
            return agents.AgentSlugs
        }
        if slices.Contains(agents.AgentSlugs, name) {
            targets[name] = true
        }
    }

    return maps.Keys(targets)
}

// isBotMentioned checks Telegram's structured entity data for a mention
// of this bot's username. More reliable than text parsing for the bot itself.
func (b *TelegramBrokerV2) isBotMentioned(msg *TGMessage) bool {
    for _, entity := range msg.Entities {
        if entity.Type == "mention" {
            mentioned := msg.Text[entity.Offset+1 : entity.Offset+entity.Length]
            if strings.EqualFold(mentioned, b.botInfo.Username) {
                return true
            }
        }
    }
    return false
}
```

If no agent is resolved (no bot mention, no agent mention, no @all), the
message is silently ignored. This is the key behavioral change from v1.

**Mention autocomplete:** Telegram natively autocompletes bot usernames,
so `@ScionH...` → `@ScionHubBot` works out of the box. For explicit agent
names, users use:
- `/agents` command to list all available agents with their current status
- On `/setup` completion, the bot announces all agent names
- The `ProjectAgents` cache refreshes every 5 minutes for new agents

#### @-Mention Routing (Outbound)

When an agent sends a reply, the bot formats it with a user @-mention so the
recipient knows the message is for them:

```
┌─────────────────────────────────────┐
│  @alice [coder]:                    │
│  Migration deployed successfully.   │
│  3 tables updated, 0 errors.        │
└─────────────────────────────────────┘
```

The bot uses Telegram's `reply_to_message_id` when the agent reply correlates
to a specific user message (tracked via `ConversationContext`). This creates
visual threading in the group.

For broadcasts (no specific recipient), the agent name is shown without a
user mention:

```
┌─────────────────────────────────────┐
│  [coder]:                           │
│  Deployment complete for all envs.  │
└─────────────────────────────────────┘
```

#### Agent-to-Agent Message Visibility

By default, inter-agent messages (agent A talking to agent B within the same
project) are NOT rendered in Telegram groups — they're internal coordination.
However, a group can opt in to seeing agent-to-agent traffic:

```
/settings
Bot replies:
┌─────────────────────────────────────┐
│  Group Settings                     │
│  Project: Alpha                     │
│                                     │
│  Agent-to-agent messages:           │
│    [Show in chat]  [Hidden] (current)│
└─────────────────────────────────────┘
```

When enabled, the bot subscribes to agent-to-agent message topics for the
linked project and renders them in a distinct format:

```
┌─────────────────────────────────────┐
│  [coder → reviewer]:               │
│  Can you review the migration in    │
│  PR #87? I've updated the schema.   │
└─────────────────────────────────────┘
```

This is useful for teams that want full visibility into how agents are
collaborating.

**Hub change required:** Currently, agent-to-agent messages bypass the broker
entirely — `handleAgentMessage()` in `pkg/hub/handlers.go:2214` dispatches
directly via `dispatcher.DispatchAgentMessage()` without calling
`broker.Publish()`. To support this feature, the hub needs a small change:

```go
// In handleAgentMessage(), after dispatching to the target agent,
// also publish a copy to the broker for observability:
if bp := s.getBrokerProxy(); bp != nil {
    topic := fmt.Sprintf("scion.grove.%s.agent.%s.agent.%s.messages",
        projectID, srcAgent.Slug, dstAgent.Slug)
    bp.Publish(ctx, topic, structuredMsg)
}
```

The Telegram plugin subscribes to the agent-to-agent pattern when any linked
group has `ShowAgentToAgent` enabled:

```go
// Agent-to-agent subscription (opt-in):
"scion.grove.<projectID>.agent.*.agent.*.messages"
```

This publish is additive — it doesn't change the existing direct dispatch
path. The broker copy is fire-and-forget for observability only.

#### Multiple Groups per Project

A project can be linked from multiple groups. Conversation-scoped reply routing
uses `ConversationContext` — the agent's reply goes to the group where the
user most recently mentioned that agent.

```
Group A (linked to Project Alpha) — Alice says "@coder deploy"
Group B (linked to Project Alpha) — Bob says "@coder status"
coder replies to Alice → routed to Group A
coder replies to Bob → routed to Group B
```

Broadcasts go to ALL groups linked to that project.

#### Unlinking

`/unlink` in a group removes the group→project mapping. Only project admins or
the user who ran `/setup` can unlink.

#### Admin DMs

Direct messages to the bot are treated as admin commands, not routed to agents:
- `/setup` in DM → error: "Use /setup in a group chat"
- `/status` in DM → shows all linked groups and their project mappings
- `/register` / `/unregister` in DM → identity management
- `/notifications` in DM → notification preferences (see Section 3.3)
- Free text in DM → ignored with a help message listing available commands

### 3.3 Rich Bot UI

#### 3.3.1 Telegram API Extensions Required

The current `api.go` only supports `sendMessage`. The v2 plugin needs:

| Method | Purpose |
|--------|---------|
| `sendMessage` (extended) | Add `reply_markup` parameter for inline keyboards |
| `editMessageText` | Update existing message content (for card updates) |
| `editMessageReplyMarkup` | Update just the keyboard on an existing message |
| `answerCallbackQuery` | Acknowledge inline keyboard button presses |
| `deleteMessage` | Clean up expired cards |

**InlineKeyboardMarkup** structure:

```go
type InlineKeyboardMarkup struct {
    InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
    Text         string `json:"text"`
    CallbackData string `json:"callback_data,omitempty"` // max 64 bytes
    URL          string `json:"url,omitempty"`
}
```

**MessageEntity** (for structured mention detection):

```go
type MessageEntity struct {
    Type   string `json:"type"`   // "mention", "bot_command", etc.
    Offset int    `json:"offset"` // UTF-16 offset in text
    Length int    `json:"length"` // UTF-16 length
}
```

The `TGMessage` struct must be extended to include `Entities`:

```go
type TGMessage struct {
    // ... existing fields ...
    Entities []MessageEntity `json:"entities,omitempty"`
}
```

This is used by `isBotMentioned()` to reliably detect when users @-mention
the bot (Tier 1 routing to default agent), rather than relying on text parsing
which could false-positive on substrings.

**CallbackQuery** (received via `getUpdates`):

```go
type CallbackQuery struct {
    ID      string     `json:"id"`
    From    *TGUser    `json:"from"`
    Message *TGMessage `json:"message,omitempty"`
    Data    string     `json:"data,omitempty"` // from callback_data
}
```

The `Update` struct must be extended:

```go
type Update struct {
    UpdateID      int64          `json:"update_id"`
    Message       *TGMessage     `json:"message,omitempty"`
    CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}
```

#### 3.3.2 Callback Data Schema

Telegram limits `callback_data` to 64 bytes. Use a compact encoding:

```
<action>:<entity_id>[:<extra>]
```

Examples:
- `setup:proj:abc123` — select project "abc123" during /setup
- `setup:dflt:coder` — select default agent "coder" during /setup
- `dflt:coder` — change default agent to "coder" (via /default)
- `ask:yes:req-7f3a` — answer "yes" to input request "req-7f3a"
- `ask:no:req-7f3a` — answer "no" to input request "req-7f3a"
- `ask:opt:req-7f3a:2` — select option index 2 for request "req-7f3a"
- `notif:on:coder` — enable notifications for agent "coder"
- `notif:off:coder` — disable notifications for agent "coder"

If entity IDs are too long for the 64-byte limit, use a server-side lookup
table mapping short tokens to full IDs (similar to the registration token
approach).

#### 3.3.3 Ask-User Dialogs (InputNeeded)

When an agent triggers `ask_user`, the hub publishes a `StructuredMessage` with
`Type: TypeInputNeeded`. The v2 plugin renders this as an interactive card:

**Simple yes/no:**
```
┌─────────────────────────────────────┐
│  Input Needed from agent:coder     │
│                                     │
│  Approve the deployment plan?       │
│  - Update database schema           │
│  - Run migration                    │
│  - Deploy new service               │
│                                     │
│  [Yes]  [No]                        │
└─────────────────────────────────────┘
```

**Multiple choice (when message metadata contains `choices`):**
```
┌─────────────────────────────────────┐
│  Input Needed from agent:coder     │
│                                     │
│  Which environment should I deploy  │
│  to?                                │
│                                     │
│  [Staging]  [Production]  [Both]    │
└─────────────────────────────────────┘
```

**Implementation:**

1. On receiving a `TypeInputNeeded` message, check `Metadata` for:
   - `choices` — JSON array of choice labels: `["Staging","Production","Both"]`
   - `request_id` — unique ID for correlating the response
2. If `choices` is present, render each as an inline keyboard button with
   callback data `ask:opt:<request_id>:<index>`
3. If no `choices`, render default `[Yes] [No]` buttons
4. When user taps a button:
   a. `answerCallbackQuery` to dismiss the loading spinner
   b. `editMessageReplyMarkup` to remove the keyboard (prevent double-tap)
   c. Deliver the user's choice back to the hub as a `StructuredMessage` with
      `Type: TypeInstruction` and the selected choice text as `Msg`
   d. The existing inbound path (`POST /api/v1/broker/inbound`) handles
      delivery to the agent

**Metadata extension for choices:**

This requires a convention in the `StructuredMessage.Metadata` field. The
`choices` key is optional — agents that use `ask_user` with structured
options set it; agents that just ask a free-text question don't. The Telegram
plugin renders accordingly.

No changes to `pkg/messages/types.go` are needed — `Metadata` already supports
arbitrary key-value pairs.

#### 3.3.4 Notification Management

```
User sends: /notifications
Bot replies:
┌─────────────────────────────────────┐
│  Notification Preferences           │
│  Project: Alpha                     │
│                                     │
│  agent:coder      [On]  [Off]       │
│  agent:reviewer    [On]  [Off]       │
│  agent:ops         [On]  [Off]       │
│                                     │
│  Notify on:                         │
│  [Errors]  [Input Needed]  [All]    │
└─────────────────────────────────────┘
```

User taps a toggle → callback query → plugin updates subscription preferences
in persistent storage → `answerCallbackQuery` with confirmation →
`editMessageReplyMarkup` to update button state (e.g., bold the active option).

**Subscription filter model:**

```go
type NotificationPrefs struct {
    AgentSubscriptions map[string]bool   // agentSlug → enabled
    EventFilter        []string          // ["error", "input-needed", "state-change"]
}
```

When the plugin receives an outbound `Publish()`, it checks the recipient
user's notification preferences before forwarding to Telegram. Messages that
don't match the user's filter are silently dropped.

#### 3.3.5 Agent Listing

When a user wants to see which agents are available:

```
User sends: /agents
Bot replies:
┌─────────────────────────────────────┐
│  Agents in Project Alpha:           │
│                                     │
│  @coder    — running                │
│  @reviewer — idle                   │
│  @ops      — idle                   │
│                                     │
│  Mention an agent to talk to it:    │
│  "@coder deploy to staging"         │
└─────────────────────────────────────┘
```

This is informational only — no selection state is set. Users address
agents via @-mentions in their messages (see Section 3.2).

#### 3.3.6 Status Cards

Agent state changes (`TypeStateChange`) are rendered as report-only formatted
messages — no action buttons. Agent management (restart, stop, etc.) is done
through the hub UI or CLI, not through Telegram.

```
┌─────────────────────────────────────┐
│  agent:coder — Error                │
│                                     │
│  Task failed: migration script      │
│  returned exit code 1               │
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│  agent:reviewer — Running           │
│                                     │
│  Reviewing PR #42                   │
└─────────────────────────────────────┘
```

### 3.4 Registration Flow Redesign

Registration uses the hub's generic Platform Identity Linking service
(Section 10). The plugin generates a code, registers it with the hub, and
sends the user a link to the hub web UI where they confirm the link while
authenticated.

#### Code-Based Linking Flow

```
User DMs bot: /register
Bot generates: 6-char code (e.g., "AB3X7K")
Bot calls:     POST /api/v1/identity/link
               {platform: "telegram", platform_user_id: "98765",
                code: "AB3X7K", metadata: {username: "@alice"}}
Bot replies:
┌─────────────────────────────────────┐
│  Link your scion account            │
│                                     │
│  Click below and log in to confirm: │
│                                     │
│  [Link Account]                     │
│  (opens hub.example.com/profile/    │
│   link?platform=telegram&           │
│   code=AB3X7K&user_name=@alice)     │
└─────────────────────────────────────┘

User clicks link → hub web UI → authenticates → confirms.

Bot polls:   GET /api/v1/identity/link/status?platform=telegram&platform_user_id=98765
Hub returns: {status: "confirmed", user_id: "...", email: "alice@example.com"}
Bot stores:  TelegramUserMapping in local SQLite
Bot replies: "Linked! You are alice@example.com"
```

**Implementation:**

1. User sends `/register` to bot in DM
2. Bot checks if already linked (local SQLite lookup)
3. Bot generates 6-char code (crypto/rand, `ABCDEFGHJKMNPQRSTUVWXYZ23456789`)
4. Bot POSTs to hub `POST /api/v1/identity/link` with platform, user ID, code,
   and metadata (username, display name)
5. Bot sends inline keyboard card with URL button pointing to generic hub
   link page: `{hub_url}/profile/link?platform=telegram&code=AB3X7K&user_name=@alice`
6. Bot starts background polling (10s interval) on
   `GET /api/v1/identity/link/status?platform=telegram&platform_user_id=98765`
7. User clicks link, authenticates with hub in browser, confirms on the
   generic `/profile/link` page → hub calls `PlatformLinkService.VerifyCode()`
8. Poll returns `status: "confirmed"` with user ID and email
9. Bot stores mapping in local SQLite:
   ```go
   type TelegramUserMapping struct {
       TelegramUserID   string
       TelegramUsername string
       ScionUserID      string
       ScionEmail       string
       LinkedAt         time.Time
   }
   ```
10. Bot confirms: "Linked! You are alice@example.com"

**Security properties:**
- Proves ownership of both identities: user must be logged into hub AND have
  access to their Telegram account
- Code is short-lived (15-minute TTL, hub-enforced)
- No self-asserted email — hub knows the user from their JWT session
- Hub persists the link in `external_identities` table (survives restarts)
- Plugin's local SQLite copy is a hot-path cache of the authoritative hub record

**Unregistration:**
User sends `/unregister` in DM → bot calls
`DELETE /api/v1/identity/link?platform=telegram&platform_user_id=98765` →
mapping deleted from hub's `external_identities` and local SQLite → confirmed.

#### Migration from v1 Registration

Existing `userMappings` JSON file entries are imported on first startup:
1. Plugin reads `mappings_file` as before
2. For each `{telegramUserID: email}` entry, creates a `TelegramUserMapping`
   in SQLite with the email as `ScionEmail` (ScionUserID left blank — will
   be resolved on first message via hub API lookup)
3. After successful import, renames file to `mappings_file.v1.bak`
4. Logs deprecation warning

The v1 registration HTTP server (`register.go`) is removed entirely.

### 3.5 Data Model

#### 3.5.1 Persistent State (Plugin-Side)

The v2 plugin maintains its own persistent store (SQLite or bolt) for state
that is hot-path (needed on every message) and doesn't belong in the hub DB:

```go
// Group-to-project link, created by /setup.
// One group links to exactly one project (1:1).
// One project can be linked from multiple groups (1:many).
type GroupLink struct {
    ChatID              int64     // Telegram group chat ID
    ChatTitle           string    // Group name (for display)
    ProjectID           string    // Scion grove/project ID
    ProjectSlug         string    // Human-readable slug
    DefaultAgent        string    // Agent that receives bot @-mentions (set during /setup)
    LinkedBy            string    // Telegram user ID who ran /setup
    LinkedAt            time.Time
    Active              bool      // False if bot was removed from group
    ShowAgentToAgent    bool      // Render agent-to-agent messages in this group
}

// Per-user conversation context for reply routing.
// Tracks where each user last mentioned each agent, so replies go to the right group.
type ConversationContext struct {
    TelegramUserID string
    ProjectID      string
    AgentSlug      string    // Which agent was mentioned
    LastChatID     int64     // Group where the user last mentioned this agent
    LastMessageAt  time.Time
}

// Cached agent list per project, refreshed every 5 minutes from hub API.
// Powers @-mention matching and /agents command.
type ProjectAgents struct {
    ProjectID   string
    AgentSlugs  []string  // Known agent names for @-mention matching
    RefreshedAt time.Time
}

// Per-user notification preferences
type NotificationPrefs struct {
    TelegramUserID     string
    ProjectID          string
    AgentSubscriptions map[string]bool // agentSlug → enabled
    EventFilter        []string        // e.g., ["error", "input-needed"]
    UpdatedAt          time.Time
}

// Pending ask-user requests awaiting callback response
type PendingAskUser struct {
    RequestID   string    // Unique ID for correlating callback
    MessageID   int64     // Telegram message ID of the card
    ChatID      int64     // Where the card was sent
    AgentSlug   string    // Which agent asked
    ProjectID   string    // Which project
    Choices     []string  // Available choices (if structured)
    ExpiresAt   time.Time // Auto-dismiss after TTL
    Responded   bool      // Whether user has responded
}
```

#### 3.5.2 Hub-Side State (Generic Platform Identity Service)

The hub stores the authoritative identity link in the `external_identities`
table (see Section 10.4 for full schema). The plugin's local
`TelegramUserMapping` in SQLite is a hot-path cache of this data.

The `TelegramUserMapping` struct (in plugin SQLite) is defined in Section 3.4.

#### 3.5.3 Callback Data Lookup Table

For callback data that exceeds the 64-byte Telegram limit:

```go
// Short-lived mapping for inline keyboard callbacks
type CallbackLookup struct {
    ShortID   string    // 8-char random ID used in callback_data
    FullData  string    // Full action + entity IDs
    ExpiresAt time.Time // Same TTL as the inline keyboard card
}
```

### 3.6 Configuration Schema

#### Static Configuration (settings.yaml)

```yaml
plugins:
  broker:
    telegram:
      self_managed: true
      address: "localhost:9094"
      config:
        # Required: one bot token per hub (1:1 association)
        bot_token: "${TELEGRAM_BOT_TOKEN}"

        # Hub connection (injected by hub startup, same as v1)
        hub_url: "http://localhost:8080"
        hmac_key: "..."
        broker_id: "telegram"

        # Admin Telegram user IDs (can use admin commands)
        admin_user_ids: '["98765", "11111"]'

        # Persistent store path
        db_path: "/data/telegram-v2.db"

        # Inbound mode: "poll" (default, workstation) or "webhook" (hosted hub)
        inbound_mode: "poll"

        # Webhook config (only when inbound_mode: webhook)
        webhook_url: "https://hub.example.com/telegram/webhook"
        webhook_listen: ":9094"    # local address to listen on
        webhook_secret: "..."      # secret token for Telegram to include in requests

        # Agent cache refresh interval (default: 5m)
        agent_cache_ttl: "5m"

        # Optional: migration from v1
        v1_mappings_file: "/data/telegram_mappings.json"
        v1_chat_routes: '{"123456789": "scion.project.myproj.agent.coder.messages"}'
```

#### Dynamic State (persisted in plugin DB)

Everything else is dynamic and managed via bot commands:
- Group → project link (via `/setup`, one project per group)
- Group settings: agent-to-agent visibility (via `/settings`)
- User → scion identity mappings (via hub-verified registration)
- Notification preferences (via `/notifications`)
- Conversation context (automatic, per-user-per-agent, updated on each @-mention)
- Agent name cache (periodic refresh from hub API, powers @-mention matching)

#### Removed from Static Config

These v1 config keys are removed (replaced by dynamic equivalents):
- `chat_routes` → replaced by `GroupLink` in DB
- `outbound_routes` → replaced by `GroupLink` + `ConversationContext`
- `user_mappings` → replaced by hub-side `TelegramUserMapping`
- `register_addr` / `register_url` → registration HTTP server removed
- `mappings_file` → replaced by DB storage

### 3.7 Topic Routing Changes

#### v1 Topics (static)

```
scion.project.myproj.agent.coder.messages    # hardcoded per chat_routes
scion.telegram.chat.{chatID}.messages        # default fallback
```

#### v2 Topics (derived from GroupLink + @-mention)

Inbound topic construction (one topic per mentioned agent):

```go
func (b *TelegramBrokerV2) buildInboundTopic(link *GroupLink, agentSlug string) string {
    return fmt.Sprintf("scion.grove.%s.agent.%s.messages", link.ProjectID, agentSlug)
}
```

Outbound subscription patterns:

```go
// Subscribe to all messages for all linked projects
func (b *TelegramBrokerV2) subscribeForLinks() {
    for _, link := range b.getAllLinks() {
        pattern := fmt.Sprintf("scion.grove.%s.>", link.ProjectID)
        b.hostCallbacks.RequestSubscription(pattern)
    }
}
```

The plugin subscribes to `scion.grove.<projectID>.>` for each linked project,
receiving all agent messages, broadcasts, and user messages. Filtering by
notification preferences happens in the plugin's `Publish()` method before
forwarding to Telegram.

### 3.8 Message Handling Pipeline

#### Inbound (Telegram → Hub)

```
getUpdates
    │
    ├─ Message with text
    │   ├─ Starts with "/" → command handler
    │   │   ├─ /setup    → project linking + default agent (inline keyboard)
    │   │   ├─ /default  → change default agent (inline keyboard)
    │   │   ├─ /agents   → list available agents for this group
    │   │   ├─ /settings → group settings (agent-to-agent visibility)
    │   │   ├─ /notifications → prefs inline keyboard
    │   │   ├─ /status   → show linked projects (DM only)
    │   │   ├─ /unlink   → remove group link
    │   │   └─ /help     → command list
    │   │
    │   ├─ DM + looks like link token → verify with hub API
    │   │
    │   └─ Free text in group → mention routing
    │       ├─ Look up GroupLink for chat ID (1:1, one project)
    │       ├─ Resolve target agents (3-tier):
    │       │   ├─ Bot @-mention (@ScionHubBot) → default agent
    │       │   ├─ Agent @-mentions (@coder) → named agents
    │       │   ├─ @all → all agents in project
    │       │   └─ Deduplicate (default may overlap with explicit)
    │       ├─ If NO targets resolved → silently ignore
    │       ├─ For EACH target agent:
    │       │   ├─ Look up user identity mapping from hub
    │       │   ├─ Update ConversationContext (lastChatID for this user+agent)
    │       │   ├─ Build StructuredMessage (strip @-prefix from body)
    │       │   └─ POST to hub /api/v1/broker/inbound
    │       └─ (message may be delivered to multiple agents)
    │
    └─ CallbackQuery
        ├─ Parse callback_data (action:entity:extra)
        ├─ answerCallbackQuery (acknowledge)
        ├─ Dispatch to handler:
        │   ├─ setup:*   → continue project linking / default agent
        │   ├─ dflt:*    → change default agent
        │   ├─ ask:*     → deliver InputNeeded response to hub
        │   └─ notif:*   → update notification prefs
        └─ editMessageReplyMarkup (update card)
```

#### Outbound (Hub → Telegram)

```
Publish(topic, msg)
    │
    ├─ Parse topic → extract projectID, agentSlug, recipientType
    │
    ├─ Determine target chat(s):
    │   ├─ If msg.Metadata["telegram_chat_id"] set → direct delivery
    │   ├─ If msg.Recipient is a specific user:
    │   │   └─ Look up ConversationContext.LastChatID for that user+project
    │   ├─ If broadcast → all groups linked to project
    │   └─ If no recipient → all groups linked to project
    │
    ├─ For each target chat:
    │   ├─ Check notification preferences for chat members
    │   ├─ Format message based on Type:
    │   │   ├─ TypeInputNeeded → render with inline keyboard
    │   │   ├─ TypeStateChange → render as status card with action buttons
    │   │   ├─ TypeAssistantReply → render as formatted text
    │   │   └─ TypeInstruction → render as formatted text
    │   │
    │   └─ sendMessage with reply_markup (if applicable)
    │
    └─ Deduplication check (same as v1)
```

### 3.9 Hub API Usage

```
# Platform identity linking (new generic service — see Section 10)
POST /api/v1/identity/link          → Plugin registers pending link code
POST /api/v1/identity/link/verify   → User confirms link in web UI
GET  /api/v1/identity/link/status   → Plugin polls for confirmation

# Project and agent discovery (existing)
GET /api/v1/groves              → List projects (for /setup inline keyboard)
GET /api/v1/groves/{id}/agents  → List agents (for default agent selection, @-mention cache)

# Message delivery (unchanged from v1)
POST /api/v1/broker/inbound     → Deliver inbound messages with HMAC auth
```

### 3.10 Error Handling and Edge Cases

**Bot removed from group**: When the bot is removed from a Telegram group
(detected via `my_chat_member` update with status `left`/`kicked`), the
plugin should mark the `GroupLink` as inactive but not delete it. If the bot
is re-added, the link can be reactivated.

**Stale callback queries**: Inline keyboard buttons reference state
(request_id, agent slug) that may become stale. The `PendingAskUser` table
tracks expiry. Callbacks for expired requests receive a toast via
`answerCallbackQuery` with text: "This request has expired."

**Rate limiting**: Telegram enforces per-chat rate limits (~20 msgs/min to a
group, ~30 msgs/sec overall). The outbound path should implement a per-chat
send queue with backpressure. When a 429 is received, respect `retry_after`
and queue subsequent messages.

**Message length**: Telegram's 4096-char limit applies to messages with inline
keyboards too. The v2 formatter should be aware of keyboard overhead and
truncate message text accordingly.

**Concurrent `/setup` in same group**: Use an in-memory lock per chat ID to
serialize setup flows. If a second user starts `/setup` while one is in
progress, respond with "Setup already in progress."

## 4. Migration Path

### 4.1 v1 Compatibility Mode

On startup, if v1 config keys are present (`chat_routes`, `outbound_routes`,
`user_mappings`), the plugin operates in compatibility mode:

1. Import `chat_routes` as `GroupLink` entries in the DB
2. Import `user_mappings` by calling hub API to create `TelegramUserMapping`s
3. Import `outbound_routes` as reverse `GroupLink` entries
4. Log deprecation warnings for each v1 config key
5. Proceed with v2 behavior

### 4.2 Phased Rollout

**Phase 1 (MVP):**
- @-mention based routing (agent mentions parsed from message text)
- `@all` broadcast to all agents
- Dynamic `/setup` group linking with inline keyboards (1:1 group→project)
- Hub-verified registration (token-based)
- InputNeeded rendering with Yes/No inline buttons
- Persistent store (SQLite)
- Agent name cache with periodic refresh (powers mention matching)
- v1 config import

**Phase 2 (Rich UI):**
- `/notifications` with inline keyboard preference management
- `/settings` with agent-to-agent message visibility toggle
- Status cards (report-only)
- OAuth-based registration option

**Phase 3 (Polish):**
- Per-chat send queue with rate limiting
- Message threading (Telegram reply-to for context)
- File/attachment forwarding
- Bot commands menu registration (via `setMyCommands` API)
- Structured `choices` in InputNeeded metadata

### 4.3 Breaking Changes

| v1 Behavior | v2 Change | Migration |
|-------------|-----------|-----------|
| `chat_routes` config | Dynamic `/setup` | Auto-imported on first run |
| `user_mappings` config | Hub-verified registration | Auto-imported via hub API |
| `outbound_routes` config | Derived from GroupLink | Auto-imported on first run |
| Registration HTTP server | Removed (hub-initiated flow) | N/A |
| Email-based registration | Hub-verified token | Users must re-register |
| Static topic construction | Dynamic from GroupLink | Automatic |

## 5. Telegram Bot API Additions to `api.go`

```go
// New methods to add to TelegramAPIClient

// SendMessageWithKeyboard sends a message with an inline keyboard
func (c *TelegramAPIClient) SendMessageWithKeyboard(
    ctx context.Context,
    chatID int64,
    text string,
    parseMode string,
    keyboard *InlineKeyboardMarkup,
) (*TGMessage, error)

// EditMessageText edits the text of an existing message
func (c *TelegramAPIClient) EditMessageText(
    ctx context.Context,
    chatID int64,
    messageID int64,
    text string,
    parseMode string,
    keyboard *InlineKeyboardMarkup,
) error

// EditMessageReplyMarkup edits just the keyboard of an existing message
func (c *TelegramAPIClient) EditMessageReplyMarkup(
    ctx context.Context,
    chatID int64,
    messageID int64,
    keyboard *InlineKeyboardMarkup,
) error

// AnswerCallbackQuery acknowledges a callback query (dismisses loading)
func (c *TelegramAPIClient) AnswerCallbackQuery(
    ctx context.Context,
    callbackQueryID string,
    text string,        // optional toast message
    showAlert bool,     // popup vs toast
) error

// DeleteMessage deletes a message
func (c *TelegramAPIClient) DeleteMessage(
    ctx context.Context,
    chatID int64,
    messageID int64,
) error

// SetMyCommands registers the bot's command menu
func (c *TelegramAPIClient) SetMyCommands(
    ctx context.Context,
    commands []BotCommand,
) error
```

## 6. Files to Change

| File | Change |
|------|--------|
| `pkg/plugin/telegram/api.go` | Add inline keyboard types, callback query types, new API methods |
| `pkg/plugin/telegram/telegram.go` | Major rewrite: callback query handling, dynamic routing, DB-backed state |
| `pkg/plugin/telegram/format.go` | Extend to return `InlineKeyboardMarkup` alongside text for interactive messages |
| `pkg/plugin/telegram/register.go` | Remove HTTP registration server; add hub-verified token flow |
| `pkg/plugin/telegram/store.go` | **NEW**: SQLite/bolt persistent store for GroupLink, prefs, context |
| `pkg/plugin/telegram/commands.go` | **NEW**: Command handlers (/setup, /agents, /notifications, /help) |
| `pkg/plugin/telegram/mentions.go` | **NEW**: @-mention parser and agent name matching |
| `pkg/plugin/telegram/callbacks.go` | **NEW**: Callback query dispatcher and handlers |
| `pkg/plugin/telegram/cards.go` | **NEW**: Inline keyboard builders for each card type |
| `pkg/hub/platform_link.go` | **NEW**: Generic `PlatformLinkService` (see Section 10) |
| `pkg/hub/handlers.go` | Add generic `/api/v1/identity/link*` handlers; add broker publish in `handleAgentMessage()` |
| `pkg/hub/server.go` | Register identity link routes, initialize `PlatformLinkService` |
| `pkg/store/models.go` | Add `PendingPlatformLink`, `ExternalIdentity` models |
| `pkg/store/store.go` | Add `PlatformLinkStore` interface |
| `pkg/store/postgres/` | Migration for `pending_platform_links` and `external_identities` tables |
| `pkg/hub/telegram_link.go` | Thin wrapper → then remove (migration path in Section 10.9) |

## 7. Open Questions

1. **Store choice**: **Resolved** — SQLite for initial implementation, with a
   storage interface abstracted so the plugin can later use the hub's main
   Postgres when configured. *(Decision: owner feedback 2026-05-13)*

2. **Hub API ownership**: **Resolved** — Registration uses a generic Platform
   Identity Linking service (Section 10) with platform-agnostic endpoints at
   `/api/v1/identity/link*`. The hub is not Telegram-aware — the platform name
   is a parameter. The plugin stores a local SQLite cache of the authoritative
   hub-side `external_identities` record. *(Revised: generalized service
   designed 2026-05-13)*

3. **Structured choices in InputNeeded**: **Resolved** — Metadata convention
   only. Agents set `Metadata["choices"]` as a JSON array of labels. No
   changes to `pkg/messages/types.go`. Each plugin renders as it sees fit
   (Telegram: inline buttons, chat-app: widgets, etc.).
   *(Decision: owner feedback 2026-05-13)*

4. **Agent action permissions**: **Resolved** — Status cards are report-only,
   no action buttons. Removes the permissions question entirely. Users manage
   agents through the hub UI or CLI, not through Telegram cards.
   *(Decision: owner feedback 2026-05-13)*

5. **Webhook vs long-polling**: **Resolved** — Support both. Long-polling is
   the default for workstation/local mode (no public endpoint needed). Webhook
   mode is preferred for hosted hubs (instant delivery, more efficient). A
   config flag selects the mode. *(Decision: owner feedback 2026-05-13)*

6. **Multi-bot in same group**: If two hub bots are in the same group, they
   both receive all messages. The bot should run with privacy mode OFF so it
   can see all messages and parse @-mentions of agent names (which are not
   real Telegram @-mentions of the bot itself). *(Resolved by @-mention routing
   model — the bot sees all messages but only forwards those containing agent
   @-mentions, so there's no noise even with privacy mode off.)*

7. **Inline keyboard expiry**: **Resolved** — Plugin tracks `PendingAskUser`
   with a TTL. Stale callback taps get a toast via `answerCallbackQuery`:
   "This request has expired." No proactive card cleanup — keeps it simple.
   *(Decision: owner feedback 2026-05-13)*

8. **Agent-to-agent topic convention**: **Investigated** — Confirmed that
   agent-to-agent messages bypass the broker entirely. When agent A messages
   agent B, the hub dispatches directly via `dispatcher.DispatchAgentMessage()`
   in `handleAgentMessage()` (`pkg/hub/handlers.go:2214`). The broker's
   `Publish()` is never called. Only agent-to-user (`PublishUserMessage`) and
   broadcasts (`PublishBroadcast`) flow through the broker.

   **Hub change required**: To support agent-to-agent visibility in Telegram
   groups, `handleAgentMessage()` needs to also publish a copy to the broker
   when the message is between two agents in the same project. Proposed topic:
   `scion.grove.<projectID>.agent.<src>.agent.<dst>.messages`. This is an
   opt-in publish (not all deployments need it), gated by a config flag or
   by whether any broker plugin has subscribed to agent-to-agent patterns.
   See Section 3.2 (Agent-to-Agent Message Visibility) for the Telegram
   plugin side.

9. **@all rate limiting**: `@all` in a project with many agents could generate
   a burst of inbound messages. Should there be a cap on how many agents @all
   expands to, or is the hub's own rate limiting sufficient?

## 8. Alternatives Considered

### 8.1 Telegram Mini App (WebApp)

Instead of inline keyboards, build a Telegram Mini App (embedded web view) for
rich interactions. Rejected because:
- Requires a separate web frontend (HTML/JS)
- Adds hosting complexity (public HTTPS endpoint)
- Inline keyboards cover the required interaction patterns
- Mini Apps are better suited for full-screen experiences (dashboards, forms)

Could be a Phase 3 addition for a project management dashboard.

### 8.2 Webhook-Only Architecture

Replace long-polling with Telegram webhooks entirely. Rejected in favor of
supporting both modes because:
- Long-polling is essential for workstation/local mode (no public endpoint)
- Webhooks are preferred for hosted hubs (instant delivery, more efficient)
- Both share the same message handling pipeline; only the inbound transport
  differs

### 8.3 Generic Chat Plugin Interface

Abstract the Telegram-specific code behind a generic chat plugin interface
(like the chat-app's platform abstraction). Deferred because:
- The chat-app already serves this role for Google Chat / Slack
- Telegram's inline keyboard model is sufficiently different from Google Chat's
  card model that a shared abstraction would be leaky
- Better to build Telegram-native first, then extract common patterns if a
  second inline-keyboard platform (Discord) is added

### 8.4 Hub-Side User Store

Store Telegram user identity mappings in the hub DB with custom API endpoints.
Rejected because:
- Registration is a plugin concern, not a core hub concern
- Adds Telegram-specific models and endpoints to the hub codebase
- The hub's existing device auth flow already provides identity verification
- Plugin-side SQLite (with future Postgres support) is sufficient
- Consistent with how scion-chat-app manages its own user mappings

## 9. Implementation Plan

**Phase 1 (MVP) — ~2 weeks:**
1. Implement `mentions.go` — @-mention parser, @all expansion, agent name matching
2. Extend `api.go` with inline keyboard types and new API methods
3. Implement `store.go` with SQLite persistent store (GroupLink 1:1,
   ConversationContext, ProjectAgents cache, NotificationPrefs, PendingAskUser)
4. Implement `/setup` command with inline keyboard project selection
5. Implement @-mention routing in inbound message handler (including @all)
6. Implement hub-verified registration (token verify endpoint + DM handling)
7. Implement InputNeeded → inline keyboard rendering (Yes/No default)
8. Implement callback query handling pipeline
9. Outbound formatting with user @-mentions and `reply_to_message_id`
10. v1 config import and compatibility
11. Tests: unit tests for mention parser, store, command handlers, callback handlers

**Phase 2 (Rich UI) — ~2 weeks:**
1. `/notifications` inline keyboard preference management
2. `/settings` — agent-to-agent message visibility toggle per group
3. Status cards (report-only formatted messages)
4. Conversation-scoped reply routing (ConversationContext per agent)
5. Per-chat rate limiting queue
6. OAuth registration option (hub web changes)

**Phase 3 (Polish) — ~1 week:**
1. `setMyCommands` for bot command menu
2. Structured `choices` metadata convention
3. Message threading via Telegram reply-to
4. Inline keyboard expiry and cleanup
5. Monitoring: metrics for message volume, callback latency, error rates

## 10. Platform Identity Linking Service (Generalized)

**Added**: 2026-05-13 — Generalization of the Telegram-specific
`TelegramLinkService` to support Discord, Slack, and future integrations.

### 10.1 Problem Statement

The current identity linking implementation is Telegram-specific:
- `TelegramLinkService` in `pkg/hub/telegram_link.go` is in-memory (lost on
  restart), hard-coded to Telegram field names, and registers routes under
  `/api/v1/telegram/link`
- `register_v2.go` in the Telegram plugin posts to these Telegram-specific
  endpoints
- Adding Discord or Slack would require duplicating the entire flow with new
  endpoint prefixes, new service structs, and new handler methods

We want a single hub-side service that any messaging platform can use for
identity linking, with the platform name as a parameter rather than baked
into the code.

### 10.2 Current Flow (Telegram-Specific)

```
Telegram Plugin                Hub                          Web UI
     |                          |                             |
     |--POST /telegram/link---->| RegisterCode(code, tgID)    |
     |   {code, tg_user_id}     | [in-memory map]             |
     |                          |                             |
     |                          |                   User opens link
     |                          |<--POST /telegram/link/verify-|
     |                          |   {code, userID, email}      |
     |                          | VerifyCode → "confirmed"     |
     |                          |                             |
     |--GET /telegram/link/---->| GetStatusByTelegramUser     |
     |  status?tg_user_id=X     | → {confirmed, userID, email}|
     |                          |                             |
     | Store mapping locally    |                             |
```

**Weaknesses:**
- In-memory storage (`sync.Mutex` + `map[string]*telegramPendingLink`) — lost
  on restart, no audit trail
- Telegram-specific field names throughout (`TelegramUserID`, `/telegram/link`)
- No persistent record of linked identities — the hub forgets the link after
  the plugin consumes it
- Each new platform would require copy-pasting the service

### 10.3 Proposed Design: Generic Platform Identity Service

#### API Endpoints

All endpoints are platform-agnostic. The platform is a parameter:

```
POST   /api/v1/identity/link
       Auth: Broker (HMAC)
       Body: {
         "platform": "telegram",
         "platform_user_id": "98765",
         "code": "AB3X7K",
         "metadata": {
           "username": "@alice",
           "display_name": "Alice"
         }
       }
       → 201 Created

POST   /api/v1/identity/link/verify
       Auth: User (JWT)
       Body: {
         "code": "AB3X7K"
       }
       → 200 OK { "platform": "telegram", "platform_user_id": "98765" }

GET    /api/v1/identity/link/status?platform=telegram&platform_user_id=98765
       Auth: Broker (HMAC)
       → 200 OK { "status": "confirmed", "user_id": "...", "email": "..." }
       → 200 OK { "status": "pending" }
       → 404 Not Found (no pending link)

DELETE /api/v1/identity/link?platform=telegram&platform_user_id=98765
       Auth: Broker (HMAC) or User (JWT)
       → 204 No Content (removes the link)

GET    /api/v1/identity/links
       Auth: User (JWT)
       → 200 OK [ { "platform": "telegram", "platform_user_id": "98765",
                     "metadata": {...}, "linked_at": "..." }, ... ]
       (User can see their own linked platforms)
```

#### How It Replaces the Telegram-Specific Endpoints

| Current (Telegram-specific) | New (Generic) |
|------------------------------|---------------|
| `POST /api/v1/telegram/link` | `POST /api/v1/identity/link` with `platform: "telegram"` |
| `POST /api/v1/telegram/link/verify` | `POST /api/v1/identity/link/verify` |
| `GET /api/v1/telegram/link/status?telegram_user_id=X` | `GET /api/v1/identity/link/status?platform=telegram&platform_user_id=X` |

#### Registration Link URL Convention

When the plugin sends a registration link to the user, the URL includes
platform-specific data as query parameters so the hub web UI page is generic:

```
{hub_url}/profile/link?platform=telegram&code=AB3X7K&user_name=@alice
{hub_url}/profile/link?platform=discord&code=XY9M2P&user_name=alice%230001
{hub_url}/profile/link?platform=slack&code=QR4T8N&user_name=@alice
```

The hub web UI renders a single generic page:

```
┌─────────────────────────────────────┐
│  Link your Telegram account         │
│                                     │
│  Linking @alice to your scion       │
│  account.                           │
│                                     │
│  Code: [AB3X7K] (pre-filled)        │
│                                     │
│  [Confirm Link]                     │
└─────────────────────────────────────┘
```

The page title and description are derived from the `platform` query param.
The code is pre-filled from the `code` param. On submit, the page POSTs to
`/api/v1/identity/link/verify` with the user's JWT.

### 10.4 Database Schema

Two tables: one for pending links (transient), one for confirmed links
(persistent). This replaces the in-memory map and adds a durable record.

#### `pending_platform_links` (transient, TTL-cleaned)

```sql
CREATE TABLE pending_platform_links (
    code              TEXT PRIMARY KEY,        -- 6-char code, uppercase
    platform          TEXT NOT NULL,           -- "telegram", "discord", "slack"
    platform_user_id  TEXT NOT NULL,           -- external ID on that platform
    metadata          JSONB,                   -- platform-specific: username, display_name, etc.
    status            TEXT NOT NULL DEFAULT 'pending',  -- "pending" | "confirmed"
    confirmed_user_id TEXT,                    -- scion user ID (set on verify)
    confirmed_email   TEXT,                    -- scion email (set on verify)
    created_at        TIMESTAMP NOT NULL DEFAULT NOW(),
    expires_at        TIMESTAMP NOT NULL,      -- created_at + 15 minutes
    UNIQUE (platform, platform_user_id)        -- one pending code per platform user
);

CREATE INDEX idx_pending_platform_user
    ON pending_platform_links (platform, platform_user_id);
```

The `UNIQUE (platform, platform_user_id)` constraint means registering a new
code for the same platform user replaces the previous one (same as the current
Telegram behavior). A background cleanup job deletes expired rows.

#### `external_identities` (persistent, the authoritative link record)

```sql
CREATE TABLE external_identities (
    id                TEXT PRIMARY KEY,        -- UUID
    user_id           TEXT NOT NULL,           -- FK to users.id
    platform          TEXT NOT NULL,           -- "telegram", "discord", "slack"
    platform_user_id  TEXT NOT NULL,           -- external platform ID
    metadata          JSONB,                   -- username, display_name, avatar, etc.
    linked_at         TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (platform, platform_user_id),       -- one scion user per platform identity
    UNIQUE (user_id, platform)                 -- one platform identity per scion user per platform
);

CREATE INDEX idx_ext_id_user ON external_identities (user_id);
CREATE INDEX idx_ext_id_platform ON external_identities (platform, platform_user_id);
```

**Constraint semantics:**
- `UNIQUE (platform, platform_user_id)` — a Telegram user can only be linked
  to one scion user (prevents identity sharing)
- `UNIQUE (user_id, platform)` — a scion user can only have one Telegram
  identity (simplifies routing; if multi-account is needed later, drop this)

When `/api/v1/identity/link/verify` succeeds, the service:
1. Marks the `pending_platform_links` row as `status = 'confirmed'`
2. Inserts a row into `external_identities`
3. The plugin polls `GET .../status`, sees `confirmed`, and stores a local copy

### 10.5 Service Implementation

```go
// pkg/hub/platform_link.go

type PlatformLinkService struct {
    store  store.PlatformLinkStore
    logger *slog.Logger
}

type PlatformLinkStore interface {
    // Pending links (transient)
    CreatePendingLink(ctx context.Context, link *PendingPlatformLink) error
    GetPendingByCode(ctx context.Context, code string) (*PendingPlatformLink, error)
    GetPendingByPlatformUser(ctx context.Context, platform, platformUserID string) (*PendingPlatformLink, error)
    ConfirmPending(ctx context.Context, code, userID, email string) error
    DeleteExpiredPending(ctx context.Context) (int, error)

    // External identities (persistent)
    CreateExternalIdentity(ctx context.Context, id *ExternalIdentity) error
    GetExternalIdentity(ctx context.Context, platform, platformUserID string) (*ExternalIdentity, error)
    GetExternalIdentitiesByUser(ctx context.Context, userID string) ([]*ExternalIdentity, error)
    DeleteExternalIdentity(ctx context.Context, platform, platformUserID string) error
}

// RegisterCode is called by broker plugins via POST /api/v1/identity/link
func (s *PlatformLinkService) RegisterCode(ctx context.Context, platform, platformUserID, code string, metadata map[string]any) error {
    return s.store.CreatePendingLink(ctx, &PendingPlatformLink{
        Code:           strings.ToUpper(code),
        Platform:       platform,
        PlatformUserID: platformUserID,
        Metadata:       metadata,
        Status:         "pending",
        ExpiresAt:      time.Now().Add(15 * time.Minute),
    })
}

// VerifyCode is called by the web UI via POST /api/v1/identity/link/verify
func (s *PlatformLinkService) VerifyCode(ctx context.Context, code, userID, email string) (*PendingPlatformLink, error) {
    pending, err := s.store.GetPendingByCode(ctx, strings.ToUpper(code))
    if err != nil {
        return nil, fmt.Errorf("invalid code")
    }
    if pending.Status != "pending" {
        return nil, fmt.Errorf("code already used")
    }
    if time.Now().After(pending.ExpiresAt) {
        return nil, fmt.Errorf("code expired")
    }

    // Confirm the pending link
    if err := s.store.ConfirmPending(ctx, pending.Code, userID, email); err != nil {
        return nil, err
    }

    // Create the persistent external identity record
    if err := s.store.CreateExternalIdentity(ctx, &ExternalIdentity{
        ID:             uuid.NewString(),
        UserID:         userID,
        Platform:       pending.Platform,
        PlatformUserID: pending.PlatformUserID,
        Metadata:       pending.Metadata,
    }); err != nil {
        return nil, err
    }

    pending.Status = "confirmed"
    pending.ConfirmedUserID = userID
    pending.ConfirmedEmail = email
    return pending, nil
}

// GetStatus is called by broker plugins via GET /api/v1/identity/link/status
func (s *PlatformLinkService) GetStatus(ctx context.Context, platform, platformUserID string) (string, string, string, error) {
    pending, err := s.store.GetPendingByPlatformUser(ctx, platform, platformUserID)
    if err != nil {
        return "", "", "", err
    }
    return pending.Status, pending.ConfirmedUserID, pending.ConfirmedEmail, nil
}
```

### 10.6 Plugin-Side Changes

The Telegram plugin's `register_v2.go` changes its HTTP calls from
Telegram-specific to generic endpoints:

```go
// Before (Telegram-specific):
POST {hubURL}/api/v1/telegram/link
     {"code": "AB3X7K", "telegram_user_id": "98765"}

// After (generic):
POST {hubURL}/api/v1/identity/link
     {"platform": "telegram", "platform_user_id": "98765",
      "code": "AB3X7K", "metadata": {"username": "@alice"}}
```

```go
// Before:
GET {hubURL}/api/v1/telegram/link/status?telegram_user_id=98765

// After:
GET {hubURL}/api/v1/identity/link/status?platform=telegram&platform_user_id=98765
```

The registration link sent to the user changes format:

```go
// Before:
fmt.Sprintf("%s/profile/telegram-link?code=%s", hubURL, code)

// After:
fmt.Sprintf("%s/profile/link?platform=telegram&code=%s&user_name=%s",
    hubURL, code, url.QueryEscape("@"+username))
```

The plugin still stores a local `TelegramUserMapping` in its SQLite for
hot-path lookups. The hub's `external_identities` table is the authoritative
record; the plugin's local copy is a cache.

### 10.7 How Other Platforms Use the Same Service

#### Discord Plugin

```go
// On /register command in Discord:
code := generateLinkingCode()
httpPost(hubURL+"/api/v1/identity/link", map[string]any{
    "platform":         "discord",
    "platform_user_id": discordUser.ID,
    "code":             code,
    "metadata": map[string]any{
        "username":      discordUser.Username + "#" + discordUser.Discriminator,
        "display_name":  discordUser.GlobalName,
    },
})
linkURL := fmt.Sprintf("%s/profile/link?platform=discord&code=%s&user_name=%s",
    hubURL, code, url.QueryEscape(discordUser.Username))
// Send linkURL to user via Discord DM
// Start polling GET /api/v1/identity/link/status?platform=discord&platform_user_id=...
```

#### Slack Plugin

```go
// On /register slash command in Slack:
code := generateLinkingCode()
httpPost(hubURL+"/api/v1/identity/link", map[string]any{
    "platform":         "slack",
    "platform_user_id": slackUser.ID,
    "code":             code,
    "metadata": map[string]any{
        "username":     slackUser.Name,
        "workspace_id": slackWorkspace.ID,
    },
})
linkURL := fmt.Sprintf("%s/profile/link?platform=slack&code=%s&user_name=%s",
    hubURL, code, url.QueryEscape("@"+slackUser.Name))
// Send linkURL to user via Slack DM (as a button)
// Start polling...
```

#### Chat-App (Google Chat)

The existing chat-app uses device auth (`Auth().RequestDeviceCode`). It could
optionally migrate to this service, or keep device auth as an alternative flow.
Both are valid — device auth is pull-based (user authenticates at a URL), while
platform linking is push-based (user confirms a code in the hub UI). The
`external_identities` table should be populated by either flow.

### 10.8 Relationship to Invite Codes

The invite code system (`InviteService` in `pkg/hub/invite_service.go`) and
platform linking serve different purposes but share structural patterns:

| Aspect | Invite Codes | Platform Linking |
|--------|-------------|------------------|
| **Purpose** | Onboard new user to hub | Link existing user to external platform |
| **Who initiates** | Admin creates code | Plugin creates code on user's behalf |
| **Who redeems** | New user | Existing authenticated user |
| **Auth required to redeem** | None (code grants access) | User JWT (must already be logged in) |
| **Persistence** | DB (InviteCodeStore) | DB (PlatformLinkStore) |
| **Code format** | `scion_inv_` + 24 random bytes | 6-char alphanumeric |

They are **peer concepts** — both involve a code-mediated trust bridge between
an outside context and the hub. However, the semantics are different enough
that sharing implementation would be forced. The recommended approach:

- **Shared patterns**: Both use the same DB, same code validation patterns,
  same cleanup goroutine approach
- **Separate services**: `InviteService` and `PlatformLinkService` remain
  distinct because their auth models differ (invite = unauthenticated redeem,
  platform link = authenticated verify)
- **Shared table patterns**: The `pending_platform_links` table mirrors the
  invite code's hash + expiry + use-tracking pattern, adapted for the
  two-phase (register + verify) flow

### 10.9 Migration Path

#### Phase 1: Add Generic Service Alongside Telegram-Specific

1. Create `pkg/hub/platform_link.go` with `PlatformLinkService`
2. Add `pending_platform_links` and `external_identities` tables
3. Register generic endpoints at `/api/v1/identity/link*`
4. Keep existing `/api/v1/telegram/link*` endpoints as thin wrappers that
   delegate to `PlatformLinkService` with `platform = "telegram"`
5. Update Telegram plugin `register_v2.go` to call generic endpoints

#### Phase 2: Remove Telegram-Specific Code

1. Remove `TelegramLinkService` from `pkg/hub/telegram_link.go`
2. Remove `/api/v1/telegram/link*` route registrations
3. Remove Telegram-specific handler methods from server
4. All Telegram linking flows through generic service

#### Phase 3: Onboard Other Platforms

1. Discord plugin uses `POST /api/v1/identity/link` with `platform: "discord"`
2. Slack plugin uses same with `platform: "slack"`
3. Hub web UI `/profile/link` page already handles all platforms generically
4. `external_identities` table accumulates all platform links

### 10.10 Files to Change

| File | Change |
|------|--------|
| `pkg/hub/platform_link.go` | **NEW**: `PlatformLinkService` implementation |
| `pkg/hub/handlers.go` | Add generic `/api/v1/identity/link*` handlers |
| `pkg/hub/server.go` | Register new routes, initialize `PlatformLinkService` |
| `pkg/store/models.go` | Add `PendingPlatformLink`, `ExternalIdentity` models |
| `pkg/store/store.go` | Add `PlatformLinkStore` interface |
| `pkg/store/postgres/` | Add migration for `pending_platform_links` and `external_identities` tables |
| `pkg/hub/telegram_link.go` | Phase 1: thin wrapper → Phase 2: remove |
| `pkg/plugin/telegram/register_v2.go` | Update HTTP calls to generic endpoints |
| Web UI | Add generic `/profile/link` page (replaces `/profile/telegram-link`) |

---

## Future Direction: DM-as-Private-Agent-Channel (/talk command)

**Concept**: Users can initiate a private 1:1 conversation with a specific agent via the bot DM, without involving a group chat.

**Proposed UX:**
- `/talk chat2 coordinator` in DM → routes subsequent DM messages to that agent
- `/endtalk` → stops DM routing, returns to command-only DM mode
- Agent replies come back to the user's DM (not any group)

**Architecture fit:**
- `ConversationContext` already tracks (userID, projectID, agentSlug) → lastChatID
- If DM chatID (= user's Telegram userID for private chats) is stored as lastChatID, agent replies automatically route to DM
- Missing: routing of non-command DM messages to the configured agent

**Related:** `scion message --thread-id` flag (exists in CLI) may allow agents to reference a specific conversation thread. Future implementation could use thread IDs to link DM conversations to specific agent sessions, enabling proper conversation continuity across DM ↔ group transitions.

**Open questions:**
- How does user end a /talk session and return to group routing?
- How to handle multiple concurrent /talk sessions (one per agent)?
- Should /talk persist across bot restarts (stored in DB)?
