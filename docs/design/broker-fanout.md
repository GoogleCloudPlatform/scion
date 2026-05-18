# Design: Multi-Broker Fan-Out for Scion Hub

**Status**: Draft (updated with owner feedback)  
**Author**: Research Agent  
**Date**: 2026-05-12  
**Updated**: 2026-05-12 — resolved open questions, added A2A bridge analysis

## 1. Problem Statement

The hub currently supports exactly one message broker plugin at a time, selected
via `server.message_broker.type` in settings.yaml. To run multiple integrations
simultaneously (e.g., scion-broker-log for observability, scion-chat-app for
Google Chat, scion-a2a-bridge for A2A protocol), users must resort to manual
chaining via the `--forward` flag in scion-broker-log. This is fragile, creates
a single chain of failure, and doesn't scale to N integrations.

**Goal**: The hub should natively support N simultaneous broker integrations with
independent lifecycle, health, and failure isolation.

## 2. Current Architecture Summary

### 2.1 Core Interface (`pkg/broker/broker.go`)

```go
type MessageBroker interface {
    Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error
    Subscribe(pattern string, handler MessageHandler) (Subscription, error)
    Close() error
}
```

The hub doesn't call `Subscribe` on the external broker plugin in the usual
sense. External plugins receive messages via `Publish()` RPC calls from the hub.
Inbound messages from external systems arrive via `POST /api/v1/broker/inbound`.

### 2.2 Message Flow

**Outbound (hub → external systems):**
1. Hub handler or agent dispatch calls `MessageBrokerProxy.PublishMessage/PublishBroadcast/PublishUserMessage`
2. Proxy calls `broker.Publish(topic, msg)` on the single broker
3. If broker is a plugin adapter, this becomes an RPC `Publish` call to the plugin process
4. Plugin delivers to external system (Google Chat, log file, etc.)

**Inbound (external systems → hub):**
1. External plugin receives message from its platform
2. Plugin calls `POST /api/v1/broker/inbound` with HMAC auth
3. Hub handler parses topic, finds agent, dispatches directly (bypassing broker to avoid loops)

### 2.3 Key Components

| Component | File | Role |
|-----------|------|------|
| `broker.MessageBroker` | `pkg/broker/broker.go` | Core interface |
| `InProcessBroker` | `pkg/broker/inprocess.go` | In-memory pub/sub with NATS-style matching |
| `MessageBrokerProxy` | `pkg/hub/messagebroker.go` | Bridges broker ↔ hub agent lifecycle |
| `BrokerPluginAdapter` | `pkg/plugin/broker_plugin.go` | Wraps RPC client as `broker.MessageBroker` |
| `reconnectingBrokerAdapter` | `pkg/plugin/broker_plugin.go` | Auto-reconnect wrapper for self-managed plugins |
| `PluginsConfig` | `pkg/plugin/config.go` | Config: `map[string]PluginEntry` (already supports N entries) |
| `V1MessageBrokerConfig` | `pkg/config/settings_v1.go` | Settings: `enabled` + `type` (single string) |
| `Server.StartMessageBroker` | `pkg/hub/server.go:1286` | Creates proxy with single broker, no-op on subsequent calls |
| Startup wiring | `cmd/server_foreground.go:296` | Reads single `type`, loads one plugin or inprocess |

### 2.4 Host Callbacks

Plugins can call back to the hub to request/cancel subscriptions via a reverse
RPC channel (`HostCallbacks` interface). The `Manager` holds a single
`HostCallbacksForwarder` that delegates to `MessageBrokerProxy`, which
implements `HostCallbacks`. All plugins currently share this single forwarder.

### 2.5 The Tee Workaround

`scion-broker-log --forward localhost:9090` manually chains to scion-chat-app.
broker-log acts as primary broker, forwards all Publish/Subscribe/Configure calls
to the downstream. This means:
- broker-log must be started first
- Chat-app failure takes down logging too (shared failure domain)
- Adding a third integration requires modifying broker-log's chain
- Host callbacks from the downstream are forwarded through broker-log

## 3. Proposed Design

### 3.1 New Type: `FanOutBroker`

Introduce a `FanOutBroker` in `pkg/broker/` that implements `broker.MessageBroker`
and delegates to N child brokers:

```go
// pkg/broker/fanout.go
type FanOutBroker struct {
    brokers []NamedBroker
    log     *slog.Logger
}

type NamedBroker struct {
    Name   string
    Broker MessageBroker
}

func NewFanOutBroker(brokers []NamedBroker, log *slog.Logger) *FanOutBroker

func (f *FanOutBroker) Publish(ctx context.Context, topic string, msg *StructuredMessage) error
func (f *FanOutBroker) Subscribe(pattern string, handler MessageHandler) (Subscription, error)
func (f *FanOutBroker) Close() error
```

**Publish semantics**: Fan-out to all children. Each child's Publish is called
concurrently. Errors are collected but non-fatal — a single plugin failure
doesn't block delivery to others. The method returns an aggregate error if any
child failed, logged with the plugin name for diagnosability.

**Subscribe semantics**: The InProcessBroker (always present as the "local" broker)
handles all local subscription matching. Plugin brokers receive Subscribe calls
as hints to start their external listeners, but local dispatch is handled by
the in-process broker. The FanOutBroker delegates Subscribe to all children.

**Close semantics**: Close all children, collect errors.

### 3.2 Architecture: InProcess Core + Plugin Spokes

The FanOutBroker always includes one InProcessBroker as the "core" for local
pub/sub routing. Plugin brokers are spokes that receive outbound messages and
deliver inbound messages via the existing `/api/v1/broker/inbound` endpoint.

```
                    ┌──────────────────────┐
                    │   MessageBrokerProxy │
                    │  (hub agent lifecycle)│
                    └──────────┬───────────┘
                               │
                    ┌──────────▼───────────┐
                    │    FanOutBroker       │
                    │                      │
                    ├──────────────────────┤
                    │  InProcessBroker     │  ← local pub/sub (always present)
                    │  scion-broker-log    │  ← spoke: logging/debug
                    │  scion-chat-app      │  ← spoke: Google Chat bridge
                    │  scion-a2a-bridge    │  ← spoke: A2A protocol
                    └──────────────────────┘
```

### 3.3 Configuration Schema Changes

**Current** (`V1MessageBrokerConfig`):
```yaml
server:
  message_broker:
    enabled: true
    type: "broker-log"       # single broker
```

**Proposed** — two options, both backward-compatible:

**Option A: `types` list (minimal change)**
```yaml
server:
  message_broker:
    enabled: true
    type: "broker-log"       # still works (single), backward compat
    types:                   # NEW: list of plugin names for fan-out
      - "broker-log"
      - "chat-app"
      - "a2a-bridge"
```

**Option B: structured brokers map (richer)**
```yaml
server:
  message_broker:
    enabled: true
    brokers:
      broker-log:
        enabled: true
        role: observer         # optional metadata
      chat-app:
        enabled: true
        role: bridge
      a2a-bridge:
        enabled: false         # can disable individual brokers
        role: bridge
```

**Recommendation**: Option A for initial implementation. It's backward-compatible
(`type` still works for single-broker), and the `plugins.broker` map already
carries per-plugin config. Option B adds value only if we need per-broker
enable/disable or role metadata at the hub level, which can be added later.

The `V1MessageBrokerConfig` struct changes:

```go
type V1MessageBrokerConfig struct {
    Enabled bool     `yaml:"enabled"`
    Type    string   `yaml:"type,omitempty"`    // single broker (backward compat)
    Types   []string `yaml:"types,omitempty"`   // multiple brokers (fan-out)
}
```

Resolution logic: if `Types` is non-empty, use fan-out. Otherwise fall back to
single `Type`.

### 3.4 Hub Startup Changes (`cmd/server_foreground.go`)

The current startup code at line 296-328 creates one broker. Replace with:

```go
// Collect all active brokers
var namedBrokers []broker.NamedBroker

// Always include inprocess broker for local pub/sub
inproc := broker.NewInProcessBroker(logging.Subsystem("hub.broker.inprocess"))
namedBrokers = append(namedBrokers, broker.NamedBroker{Name: "inprocess", Broker: inproc})

// Add plugin brokers
brokerTypes := vs.Server.MessageBroker.Types
if len(brokerTypes) == 0 && vs.Server.MessageBroker.Type != "" && vs.Server.MessageBroker.Type != "inprocess" {
    brokerTypes = []string{vs.Server.MessageBroker.Type}
}

for _, bt := range brokerTypes {
    if pluginMgr.HasPlugin(scionplugin.PluginTypeBroker, bt) {
        b, err := pluginMgr.GetBroker(bt)
        if err != nil {
            log.Printf("Warning: failed to load broker plugin %q: %v", bt, err)
            continue
        }
        namedBrokers = append(namedBrokers, broker.NamedBroker{Name: bt, Broker: b})
    }
}

fanout := broker.NewFanOutBroker(namedBrokers, logging.Subsystem("hub.broker.fanout"))
hubSrv.StartMessageBroker(fanout)
```

### 3.5 Host Callbacks: Per-Plugin Routing

Currently all plugins share one `HostCallbacksForwarder`. With N plugins, each
plugin still shares the same hub-side subscription state (managed by
`MessageBrokerProxy`). The `HostCallbacksForwarder` already works because
`RequestSubscription`/`CancelSubscription` are idempotent — multiple plugins
requesting the same pattern is a no-op after the first.

**No change needed** to `HostCallbacksForwarder` or `MessageBrokerProxy` for
host callbacks. All plugins share the same subscription namespace on the hub
side, which is correct — subscriptions control what the hub listens for, not
what each plugin receives.

### 3.6 FanOutBroker.Publish Implementation

```go
func (f *FanOutBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
    var wg sync.WaitGroup
    errs := make([]error, len(f.brokers))

    for i, nb := range f.brokers {
        wg.Add(1)
        go func(idx int, b NamedBroker) {
            defer wg.Done()
            if err := b.Broker.Publish(ctx, topic, msg); err != nil {
                f.log.Error("fan-out publish failed",
                    "broker", b.Name, "topic", topic, "error", err)
                errs[idx] = err
            }
        }(i, nb)
    }

    wg.Wait()
    return errors.Join(errs...)
}
```

**Error policy**: Capability-based. Brokers that report an `"observer"`
capability in `PluginInfo.Capabilities` are fire-and-forget — errors are logged
but not surfaced to the caller. All other brokers ("bridge" role) have their
errors aggregated and returned. This ensures a logging plugin crash doesn't
block chat-app or A2A delivery, while still surfacing failures from critical
integrations.

The FanOutBroker queries each child's capabilities during construction (via
`GetInfo()` on the plugin adapter) and caches the classification. The
InProcessBroker is always treated as critical (non-observer).

```go
type NamedBroker struct {
    Name     string
    Broker   MessageBroker
    Observer bool // true = fire-and-forget, errors logged but not returned
}
```

### 3.7 Inbound Message Path

No changes needed. Each plugin already uses `POST /api/v1/broker/inbound` with
its own broker HMAC credentials. The hub dispatches directly to agents,
bypassing the broker (to avoid re-publishing). Multiple plugins can
independently POST inbound messages.

### 3.8 Health Checks

The `Manager` already supports per-plugin health checks via `GetInfo()` and
`HealthCheck()`. The FanOutBroker could expose aggregate health:

```go
func (f *FanOutBroker) HealthCheck() map[string]*HealthStatus {
    // iterate brokers, collect per-broker health
}
```

This is optional for the initial implementation. The existing plugin-level
health endpoint (`/api/v1/plugins/{name}/health` or similar) would continue
to work per-plugin.

## 4. Impact on Existing Plugins

### 4.1 scion-broker-log

- **`--forward` flag**: Can be deprecated. Users configure both broker-log and
  chat-app as separate plugins in settings.yaml.
- **No code changes required** to the plugin itself. It implements
  `MessageBrokerPluginInterface` and will work as a spoke in the fan-out.
- The `HostCallbacks` forwarding logic in broker-log (lines 280-298) becomes
  unnecessary since each plugin gets its own direct host callbacks channel.

### 4.2 scion-chat-app

- **No code changes required**. Already implements the full plugin interface.
- Currently must be chained behind broker-log; with fan-out, it runs independently.

### 4.3 scion-a2a-bridge

The A2A bridge is fully implemented and merged into `main`
(`extras/scion-a2a-bridge/`). Key findings from code review:

- **Broker plugin**: `internal/bridge/broker.go` implements the full
  `MessageBrokerPluginInterface` + `HostCallbacksAware`. The pattern is nearly
  identical to scion-chat-app's `BrokerServer` — same self-managed plugin,
  same `SetHostCallbacks` retry loop, same `RequestSubscription` flow. **No
  code changes required** for fan-out compatibility.

- **Message handling**: The bridge receives outbound messages via `Publish()`
  RPC, enqueues them into a buffered `brokerMsgs` channel (cap 256), and a
  `brokerWorker` goroutine dispatches them asynchronously. This design already
  tolerates slow dispatch without blocking the hub's Publish call — important
  for fan-out where one slow broker shouldn't block others.

- **Inbound messages**: The A2A bridge sends messages to agents via the Hub
  API (`hubClient.Agents().SendStructuredMessage`), not via
  `POST /api/v1/broker/inbound`. This is a higher-level path that goes through
  the Hub's authenticated API. Either inbound path works fine with fan-out.

- **Subscriptions**: The bridge subscribes to user-scoped topics
  (`scion.grove.<groveID>.user.<user>.messages`) to receive agent responses.
  These subscriptions use the standard `HostCallbacks.RequestSubscription()`
  mechanism.

- **Streaming/Push**: SSE streaming (`StreamManager`) and webhook push
  (`PushDispatcher`) are internal to the bridge and don't affect the broker
  plugin protocol. No bidirectional streaming requirements at the plugin RPC
  level.

- **Capabilities**: Reports `["a2a-bridge"]` in `PluginInfo.Capabilities`,
  which can be used for error policy classification (bridge role = surface
  errors).

### 4.4 InProcessBroker

- Continues to serve as the core local pub/sub engine.
- Always included as the first child in FanOutBroker.
- No code changes needed.

## 5. Migration Path

### 5.1 Backward Compatibility

The `type` field (singular) continues to work exactly as before. A single-broker
configuration is equivalent to a fan-out with one spoke. No existing deployments
break.

### 5.2 Migration Steps

1. **No-migration path**: Existing `type: "broker-log"` configs work unchanged.
2. **Opt-in to fan-out**: Add `types: [...]` to settings.yaml.
3. **Deprecate `--forward`**: After fan-out is available, mark the flag as
   deprecated in broker-log. Remove in a future release.

### 5.3 Example Migration

**Before** (chain-based):
```yaml
server:
  message_broker:
    enabled: true
    type: "broker-log"
  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
        config:
          forward: "localhost:9090"  # chain to chat-app
      # chat-app not listed here — broker-log forwards manually
```
Start: `scion-broker-log --forward localhost:9090 & scion-chat-app &`

**After** (native fan-out):
```yaml
server:
  message_broker:
    enabled: true
    types:
      - broker-log
      - chat-app
      - a2a-bridge
  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
      chat-app:
        self_managed: true
        address: "localhost:9090"
      a2a-bridge:
        self_managed: true
        address: "localhost:9092"
```
Start: `scion-broker-log & scion-chat-app & scion-a2a-bridge &` (no `--forward` needed)

## 6. Files to Change

| File | Change |
|------|--------|
| `pkg/broker/fanout.go` | **NEW**: `FanOutBroker` implementation |
| `pkg/broker/fanout_test.go` | **NEW**: Unit tests |
| `pkg/config/settings_v1.go` | Add `Types []string` to `V1MessageBrokerConfig` |
| `cmd/server_foreground.go` | Replace single-broker init with fan-out init (lines 296-328) |
| `pkg/hub/server.go` | No change — `StartMessageBroker(b broker.MessageBroker)` already takes the interface |
| `pkg/hub/messagebroker.go` | No change — works with any `broker.MessageBroker` |
| `pkg/plugin/manager.go` | No change — already supports N broker plugins |
| `extras/scion-broker-log/main.go` | Optional: deprecation warning on `--forward` flag |

## 7. Resolved Questions

1. **Error policy granularity**: **Yes** — implement per-broker error policies.
   Brokers with the `"observer"` capability (e.g., broker-log) are
   fire-and-forget. Bridge brokers (chat-app, a2a-bridge) surface errors.
   Driven by `PluginInfo.Capabilities`. *(Decision: owner feedback 2026-05-12)*

2. **A2A bridge requirements**: **Resolved** — the A2A bridge is fully
   implemented and merged into `main`. It uses the standard
   `MessageBrokerPluginInterface` with no special protocol requirements. SSE
   streaming and webhook push are internal to the bridge, not at the plugin
   RPC level. No changes needed. *(Verified via code review 2026-05-12)*

3. **InProcess broker always included?**: **Yes** — always include it. It's
   required for the web UI's local pub/sub routing. *(Decision: owner feedback
   2026-05-12)*

4. **Dynamic add/remove**: **Restart-to-reconfigure** is acceptable for v1.
   No dynamic add/remove needed now. *(Decision: owner feedback 2026-05-12)*

## 8. Remaining Open Questions

1. **Ordering / priority**: Should brokers have a defined publish order? The
   in-process broker should probably publish first to ensure local delivery
   (web UI, SSE events) happens before external RPC calls. The current design
   uses concurrent goroutines. One option: publish to InProcessBroker
   synchronously first, then fan-out to plugin brokers concurrently.

2. **Subscription deduplication across plugins**: If broker-log requests
   `scion.>` and chat-app requests `scion.grove.*.user.*.messages`, the hub
   receives the union. Publishing fans out to all plugins regardless of their
   individual subscription requests. This appears correct — plugins already
   receive all messages via `Publish()`, and their subscriptions are hints for
   their external listeners. Confirm this is the intended behavior.

3. **Per-plugin filtering**: Should the FanOutBroker support topic-based routing
   so that only relevant messages go to each plugin? This would be an
   optimization — currently all plugins get all messages, which is simple and
   correct for the known use cases (broker-log wants everything, chat-app and
   a2a-bridge filter internally). Defer to Phase 3?

## 9. Alternatives Considered

### 8.1 Plugin-level chaining (current approach)
Keep `--forward` and have plugins chain manually. Rejected because it creates
coupled failure domains, doesn't scale, and puts broker topology knowledge in
plugin configs instead of the hub.

### 8.2 Multiple MessageBrokerProxy instances
Create one `MessageBrokerProxy` per plugin. Rejected because the proxy manages
agent lifecycle subscriptions and dispatch — having N proxies would result in
N duplicate dispatches to agents. The proxy should remain singular.

### 8.3 Event bus approach
Replace the broker plugin interface with an event bus where plugins subscribe
to specific event types. Rejected as too large a redesign for the immediate
need, though it could be a future evolution.

## 10. Implementation Plan

**Phase 1 (MVP)**:
1. Implement `FanOutBroker` in `pkg/broker/fanout.go` with capability-based
   error policy (observer = fire-and-forget)
2. Add `Types` field to `V1MessageBrokerConfig`
3. Update `cmd/server_foreground.go` startup to build fan-out from `Types` list
4. Tests: unit tests for FanOutBroker, integration test with two mock plugins
5. Verify all three plugins (broker-log, chat-app, a2a-bridge) work as
   independent spokes with no code changes

**Phase 2 (Polish)**:
1. Health aggregation endpoint
2. Deprecate `--forward` flag in broker-log
3. Update sample configs and documentation

**Phase 3 (Future)**:
1. Per-plugin topic filtering
2. Dynamic plugin add/remove (hot-reconfigure)
3. Admin UI for broker management
