# Event Bus Rename: Disambiguate "broker"

**Date:** 2026-05-31
**Task:** Rename pkg/broker to pkg/eventbus, rename CLI `broker` to `runtime-broker`
**Issue:** #95

## What Changed

### Package Rename: `pkg/broker` -> `pkg/eventbus`

The `pkg/broker` package was a NATS-style pub/sub event bus used for routing
real-time change events to SSE subscribers. Its name collided with the Message
Broker plugin system (`PluginTypeBroker`, `pkg/plugin/broker_plugin.go`),
causing confusion.

**Key type renames:**
- `MessageBroker` -> `EventBus`
- `MessageHandler` -> `EventHandler`
- `InProcessBroker` -> `InProcessEventBus`
- `FanOutBroker` -> `FanOutEventBus`
- `NamedBroker` -> `NamedEventBus`
- `ErrBrokerClosed` -> `ErrEventBusClosed`

**Files modified (imports/references):**
- `cmd/server_foreground.go`
- `pkg/plugin/broker_plugin.go`
- `pkg/plugin/manager.go`
- `pkg/hub/server.go`
- `pkg/hub/messagebroker.go`
- `pkg/hub/messagebroker_test.go`
- `pkg/hub/notifications_test.go`

### CLI Command Rename: `scion broker` -> `scion runtime-broker`

The CLI command was renamed using cobra's `Aliases` mechanism, keeping
`broker` as a backward-compatible deprecated alias. All help text
examples were updated to show the new name.

## Coordination Notes

- The `BrokerPluginAdapter` in `pkg/plugin/broker_plugin.go` now satisfies
  `eventbus.EventBus` instead of `broker.MessageBroker`. Its name was NOT
  renamed because it still wraps a *broker plugin* RPC client — "Broker" here
  refers to the Message Broker plugin system, not the event bus.
- Topic helper functions (`TopicAgentMessages`, etc.) moved to `pkg/eventbus`
  unchanged. Any code that imports them needs to update the import path.
- The `MessageBrokerProxy` in `pkg/hub/messagebroker.go` kept its name because
  it proxies between the event bus and the Message Broker plugin system.
  Its internal field was renamed from `broker` to `bus`.
