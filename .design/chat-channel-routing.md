# Design: Chat Channel Routing

**Issue:** #113 — Chat channel integration
**Author:** chatchannel agent
**Status:** Draft
**Date:** 2026-05-31

## Problem Statement

Today, when multiple broker plugins are configured (e.g., Telegram, Google Chat, web UI), a reply to a user is published through the `FanOutBroker`, which delivers it to **all** broker plugins simultaneously. There is no way for an agent to direct a reply to a specific channel (e.g., "reply only on Telegram") or maintain thread context within a channel.

This limits conversational quality: users may receive duplicate replies across platforms, and threaded conversations in chat systems like Telegram or Google Chat lose their context.

## Goals

1. Add `Channel` and `ThreadID` fields to `StructuredMessage` so messages can carry routing metadata.
2. When `Channel` is specified on an outbound message, deliver only through the matching broker plugin — not all plugins.
3. When `Channel` is absent, preserve current fan-out behavior (deliver to all).
4. When `ThreadID` is specified (implies `Channel`), the target plugin should deliver the message within that thread context.
5. Inbound messages from broker plugins should populate `Channel` (and optionally `ThreadID`) so agents know where the message originated.
6. Agents can receive on one channel and reply on another (e.g., receive on GChat, reply on Telegram).
7. Error on delivery failures: unmatched channel names and unresolved users on a specific channel must return errors to the agent.

## Non-Goals

- Multi-channel fan-out with explicit channel list (only one channel or all).
- Changing the notification channel system (`NotificationChannel` interface in `channels.go`). That system is separate from the message broker.
- Thread management UI in the web interface (web UI messages have no thread concept today).
- Configurable channel names per plugin instance (static names for now).

## Current Architecture

```
                         ┌──────────────┐
                         │   CLI / Web  │
                         └──────┬───────┘
                                │ POST /api/v1/agents/{id}/messages
                                ▼
                         ┌──────────────┐
                         │   Hub Server │
                         └──────┬───────┘
                                │ PublishMessage() / PublishUserMessage()
                                ▼
                    ┌───────────────────────┐
                    │  MessageBrokerProxy   │
                    │  (subscription mgmt)  │
                    └───────────┬───────────┘
                                │ broker.Publish(topic, msg)
                                ▼
                    ┌───────────────────────┐
                    │     FanOutBroker      │
                    │  (delegates to N      │
                    │   child brokers)      │
                    └───┬────────┬────────┬─┘
                        │        │        │
                        ▼        ▼        ▼
                  InProcess   Telegram   ChatApp
                  Broker      Plugin     Plugin
                  (local      (RPC)      (RPC)
                   dispatch)
```

Key files:
- `pkg/messages/types.go` — `StructuredMessage` struct
- `pkg/messages/format.go` — `FormatForDelivery()` / `deliveryMessage` struct
- `pkg/broker/fanout.go` — `FanOutBroker.Publish()` fans out to all children
- `pkg/hub/messagebroker.go` — `MessageBrokerProxy` bridges broker ↔ hub dispatch
- `pkg/plugin/broker_plugin.go` — `MessageBrokerPluginInterface` (RPC contract)
- `extras/scion-telegram/` — Telegram broker plugin
- `extras/scion-chat-app/` — Chat app broker plugin
- `cmd/message.go` — `scion message` CLI command

## Proposed Design

### 1. New Fields on `StructuredMessage`

Add two new optional fields to `StructuredMessage` in `pkg/messages/types.go`:

```go
type StructuredMessage struct {
    // ... existing fields ...

    // Channel identifies the message broker plugin that should deliver this
    // message. When set, only the broker plugin whose name matches this
    // value will publish the message. When empty, all plugins receive it
    // (current fan-out behavior).
    //
    // Inbound: set by broker plugins to identify themselves as the origin.
    // Outbound: set by agents to target a specific delivery channel.
    Channel  string `json:"channel,omitempty"`

    // ThreadID identifies a thread within the channel's native threading
    // system (e.g., Telegram message_thread_id, Google Chat thread name).
    // Requires Channel to be set. Plugins that don't support threading
    // ignore this field. Format is opaque — each plugin defines its own.
    ThreadID string `json:"thread_id,omitempty"`
}
```

**Validation rules** (added to `StructuredMessage.Validate()`):
- If `ThreadID` is set, `Channel` must also be set.
- `Channel` must be a non-empty string when set (max 64 characters, alphanumeric + hyphens).
- Only one channel can be specified (it's a string, not a list).

### 2. Delivery Format Changes

Update `deliveryMessage` in `pkg/messages/format.go` to include `Channel` and `ThreadID` so agents can see where messages came from:

```go
type deliveryMessage struct {
    // ... existing fields ...
    Channel  string `json:"channel,omitempty"`
    ThreadID string `json:"thread_id,omitempty"`
}
```

### 3. Channel-Aware Routing in `FanOutBroker`

Modify `FanOutBroker.Publish()` to check `msg.Channel`. When set, only publish to the child broker whose `NamedBroker.Name` matches `msg.Channel`, plus the InProcessBroker (name `"inprocess"`) which always receives all messages for local dispatch. When `Channel` is empty, fan out to all (current behavior).

The InProcessBroker is always included because it's the hub's internal dispatch layer, not a user-visible channel. Channel filtering applies only to external plugin brokers.

When the specified channel doesn't match any registered broker, return an error so the agent can react to the failure.

```go
func (f *FanOutBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
    if msg.Channel != "" {
        var matched bool
        // Always publish to InProcessBroker for local dispatch
        for _, nb := range f.brokers {
            if nb.Name == InProcessBrokerName {
                if err := nb.Broker.Publish(ctx, topic, msg); err != nil {
                    // InProcessBroker errors are critical
                    return err
                }
            }
            if nb.Name == msg.Channel {
                matched = true
                if err := nb.Broker.Publish(ctx, topic, msg); err != nil {
                    return fmt.Errorf("channel %q publish failed: %w", msg.Channel, err)
                }
            }
        }
        if !matched {
            return fmt.Errorf("no broker registered for channel %q", msg.Channel)
        }
        return nil
    }
    // Fan-out to all (existing behavior)
    // ...
}
```

### 4. Error Handling: User Not Registered on Channel

When an agent sends a message to a user on a specific channel (e.g., `channel: "telegram"`), but that user is not registered on that channel's platform, the plugin must return an error. This surfaces back through the `FanOutBroker` to the agent.

For example, in the Telegram plugin, if `msg.Recipient` is `"user:alice@example.com"` but Alice has no Telegram user mapping, `Publish()` should return an error like `"recipient user:alice@example.com is not registered on telegram"`.

This is distinct from the "no broker for channel" error — the channel exists and is healthy, but the specific user can't be reached on it.

### 5. Inbound Channel Tagging

Each broker plugin must set `msg.Channel` when constructing inbound messages.

**Telegram** (`extras/scion-telegram/internal/telegram/broker_v2.go`):
In `handleGroupMessage()` and `handleCallbackQuery()`, set `Channel: "telegram"` on the constructed `StructuredMessage`.

**Chat App** (`extras/scion-chat-app/`):
Set `Channel: "gchat"` (or whatever the configured channel name is).

**Web UI** (messages from the web interface):
Set `Channel: "web"` in the hub's `handleAgentMessage()` handler.

**Broker Log** (`extras/scion-broker-log/`):
Observer-only; no inbound messages. No changes needed.

**Thread ID mapping:**
- Telegram: Use Telegram's `message_thread_id` for forum topics. Format is opaque to the hub.
- Google Chat: Use the thread name/ID from Google Chat's API. Format is opaque to the hub.
- Web UI: No threading concept; leave `ThreadID` empty.

### 6. CLI Changes

#### `scion message` — add `--channel` and `--thread-id` flags

```
scion message agent:coder "do the thing" --channel telegram
scion message user:ptone@google.com "done!" --channel telegram --thread-id 12345
```

When agents send messages via `sciontool` or the hub API, the `Channel` and `ThreadID` fields in the structured message are forwarded.

#### `scion message channels` — new subcommand

List the available message channels (registered broker plugins). This gives agents and users a way to discover what channels are available without auto-injecting info into agent prompts.

```
$ scion message channels
NAME        STATUS    CAPABILITIES
inprocess   healthy   local-dispatch
telegram    healthy   echo-filter, long-polling, telegram-bot-api, ...
gchat       healthy   chat-bridge, notification-relay
```

**Implementation:** Add a new hub API endpoint `GET /api/v1/message-channels` that queries the plugin manager for registered broker plugins and their health status. The CLI `scion message channels` calls this endpoint and formats the output.

The hub already has `PluginInfo` and `HealthStatus` types in the plugin system. The endpoint aggregates `GetInfo()` and `HealthCheck()` from each registered broker plugin.

### 7. Hub API Changes

**New endpoint: `GET /api/v1/message-channels`**
Returns the list of registered message broker plugins (channels) with their names, health status, and capabilities.

**`OutboundMessageRequest`** (`pkg/hub/handlers.go`):
Add `Channel` and `ThreadID` fields so agent outbound messages can specify channel routing.

**`handleAgentOutboundMessage()`**:
Forward `Channel` and `ThreadID` to the `StructuredMessage` before publishing.

**`MessageRequest`** and **`handleAgentMessage()`**:
When a `structured_message` is provided, the `Channel` and `ThreadID` are already part of `StructuredMessage`. No additional work needed beyond validation.

### 8. Store Changes

Add `Channel` and `ThreadID` to `store.Message` so persisted messages retain channel context:

```go
type Message struct {
    // ... existing fields ...
    Channel  string `json:"channel,omitempty"`
    ThreadID string `json:"threadId,omitempty"`
}
```

This allows the web UI to display which channel a message came from/was sent to.

### 9. Plugin Interface Considerations

The `MessageBrokerPluginInterface.Publish()` method already receives the full `StructuredMessage`, which will include the new `Channel` and `ThreadID` fields after the struct change. No RPC protocol changes are needed — the fields travel inside the existing `PublishArgs.Msg`.

Plugins that support threading should read `msg.ThreadID` and deliver within the appropriate thread. Plugins that don't support threading simply ignore it.

Plugins should return errors when a targeted recipient can't be reached on their channel (e.g., user not registered on Telegram).

## Resolved Design Decisions

1. **Channel name registry:** Use static `NamedBroker.Name` values (e.g., `"telegram"`, `"gchat"`, `"web"`). Multi-instance naming can be addressed later.

2. **InProcessBroker handling:** Always include InProcessBroker in publishes regardless of `Channel` filtering. It's the hub's internal dispatch layer, not a user-visible channel.

3. **Error behavior on unmatched channel:** Return an error to the sender. Agents need to be able to react to delivery failures.

4. **Thread ID format:** Opaque — each plugin defines its own format. The hub treats ThreadID as a passthrough string. Plugins handle validation and graceful degradation.

5. **Channel discovery:** No auto-injection into agent instructions. Instead, add `scion message channels` subcommand to list available channels. Agents learn about channels from the `channel` field in messages they receive.

## Phased Implementation Plan

### Phase 1: Core Message Schema (foundation)

**Scope:** Add `Channel` and `ThreadID` fields to the message types and ensure they pass through the system without breaking anything.

**Changes:**
1. `pkg/messages/types.go` — Add `Channel` and `ThreadID` fields to `StructuredMessage`; add validation rules.
2. `pkg/messages/format.go` — Add `Channel` and `ThreadID` to `deliveryMessage` struct and `FormatForDelivery()`.
3. `pkg/store/models.go` — Add `Channel` and `ThreadID` to `store.Message`.
4. `pkg/hub/messagebroker.go` — Update `deliverToUser()` and `deliverToAgent()` to persist `Channel`/`ThreadID` in store messages.
5. Unit tests for validation (ThreadID requires Channel, etc.).

**Backward compatibility:** All new fields are `omitempty`. Existing messages without these fields work exactly as before.

### Phase 2: Channel-Aware Routing

**Scope:** Implement channel-based routing in `FanOutBroker` so outbound messages with `Channel` set are delivered only to the matching broker plugin. Return errors for unmatched channels.

**Changes:**
1. `pkg/broker/fanout.go` — Add channel filtering logic to `Publish()`. InProcessBroker (name `"inprocess"`) always receives. Error on unmatched channel.
2. `pkg/hub/handlers.go` — Add `Channel`/`ThreadID` to `OutboundMessageRequest`.
3. Integration tests for channel-targeted vs. fan-out delivery.
4. Integration tests for error on unmatched channel.

### Phase 3: Inbound Channel Tagging

**Scope:** Broker plugins set `Channel` (and optionally `ThreadID`) on all inbound messages.

**Changes:**
1. `extras/scion-telegram/` — Set `Channel: "telegram"` on all inbound messages; set `ThreadID` from `message_thread_id` for forum topics.
2. `extras/scion-chat-app/` — Set `Channel` to the configured channel name on inbound messages.
3. `pkg/hub/handlers.go` — Set `Channel: "web"` for messages from the web UI.
4. `extras/scion-broker-log/` — Log channel/thread fields.

### Phase 4: CLI and Agent Support

**Scope:** Allow agents and CLI users to specify channel routing and discover available channels.

**Changes:**
1. `scion message` CLI command — Add `--channel` and `--thread-id` flags.
2. `scion message channels` — New subcommand to list available message channels.
3. Hub API — New `GET /api/v1/message-channels` endpoint.
4. Update `sciontool` to forward channel metadata in messages.
5. Documentation updates for agent authors.

### Phase 5: User-Channel Validation in Plugins

**Scope:** Plugins return errors when a targeted recipient can't be reached on their channel.

**Changes:**
1. Telegram plugin — Return error when `msg.Recipient` resolves to a user with no Telegram mapping and `msg.Channel == "telegram"`.
2. Google Chat/chat-app plugin — Return error when user has no mapping on this channel.
3. Ensure errors propagate through `FanOutBroker` back to the agent.

### Phase 6: Thread Support in Plugins

**Scope:** Implement thread-aware delivery in plugins that support threading.

**Changes:**
1. Telegram plugin — Deliver within the correct `message_thread_id` when `ThreadID` is set.
2. Google Chat/chat-app plugin — Deliver within the correct thread when `ThreadID` is set.
3. Handle edge cases: invalid thread IDs, expired threads, etc.

## Testing Strategy

- **Unit tests:** Validation logic for Channel/ThreadID, FanOutBroker channel filtering, error on unmatched channel.
- **Integration tests:** End-to-end message flow with channel routing through FanOutBroker with mock plugins. User-not-registered error paths.
- **Manual testing:** Send messages via CLI with `--channel` flag and verify only the targeted plugin receives them. Test `scion message channels` output.

## Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Breaking existing message flow | All new fields are optional/omitempty; no-channel = current fan-out behavior |
| Plugin RPC compatibility | Fields are part of the existing StructuredMessage passed via RPC; no protocol changes needed |
| Agent confusion about channels | `scion message channels` provides discovery; agents learn from inbound message channel field |
| Thread ID mismatch | ThreadID is opaque; plugins validate their own thread IDs |
| User not registered on channel | Plugins return errors; agents can catch and fall back to fan-out or try another channel |
