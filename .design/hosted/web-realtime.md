# Web Server Consolidation & Real-Time Events

**Created:** 2026-02-19

## Overview

The Scion web frontend currently runs as a separate Node.js/Koa BFF (Backend-for-Frontend) that proxies API requests to the Hub, manages browser sessions, serves static assets, performs Lit SSR, and bridges real-time events from NATS to the browser via SSE.

This design consolidates the BFF's functionality into the Go `scion` binary, adding a new web server capability alongside the existing Hub API and Runtime Broker. The result is a single binary that serves the API, executes agents, and hosts the web UI — eliminating Node.js and NATS as runtime dependencies.

Real-time event delivery uses an in-process `EventPublisher` backed by Go channels (`ChannelEventPublisher`) for single-node deployments. The design accommodates a future move to PostgreSQL `LISTEN/NOTIFY` for multi-node scaling without introducing new dependencies.

> **Historical note:** An earlier design (`hub-nats.md`, now superseded) explored using NATS as the event transport. That approach was abandoned in favor of the simpler in-process channel design described here. NATS added a runtime dependency and operational complexity that wasn't justified for a system where the Hub is the single source of truth and all state changes flow through one process. See [Appendix A: Why Not NATS](#appendix-a-why-not-nats).

---

## Design Principles

1. **Single binary, multiple capabilities.** The `scion` binary serves the API, runs agents, and hosts the web UI — toggled by flags (`--enable-hub`, `--enable-runtime-broker`, `--enable-web`). No external runtime dependencies beyond the database.
2. **Fire-and-forget events.** Event publish failures are logged but never fail the HTTP request. The database write is the commit point; real-time notification is best-effort.
3. **Publish after commit.** Events are published only after the store operation succeeds, avoiding notifications about rolled-back changes.
4. **Dual-publish for status.** Agent status changes are published to both the agent-scoped subject (`agent.{id}.status`) and the grove-scoped subject (`grove.{groveId}.agent.status`), enabling grove-level subscribers to receive lightweight updates without per-agent subscriptions.
5. **Subject hierarchy is the filter.** The subject hierarchy controls which subscribers receive which events. Heavy payloads (harness output) are published only to agent-scoped subjects; lightweight/medium events are published to grove-scoped subjects.
6. **Preserve the SSE contract.** The browser's SSE client and subject-based subscription model are unchanged. The transport behind the SSE endpoint changes from NATS to in-process channels, but the wire format and subscription semantics remain identical.
7. **Incremental migration.** The Koa server continues to function during the port. Each migration stage is independently deployable and testable.

---

## Architecture

### Target State

```
scion server start --enable-hub --enable-runtime-broker --enable-web
```

One binary. One process. Three capabilities toggled by flags.

```
Browser (cookie auth)
  |
Go Server (:8080 web, :9810 API)
  |-- /assets/*              -> static file serving (embedded or from disk)
  |-- /auth/*                -> OAuth browser flow (sessions, cookies)
  |-- /events?sub=...        -> SSE (in-process ChannelEventPublisher)
  |-- /api/v1/*              -> Hub API (direct handler call, no proxy)
  |-- /api/agents/*/pty      -> WebSocket PTY (direct, no proxy)
  |-- /healthz, /readyz      -> health checks
  |-- /*                     -> SPA shell (Go template, static HTML)
```

### What Changes

| Concern | Koa BFF (current) | Go Web Server (target) |
|---------|-------------------|------------------------|
| API proxy | HTTP proxy to Hub on :9810 | **Eliminated.** Go server IS the API. |
| Session management | `koa-session` with signed cookies | `gorilla/sessions` + `securecookie` |
| OAuth browser flow | Koa routes (~480 lines) | Go handlers (~300 lines) |
| SSE real-time bridge | NATS subscription -> SSE stream | **In-process channels** -> SSE stream |
| WebSocket PTY proxy | Koa WS proxy to Hub | **Eliminated.** Hub already has PTY handlers. |
| SSR | `@lit-labs/ssr` (Node.js) | **Dropped.** SPA shell only. |
| Static assets | `koa-static` from Vite build | `http.FileServer` or `//go:embed` |
| Security headers | Koa middleware (~70 lines) | Go middleware (~30 lines) |
| Health checks | Koa routes | Already exists in Hub |
| Dev auth | Koa middleware | Already exists in Hub |
| Request logging | Koa middleware | Already exists in Hub (`slog`) |

**Eliminated by consolidation:** ~650 lines (API proxy, SSE/NATS bridge, PTY proxy).
**Already exists in Hub:** ~560 lines (health, dev auth, logging).
**Needs porting:** ~700 lines (session management, OAuth browser flow, security headers).
**Dropped:** ~750 lines (SSR renderer + templates).

### What Does NOT Change

- **Client-side code** — Lit components, xterm.js, Vite build, `StateManager`, `SSEClient` — all remain TypeScript. The browser code is unchanged.
- **SSE wire format** — `id`, `event`, `data` fields, subject-based subscriptions via query parameters, heartbeats.
- **Subject hierarchy** — Grove-scoped, agent-scoped, and broker-scoped subjects remain the same.
- **Build tooling** — Node.js is still required at build time to compile client assets. Only the runtime dependency is removed.
- **Multi-node support path** — The `EventPublisher` interface provides the abstraction for swapping to `PostgresEventPublisher` later.

---

## SSR Decision: SPA Shell (No SSR)

The current Koa BFF uses `@lit-labs/ssr` to render Lit components server-side. Porting this to Go would require either a Node.js sidecar or duplicating layout logic in Go templates.

**Decision: Drop SSR. Use a SPA shell.**

The Go server returns a minimal HTML page with `<script>` tags. Lit components render entirely client-side. The `__SCION_DATA__` hydration pattern still works — embed initial JSON in the HTML template, the client reads it on load.

```go
const spaShell = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Scion</title>
  <link rel="stylesheet" href="/assets/main.css">
  {{.InlineCriticalCSS}}
</head>
<body>
  <scion-app></scion-app>
  <script id="__SCION_DATA__" type="application/json">{{.InitialData}}</script>
  <script type="module" src="/assets/main.js"></script>
</body>
</html>`
```

**Why this is acceptable:**

- The app requires authentication — search engines can't see content. SSR provides no SEO benefit.
- The brief flash of unstyled content on first load is mitigated with inline critical CSS (already present in the Koa template — it moves to the Go template unchanged).
- SSR was the single largest porting cost (~750 lines, plus a Node.js runtime dependency). Dropping it is the biggest simplification win.

---

## Static Asset Serving

Client-side code (Lit components, xterm.js, CSS) is built with Vite and needs to be served by the Go binary. Two serving modes are supported:

### Option 1: Filesystem-Based (Development & Flexible Deployment)

```
scion server start --enable-web --web-assets-dir ./web/dist/client
```

The Go server uses `http.FileServer` to serve assets from a directory on disk. This is the default for development workflows where a Vite dev server rebuilds assets and the Go server picks up changes.

### Option 2: Embedded via `//go:embed` (Self-Contained Binary)

```go
//go:embed web/dist/client
var clientAssets embed.FS
```

Assets are compiled into the Go binary at build time. The resulting binary is fully self-contained — a single file that includes the API server, runtime broker, and web UI.

### Build-Time vs Runtime Selection

The selection between embedded and filesystem serving is a **runtime flag**, not a build-time flag:

```go
type ServerConfig struct {
    // ...
    WebEnabled   bool
    WebAssetsDir string // If non-empty, serve from disk. If empty, use embedded.
}
```

**Rationale for runtime flag over build tag:**

- A build tag (`-tags embed_assets`) means maintaining two build configurations and two binaries. This adds CI complexity for a marginal benefit.
- The `//go:embed` directive is always compiled in. The `--web-assets-dir` flag overrides it at runtime when present. This gives developers the flexibility to point at a local Vite output directory without rebuilding the Go binary.
- The embedded fallback means the default binary (`go build`) includes assets and works out of the box. CI builds a single artifact.

```go
func (s *Server) staticHandler() http.Handler {
    if s.cfg.WebAssetsDir != "" {
        // Serve from disk — development or custom deployment
        return http.FileServer(http.Dir(s.cfg.WebAssetsDir))
    }
    // Serve from embedded assets — production default
    sub, _ := fs.Sub(clientAssets, "web/dist/client")
    return http.FileServer(http.FS(sub))
}
```

### Development Workflow with Vite Dev Server

During development, the JS toolchain runs separately:

```bash
# Terminal 1: Vite dev server (rebuilds on change, HMR)
cd web && npm run dev    # Serves on :5173 with HMR

# Terminal 2: Go server (serves API + web shell)
scion server start --enable-hub --enable-web --enable-runtime-broker \
  --web-assets-dir ./web/dist/client --dev-auth
```

The Vite dev server handles hot module replacement (HMR) for the client code. The Go server handles API requests and SSE events. In development, the SPA shell template can include a `<script>` tag pointing at the Vite dev server for HMR support, or the developer can use Vite's proxy configuration to forward API requests to the Go server.

**Alternative: Vite proxy mode.** The Vite dev server can proxy `/api/*` and `/events` to the Go server. The developer visits `:5173` (Vite) for full HMR, and API/SSE requests are forwarded to `:9810`/`:8080` (Go). This requires no `--web-assets-dir` flag — the Go server doesn't serve assets in this mode.

---

## EventPublisher System

### Interface

The `EventPublisher` interface abstracts the event delivery mechanism. Handlers call publish methods unconditionally after successful database writes. The implementation behind the interface determines how events reach SSE subscribers.

```go
// EventPublisher publishes state-change events to subscribers.
// All methods are no-ops when the receiver is nil, allowing handlers
// to call unconditionally without nil checks.
type EventPublisher interface {
    PublishAgentStatus(ctx context.Context, agent *store.Agent)
    PublishAgentCreated(ctx context.Context, agent *store.Agent)
    PublishAgentDeleted(ctx context.Context, agentID, groveID string)
    PublishGroveCreated(ctx context.Context, grove *store.Grove)
    PublishGroveUpdated(ctx context.Context, grove *store.Grove)
    PublishGroveDeleted(ctx context.Context, groveID string)
    PublishBrokerStatus(ctx context.Context, brokerID, status string)
    PublishBrokerConnected(ctx context.Context, brokerID, brokerName string, groveIDs []string)
    PublishBrokerDisconnected(ctx context.Context, brokerID string, groveIDs []string)
    Close()
}
```

Note: `PublishGroveCreated` is included (it was identified as missing in the earlier NATS design's open questions).

### `ChannelEventPublisher` (Single-Node, Default)

For single-node deployments, events are delivered via Go channels. The SSE endpoint subscribes to the same event bus in-process — no serialization overhead, no network, no external dependencies.

```go
type ChannelEventPublisher struct {
    mu          sync.RWMutex
    subscribers map[string][]chan Event  // subject pattern -> subscriber channels
}

type Event struct {
    Subject string
    Data    []byte  // JSON-encoded payload
}

// Subscribe returns a channel that receives events matching the given
// subject patterns. Supports NATS-style wildcards (* and >).
func (p *ChannelEventPublisher) Subscribe(patterns ...string) (<-chan Event, func()) {
    ch := make(chan Event, 64)
    p.mu.Lock()
    for _, pattern := range patterns {
        p.subscribers[pattern] = append(p.subscribers[pattern], ch)
    }
    p.mu.Unlock()

    unsubscribe := func() {
        p.mu.Lock()
        defer p.mu.Unlock()
        for _, pattern := range patterns {
            subs := p.subscribers[pattern]
            for i, s := range subs {
                if s == ch {
                    p.subscribers[pattern] = append(subs[:i], subs[i+1:]...)
                    break
                }
            }
        }
        close(ch)
    }
    return ch, unsubscribe
}

// publish fans out an event to all subscribers whose patterns match the subject.
func (p *ChannelEventPublisher) publish(subject string, event interface{}) {
    data, err := json.Marshal(event)
    if err != nil {
        slog.Error("Failed to marshal event", "subject", subject, "error", err)
        return
    }
    evt := Event{Subject: subject, Data: data}

    p.mu.RLock()
    defer p.mu.RUnlock()
    for pattern, subscribers := range p.subscribers {
        if subjectMatchesPattern(pattern, subject) {
            for _, ch := range subscribers {
                select {
                case ch <- evt:
                default:
                    slog.Warn("Dropping event, subscriber buffer full",
                        "subject", subject, "pattern", pattern)
                }
            }
        }
    }
}
```

The `subjectMatchesPattern` function implements NATS-style wildcard matching (`*` for single token, `>` for remainder). This is ~40 lines of code.

### Subject Hierarchy & Message Formats

All payloads are JSON. Timestamps use RFC 3339.

#### Grove-Scoped Subjects

| Subject | Trigger | Payload |
|---------|---------|---------|
| `grove.{groveId}.agent.status` | Agent status change | `AgentStatusEvent` |
| `grove.{groveId}.agent.created` | Agent created | `AgentCreatedEvent` |
| `grove.{groveId}.agent.deleted` | Agent deleted | `AgentDeletedEvent` |
| `grove.{groveId}.created` | Grove created | `GroveCreatedEvent` |
| `grove.{groveId}.updated` | Grove metadata change | `GroveUpdatedEvent` |
| `grove.{groveId}.broker.connected` | Broker joined grove | `BrokerGroveEvent` |
| `grove.{groveId}.broker.disconnected` | Broker left grove | `BrokerGroveEvent` |

#### Agent-Scoped Subjects

| Subject | Trigger | Payload |
|---------|---------|---------|
| `agent.{agentId}.status` | Agent status change | `AgentStatusEvent` |
| `agent.{agentId}.created` | Agent created | `AgentCreatedEvent` |
| `agent.{agentId}.deleted` | Agent deleted | `AgentDeletedEvent` |

#### Broker-Scoped Subjects

| Subject | Trigger | Payload |
|---------|---------|---------|
| `broker.{brokerId}.status` | Broker heartbeat / status change | `BrokerStatusEvent` |

#### Message Types

```go
type AgentStatusEvent struct {
    AgentID         string `json:"agentId"`
    Status          string `json:"status"`
    SessionStatus   string `json:"sessionStatus,omitempty"`
    ContainerStatus string `json:"containerStatus,omitempty"`
    Timestamp       string `json:"timestamp"`
}

type AgentCreatedEvent struct {
    AgentID  string `json:"agentId"`
    Name     string `json:"name"`
    Template string `json:"template,omitempty"`
    GroveID  string `json:"groveId"`
    Status   string `json:"status"`
}

type AgentDeletedEvent struct {
    AgentID string `json:"agentId"`
}

type GroveCreatedEvent struct {
    GroveID string `json:"groveId"`
    Name    string `json:"name"`
}

type GroveUpdatedEvent struct {
    GroveID string            `json:"groveId"`
    Name    string            `json:"name,omitempty"`
    Labels  map[string]string `json:"labels,omitempty"`
}

type GroveDeletedEvent struct {
    GroveID string `json:"groveId"`
}

type BrokerGroveEvent struct {
    BrokerID   string `json:"brokerId"`
    BrokerName string `json:"brokerName,omitempty"`
}

type BrokerStatusEvent struct {
    BrokerID string `json:"brokerId"`
    Status   string `json:"status"`
}
```

### Handler Integration Points

Each handler calls the publisher **after** the store operation succeeds. The call is a single line appended to the success path.

#### Agent Handlers

| Handler | Publish Call |
|---------|-------------|
| `createAgent()` | `s.events.PublishAgentCreated(ctx, agent)` |
| `createGroveAgent()` | `s.events.PublishAgentCreated(ctx, agent)` |
| `updateAgentStatus()` | `s.events.PublishAgentStatus(ctx, agent)` |
| `handleAgentLifecycle()` | `s.events.PublishAgentStatus(ctx, agent)` |
| `deleteAgent()` | `s.events.PublishAgentDeleted(ctx, agentID, groveID)` |
| `deleteGroveAgent()` | `s.events.PublishAgentDeleted(ctx, agentID, groveID)` |

#### Grove Handlers

| Handler | Publish Call |
|---------|-------------|
| `createGrove()` | `s.events.PublishGroveCreated(ctx, grove)` |
| `updateGrove()` | `s.events.PublishGroveUpdated(ctx, grove)` |
| `deleteGrove()` | `s.events.PublishGroveDeleted(ctx, groveID)` |

#### Broker Handlers

| Handler | Publish Call |
|---------|-------------|
| `controlChannel.SetOnDisconnect` | `s.events.PublishBrokerDisconnected(ctx, brokerID, groveIDs)` |
| `markBrokerOnline()` | `s.events.PublishBrokerConnected(ctx, brokerID, brokerName, groveIDs)` |
| `handleGroveRegister()` | `s.events.PublishBrokerConnected(ctx, brokerID, brokerName, []string{groveID})` |

### Server Integration

```go
type Server struct {
    // ... existing fields ...
    events EventPublisher // nil when --enable-web is not set
}

// Nil-safe: all methods return immediately on nil receiver
func (p *ChannelEventPublisher) PublishAgentStatus(ctx context.Context, agent *store.Agent) {
    if p == nil {
        return
    }
    event := AgentStatusEvent{
        AgentID: agent.ID, Status: agent.Status,
        SessionStatus: agent.SessionStatus, Timestamp: time.Now().UTC().Format(time.RFC3339),
    }
    p.publish(fmt.Sprintf("agent.%s.status", agent.ID), event)
    p.publish(fmt.Sprintf("grove.%s.agent.status", agent.GroveID), event)
}
```

---

## SSE Endpoint

The SSE endpoint lives in the Go web server and reads from the `ChannelEventPublisher` directly. No serialization to an external system, no network hop.

```go
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    subjects := r.URL.Query()["sub"]
    if len(subjects) == 0 {
        http.Error(w, "at least one sub parameter required", http.StatusBadRequest)
        return
    }
    // ... validate subjects, check auth ...

    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("X-Accel-Buffering", "no")
    flusher.Flush()

    ch, unsubscribe := s.events.(*ChannelEventPublisher).Subscribe(subjects...)
    defer unsubscribe()

    eventID := 0
    for {
        select {
        case event := <-ch:
            eventID++
            fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n",
                eventID, event.Subject, event.Data)
            flusher.Flush()
        case <-r.Context().Done():
            return
        case <-time.After(30 * time.Second):
            fmt.Fprintf(w, "event: heartbeat\ndata: %d\n\n", time.Now().UnixMilli())
            flusher.Flush()
        }
    }
}
```

The SSE wire format is identical to what the Koa server produces today. The browser's `EventSource` client requires no changes.

---

## Session Management

The Koa BFF uses `koa-session` to store Hub JWTs in signed httpOnly cookies. The Go equivalent uses `gorilla/sessions`:

```go
import "github.com/gorilla/sessions"

var sessionStore = sessions.NewCookieStore([]byte(sessionSecret))

func init() {
    sessionStore.Options = &sessions.Options{
        Path:     "/",
        MaxAge:   86400, // 24 hours
        HttpOnly: true,
        Secure:   true,  // HTTPS only in production
        SameSite: http.SameSiteLaxMode,
    }
}
```

**Key difference from the current architecture:** the OAuth callback calls the Hub's token exchange logic **directly as a function call**, not via an HTTP proxy. No network hop, no serialization overhead.

```go
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
    session, _ := sessionStore.Get(r, "scion_sess")
    // Exchange code for tokens — direct function call, no HTTP
    tokens, user, err := s.exchangeOAuthCode(r.Context(), code, provider)
    session.Values["user"] = user
    session.Values["accessToken"] = tokens.AccessToken
    session.Values["refreshToken"] = tokens.RefreshToken
    session.Save(r, w)
    http.Redirect(w, r, returnTo, http.StatusFound)
}
```

---

## Auth Middleware Layering

The consolidated server needs two auth paths on the same mux:

| Path prefix | Auth method | Consumer |
|-------------|------------|----------|
| `/api/v1/*` | Bearer JWT / API key / dev token | CLI, brokers, external API clients |
| `/auth/*`, `/*` (pages), `/events` | Session cookie | Browser |
| `/healthz`, `/readyz` | None | Load balancers |

The existing `UnifiedAuthMiddleware` handles API auth. A new `SessionAuthMiddleware` handles browser auth. Route registration determines which applies:

```go
// API routes — existing JWT/API key auth
apiMux := http.NewServeMux()
apiMux.Handle("/api/v1/", UnifiedAuthMiddleware(s.authConfig)(s.apiHandler()))

// Web routes — session cookie auth
webMux := http.NewServeMux()
webMux.Handle("/auth/", s.oauthRoutes())
webMux.Handle("/events", SessionAuthMiddleware(sessionStore)(s.sseHandler()))
webMux.Handle("/assets/", http.FileServer(http.FS(clientAssets)))
webMux.Handle("/", SessionAuthMiddleware(sessionStore)(s.spaHandler()))

// Combined
mainMux := http.NewServeMux()
mainMux.Handle("/api/", apiMux)
mainMux.Handle("/", webMux)
```

---

## Configuration

### Web-Specific Configuration

```go
type ServerConfig struct {
    // ... existing fields ...

    // Web frontend settings (when --enable-web is set)
    WebEnabled    bool
    WebAssetsDir  string   // Path to client assets (empty = use embedded)
    SessionSecret string   // HMAC secret for session cookies
    BaseURL       string   // Public URL for OAuth redirects
}
```

### CLI Flags

```
scion server start --enable-hub --enable-runtime-broker --enable-web \
  --session-secret "$(openssl rand -hex 32)" \
  --base-url https://scion.example.com \
  --web-assets-dir ./web/dist/client  # optional, defaults to embedded
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_SERVER_WEB_ENABLED` | Enable web server | `false` |
| `SCION_SERVER_SESSION_SECRET` | HMAC secret for session cookies | (required in production) |
| `SCION_SERVER_BASE_URL` | Public URL for OAuth redirects | `http://localhost:8080` |
| `SCION_SERVER_WEB_ASSETS_DIR` | Path to client assets | (empty — use embedded) |

---

## Publisher Selection

The publisher implementation is chosen at startup based on configuration:

| Configuration | Publisher | SSE Delivery |
|---------------|----------|-------------|
| `--enable-web` (default) | `ChannelEventPublisher` | In-process Go channels |
| `--enable-web`, Postgres DB (future) | `PostgresEventPublisher` | Via `LISTEN/NOTIFY` |
| No `--enable-web` | `nil` (no-op) | No real-time updates |

```go
// In cmd/server.go initialization
if enableWeb {
    if isPostgres(db) && multiNodeEnabled {
        // Multi-node: use Postgres LISTEN/NOTIFY (zero new deps)
        publisher := hub.NewPostgresEventPublisher(pool)
        hubSrv.SetEventPublisher(publisher)
    } else {
        // Single-node default: in-process channels
        publisher := hub.NewChannelEventPublisher()
        hubSrv.SetEventPublisher(publisher)
    }
}
```

---

## Future: PostgreSQL LISTEN/NOTIFY

When horizontal scaling is needed (multiple Hub instances behind a load balancer), the `ChannelEventPublisher` can be replaced with a `PostgresEventPublisher` that uses `LISTEN/NOTIFY` on the existing production database. This adds zero new dependencies.

### Key Advantage: Transactional Publish

The "publish after commit" principle requires careful sequencing with external pub/sub systems — write to the database, then publish, with a failure window between the two operations. PostgreSQL eliminates this gap. `NOTIFY` issued inside the same transaction as the database write is held by Postgres until commit and discarded on rollback:

```go
tx, _ := pool.Begin(ctx)
_, _ = tx.Exec(ctx, `UPDATE agents SET status = $1 WHERE id = $2`, status, agentID)
_, _ = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, "grove:"+groveID, payload)
tx.Commit(ctx)  // Both the write and notification happen, or neither does
```

### Channel Mapping

PostgreSQL channels are flat strings with exact-match only. The practical approach is per-grove channels with event type in the JSON payload:

| Subject Pattern | Postgres Channel | Payload Wrapper |
|----------------|-----------------|----------------|
| `grove.{id}.agent.status` | `grove:{groveId}` | `{"type":"agent.status","data":{...}}` |
| `grove.{id}.agent.created` | `grove:{groveId}` | `{"type":"agent.created","data":{...}}` |
| `agent.{id}.status` | `agent:{agentId}` | `{"type":"status","data":{...}}` |
| `broker.{id}.status` | `broker:{brokerId}` | `{"type":"status","data":{...}}` |

### Constraints

- **8,000 byte payload limit.** All current event types are well under 1 KB.
- **Dedicated listener connection.** One shared listener per Hub instance fans out to SSE clients in-process.
- **Flat channel names.** No wildcard subscriptions. Per-grove channels are practical for typical deployments.
- **Database load.** Negligible for the event volumes in this design (tens per second at peak).

### When to Move Beyond Postgres

- Notification throughput exceeds thousands per second sustained.
- Heavy payloads (>8 KB) need pub/sub delivery.
- Multi-database-cluster deployments.

None of these are near-term concerns.

### Scaling Path

```
ChannelEventPublisher       ->  PostgresEventPublisher     ->  (external pub/sub if ever needed)
(Go channels, single proc)     (LISTEN/NOTIFY, zero deps)     (dedicated server, new dependency)
```

---

## Migration Stages

The port from Koa to Go is structured as a series of stages that preserve the existing Koa server's functionality. Each stage is independently deployable — the Koa server remains operational until the Go server fully replaces it.

### Strategy: Port First, Then Add EventPublisher

The EventPublisher integration is **interleaved** with the port rather than sequenced after it. The rationale:

- **The SSE endpoint is a core piece of the port.** The Go SSE handler is naturally written against the `ChannelEventPublisher` from the start. There's no benefit to porting the SSE endpoint with a temporary NATS dependency only to replace it.
- **Handler integration is low-risk.** Adding `s.events.PublishFoo(ctx, ...)` calls to handlers is a single line per handler. These are no-ops when the publisher is nil (no `--enable-web`), so they don't affect the API-only codepath.
- **Testing is easier.** The `ChannelEventPublisher` runs in-process with no external dependencies. Integration tests can subscribe, trigger an API call, and assert events arrive — all in a single test process.

The alternative — completing the full Koa port before adding real-time events — would mean building a Go SSE endpoint that has no event source to read from, then retrofitting it. That's wasted work.

### Stage 0: EventPublisher Interface & ChannelEventPublisher

**Goal:** Implement the event publishing infrastructure in the Hub, independent of any web server.

**Scope:**
- Define `EventPublisher` interface in `pkg/hub/events.go`.
- Implement `ChannelEventPublisher` with subject-pattern matching.
- Add `events EventPublisher` field to `Server` struct with nil-safe methods.
- Add `SetEventPublisher()` setter.
- Add `Close()` to shutdown path.
- Add publish calls to all handler integration points (agent, grove, broker handlers).
- Unit tests: nil publisher safety, subject matching, dual-publish for status.
- Integration tests: create agent via API, assert events arrive on subscribed channel.

**Koa impact:** None. The Koa server continues to run. The Hub publishes to in-process channels that nothing subscribes to yet (unless `--enable-web` is set later).

### Stage 1: Static Asset Serving & SPA Shell

**Goal:** The Go server can serve the web UI's static assets and SPA shell HTML.

**Scope:**
- Add `--enable-web` and `--web-assets-dir` flags.
- Implement `//go:embed` for client assets with runtime override.
- Implement SPA shell Go template (HTML with `__SCION_DATA__` slot).
- Add security headers middleware (CSP, HSTS, X-Frame-Options).
- Serve `/assets/*` from embedded or disk.
- Serve `/*` catch-all with SPA shell HTML.
- Health check endpoint reports web server status.

**Koa impact:** The Koa server still runs for auth and SSE. The Go server serves assets in parallel for testing. Developers can verify the SPA shell renders correctly against the Go server while auth still flows through Koa.

### Stage 2: Session Management & OAuth

**Goal:** The Go server handles browser authentication end-to-end.

**Scope:**
- Add `gorilla/sessions` dependency.
- Implement session middleware with signed httpOnly cookies (`scion_sess`).
- Port OAuth flow: `/auth/login/:provider`, `/auth/callback/:provider`, `/auth/logout`, `/auth/me`.
- Port dev-auth middleware (read `~/.scion/dev-token`, auto-create session).
- Port email domain authorization (`SCION_AUTHORIZED_DOMAINS`).
- Implement `SessionAuthMiddleware` for web routes.
- Wire auth into route mux alongside existing `UnifiedAuthMiddleware`.

**Koa impact:** At this point the Go server can fully replace the Koa server for browser auth. The Koa server is no longer needed for session management.

### Stage 3: SSE Endpoint

**Goal:** The Go server delivers real-time events to the browser via SSE.

**Scope:**
- Implement `GET /events?sub=...` SSE endpoint.
- Subject validation (format, prefix, authorization).
- Subscribe to `ChannelEventPublisher` for declared subjects.
- SSE response format: `id`, `event`, `data` fields, heartbeats.
- Connection lifecycle: subscribe on connect, unsubscribe on disconnect.
- Wire into session-authenticated route mux.

**Koa impact:** The SSE endpoint moves from Koa (reading from NATS) to Go (reading from in-process channels). This is the point where NATS is no longer needed as a runtime dependency.

### Stage 4: Koa Retirement

**Goal:** Remove the Koa server from the deployment.

**Scope:**
- Verify all Koa functionality is available in the Go server.
- Update deployment scripts and documentation.
- Remove NATS from deployment requirements.
- The Koa server code remains in the repository for reference but is no longer started.
- Update `web/README.md` and deployment docs.

**Final architecture:**

```
Browser (cookie auth)
  |
Go Server (single process)
  |-- Static assets (embedded or disk)
  |-- OAuth + sessions
  |-- SSE (ChannelEventPublisher)
  |-- Hub API (direct)
  |-- WebSocket PTY (direct)
```

### Stage 5 (Future): PostgreSQL LISTEN/NOTIFY

**Goal:** Support multi-node deployments where SSE clients connected to Hub A need events from Hub B.

**Scope:**
- Implement `PostgresEventPublisher` using `LISTEN/NOTIFY`.
- Add publisher selection logic based on database type and configuration.
- Per-grove listener connection with in-process fan-out to SSE clients.
- Integration tests with Postgres.

This stage is deferred until horizontal scaling is a concrete requirement.

---

## Health Check Integration

The `/readyz` endpoint reports the web server and event publisher status:

```json
{
  "status": "healthy",
  "uptime": "1h2m3s",
  "web": {
    "enabled": true,
    "assetsSource": "embedded"
  },
  "events": {
    "enabled": true,
    "type": "channel",
    "subscribers": 3
  }
}
```

The Hub remains healthy even if the event publisher has issues — events are best-effort.

---

## Testing

### Unit Tests

- `TestEventPublisherNil` — Verify nil publisher methods don't panic.
- `TestSubjectMatching` — Verify wildcard matching (`*`, `>`).
- `TestPublishAgentStatus` — Verify dual-publish to both subjects.
- `TestPublishAgentCreated` — Verify grove-scoped and agent-scoped subjects.
- `TestPublishGroveCreated` — Verify grove subject and payload.
- `TestChannelBackpressure` — Verify events are dropped (not blocked) when subscriber is slow.
- `TestSubscribeUnsubscribe` — Verify cleanup on unsubscribe.

### Integration Tests

```go
func TestSSEEndToEnd(t *testing.T) {
    // Create a ChannelEventPublisher
    pub := hub.NewChannelEventPublisher()
    defer pub.Close()

    // Create test server with publisher
    srv := createTestServer(t, pub)

    // Open SSE connection
    resp, _ := http.Get(srv.URL + "/events?sub=grove.test.>")
    defer resp.Body.Close()

    // Publish an event
    pub.PublishAgentStatus(ctx, &store.Agent{ID: "a1", GroveID: "test", Status: "running"})

    // Read SSE event from response body
    // Assert event contains expected data
}
```

No external dependencies needed for tests — no Docker, no NATS server, no Postgres.

### Manual Testing

```bash
# Terminal 1: Start Go server with web enabled
scion server start --enable-hub --enable-runtime-broker --enable-web --dev-auth

# Terminal 2: Open SSE connection
curl -N -H "Cookie: scion_sess=..." \
  "http://localhost:8080/events?sub=grove.test.>"

# Terminal 3: Trigger a state change
scion agent start --name test-agent
# -> SSE stream should show agent.created and agent.status events
```

---

## Dependencies

### Required (new)

- `github.com/gorilla/sessions` — Session cookie management for browser auth.

### Not Required

- No NATS client or server dependencies.
- No Node.js runtime.
- The `ChannelEventPublisher` uses only the standard library.

---

## Non-Goals

- **NATS as a dependency.** The original design explored NATS for event delivery. This was abandoned in favor of in-process channels. See [Appendix A](#appendix-a-why-not-nats).
- **Server-side rendering.** SSR is dropped. The SPA shell with inline critical CSS is sufficient for an authenticated internal tool.
- **Message persistence / replay.** Events are fire-and-forget. The web frontend fetches full state on page load and SSE reconnects restart from current state.
- **Harness event relay.** Heavy harness events (`agent.{id}.event`) from the broker's status monitor are out of scope. For a co-located broker, these can flow in-process through the `ChannelEventPublisher`. For remote brokers, they flow through the existing WebSocket control channel.
- **Horizontal Hub scaling.** Multi-node is accommodated by the `EventPublisher` interface but not implemented in the initial stages. PostgreSQL `LISTEN/NOTIFY` is the planned path when needed.

---

## Open Questions

### 1. Dashboard Grove Summaries

The client `StateManager` subscribes to `grove.*.summary` for the dashboard view. This requires periodic aggregation — not a single state-change event. Options:

- **(a) Periodic summary publisher goroutine.** A timer loop in the Hub queries the store and publishes a summary for each grove at a fixed interval (e.g., every 30s).
- **(b) Reactive summaries.** Recompute and publish a grove summary whenever any agent event occurs in that grove, debounced to avoid flooding.
- **(c) Client-side aggregation.** The dashboard uses the REST API for initial load and subscribes to `grove.{id}.agent.*` events for incremental updates. The client maintains its own counts.

Recommendation: **(a)** — a periodic goroutine is simple, predictable, and keeps the client stateless.

### 2. Harness Event Relay

Heavy events from the Runtime Broker's status monitor (tool use, thinking, harness output) need a separate pipeline design. For co-located brokers, the status monitor can publish directly to the `ChannelEventPublisher`. For remote brokers, they flow through the existing WebSocket control channel to the Hub, then to subscribers.

---

## Appendix A: Why Not NATS

An earlier design explored using NATS as the event transport between the Hub and the web frontend's SSE endpoint. This was abandoned for several reasons:

1. **Unnecessary for single-node.** The Hub is the single source of truth. All state changes flow through Hub handlers. The SSE endpoint lives in the same process. There is no second process to notify — Go channels suffice.

2. **Added runtime dependency.** NATS requires either an external server (`docker run nats:latest`) or an embedded server (adding ~15 MB and 11 module dependencies to the binary). Neither is justified when in-process delivery works.

3. **Operational complexity.** NATS adds connection management, reconnection logic, authentication configuration, health monitoring, and deployment coordination — all for a feature that Go channels provide with zero configuration.

4. **No advantage for scaling.** When multi-node scaling is needed, PostgreSQL `LISTEN/NOTIFY` provides a better solution: zero new dependencies (Postgres is already required), atomic write+publish semantics (strictly better than the two-phase approach NATS requires), and sufficient throughput for this use case.

5. **The Koa BFF was the reason NATS existed.** NATS bridged events from the Go Hub to the Node.js BFF — two separate processes. Consolidating the BFF into the Go binary eliminates the reason NATS was introduced.

The `EventPublisher` interface retains the abstraction boundary. If a future requirement genuinely needs a dedicated pub/sub server, the interface allows adding a new implementation without changing handler code. But the dependency can wait until there's a concrete need.
