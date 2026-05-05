# A2A Protocol Bridge Design

This document describes the design of a bridge service that exposes Scion agents as A2A (Agent-to-Agent) protocol endpoints, and allows external A2A agents to interact with Scion agents using the standard A2A protocol. The bridge follows the same architectural pattern as `scion-chat-app` — a self-managed broker plugin with an HTTP frontend — but translates between A2A JSON-RPC/gRPC and Scion's Hub API.

## Background

### What is A2A?

A2A is an open protocol (v1.0, published by Google and partners) for communication between AI agents built by different vendors. It provides:

- **Agent Cards** — JSON metadata documents describing agent capabilities, skills, auth requirements, served at `/.well-known/agent-card.json`
- **Tasks** — the unit of work, with lifecycle states: `submitted → working → completed/failed/canceled/input-required/rejected/auth-required`
- **Messages** — user/agent turns containing typed Parts (text, file bytes/URL, structured data)
- **Artifacts** — output products of a task (distinct from messages)
- **Streaming** — SSE-based real-time updates via `SendStreamingMessage` and `SubscribeToTask`
- **Push Notifications** — webhook-based async updates for disconnected clients
- **Sessions** — `contextId` groups related tasks into multi-turn conversations

The protocol supports three transport bindings: JSON-RPC 2.0 over HTTP, gRPC, and HTTP+JSON/REST. All use camelCase JSON serialization.

### Why a Bridge?

Scion agents are powerful but speak Scion's internal protocol (broker messages, Hub API). The A2A bridge makes Scion agents discoverable and usable by any A2A-compatible client — other agent frameworks, orchestrators, or tooling — without modifying the agents themselves.

### Reference Architecture: scion-chat-app

The bridge follows the proven pattern from `extras/scion-chat-app/`:

```
External A2A Client ←JSON-RPC/gRPC→  scion-a2a-bridge  ←Hub API→  Scion Hub
                                           ↓                          ↓
                                     SQLite state           broker plugin (RPC)
                                     (task mapping)         (reverse channel)
```

The chat-app runs two servers (webhook + broker plugin RPC) and maps external identities/spaces to Scion users/groves. The A2A bridge follows the same dual-server model but replaces the Google Chat adapter with an A2A protocol server.

## Concept Mapping: A2A ↔ Scion

| A2A Concept | Scion Concept | Notes |
|---|---|---|
| Agent Card | Agent Template + running Agent | Card generated dynamically from template metadata + agent state |
| Agent Card URL | Bridge endpoint + grove/agent slug | `https://bridge.example.com/groves/{grove}/agents/{agent}` |
| Agent Skill | Template description / agent instructions | Skills derived from template metadata |
| Task | Agent message exchange | A task maps to one or more messages sent to a running agent |
| Task ID | Bridge-generated UUID | Bridge maintains task-to-agent mapping in its own state |
| contextId (session) | Scion Agent instance | Each A2A context maps to a Scion agent; agent maintains conversation state internally |
| Message (role: user) | Outbound message to agent | `POST /api/v1/agents/{id}/outbound-message` |
| Message (role: agent) | Broker message from agent | Received via broker plugin `Publish()` |
| Artifact | Agent output (parsed from message) | Bridge extracts structured outputs from agent messages |
| TaskState: submitted | Agent activity: idle → thinking | Message accepted, agent processing |
| TaskState: working | Agent activity: thinking/executing | Agent actively working |
| TaskState: completed | Agent activity: completed/idle | Agent finished processing the task |
| TaskState: input-required | Agent activity: waiting_for_input | Agent needs more information |
| TaskState: failed | Agent activity: error | Agent encountered an error |
| Push Notification | Broker plugin → webhook POST | Bridge forwards state changes as A2A webhook payloads |

## Architecture

### Component Overview

```
┌─────────────────────────────────────────────────────────────┐
│                     scion-a2a-bridge                         │
│                                                              │
│  ┌──────────────────────┐    ┌─────────────────────────┐    │
│  │   A2A Protocol Server │    │   Broker Plugin Server   │    │
│  │   (port 8443)         │    │   (port 9090)            │    │
│  │                        │    │                           │    │
│  │  - Agent Card serving  │    │  - Publish() handler      │    │
│  │  - SendMessage         │    │  - Subscribe/Unsubscribe  │    │
│  │  - SendStreamingMsg    │    │  - Host callbacks         │    │
│  │  - GetTask / ListTasks │    │  - Health checks          │    │
│  │  - CancelTask          │    │                           │    │
│  │  - SubscribeToTask     │    │                           │    │
│  │  - Push notification   │    │                           │    │
│  │    config CRUD         │    │                           │    │
│  └──────────┬─────────────┘    └────────────┬──────────────┘    │
│             │                                │                   │
│  ┌──────────┴────────────────────────────────┴──────────────┐   │
│  │                    Core Bridge Logic                       │   │
│  │                                                            │   │
│  │  - Task lifecycle management                               │   │
│  │  - Context-to-agent mapping                                │   │
│  │  - Agent card generation                                   │   │
│  │  - SSE stream management                                   │   │
│  │  - Push notification dispatch                              │   │
│  │  - Auth translation                                        │   │
│  └──────────┬────────────────────────────────┬──────────────┘   │
│             │                                │                   │
│  ┌──────────┴──────────┐    ┌───────────────┴───────────────┐   │
│  │   Hub API Client     │    │        State Store            │   │
│  │   (hubclient)        │    │        (SQLite)               │   │
│  │                      │    │                                │   │
│  │  - Agent CRUD        │    │  - Tasks (A2A ↔ Scion)        │   │
│  │  - SendMessage       │    │  - Contexts (A2A ↔ Agent)     │   │
│  │  - Grove/Template    │    │  - Push notification configs   │   │
│  │    queries            │    │  - Auth credentials cache     │   │
│  └──────────────────────┘    └───────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### Two-Server Model

Following the chat-app pattern:

1. **A2A Protocol Server (port 8443)** — Handles inbound A2A JSON-RPC and gRPC requests from external clients. Serves agent cards, processes `SendMessage`/`SendStreamingMessage`, manages task state.

2. **Broker Plugin Server (port 9090)** — Implements `MessageBrokerPluginInterface`. Hub connects via go-plugin RPC and pushes agent messages/state changes. The bridge translates these into A2A task events and dispatches to SSE streams or push notification webhooks.

### Dependencies

- **`a2aproject/a2a-go/v2`** — Official A2A Go SDK. Provides types (`a2a.Task`, `a2a.Message`, `a2a.AgentCard`, etc.), server framework (`a2asrv`), and transport bindings. The SDK's `AgentExecutor` interface and iterator-based streaming map well to our bridge pattern.
- **`pkg/plugin`** — Scion's broker plugin interface (`MessageBrokerPluginInterface`)
- **`pkg/messages`** — Scion's `StructuredMessage` type
- **Hub API client** — `hubclient` package for agent operations

## Detailed Design

### 1. Agent Card Generation

Agent Cards are generated dynamically by combining Scion template metadata with agent runtime state.

**Discovery endpoint:** `GET /.well-known/agent-card.json` returns a card for the default agent, or the bridge serves per-agent cards at:

```
GET /groves/{groveSlug}/agents/{agentSlug}/.well-known/agent-card.json
```

**Card construction:**

```
AgentCard:
  name         ← agent.Name (from Hub API)
  description  ← template.Description
  url          ← bridge base URL + grove/agent path
  version      ← template schema_version or agent creation timestamp
  provider     ← configured bridge provider info
  capabilities:
    streaming          ← true (bridge always supports SSE)
    pushNotifications  ← true (bridge always supports webhooks)
    extendedAgentCard  ← true if auth is configured (extended card reveals additional skills)
  skills       ← derived from template description + system prompt analysis
  defaultInputModes  ← ["text/plain", "application/json"]
  defaultOutputModes ← ["text/plain", "application/json"]
  securitySchemes    ← configured auth (see Auth section)
  supportedInterfaces:
    - url: bridge_url + path
      protocolBinding: "JSONRPC"
      protocolVersion: "1.0"
```

**Skill derivation:** Templates contain `description` and `system-prompt.md`. The bridge can expose a single skill per agent (the agent's primary capability) or parse structured skill metadata from a convention in the template (e.g., a `skills:` section in `scion-agent.yaml`). Initially, one skill per agent derived from the template description.

**Card caching:** Cards are cached in memory with a TTL (e.g., 5 minutes) and invalidated on agent state changes received via the broker plugin. `Cache-Control` and `ETag` headers are set per A2A spec.

### 2. Task Lifecycle

#### Creating a Task (SendMessage)

```
A2A Client                    Bridge                         Scion Hub
    │                            │                                │
    │  SendMessage(message)      │                                │
    │ ──────────────────────────>│                                │
    │                            │  resolve contextId → agent     │
    │                            │  create Task record (SQLite)   │
    │                            │                                │
    │                            │  POST /agents/{id}/outbound-   │
    │                            │       message (text)           │
    │                            │ ─────────────────────────────> │
    │                            │                                │
    │                            │  Task{state: submitted}        │
    │  <─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│                                │
    │                            │                                │
    │                            │  [agent processes message]     │
    │                            │                                │
    │                            │  Publish(topic, response_msg)  │
    │                            │ <───────────────────────────── │
    │                            │                                │
    │                            │  update Task state + artifacts │
    │  <─ ─ Task{state: done} ─ ─│                                │
```

**Blocking vs non-blocking:** The `returnImmediately` field in `SendMessageConfiguration` controls whether the bridge returns immediately after submitting the message (client must then poll or subscribe) or holds the connection until the agent responds.

Default behavior (blocking): The bridge holds the HTTP connection, subscribes to the agent's broker topic, waits for the agent response via `Publish()`, then returns the completed task. A timeout (configurable, e.g., 120s) prevents indefinite hangs.

#### Task State Mapping

The bridge maps Scion agent activities to A2A task states:

```go
func mapActivityToTaskState(activity string) a2a.TaskState {
    switch activity {
    case "IDLE":
        return a2a.TaskStateCompleted  // idle after processing = done
    case "THINKING", "EXECUTING":
        return a2a.TaskStateWorking
    case "WAITING_FOR_INPUT":
        return a2a.TaskStateInputRequired
    case "COMPLETED":
        return a2a.TaskStateCompleted
    case "ERROR":
        return a2a.TaskStateFailed
    case "STALLED", "LIMITS_EXCEEDED", "OFFLINE":
        return a2a.TaskStateFailed
    default:
        return a2a.TaskStateWorking
    }
}
```

The mapping is not 1:1 because Scion's activity model is richer. The bridge tracks state transitions in its SQLite store to correctly generate A2A `TaskStatusUpdateEvent` sequences.

#### Task State Store

```sql
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,           -- A2A task ID (UUID)
    context_id TEXT NOT NULL,      -- A2A contextId
    grove_id TEXT NOT NULL,        -- Scion grove
    agent_slug TEXT NOT NULL,      -- Scion agent
    state TEXT NOT NULL,           -- Current A2A TaskState
    created_at TIMESTAMP,
    updated_at TIMESTAMP,
    metadata TEXT                  -- JSON metadata blob
);

CREATE INDEX idx_tasks_context ON tasks(context_id);
CREATE INDEX idx_tasks_agent ON tasks(grove_id, agent_slug);
```

#### Multi-Turn Conversations (contextId → Agent)

A2A uses `contextId` to group related tasks into a session. In Scion, conversation state lives inside the agent — each agent maintains its own context across messages.

**Mapping:**

```sql
CREATE TABLE contexts (
    context_id TEXT PRIMARY KEY,   -- A2A contextId
    grove_id TEXT NOT NULL,        -- Scion grove
    agent_slug TEXT NOT NULL,      -- Scion agent slug
    created_at TIMESTAMP,
    last_active TIMESTAMP
);
```

**Context resolution flow:**

1. Client sends `SendMessage` with `contextId` → look up existing context → route to mapped agent
2. Client sends `SendMessage` without `contextId` → bridge generates one, creates mapping to the target agent
3. Client sends `SendMessage` with `taskId` referencing existing task → look up task → infer context and agent
4. Unknown `contextId` → return error (contexts must be established through the bridge)

**Agent-per-context model:** Each `contextId` maps to exactly one Scion agent. The agent may already be running (reuse) or may need to be created on demand (if the bridge is configured for auto-provisioning).

**Auto-provisioning (optional):** When the bridge receives a message for a grove but no specific agent, it can create a new agent from a configured default template:

```go
agent, err := hubClient.GroveAgents(groveID).Create(ctx, &hubclient.CreateAgentRequest{
    Name:      fmt.Sprintf("a2a-%s", contextID[:8]),
    Template:  config.DefaultTemplate,
    AutoStart: true,
})
```

This is configurable per-grove. Some deployments may require agents to be pre-created.

### 3. Message Translation

#### A2A Message → Scion StructuredMessage

```go
func translateA2AToScion(msg a2a.Message) *messages.StructuredMessage {
    var textContent strings.Builder
    var attachments []string

    for _, part := range msg.Parts {
        switch {
        case part.Text != "":
            textContent.WriteString(part.Text)
        case part.URL != "":
            attachments = append(attachments, part.URL)
        case part.Data != nil:
            // Serialize structured data as JSON in message body
            jsonBytes, _ := json.Marshal(part.Data)
            textContent.WriteString(string(jsonBytes))
        case part.Raw != nil:
            // Binary content: write to temp file, attach path
            path := writeTempFile(part.Raw, part.Filename, part.MediaType)
            attachments = append(attachments, path)
        }
    }

    return &messages.StructuredMessage{
        Version:     1,
        Timestamp:   time.Now().Format(time.RFC3339),
        Msg:         textContent.String(),
        Type:        "instruction",
        Attachments: attachments,
    }
}
```

**Limitations:** Scion's `StructuredMessage.Msg` is a single string (max 64KB). Multi-part A2A messages with mixed content types are flattened. File parts are handled via attachments. This is acceptable for text-heavy agent interactions but may need enhancement for binary-heavy workflows.

#### Scion StructuredMessage → A2A Response

```go
func translateScionToA2A(msg *messages.StructuredMessage) (a2a.Message, []a2a.Artifact) {
    parts := []a2a.Part{{Text: msg.Msg, MediaType: "text/plain"}}

    for _, att := range msg.Attachments {
        parts = append(parts, a2a.Part{URL: att})
    }

    message := a2a.Message{
        MessageID: generateID(),
        Role:      a2a.RoleAgent,
        Parts:     parts,
    }

    // If the message looks like a final output, also create an artifact
    var artifacts []a2a.Artifact
    if msg.Type == "" || msg.Type == "instruction" {
        artifacts = append(artifacts, a2a.Artifact{
            ArtifactID: generateID(),
            Parts:      parts,
        })
    }

    return message, artifacts
}
```

**Message vs artifact distinction:** A2A distinguishes between messages (communication) and artifacts (outputs). The bridge treats the final agent response as an artifact and intermediate messages (e.g., status updates) as messages.

### 4. Streaming (SSE)

#### SendStreamingMessage

When a client calls `SendStreamingMessage`, the bridge:

1. Opens an SSE connection to the client
2. Sends initial `Task` object with state `submitted`
3. Forwards the message to the Scion agent via Hub API
4. Subscribes to the agent's broker topic for responses
5. As agent responses arrive via `Publish()`, translates and sends as SSE events:
   - `TaskStatusUpdateEvent` for state changes
   - `TaskArtifactUpdateEvent` for output content
6. Sends final `TaskStatusUpdateEvent` with terminal state, closes SSE

**SSE event format (JSON-RPC binding):**
```
data: {"jsonrpc":"2.0","id":1,"result":{"task":{"id":"...","status":{"state":"TASK_STATE_SUBMITTED"}}}}

data: {"jsonrpc":"2.0","id":1,"result":{"statusUpdate":{"taskId":"...","status":{"state":"TASK_STATE_WORKING"}}}}

data: {"jsonrpc":"2.0","id":1,"result":{"artifactUpdate":{"taskId":"...","artifact":{"artifactId":"...","parts":[{"text":"Hello"}],"lastChunk":true}}}}

data: {"jsonrpc":"2.0","id":1,"result":{"statusUpdate":{"taskId":"...","status":{"state":"TASK_STATE_COMPLETED"},"final":true}}}
```

#### SubscribeToTask

Allows clients to reconnect to an in-progress task's event stream. The bridge looks up the task in its store, verifies it's not in a terminal state, and opens a new SSE connection. Any new events from the broker are forwarded.

#### Stream Management

Active SSE streams are tracked in memory:

```go
type streamManager struct {
    mu      sync.RWMutex
    streams map[string][]chan a2a.StreamResponse  // taskID → channels
}
```

When a broker `Publish()` arrives, the bridge fans out the translated event to all active streams for that task. Streams are cleaned up on client disconnect or task completion.

### 5. Push Notifications

Push notifications allow A2A clients to receive task updates via webhooks instead of holding open SSE connections.

**Storage:**

```sql
CREATE TABLE push_notification_configs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    url TEXT NOT NULL,
    token TEXT,
    auth_scheme TEXT,
    auth_credentials TEXT,
    created_at TIMESTAMP,
    FOREIGN KEY (task_id) REFERENCES tasks(id)
);

CREATE INDEX idx_push_task ON push_notification_configs(task_id);
```

**Dispatch flow:**

When the broker plugin receives an agent state change:
1. Look up push notification configs for the task
2. For each config, POST a `StreamResponse` JSON payload to the webhook URL
3. Include auth credentials (`Authorization: {scheme} {credentials}`)
4. Retry with exponential backoff on failure (max 3 retries)
5. Remove config after consecutive failures (configurable threshold)

**Content-Type:** `application/a2a+json` per spec.

### 6. Broker Plugin Integration

The bridge implements `MessageBrokerPluginInterface` exactly as the chat-app does:

```go
type BrokerServer struct {
    bridge        *Bridge
    hostCallbacks plugin.HostCallbacks
    subscriptions map[string]bool
    mu            sync.RWMutex
}

func (b *BrokerServer) Configure(config map[string]string) error {
    // Store host callbacks for subscription management
    // Request initial subscriptions
    return nil
}

func (b *BrokerServer) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
    // Parse topic: scion.grove.<groveId>.agent.<agentSlug>.messages
    // OR: scion.grove.<groveId>.user.<userId>.messages
    // Translate to A2A events, dispatch to streams and push webhooks
    return b.bridge.handleAgentMessage(ctx, topic, msg)
}
```

**Subscription patterns:**

On startup and when new contexts are created, the bridge requests subscriptions:

```go
// Subscribe to all agent messages in a grove
b.hostCallbacks.RequestSubscription(fmt.Sprintf("scion.grove.%s.agent.>.messages", groveID))

// Or more targeted: specific agent
b.hostCallbacks.RequestSubscription(fmt.Sprintf("scion.grove.%s.agent.%s.messages", groveID, agentSlug))
```

The bridge subscribes to user-targeted messages (`scion.grove.*.user.>.messages`) like the chat-app, filtering to only relay messages relevant to active A2A tasks.

**Message type handling:**

| StructuredMessage.Type | A2A Action |
|---|---|
| `instruction` / `""` | Agent response → create TaskArtifactUpdateEvent + status update |
| `state-change` | Activity changed → create TaskStatusUpdateEvent |
| `input-needed` | Agent needs input → TaskState: INPUT_REQUIRED |

### 7. Authentication

Authentication is layered, following the chat-app's three-context model:

#### Layer 1: A2A Client → Bridge (External Auth)

The bridge authenticates incoming A2A requests. Configurable schemes declared in the agent card:

- **API Key** — simple key in header (`X-API-Key`), suitable for service-to-service
- **Bearer Token (JWT)** — for user-context requests, validated against a configured JWKS endpoint or shared secret
- **OAuth2 Client Credentials** — for machine-to-machine, bridge acts as OAuth2 resource server

The auth scheme is configured per-deployment:

```yaml
auth:
  scheme: "bearer"           # or "apiKey", "oauth2"
  jwks_url: "https://..."    # For JWT validation
  api_key: "${A2A_API_KEY}"  # For API key auth
  oauth2:
    token_url: "https://..."
    client_id: "..."
```

The agent card's `securitySchemes` and `securityRequirements` are populated from this config.

#### Layer 2: Bridge → Hub (Internal Auth)

The bridge authenticates to the Hub using the same token-minting pattern as the chat-app:

```go
type TokenMinter struct {
    signer jose.Signer  // HS256 with hub signing key
}

func (m *TokenMinter) MintToken(userID, email, role string, duration time.Duration) string {
    // Mint short-lived JWT signed with Hub's shared key
}
```

- **Admin auth** — auto-refreshing admin token for system operations (list agents, manage groves)
- **User impersonation** — if the A2A client identity maps to a Scion user, mint a scoped token for that user

#### Layer 3: Identity Mapping (A2A Client → Scion User)

```sql
CREATE TABLE identity_mappings (
    external_id TEXT PRIMARY KEY,    -- A2A client identity (from JWT sub, API key, etc.)
    hub_user_id TEXT NOT NULL,       -- Scion Hub user ID
    hub_user_email TEXT NOT NULL,    -- Scion Hub user email
    mapped_at TIMESTAMP
);
```

If no mapping exists, the bridge falls back to the admin user for agent operations. Identity mapping can be configured statically (config file) or dynamically (registration endpoint).

### 8. Configuration

```yaml
# scion-a2a-bridge config
bridge:
  listen_address: ":8443"
  external_url: "https://a2a.example.com"
  provider:
    organization: "My Org"
    url: "https://example.com"

hub:
  endpoint: "https://hub.example.com"
  user: "a2a-bridge@example.com"
  signing_key: "/path/to/signing-key.b64"    # or signing_key_secret for GCP SM

plugin:
  listen_address: "localhost:9090"

auth:
  scheme: "bearer"
  jwks_url: "https://auth.example.com/.well-known/jwks.json"

groves:
  - slug: "my-grove"
    default_template: "default"
    auto_provision: true           # Create agents on demand
    exposed_agents:                # Explicit allowlist (empty = all)
      - "research-agent"
      - "coding-agent"

state:
  database: "/var/lib/scion-a2a-bridge/state.db"

timeouts:
  send_message: 120s               # Max wait for blocking SendMessage
  sse_keepalive: 30s               # SSE keepalive ping interval
  push_retry_max: 3                # Max push notification retries

logging:
  level: "info"
  format: "json"
```

### 9. A2A SDK Integration

The bridge uses the official `a2aproject/a2a-go/v2` SDK to avoid reimplementing A2A protocol handling:

**Server-side:** The SDK's `a2asrv` package provides the `AgentExecutor` interface:

```go
type AgentExecutor interface {
    Execute(ctx context.Context, execCtx ExecutionContext) iter.Seq2[a2a.Event, error]
    Cancel(ctx context.Context, taskID string, contextID string) error
}
```

The bridge implements this interface. `Execute()` translates the A2A message to a Scion outbound message, sends it via Hub API, then yields events as they arrive from the broker plugin. The iterator pattern maps naturally to the bridge's async message flow:

```go
func (b *Bridge) Execute(ctx context.Context, execCtx a2asrv.ExecutionContext) iter.Seq2[a2a.Event, error] {
    return func(yield func(a2a.Event, error) bool) {
        // 1. Resolve context → agent
        // 2. Create task record
        // 3. Send message to agent via Hub API
        // 4. Subscribe to broker events for this agent
        // 5. Yield events as they arrive, translating Scion → A2A
        // 6. Yield final status event on completion/error
    }
}
```

**Types:** `a2a.Task`, `a2a.Message`, `a2a.Part`, `a2a.AgentCard`, `a2a.Artifact`, `a2a.TaskStatusUpdateEvent`, `a2a.TaskArtifactUpdateEvent` — all from the SDK.

**Transport:** The SDK handles JSON-RPC dispatch, SSE framing, and gRPC bindings. The bridge registers its `AgentExecutor` with the SDK's server and gets protocol handling for free.

**Version compatibility:** The SDK supports both A2A v0.3 and v1.0 via `a2acompat`. The bridge targets v1.0 but can serve v0.3 clients through the compatibility layer.

## Routing and URL Structure

### Per-Agent Endpoints

Each exposed Scion agent gets its own A2A endpoint:

```
Base:  https://bridge.example.com/groves/{groveSlug}/agents/{agentSlug}

Card:  GET  .../.well-known/agent-card.json
RPC:   POST .../jsonrpc    (JSON-RPC 2.0)
gRPC:  (same host, gRPC service)
REST:  POST .../message:send
       POST .../message:stream
       GET  .../tasks/{taskId}
       ...
```

### Discovery

A top-level agent card at `/.well-known/agent-card.json` can serve as a **registry card** that lists all available agents (as skills), or redirect to individual agent cards.

Alternatively, a `GET /groves/{groveSlug}/agents` endpoint returns a JSON array of agent card URLs for programmatic discovery.

## Deployment

### As a Standalone Service

Like the chat-app, the bridge runs as a single binary with two listeners:

```
scion-a2a-bridge --config /etc/scion-a2a-bridge/config.yaml
```

- Port 8443: A2A protocol server (TLS recommended)
- Port 9090: Broker plugin RPC (localhost only)

### Hub Registration

The bridge registers its broker plugin with the Hub:

```yaml
# Hub config addition
plugins:
  - name: "a2a-bridge"
    type: "broker"
    managed: false              # Self-managed
    address: "localhost:9090"
```

Or via API: `POST /api/v1/brokers` with the plugin's listen address.

### Docker / Cloud Run

Multi-stage Docker build from the repo root (same pattern as chat-app). The bridge is stateless except for the SQLite database, which can be mounted as a persistent volume.

## Error Handling

### A2A Error Codes

The bridge returns standard A2A error codes:

| Scenario | A2A Error | Code |
|---|---|---|
| Unknown task ID | TaskNotFoundError | -32001 |
| Cancel terminal task | TaskNotCancelableError | -32002 |
| Push not configured | PushNotificationNotSupportedError | -32003 |
| Unsupported method | UnsupportedOperationError | -32004 |
| Bad content type | ContentTypeNotSupportedError | -32005 |
| Hub unreachable | InternalError | -32603 |
| Agent not found | TaskNotFoundError | -32001 |
| Auth required | Standard HTTP 401/403 | — |

### Resilience

- **Broker reconnection** — uses the `reconnectingBrokerAdapter` wrapper from `pkg/plugin`; re-subscribes patterns on reconnect
- **Hub API failures** — retried with backoff; tasks transition to `failed` after max retries
- **SSE disconnects** — clients can reconnect via `SubscribeToTask`; bridge replays latest state
- **Graceful shutdown** — 30-second timeout, drain active SSE connections, flush pending push notifications

## Open Questions

1. **Agent auto-provisioning vs pre-created:** Should the bridge create agents on demand when a new `contextId` arrives, or require agents to be pre-provisioned? The design supports both via config, but the default behavior needs deciding.

2. **Artifact extraction:** Scion agents return unstructured text. How should the bridge identify and extract structured artifacts from agent output? Options: (a) entire response is one artifact, (b) parse markdown code blocks as separate artifacts, (c) agent-specific parsers.

3. **Multi-grove routing:** Should one bridge instance serve multiple groves, or one bridge per grove? The design supports multi-grove but the URL structure and auth implications differ.

4. **Task history and persistence:** How long should task records be retained? A2A clients may call `GetTask` or `ListTasks` long after completion. Options: TTL-based cleanup, configurable retention, or rely on external storage.

5. **Extended Agent Cards:** The A2A spec supports authenticated extended cards with additional skills. Should the bridge expose different skill sets based on the caller's auth context?

6. **CancelTask implementation:** Scion agents don't have a direct "cancel current task" operation. `CancelTask` could (a) stop the agent, (b) send an interrupt message, or (c) just mark the task as canceled in the bridge without affecting the agent.

7. **gRPC support priority:** The official SDK supports gRPC bindings. Should the bridge support gRPC from day one or start with JSON-RPC only?

## Implementation Phases

### Phase 1: Core Bridge (MVP)

- Agent card serving (static, from template metadata)
- `SendMessage` (blocking mode only)
- `GetTask` and `ListTasks`
- Broker plugin integration (receive agent responses)
- Single grove, pre-created agents only
- API key auth
- SQLite state store

### Phase 2: Streaming and Push

- `SendStreamingMessage` with SSE
- `SubscribeToTask`
- Push notification config CRUD and dispatch
- Non-blocking `SendMessage` (`returnImmediately: true`)

### Phase 3: Advanced Features

- Agent auto-provisioning
- Multi-grove support
- OAuth2/OIDC auth
- Extended agent cards
- `CancelTask`
- gRPC transport binding
- Dynamic skill derivation from agent capabilities

### Phase 4: Production Hardening

- Prometheus metrics (request latency, task throughput, SSE active connections)
- Distributed state store option (PostgreSQL)
- Rate limiting
- Multi-tenancy
- Agent card signing (JWS)
- Health check endpoints (`/healthz`, `/readyz`)

## References

- [A2A Protocol Specification v1.0](https://github.com/a2aproject/A2A/blob/main/docs/specification.md)
- [Official Go SDK: a2aproject/a2a-go](https://github.com/a2aproject/a2a-go)
- [Scion Message Broker Plugin Design](message-broker-plugins.md)
- [Scion Chat App (reference bridge)](../extras/scion-chat-app/)
- [Scion Plugin System](scion-plugins.md)
