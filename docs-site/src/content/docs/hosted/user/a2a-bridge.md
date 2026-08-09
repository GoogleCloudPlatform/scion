---
title: A2A Protocol Bridge
description: Connect external Agent-to-Agent (A2A) clients and desktop applications to your Scion agents.
---

The **A2A (Agent-to-Agent) Protocol Bridge** is a standalone, self-managed service that bridges the Scion Hub with external systems using the open [A2A Protocol specification](https://google.github.io/A2A/). 

By running the bridge, you expose Scion agents as standard A2A-compliant JSON-RPC endpoints. This allows programmatic multi-agent orchestration, third-party platform integrations, and federation with desktop clients such as Claude Desktop or Codex Desktop.

---

## 1. Overview

The [A2A protocol](https://google.github.io/A2A/) is an open standard developed by Google for secure, structured communication between independent artificial agents. It defines a lightweight JSON-RPC 2.0 dialect over HTTP, alongside standard discovery mechanics ("Agent Cards") for advertising capabilities.

The Scion A2A Protocol Bridge (`scion-a2a-bridge`) translates these standard JSON-RPC payloads into Scion Hub API calls and vice versa. It enables:

* **Universal Client Access**: Any software that speaks the A2A protocol can discover, query, and message Scion agents without needing a dedicated Scion SDK.
* **Desktop App Federation**: Users can run agents locally inside desktop wrappers (like Claude Desktop or Codex Desktop) while leveraging the orchestration, resources, and hardware runtimes managed by a hosted Scion Hub.
* **Programmatic Integration**: Connect Scion agents to external multi-agent systems or automated workflow engines.

---

## 2. Architecture

The A2A bridge runs as an independent, standalone Go process alongside the Scion Hub. It acts as both a **client** of the Hub (for calling Hub APIs) and a **broker plugin** (for receiving real-time stream messages).

```
   ┌─────────────────┐             ┌──────────────┐             ┌───────────┐
   │   A2A Client    │───JSON-RPC─&gt;│ A2A Bridge   │───REST API─&gt;│ Scion Hub │
   │ (e.g., Desktop) │&lt;───Cards────│   (Port 8443)│             │           │
   └─────────────────┘             └──────────────┘             └───────────┘
                                           ▲                          │
                                           │       Broker Plugin      │
                                           └──────(go-plugin RPC)─────┘
                                                  (Port 9090)
```

### Dual-Server Design
The `scion-a2a-bridge` process runs two concurrent RPC servers:
1. **A2A HTTP Server (Default: Port 8443)**: Serves external A2A clients. Handles agent discovery requests (`/.well-known/agent-card.json`), JSON-RPC message exchanges, operational health checks (`/healthz`, `/readyz`), and Prometheus metrics (`/metrics`).
2. **Broker Plugin RPC Server (Default: Port 9090)**: A `go-plugin` RPC interface. The Scion Hub connects to this port to push real-time agent-emitted messages, events, and status changes back to the bridge.

### High Availability (HA) & Cloud Run Deployment

To support robust, load-balanced hosted environments, the A2A Bridge includes advanced production capabilities:

#### Leaderless HA Architecture
The A2A Bridge supports High Availability (HA) natively through a **leaderless architecture**. Every replica is identical and interchangeable — there is no leader election, coordination overhead, or instance-identity requirement. Behind a load balancer, any replica can handle any incoming A2A request or receive broker plugin events, enabling seamless horizontal scaling.

HA mode requires **standalone mode** (`--standalone` flag or `A2A_STANDALONE=true`) with a shared **PostgreSQL** backend (`DATABASE_URL`). This replaces the default local SQLite store so that all replicas share webhook subscriptions, task state, and admin configuration. Without a shared database, per-replica local state (SQLite) would drift across replicas and be lost on restart — making SQLite unsuitable for multi-replica or ephemeral deployments like Cloud Run.

#### Single-Port h2c Multiplexing (Cloud Run)
Google Cloud Run enforces a strict single-port limitation for incoming traffic. To run both the A2A HTTP server and the Broker Plugin RPC gRPC server on a single container port, the bridge implements **h2c port multiplexing**:
- **Auto-Detection**: The bridge automatically detects when it is running on Cloud Run by checking for the presence of the `K_SERVICE` environment variable.
- **Traffic Routing**: When detected, the bridge binds to the designated `$PORT` and multiplexes incoming HTTP/1.1 (standard A2A protocol JSON-RPC, health checks, and metrics) and HTTP/2 (gRPC broker traffic) over that single port. This eliminates the need for separate external ports or complex sidecar routing proxies.

### Interaction Modes
The bridge supports three distinct A2A communication mechanics:
* **Synchronous Blocking (SendMessage)**: The client POSTs a message and holds the HTTP connection open (up to `timeouts.send_message`, default 120s) until the agent produces its final response.
* **Server-Sent Events (SSE) Streaming (SendStreamingMessage)**: The client opens an SSE connection (`message/stream`) to receive real-time, token-by-token streaming updates as the agent executes.
* **Asynchronous Webhooks (Push Notifications)**: Clients register a callback URL (`tasks/pushNotification/set`). The bridge stores this subscription in its state database (SQLite in default mode, PostgreSQL in standalone/HA mode) and POSTs state-change alerts (running, completed, input-required, error) to the webhook as they occur.

---

## 3. Setup & Installation

The A2A bridge can be built from source and is typically installed and managed via the Hub Admin UI.

### Step 1: Install via Admin UI (Recommended)
1. Log in to the Scion Web Dashboard as an administrator.
2. Navigate to **Admin > Integrations** in the sidebar.
3. Locate **A2A Bridge** under the **Available Integrations** list. Its description reads: *"External service — installed separately, managed via admin UI"*.
4. Click **Install**.
5. The Hub registers the bridge as a `self_managed` plugin under `settings.yaml` and displays detailed setup instructions, including the location of the generated operator bootstrap config template (e.g., `~/.scion/scion-a2a-bridge.yaml`) and the startup command.

:::note[Self-Managed Plugin Pattern]
Unlike regular containerized plugins, the Hub does not manage the lifecycle (starting, stopping, or running) of the A2A bridge binary. The operator must build and run the binary on their own infrastructure (VM, container, or local process).
:::

### Step 2: Build the Binary
From your Scion repository root, compile the Go binary:

```sh
# Using the project Makefile
make build-a2a-bridge

# Or compiling manually
cd extras/scion-a2a-bridge
go build -o scion-a2a-bridge ./cmd/scion-a2a-bridge/
```

Verify the binary is available:
```sh
./bin/scion-a2a-bridge --help
```

### Step 3: Run the Bridge
Run the bridge binary using the bootstrap configuration file created during the Admin UI installation:

```sh
# If using a static API key, export it as an environment variable
export A2A_API_KEY="your-secure-static-api-key"

# Run the process
./bin/scion-a2a-bridge --config ~/.scion/scion-a2a-bridge.yaml
```

### Step 4: Verify Connection
Return to the Hub **Admin > Integrations** page. Click **Reconnect** on the A2A Bridge integration card. Once the Hub successfully dials the bridge's RPC server on port `9090`, the status badge will update to **Connected**.

### Dev-Mode Rebuild
When running the Scion Hub in development mode (`MaintenanceConfig.RepoPath` is configured in `settings.yaml`), clicking **Update** inside the A2A integration detail page triggers an automatic compilation of the bridge binary directly from your local repository. The Hub will output instructions for you to restart the process manually to apply the new binary.

---

## 4. Configuration

The A2A bridge uses a two-tier configuration model:
1. **Operator-Managed Bootstrap Config (`scion-a2a-bridge.yaml`)**: Low-level infrastructure settings (listen ports, DB paths, and signing keys) that require a process restart.
2. **Admin-Managed Overlay Config (Hot-Reloaded)**: Dynamic operational settings configured directly from the Scion Admin UI that take effect instantly.

### Operator Bootstrap YAML Reference

Below is a complete, annotated example of `scion-a2a-bridge.yaml`:

```yaml
# A2A protocol server settings
bridge:
  # Port and address for the A2A HTTP server (JSON-RPC, agent cards, health check).
  listen_address: ":8443"

  # Public-facing base URL where A2A clients reach this bridge. 
  # Used to construct agent card URLs and self-referencing links.
  external_url: "https://a2a.example.com"

  # Identity included in generated Agent Cards.
  provider:
    organization: "My Organization"
    url: "https://example.com"

# Scion Hub credentials
hub:
  # The HTTP API endpoint of your Scion Hub.
  endpoint: "https://hub.example.com"

  # Hub administrator email the bridge authenticates as for background operations.
  user: "a2a-bridge-admin@example.com"

  # Base64-encoded Hub HS256 signing key file. Used to sign JWTs for Hub API calls.
  # Exactly one of signing_key or signing_key_secret must be set.
  signing_key: "/etc/scion-a2a-bridge/signing-key.b64"

  # Alternatively, retrieve the signing key from GCP Secret Manager:
  # signing_key_secret: "projects/my-gcp-project/secrets/hub-signing-key/versions/latest"

# Broker plugin RPC server (Hub dials this to send agent stream events)
plugin:
  listen_address: "localhost:9090"
  # Set to true to listen on external networks. Requires strict firewall rules!
  allow_remote: false

# Client Authentication
auth:
  # Schemes: "apiKey", "bearer", "hubUAT", "hubJWT", or "none" (not recommended)
  scheme: "apiKey"
  
  # Static key (required only for "apiKey" and "bearer" schemes).
  # Supports environment variable expansion.
  api_key: "${A2A_API_KEY}"

  # UAT validation cache duration for "hubUAT" scheme (default: 60s, max: 300s).
  uat_cache_ttl: 60s

# SQLite State Database
state:
  database: "/var/lib/scion-a2a-bridge/state.db"

# Timeout Configuration
timeouts:
  send_message: 120s      # Maximum wait time for synchronous blocking calls
  sse_keepalive: 30s     # Interval between SSE ping packets
  push_retry_max: 3      # Max delivery retries for webhook push notifications

# Client Request Rate Limiting
rate_limit:
  enabled: true
  requests_per_sec: 10   # Sustained token bucket rate
  burst: 20              # Maximum transient spike capacity
  trust_proxy: false     # Set to true only behind a trusted reverse proxy

# Logging Configuration
logging:
  level: "info"          # debug, info, warn, error
  format: "json"         # "json" for structured logs, "text" for human-readable console
```

:::caution[Literal Dollars in Configuration]
The bridge uses `os.Expand` to replace variables formatted as `${VAR_NAME}` in the YAML file at load time. There is no escaping mechanism. If your static API key or other credentials contain a literal `$` character (e.g., `P@$$w0rd`), the parser will interpret it as an empty environment variable reference and break. Avoid literal `$` characters in values, or inject them entirely via environment variables.
:::

---

## 5. The Admin Overlay & Hot-Reloading

When the A2A bridge connects to the Hub via port `9090`, it registers itself for live configuration updates. 

* **How it works**: Saved changes in the **Admin > Integrations** web UI are serialized by the Hub and pushed instantly to the running bridge via an active RPC stream (the `Configure()` method).
* **Atomic Snapshots**: The bridge takes the updated values, parses and validates them, and builds a new, immutable `ConfigSnapshot` containing the pre-compiled rate limiters, timeouts, and auth validators. This snapshot is swapped atomically into a lock-free memory pointer (`SnapshotHolder`), allowing active connections to read settings without locking overhead.
* **No-Restart Apply**: Settings like rate-limit thresholds, timeouts, and active auth schemes take effect instantly without restarting the bridge process or dropping ongoing client connections.
* **Config Persistence**: To survive process restarts, the bridge automatically persists its current admin settings in a local JSON file named `admin-overlay.json` in its configured `state` database directory. At boot, the bridge loads this file first, applying all user-customized settings before joining the network.

---

## 6. Project & Agent Exposure Controls

By default, the A2A bridge does not expose any Scion projects. Operators must explicitly allow projects and configure which agents within those projects are accessible to A2A clients.

This is managed using the **A2A Projects Editor** on the Integration Detail page in the Admin UI:

```
   ┌─────────────────────────────────────────────────────────────┐
   │ A2A Projects Editor                                         │
   ├─────────────────────────────────────────────────────────────┤
   │ [+] Add Project                                             │
   │                                                             │
   │  ▼ my-awesome-project                               [Delete]│
   │    Exposed Agents: [ research-agent ⓧ ] [ coding-agent ⓧ ] │
   │                    (Leave empty to expose all agents)       │
   │                                                             │
   │    [✔] Auto-Provision New Agents                            │
   │        Default Template: [ standard-dev                 ▼ ] │
   └─────────────────────────────────────────────────────────────┘
```

### Controls & Behaviors

* **Add Project**: Select a project from the dropdown. Only agents belonging to selected projects can be reached via the bridge.
* **Granular Agent Allowlist (`exposed_agents`)**: Enter specific agent slugs. If left empty, all agents in the selected project are exposed to discovery. If populated, only listed agents appear in the project's agent card.
* **Auto-Provisioning (`auto_provision`)**: 
  * If **Disabled** (default): Clients can only connect to pre-existing, running agents in the project.
  * If **Enabled**: When a client sends a request with an unrecognized `contextId` (representing a new conversation), the bridge calls the Hub to automatically provision and start a new agent for that user on demand.
* **Default Template**: Select which template is used when auto-provisioning new agents.

---

## 7. Desktop App Federation

A primary use case for the A2A bridge is connecting local desktop chat applications (such as **Claude Desktop** or **Codex Desktop**) to robust Scion deep agents. 

To secure and isolate desktop client sessions, Scion uses **User Access Token (UAT) Authentication**.

```
  ┌────────────────┐               ┌────────────┐               ┌───────────┐
  │ Claude Desktop │─Bearer Token─&gt;│ A2A Bridge │─Introspect───&gt;│ Scion Hub │
  │ (A2A Client)   │               │  (hubUAT)  │               │(/auth/me) │
  └────────────────┘               └────────────┘               └───────────┘
```

### Setup Walkthrough

#### Step 1: Create a Personal User Access Token
To authorize your desktop application, generate a Scion Personal Access Token scoped to the project you wish to target:

```bash
scion token create \
  --name "claude-desktop-integration" \
  --project "my-coding-project" \
  --scope agent:message,agent:read \
  --expires 365d
```

This command returns a secure token starting with `scion_pat_...`. Copy it immediately; it will not be shown again.

#### Step 2: Configure Your Desktop App
In your desktop app (e.g., Claude Desktop, Codex Desktop) provider or integration settings, configure the connection:

* **Endpoint**: `https://<a2a-bridge-domain>/projects/<project-slug>/agents/<agent-slug>`
* **Authentication Method**: Bearer Token
* **Token**: Paste your generated `scion_pat_...` token.

#### Step 3: Test with curl
Verify that your token and endpoint are correct by querying the agent card:

```bash
curl -s -H "Authorization: Bearer scion_pat_..." \
  https://a2a.example.com/projects/my-coding-project/agents/code-helper/.well-known/agent-card.json
```

Send your first programmatic test message:

```bash
curl -X POST https://a2a.example.com/projects/my-coding-project/agents/code-helper/jsonrpc \
  -H "Authorization: Bearer scion_pat_..." \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": "desktop-test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"kind": "text", "text": "Are you connected?"}]
      }
    }
  }'
```

---

## 8. Per-User Authentication & Isolation

When the A2A bridge is configured with per-user authentication, callers present their own individual credentials instead of a shared static API key. This activates **CallerIdentity context propagation** and granular task isolation.

### The Two Per-User Schemes

The A2A bridge supports two per-user authentication schemes, specified via `auth.scheme` in the bridge configuration:

#### 1. `hubUAT` (Recommended for Desktop App Federation)
* **How it works**: Callers present a Scion User Access Token (`Authorization: Bearer scion_pat_...`) created via the CLI.
* **Validation (`UATValidator`)**: The bridge dynamically introspects each incoming token by calling the Hub's `/api/v1/auth/me` endpoint.
* **SHA-256 Keyed Cache**: To avoid overwhelming the Hub with authentication requests, validated tokens are cached in memory using a SHA-256 hash of the token as the key.
* **Configurable TTL**: The cache TTL is configurable via `auth.uat_cache_ttl` (default: `60s`, maximum: `300s`). If a user revokes their UAT, access is completely cut off once the cache TTL expires (within 60 seconds by default).

#### 2. `hubJWT` (Recommended for CLI & Scripted Access)
* **How it works**: Callers present a Scion-signed User JWT.
* **Local Validation**: The bridge validates the JWT signature locally using the HS256 `hub.signing_key` secret shared with the Hub. Since this happens entirely locally, it requires no active API calls to the Hub, making it extremely fast.

### Per-User Isolation Benefits

* **Task Ownership & Visibility**: Users can only see, query, and cancel/interrupt tasks they created. One user cannot view or modify the active tasks of another user. This is enforced at the SQLite level using a `ScopedTaskStore`.
* **Audit Trails & Attribution**: All downstream Hub API calls made by the bridge (such as sending messages or interrupting containers) propagate the user's actual `CallerIdentity`. The Hub's audit logs will show the real user's identity as the initiator rather than the bridge admin's service account.

---

## 9. API Reference

### HTTP Endpoints

The bridge exposes the following HTTP endpoints:

| Endpoint | Method | Authentication | Description |
| :--- | :--- | :--- | :--- |
| `/.well-known/agent-card.json` | GET | None | Base bridge registry agent card. |
| `/projects/{projectSlug}/agents/{agentSlug}/.well-known/agent-card.json` | GET | Configured Scheme | Per-agent capabilities card. |
| `/projects/{projectSlug}/agents/{agentSlug}/jsonrpc` | POST | Configured Scheme | Primary A2A JSON-RPC 2.0 communication endpoint. |
| `/healthz` | GET | None | Liveness check (returns HTTP 200). |
| `/readyz` | GET | None | Readiness check (checks DB and active broker plugin connection). |
| `/metrics` | GET | Configured Scheme | Prometheus metrics. |

:::note[Backward Compatibility]
To support legacy configurations, the bridge also routes endpoints prefixed with `/groves/` (e.g., `/groves/{projectSlug}/agents/{agentSlug}/jsonrpc`) to the exact same handlers.
:::

---

### Supported JSON-RPC Methods

Standard A2A JSON-RPC methods supported at the `/jsonrpc` endpoint:

| JSON-RPC Method | Description |
| :--- | :--- |
| `message/send` | Send a message to the agent. Supports standard and blocking modes. Returns the agent's completed response. |
| `message/stream` | Send a message and establish an SSE streaming connection. Real-time token updates are pushed over the stream. |
| `tasks/get` | Retrieve detailed status and execution state of a specific task by its unique task ID. |
| `tasks/list` | List all tasks associated with a particular `contextId` (conversation). |
| `tasks/cancel` | Cancel an in-progress agent execution task. |
| `tasks/resubscribe` | Re-attach an active SSE streaming connection to an ongoing task (useful on connection drops). |
| `tasks/pushNotification/set` | Register a webhook callback URL to receive real-time POST alerts on task state changes. |
| `tasks/pushNotification/get` | Retrieve registered webhooks for a specific task. |
| `tasks/pushNotification/delete` | Remove a webhook callback subscription. |

---

## 10. Troubleshooting & Operations

### Health Checks (`/healthz` & `/readyz`)
* **Liveness (`/healthz`)**: Returns `HTTP 200 OK` as long as the HTTP server is listening. Used by container runtimes to detect deadlocks.
* **Readiness (`/readyz`)**: Returns `HTTP 200 OK` only when:
  1. The local SQLite database is reachable and writable.
  2. The Hub's broker plugin has successfully dialed and established its active RPC connection.
  * If either component fails, `/readyz` returns `HTTP 503 Service Unavailable`, preventing traffic from hitting the node.

### SQLite State Database Permissions
The SQLite database stores task metadata and webhook credentials (`auth_credentials`) in plain text.
* **File Permissions**: Ensure the database file (default: `state.db`) is readable and writable *only* by the bridge process user (`chmod 0600`).
* **Docker Container Security**: The official `scion-a2a-bridge` Docker image runs as a non-root user `bridge` (UID `1000`). It expects its state directory at `/var/lib/scion-a2a-bridge/` to be mounted as a persistent volume writable by UID `1000`.

### Security of Broker Plugin RPC (`plugin.allow_remote`)
The broker plugin RPC channel on port `9090` is the pathway through which the Hub pushes agent-emitted logs.
* **No Transport Auth**: The `go-plugin` RPC protocol has **no built-in transport authentication**. By default, it binds only to `localhost` (`127.0.0.1:9090`).
* **Remote Warning**: If you must separate the Hub and A2A bridge across distinct machines/containers and set `plugin.allow_remote: true`, **you must restrict access to port 9090 at the network layer** (e.g., using a security group, firewall rule, or private VPC) so that *only* the Hub's IP can dial the bridge. Anyone who can dial port 9090 can forge agent execution output.

### SSRF Protection in Webhooks
To prevent Server-Side Request Forgery (SSRF) when making push notification callback requests, the bridge:
1. Validates all registered webhook URLs against a private IP blacklist (rejecting loopback, local network, link-local, and multicast ranges).
2. Uses an active connection dialer callback (`ssrfSafeDialer`) that checks the resolved IP *after* DNS resolution to prevent DNS-rebinding attacks.
3. Completely blocks HTTP redirection requests on webhook callbacks to prevent credential leaks.
