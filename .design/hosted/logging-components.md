# Structured Logging: Component Granularity

## Status
**Implemented (Tier 1 & 2)** | March 2026

## Problem

All server-side logs currently share a single `component` field value determined at startup: `scion-server` (combo mode), `scion-hub` (hub-only), or `scion-broker` (broker-only). This makes it impossible to filter, aggregate, or alert on logs from specific subsystems.

In practice, this means:
- A noisy heartbeat cycle drowns out important notification dispatch failures in the same log stream.
- Debugging an auth issue requires manually scanning through unrelated agent lifecycle and template hydration logs.
- In combo server mode, broker-side logs are tagged `scion-server` rather than identifying their broker origin, losing the distinction entirely.
- Cloud Logging queries and alerts cannot target specific subsystems without fragile text matching on log messages.

## Current Architecture

Logging is initialized in `cmd/server.go:186-200`:

```go
component := "scion-server"
if enableHub && !enableRuntimeBroker {
    component = "scion-hub"
} else if !enableHub && enableRuntimeBroker {
    component = "scion-broker"
}
```

This value is baked into the root handler via `logging.Setup()` in `pkg/util/logging/logging.go`, which attaches it as a static attribute on every log record. All subsystems inherit this single value through `slog.Default()`.

### Standard Attributes (from `pkg/util/logging/logging.go`)

```go
AttrComponent = "component"
AttrTraceID   = "trace_id"
AttrGroveID   = "grove_id"
AttrAgentID   = "agent_id"
AttrRequestID = "request_id"
AttrUserID    = "user_id"
```

The `component` field is the only one set globally. Others are added ad-hoc at individual call sites.

---

## Proposed Components

### Naming Convention

Use a **dotted hierarchy** rooted at the server role: `hub.<subsystem>` or `broker.<subsystem>`. This provides:
- Clear attribution in combo server mode (both hub and broker components appear distinctly).
- Natural Cloud Logging filter patterns: `labels.component =~ "hub\\..*"` for all hub logs.
- Room for future depth (e.g., `hub.auth.oauth`) without a naming redesign.

The top-level `component` field on the root logger remains as-is (`scion-server`, `scion-hub`, `scion-broker`) for backward compatibility. Subsystem loggers add a **`subsystem`** attribute alongside it.

### New Attribute

```go
const AttrSubsystem = "subsystem"
```

### Tier 1: High Priority

These subsystems are operationally critical, high-volume, or difficult to debug without isolation.

| Subsystem | Code Location | Rationale |
|---|---|---|
| `hub.notifications` | `pkg/hub/notifications.go`, `handlers_notifications.go` | Event-driven dispatch with subscription matching, dedup, and cross-agent messaging. Failures here are silent without dedicated filtering. |
| `hub.messages` | `pkg/hub/handlers.go` (message dispatch), `httpdispatcher.go` | Routes `scion message` through control channel to brokers. Should capture `sender` and `recipient` as structured fields for traceability. |
| `broker.messages` | `pkg/runtimebroker/handlers.go` (sendMessage) | Broker-side message injection into agent tmux sessions. Same `sender`/`recipient` fields. |
| `hub.control-channel` | `pkg/hub/controlchannel.go` | WebSocket lifecycle for broker connections — connect, disconnect, heartbeat, stream proxy. Already has extensive logging but no component tag. Critical for diagnosing broker connectivity. |
| `broker.control-channel` | `pkg/runtimebroker/controlchannel.go` | Broker-side of the WebSocket connection to the hub. Same rationale. |
| `broker.agent-lifecycle` | `pkg/runtimebroker/handlers.go` (create/start/stop/delete) | Container provisioning, environment resolution, template hydration. The most common debugging target on the broker. |
| `hub.agent-lifecycle` | `pkg/hub/handlers.go` (create/start/stop/delete/restore) | Hub-side orchestration: dispatching to brokers, state transitions, soft-delete. |
| `hub.auth` | `pkg/hub/auth.go`, `authz.go`, `handlers_auth.go`, `brokerauth.go`, `devauth.go` | Authentication and authorization decisions. Security-relevant — useful for audit trails and access debugging. |

### Tier 2: Medium Priority

Useful for debugging specific operational areas; lower log volume or less frequent troubleshooting.

| Subsystem | Code Location | Rationale |
|---|---|---|
| `hub.scheduler` | `pkg/hub/scheduler.go` | Background recurring/one-shot event handlers. Includes panic recovery logging that should be easily isolatable. |
| `broker.heartbeat` | `pkg/runtimebroker/heartbeat.go` | Periodic status reports to hub. Very noisy at DEBUG level — filtering by subsystem prevents it from overwhelming other broker logs. |
| `hub.env-secrets` | `pkg/hub/handlers.go` (env/secret handlers) | Environment variable and secret management. Sensitive operations worth isolating. |
| `broker.env-secrets` | `pkg/runtimebroker/handlers.go` (finalizeEnv, secret resolution) | Broker-side environment gathering and finalization — multi-step process with detailed debug logging. |
| `hub.templates` | `pkg/hub/template_handlers.go`, `template_bootstrap.go`, `harness_config_handlers.go` | Template CRUD, hydration, and hub-managed grove bootstrap. |
| `hub.workspace` | `pkg/hub/workspace_handlers.go`, `grove_workspace_handlers.go` | Git worktree sync operations. Failures here can be subtle and hard to trace. |

### Tier 3: Lower Priority

Standard CRUD operations with straightforward request/response patterns.

| Subsystem | Code Location | Rationale |
|---|---|---|
| `hub.groves` | `pkg/hub/handlers.go` (grove CRUD) | Grove management — less operationally complex but high volume in multi-grove setups. |
| `hub.brokers` | `pkg/hub/handlers_brokers.go` | Broker registration and management. |
| `hub.groups` | `pkg/hub/handlers_groups.go` | User group management. |
| `hub.policies` | `pkg/hub/handlers_policies.go` | RBAC policy evaluation. |
| `hub.audit` | `pkg/hub/audit.go` | Audit log recording (meta — the audit system's own operational logs). |
| `hub.events` | `pkg/hub/events.go` | Event publisher internals (fan-out, subscription management). |
| `hub.web` | `pkg/hub/web.go` | Static file serving and SPA routing. |

---

## Implementation Approach

Decision: Option A

### Option A: Subsystem Logger Injection (Recommended)

Create child loggers with the subsystem attribute and pass them to subsystem constructors or store them on structs.

```go
// At subsystem initialization (e.g., in hub server setup)
notifLogger := slog.Default().With(slog.String(logging.AttrSubsystem, "hub.notifications"))
dispatcher := NewNotificationDispatcher(store, events, agentDispatcher, notifLogger)

// In the subsystem
type NotificationDispatcher struct {
    log   *slog.Logger
    // ...
}

func (n *NotificationDispatcher) handleEvent(evt Event) {
    n.log.Info("Processing status event",
        slog.String(logging.AttrAgentID, statusEvt.AgentID),
        slog.String("activity", statusEvt.Activity),
    )
}
```

For message dispatch specifically, add sender/recipient fields:

```go
msgLogger.Info("Message dispatched",
    slog.String(logging.AttrSubsystem, "hub.messages"),
    slog.String("sender", senderSlug),
    slog.String("recipient", recipientSlug),
    slog.String(logging.AttrGroveID, groveID),
)
```

**Pros:**
- Type-safe, IDE-friendly — subsystem loggers are regular `*slog.Logger` values.
- Zero overhead at call sites (attribute is pre-baked into the logger).
- Testable — inject a test logger and assert on attributes.
- No global state changes.

**Cons:**
- Requires threading a logger through constructors or struct fields.
- Incremental migration — existing `slog.Info(...)` calls need updating per subsystem.

### Option B: Logging Utility Function

Add a helper to `pkg/util/logging` that returns a subsystem logger:

```go
func Subsystem(name string) *slog.Logger {
    return slog.Default().With(slog.String(AttrSubsystem, name))
}
```

Subsystems initialize a package-level or struct-level logger:

```go
var log = logging.Subsystem("hub.notifications")
```

**Pros:**
- Minimal boilerplate — one line per subsystem.
- No constructor signature changes.

**Cons:**
- Package-level vars couple to `slog.Default()` at init time, which may not yet be configured.
- Less explicit about dependencies.

### Option C: Hybrid

Use Option B for quick adoption in handlers (where loggers aren't struct fields), and Option A for long-lived subsystems like dispatchers and background goroutines that already have struct fields.


---

## Message Dispatcher: Additional Fields

The `hub.messages` and `broker.messages` subsystems should capture structured fields beyond the subsystem tag:

| Field | Type | Description |
|---|---|---|
| `sender` | `string` | Slug or ID of the message sender (agent or user) |
| `recipient` | `string` | Slug or ID of the target agent |
| `grove_id` | `string` | Grove context (already a standard attribute) |
| `interrupt` | `bool` | Whether the message was sent with interrupt mode |
| `source` | `string` | Origin: `cli`, `notification`, `api` |

---

## Combo Server Considerations

In combo server mode, both `hub.*` and `broker.*` subsystem logs appear in the same stream. This is intentional and desirable — the dotted prefix makes them distinguishable without needing separate processes. The root `component` field remains `scion-server` for combo mode, while `subsystem` provides the granularity.

Example log line in combo mode:
```json
{
  "level": "INFO",
  "msg": "Broker control channel connected",
  "component": "scion-server",
  "subsystem": "hub.control-channel",
  "brokerID": "broker-1",
  "sessionID": "abc123"
}
```

Cloud Logging filter examples:
```
-- All hub subsystem logs
jsonPayload.subsystem =~ "^hub\\."

-- All message-related logs with sender context
jsonPayload.subsystem =~ "\\.messages$" AND jsonPayload.sender != ""

-- Broker agent lifecycle only
jsonPayload.subsystem = "broker.agent-lifecycle"
```

---

## Migration Strategy

1. **Add `AttrSubsystem` constant** to `pkg/util/logging/logging.go`.
2. **Add `Subsystem()` helper** for convenience.
3. **Tier 1 subsystems first** — notifications, messages, control-channel, agent-lifecycle, auth. These provide the most immediate value.
4. **Incremental rollout** — each subsystem can be migrated independently by creating a child logger and updating call sites within that subsystem's files.
5. **No breaking changes** — the root `component` attribute is preserved. `subsystem` is additive.

---

## HTTP Request Log Stream

In addition to subsystem-level application logs, the server produces a dedicated **HTTP request log stream** using the `google.logging.type.HttpRequest` format. This stream is separate from application logs and captures per-request metadata including method, path, status, latency, response size, and contextual IDs (grove, agent, request, trace).

### Routing

- **File**: `SCION_SERVER_REQUEST_LOG_PATH` env var directs request logs to a file (JSON lines).
- **Cloud Logging**: When `SCION_CLOUD_LOGGING=true`, request logs are sent to a separate log name (`scion_request_log`) using the same GCP client as application logs.
- **Stdout**: In background/piped mode without file or cloud targets. Suppressed in `--foreground` mode to reduce noise.

### Trace Context Propagation

The request logging middleware generates a `request_id` (UUID) for every request and captures trace headers (`X-Cloud-Trace-Context`, `traceparent`, `X-Trace-ID`). These are stored in a `RequestMeta` context value that `logging.Logger(ctx)` automatically reads. This means any application log emitted during a request carries the same `request_id` and `trace_id`, enabling correlation between the request log entry and downstream application logs.

### Implementation

- `pkg/util/logging/request_log.go` — Core types (`HttpRequest`, `RequestMeta`, `InstrumentedResponseWriter`), context functions, logger factory (`NewRequestLogger`), and middleware factory (`RequestLogMiddleware`).
- `pkg/util/logging/cloud_handler.go` — `NewCloudHandlerFromClient()` for reusing the GCP client with a different log ID.
- `pkg/util/logging/logging.go` — `Logger(ctx)` enriched with `RequestMeta` from context.
- `cmd/server.go` — Initializes the request logger and wires it to Hub, Broker, and Web servers.

## Related Documents

- [Metrics Improvements](metrics-improvements.md) — OTel metrics pipeline and observability direction.
- [Notifications](notifications.md) — Notification dispatcher architecture (primary consumer of `hub.notifications` logging).
- [Hub Messaging](hub-messaging.md) — Message routing through Hub (primary consumer of `hub.messages` / `broker.messages` logging).
- [Runtime Broker WebSocket](runtimebroker-websocket.md) — Control channel design (primary consumer of `*.control-channel` logging).
