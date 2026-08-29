# Messages Evolution: Built-In Inbox and Bidirectional Human-Agent Messaging

## Status
**Complete** | March 2026 — All phases complete

## Problem

Today the system has a complete **human → agent** message path (`scion message`, Hub API, broker dispatch) and a robust **notification system** for status-change alerts (COMPLETED, WAITING_FOR_INPUT, etc.). However, there is no **agent → human** message path beyond status alerts. When an agent calls `sciontool status ask_user "What should I do?"`, it:

1. Sets `activity = waiting_for_input` (sticky) in `agent-info.json`
2. Reports the status to the Hub
3. Triggers a notification if someone is subscribed to WAITING_FOR_INPUT

The notification contains the question text, but it is a status alert, not a conversational message. It cannot be replied to, it is not stored in an inbox, and the human must use `scion attach` or `scion message` to respond — with no connection between the question asked and the answer given.

Additionally, message history is only available via Cloud Logging (`scion-messages` log). There is no built-in message store, which means:
- Self-hosted / local deployments have no message history at all
- The web `agent-message-viewer` only works with GCP Cloud Logging configured
- There is no CLI for a human to check "what messages have my agents sent me?"

### What Works Today

| Path | Mechanism | Status |
|---|---|---|
| Human → Agent | `scion message`, Hub API, broker dispatch, tmux injection | Complete |
| Agent → Agent | Structured message via Hub dispatcher, broker topics | Complete |
| Status alerts → Human | Notification subscriptions, SSE, external channels | Complete |
| Agent → Human (message) | **None** | Missing |
| Human inbox (retrieve) | Cloud Logging only (GCP-dependent) | Partial |

## Goals

1. **Built-in message store** — persist all structured messages (both directions) in the Hub database, independent of Cloud Logging.
2. **Agent → human messaging** — let agents send explicit messages to humans, beyond status alerts. `sciontool status ask_user` should both set state and send a message.
3. **Human inbox CLI** — `scion messages` command for humans to list, read, and acknowledge messages, mirroring `scion notifications`.
4. **Broker integration** — route human-targeted messages through the existing `MessageBrokerProxy` and `ChannelRegistry` infrastructure.
5. **No threading** — messages are flat, not threaded. No conversation or correlation model.

## Non-Goals

- Threaded conversations or request/response pairing
- Real-time chat UI (the web `agent-message-viewer` already exists and can be adapted later)
- Replacing Cloud Logging message audit (the dedicated message log remains for observability)
- Changing the notification system (notifications remain status-change alerts; messages are a separate concept)

## Related: Message Mode Authorization

Message delivery is subject to the per-agent **message mode** system, which
controls who can deliver messages to each agent. All ingresses described in
this document (direct API, chat v2, broadcast, broker inbound) pass through
the `authorizeAgentMessage` choke point before delivery. See
[docs/messaging-authorization.md](../docs/messaging-authorization.md) for the
mode definitions, decision table, and piercing rules.

---

## Design

### 1. Message Store

#### Data Model

A new `messages` table stores all structured messages that transit the Hub, in both directions.

```go
// pkg/store/models.go

// Message represents a persisted structured message.
type Message struct {
    ID          string    `json:"id"`
    GroveID     string    `json:"groveId"`
    Sender      string    `json:"sender"`       // "user:alice", "agent:code-reviewer"
    SenderID    string    `json:"senderId"`      // UUID or identity key
    Recipient   string    `json:"recipient"`     // "user:alice", "agent:code-reviewer"
    RecipientID string    `json:"recipientId"`
    Msg         string    `json:"msg"`
    Type        string    `json:"type"`          // "instruction", "input-needed", "state-change"
    Urgent      bool      `json:"urgent,omitempty"`
    Broadcasted bool      `json:"broadcasted,omitempty"`
    Read        bool      `json:"read"`          // Whether recipient has read/acknowledged
    AgentID     string    `json:"agentId"`       // The agent involved (sender or recipient)
    CreatedAt   time.Time `json:"createdAt"`
}
```

The `AgentID` field normalizes which agent is involved regardless of direction, enabling efficient queries for "all messages involving agent X". For human-to-agent messages, `AgentID = RecipientID`. For agent-to-human messages, `AgentID = SenderID`.

#### Schema

```sql
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    grove_id TEXT NOT NULL,
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    msg TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'instruction',
    urgent INTEGER NOT NULL DEFAULT 0,
    broadcasted INTEGER NOT NULL DEFAULT 0,
    read INTEGER NOT NULL DEFAULT 0,
    agent_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_messages_grove ON messages(grove_id);
CREATE INDEX IF NOT EXISTS idx_messages_recipient ON messages(recipient_id, read);
CREATE INDEX IF NOT EXISTS idx_messages_agent ON messages(agent_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at DESC);
```

No foreign key to `agents` — messages survive agent deletion for audit purposes. The `agent_id` index enables the common "show me messages for this agent" query from the agent detail view.

#### Store Interface

```go
// pkg/store/store.go

// MessageStore manages persisted structured messages.
type MessageStore interface {
    // CreateMessage persists a new message.
    CreateMessage(ctx context.Context, msg *Message) error

    // GetMessage returns a single message by ID.
    // Returns ErrNotFound if the message doesn't exist.
    GetMessage(ctx context.Context, id string) (*Message, error)

    // ListMessages returns messages matching the given filter.
    // Results are ordered by created_at DESC.
    ListMessages(ctx context.Context, filter MessageFilter, opts ListOptions) (*ListResult[Message], error)

    // MarkMessageRead marks a message as read.
    // Returns ErrNotFound if the message doesn't exist.
    MarkMessageRead(ctx context.Context, id string) error

    // MarkAllMessagesRead marks all messages for a recipient as read.
    MarkAllMessagesRead(ctx context.Context, recipientID string) error
}

// MessageFilter defines query parameters for listing messages.
type MessageFilter struct {
    GroveID     string // Filter by grove
    AgentID     string // Filter by involved agent
    RecipientID string // Filter by recipient
    SenderID    string // Filter by sender
    OnlyUnread  bool   // Only unread messages
    Type        string // Filter by message type
}
```

The `MessageStore` is added to the composite `Store` interface alongside the existing `NotificationStore`.

#### Retention

Messages accumulate over time. A configurable retention policy should be applied:

- Default: 30 days
- Configurable via Hub server config (`message_retention_days`)
- Cleanup runs as part of the existing maintenance cycle
- Read messages older than retention are deleted; unread messages are retained longer (90 days) to avoid silent data loss

### 2. Message Persistence in Hub Handlers

#### Write Path

Messages are persisted at the Hub layer when they transit through the existing dispatch handlers. This is a write-through approach — the message is stored and then dispatched.

**`handleAgentMessage`** (`pkg/hub/handlers.go`): After constructing the `StructuredMessage` and before calling `dispatcher.DispatchAgentMessage()`, persist to the message store:

```go
// Persist to message store
storeMsg := &store.Message{
    ID:          api.NewUUID(),
    GroveID:     agent.GroveID,
    Sender:      structuredMsg.Sender,
    SenderID:    structuredMsg.SenderID,
    Recipient:   structuredMsg.Recipient,
    RecipientID: structuredMsg.RecipientID,
    Msg:         structuredMsg.Msg,
    Type:        structuredMsg.Type,
    Urgent:      structuredMsg.Urgent,
    Broadcasted: structuredMsg.Broadcasted,
    AgentID:     agent.ID,
    CreatedAt:   time.Now(),
}
if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
    s.log.Error("Failed to persist message", "error", err)
    // Non-fatal: dispatch continues even if persistence fails
}
```

This is added to:
- `handleAgentMessage` — human/agent → agent messages
- `handleGroveBroadcast` / `broadcastDirect` — broadcast messages (one record per recipient)
- `NotificationDispatcher.dispatchToAgent` — notification messages dispatched as structured messages
- The new `handleAgentOutboundMessage` endpoint (see section 3)

The dedicated Cloud Logging message log (`scion-messages`) remains as a parallel audit trail.

#### Read Path: New API Endpoints

```
GET  /api/v1/messages                    — List messages for the authenticated user
GET  /api/v1/messages/{id}               — Get a single message
POST /api/v1/messages/{id}/read          — Mark a message as read
POST /api/v1/messages/read-all           — Mark all messages as read
GET  /api/v1/agents/{id}/messages        — List messages involving a specific agent
```

**`GET /api/v1/messages`** query parameters:
- `unread` (bool) — filter to unread only (default: false)
- `grove` (string) — filter by grove ID
- `agent` (string) — filter by involved agent ID
- `type` (string) — filter by message type
- `limit` / `cursor` — pagination (standard `ListOptions`)

The authenticated user's identity determines the `recipientID` filter. Agent-authenticated requests filter by the agent's own ID.

### 3. Agent → Human Outbound Messages

#### Hub API Endpoint

A new endpoint allows agents (and the broker) to send messages addressed to humans:

```
POST /api/v1/agents/{id}/outbound-message
```

Request body:
```json
{
  "recipient": "user:alice",
  "recipient_id": "user-uuid-or-email",
  "msg": "I need clarification on the auth module scope.",
  "type": "input-needed",
  "urgent": false
}
```

The sender is auto-populated from the agent's identity (agent token auth). The handler:

1. Validates the message and auto-populates sender fields from the agent record
2. Persists to the message store
3. Publishes a `grove.{groveID}.user.message` event via `ChannelEventPublisher` (for SSE)
4. Dispatches to external notification channels (`ChannelRegistry`) if configured (Slack, webhook, email)
5. Returns 200 OK

This endpoint is authenticated via agent token (same as `/api/v1/agent/status`).

#### Recipient Resolution

When an agent sends a message, the recipient may be:
- **Implicit (empty)** — the message targets the agent's `CreatedBy` user. The Hub resolves this from the agent record. The stored `recipient` becomes `user:<createdBy-identity>`.
- **Explicit** — `"user:alice@example.com"` or `"user:<user-id>"` targets a specific user.

Implicit resolution is the common case: the agent doesn't know or care who launched it, it just needs to ask a question.

#### Broker Integration

Outbound messages from agents to humans are routed through the existing infrastructure:

1. **MessageBrokerProxy**: Add a new topic pattern `grove.{groveID}.user.{userId}` for user-targeted messages. The proxy subscribes on behalf of connected users (via SSE) and delivers via the event publisher.
2. **ChannelRegistry**: The existing `dispatchToChannels` logic (Slack, webhook, email) is reused. The `ChannelRegistry.Dispatch()` method already accepts `StructuredMessage` — it just needs to be called for outbound agent messages, not only notifications.
3. **SSE**: The `ChannelEventPublisher` publishes a new event type `user.message` that the SSE endpoint can deliver to connected browser clients.

#### Agent Messaging via `scion message`

For explicit outbound messaging from inside a container, agents use the primary `scion message` CLI — not `sciontool`. Agents are already instructed to use `scion message` for any deliberate communication with humans or other agents. `sciontool` is reserved for hooks and implicit state changes (e.g., `ask_user` is a special case because it combines state mutation with messaging).

The `POST /api/v1/agents/{id}/outbound-message` endpoint is therefore called by the `scion message` command (running inside the container via the existing harness toolchain), not by a new sciontool subcommand.

### 4. `ask_user` Dual Behavior

Currently, `sciontool status ask_user "question"` only sets the activity to `waiting_for_input` and reports the status. With this change, it **also sends a message**:

```go
// cmd/sciontool/commands/status.go — runStatusAskUser()

func runStatusAskUser(message string) {
    statusHandler := handlers.NewStatusHandler()
    loggingHandler := handlers.NewLoggingHandler()

    // 1. Update activity to waiting_for_input (sticky) — existing behavior
    if err := statusHandler.UpdateActivity(state.ActivityWaitingForInput, ""); err != nil {
        log.Error("Failed to update status: %v", err)
    }

    // 2. Log the event — existing behavior
    logMessage := fmt.Sprintf("Agent requested input: %s", message)
    if err := loggingHandler.LogEvent(string(state.ActivityWaitingForInput), logMessage); err != nil {
        log.Error("Failed to log event: %v", err)
    }

    // 3. Report status to Hub — existing behavior
    hubClient := hub.NewClient()
    if hubClient != nil && hubClient.IsConfigured() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
        if err := hubClient.UpdateStatus(ctx, hub.StatusUpdate{
            Activity: state.ActivityWaitingForInput,
            Status:   as.DisplayStatus(),
            Message:  message,
        }); err != nil {
            log.Error("Failed to report to Hub: %v", err)
        }

        // 4. NEW: Send an outbound message with the question
        if err := hubClient.SendOutboundMessage(ctx, hub.OutboundMessage{
            Msg:  message,
            Type: "input-needed",
        }); err != nil {
            log.Error("Failed to send outbound message: %v", err)
        }
    }

    log.Info("Agent asked: %s", message)
}
```

The `SendOutboundMessage` call hits `POST /api/v1/agents/{id}/outbound-message`. The Hub handler persists the message to the inbox, publishes SSE events, and dispatches to external channels.

This means a single `sciontool status ask_user "question"` call:
- Sets sticky `waiting_for_input` activity (for status display and notification triggers)
- Sends a persisted `input-needed` message to the human inbox (for retrieval and display)
- Triggers notification subscriptions (existing behavior, unchanged)
- Dispatches to Slack/webhook/email channels (via both notification and message channel paths)

The notification system continues to work independently — subscribers to WAITING_FOR_INPUT still get notifications. The message is an additional delivery that provides the question text in an inbox that can be queried later.

### 5. CLI: `scion messages`

A new top-level command group mirrors the structure of `scion notifications`:

```
scion messages                              List your messages (unread by default)
scion messages --all                        List all messages (including read)
scion messages --agent <name>               Filter by agent
scion messages --json                       Output in JSON format
scion messages read [id]                    Mark message(s) as read
scion messages read --all                   Mark all messages as read
```

#### Command Definition

```go
// cmd/messages.go

var messagesCmd = &cobra.Command{
    Use:     "messages",
    Aliases: []string{"msgs", "inbox"},
    Short:   "View messages from agents",
    Long: `View and manage messages sent to you by agents.

Messages require Hub mode. Enable with 'scion hub enable <endpoint>'.

Commands:
  scion messages                            List your unread messages
  scion messages --all                      List all messages (including read)
  scion messages --agent <name>             Filter by agent
  scion messages read [id]                  Mark message(s) as read
  scion messages read --all                 Mark all messages as read`,
    RunE: runMessagesList,
}

var messagesReadCmd = &cobra.Command{
    Use:   "read [message-id]",
    Short: "Mark message(s) as read",
    Long: `Mark one or all messages as read.

With an ID argument, marks that specific message as read.
With --all flag, marks all unread messages as read.

Examples:
  scion messages read a1b2c3d4
  scion messages read --all`,
    RunE: runMessagesRead,
}
```

#### Output Format

```
$ scion messages
ID            AGENT           TYPE            TIME                  MESSAGE
------------  --------------  --------------  --------------------  -------
a1b2c3d4e5f6  code-reviewer   input-needed    2026-03-26 14:30      I need clarification on the auth module...
b2c3d4e5f6a1  deploy-agent    state-change    2026-03-26 14:25      deploy-agent has reached COMPLETED: De...

$ scion messages read --all
All messages marked as read.
```

### 6. Hub Client Extensions

The `hubclient` package needs new service methods:

```go
// pkg/hubclient/messages.go

// MessageService provides operations on the user's message inbox.
type MessageService interface {
    // List returns messages for the authenticated user.
    List(ctx context.Context, opts *ListMessagesOptions) ([]store.Message, error)

    // Get returns a single message by ID.
    Get(ctx context.Context, id string) (*store.Message, error)

    // MarkRead marks a message as read.
    MarkRead(ctx context.Context, id string) error

    // MarkAllRead marks all messages as read.
    MarkAllRead(ctx context.Context) error
}

type ListMessagesOptions struct {
    OnlyUnread bool
    AgentID    string
    GroveID    string
    Type       string
    Limit      int
    Cursor     string
}
```

The `sciontool/hub` client also needs `SendOutboundMessage`:

```go
// pkg/sciontool/hub/client.go

type OutboundMessage struct {
    Msg       string `json:"msg"`
    Type      string `json:"type"`
    Recipient string `json:"recipient,omitempty"`
    Urgent    bool   `json:"urgent,omitempty"`
}

func (c *Client) SendOutboundMessage(ctx context.Context, msg OutboundMessage) error {
    // POST /api/v1/agents/{agentID}/outbound-message
    // agentID is resolved from the agent's own identity token
}
```

### 7. Relationship Between Messages and Notifications

Messages and notifications are **parallel systems** that serve different purposes:

| Aspect | Notifications | Messages |
|---|---|---|
| Trigger | Agent status change (automatic) | Explicit send by agent or human |
| Content | Formatted status string | Arbitrary text from sender |
| Subscription | Required (opt-in via `--notify` or `subscribe`) | Inbox is always available |
| Persistence | `notifications` table | `messages` table |
| Acknowledge | `ack` (binary) | `read` (binary) |
| Direction | System → subscriber | Agent ↔ human, agent ↔ agent |
| External delivery | Slack, webhook, email (via channels) | Same channels, plus inbox |

The notification system is not modified. When `ask_user` fires:
- A notification is created for WAITING_FOR_INPUT subscribers (existing behavior)
- A message is created in the inbox (new behavior)

These are independent records. Acknowledging a notification does not mark the message as read, and vice versa.

### 8. Web Frontend: Inbox Tray

A new **inbox tray** component will be added to the web UI, positioned adjacent to the existing notification tray in the top navigation bar. It uses an envelope icon (`envelope` from Bootstrap Icons) to distinguish it visually from the bell icon used for notifications.

#### Inbox Tray Behavior

- Displays a badge with the count of unread messages (fetched from `GET /api/v1/messages?unread=true`)
- Clicking the icon opens a tray panel listing recent messages, similar in layout and interaction to the notification tray
- Each message entry shows: sender agent name, message type, truncated message text, and relative timestamp
- Clicking a message marks it as read (`POST /api/v1/messages/{id}/read`) and optionally expands the full text
- A "Mark all read" action calls `POST /api/v1/messages/read-all`
- Real-time updates: subscribes to the `user.message` SSE event to update the badge count and prepend new messages without a page reload

#### Shared UI Componentry

The notification tray and inbox tray share structural patterns (panel open/close, badge overlay, item list, timestamp formatting, read/unread state). The implementation should extract reusable pieces:

- **`<TrayPanel>`** — generic slide-out panel shell (trigger icon + badge + panel body)
- **`<TrayItem>`** — generic list item with read/unread state, timestamp, and expandable body
- The notification tray is refactored to use these shared components before or alongside building the inbox tray

#### `agent-message-viewer` Migration

The existing `agent-message-viewer` reads from Cloud Logging. With the new message store available, it can be updated to read from `GET /api/v1/agents/{id}/messages` as the primary source, falling back to the Cloud Logging proxy for historical records predating the migration. This is a lower priority than the inbox tray.

---

## Implementation Plan

### Phase 1: Message Store and Persistence ✅ COMPLETE

1. ~~Add `Message` model to `pkg/store/models.go`~~
2. ~~Add `MessageStore` interface to `pkg/store/store.go`~~
3. ~~Add `messages` table to `pkg/store/sqlite/sqlite.go` (schema migration V37)~~
4. ~~Implement `MessageStore` in `pkg/store/sqlite/messages.go`~~
5. ~~Wire `MessageStore` into the composite `Store` interface~~
6. ~~Add message persistence to `handleAgentMessage` (write-through)~~
7. ~~Add message persistence to broadcast handlers~~

### Phase 2: Agent Outbound and `ask_user` ✅ COMPLETE

1. ~~Add `POST /api/v1/agents/{id}/outbound-message` Hub handler~~
2. ~~Add `SendOutboundMessage` to `sciontool/hub` client~~
3. ~~Update `sciontool status ask_user` to dual-send (state + message)~~
4. ~~Add recipient resolution logic (implicit → agent creator / subscribers)~~
5. ~~Integrate outbound messages with `ChannelRegistry` (Slack, webhook, email)~~

### Phase 3: Human Inbox CLI and API ✅ COMPLETE

1. ~~Add `GET /api/v1/messages` and related endpoints to Hub server~~
2. ~~Add `MessageService` to `pkg/hubclient/messages.go`~~
3. ~~Add `scion messages` command group to `cmd/messages.go`~~
4. ~~Add `scion messages read` subcommand~~

### Phase 4: Broker Integration ✅ COMPLETE

1. ~~Add `grove.{groveID}.user.{userId}` topic pattern to `MessageBrokerProxy`~~
2. ~~Subscribe on behalf of SSE-connected users~~
3. ~~Publish `user.message` events for real-time delivery~~
4. ~~Add message persistence in `MessageBrokerProxy.deliverToAgent` (for broker-routed messages)~~

### Phase 5: Web Frontend Inbox Tray ✅ COMPLETE

1. ~~Extract `<TrayPanel>` and `<TrayItem>` shared components from the existing notification tray~~ (skipped — kept self-contained per simplicity; both trays share visual patterns without a premature abstraction)
2. ~~Refactor `notification-tray` to use the shared components~~ (skipped for same reason)
3. ~~Build `inbox-tray` component using the shared components; wire to `GET /api/v1/messages`~~
4. ~~Add envelope icon to the icon registry (`web/scripts/copy-shoelace-icons.mjs`)~~
5. ~~Subscribe to `user.message` SSE events for real-time badge updates~~
6. ~~Implement mark-read and mark-all-read actions~~
7. ~~Update `agent-message-viewer` to read from `GET /api/v1/agents/{id}/messages` as primary source~~

---

## Decisions

1. **Message retention** — 30 days for read messages, 90 days for unread. Cleanup is implemented as a scheduled event using the Hub's built-in scheduler, not a separate cron or background goroutine.

2. **Implicit recipient resolution** — unaddressed outbound messages (e.g. from `ask_user`) go to the agent's `CreatedBy` user only. Notifications (status-change alerts) continue to go to all subscribers. These are distinct systems with distinct audiences: a question is directed at whoever started the work, not broadcast to all watchers.

3. **Notifications and messages both fire** — when `ask_user` fires, both a WAITING_FOR_INPUT notification and an `input-needed` message are created independently. They may be routed differently: notifications drive external alert channels (Slack, webhook, email) for all subscribers; messages populate the inbox for the creator. Neither suppresses the other.

4. **Hub-only** — the message store and inbox are Hub-only, matching the notification system. Local (non-Hub) mode is out of scope.
