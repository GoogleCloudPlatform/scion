---
title: Architecture Deep Dive
description: Internal component interactions of the Scion platform.
---

:::caution[Draft]
This document is a **draft**, current as of **2026-02-11**. The Scion project is in pre-release/alpha and the architecture described here is subject to change.
:::

## Overview

Scion is a container-based orchestration platform for managing concurrent LLM-based code agents. It operates in two distinct modes:

- **Solo Mode** &mdash; A local-only, zero-config experience where the CLI manages agents directly via a local container runtime.
- **Hosted Mode** &mdash; A distributed architecture where a centralized **Hub** coordinates state and dispatches work to one or more **Runtime Brokers** that execute agents on remote or local compute.

Both modes share the same core abstractions (Projects, Agents, Templates, Harnesses, Runtimes) but differ in where state is persisted and how lifecycle operations are routed.

---

## System Architecture Diagram

```d2
direction: right
solo: Solo Mode {
  cli: scion CLI
  manager: Agent Manager
  runtime: Container Runtime {
    tooltip: Docker / Apple / K8s
  }
  agent: Agent Container

  cli -> manager: direct
  manager -> runtime
  runtime -> agent
}

hosted: Hosted Mode {
  cli: scion CLI
  hub: Scion Hub {
    tooltip: API + Store
  }
  web: Web Dashboard
  broker: Runtime Broker {
    tooltip: Agent Manager + Runtime
  }
  runtime: Container Runtime
  agent: Agent Container

  cli -> hub: REST / WS
  hub <-> web
  hub -> broker: HTTP / WebSocket\nControl Channel
  broker -> runtime
  runtime -> agent
}
```

---

## Core Abstractions

### Project

A **Project** is the top-level grouping construct for agents. In Solo mode it is represented by a `.scion` directory on the filesystem; in Hosted mode it is a database record identified by its git remote URL.

**Resolution order (Solo):**
1. Explicit `--project` flag
2. Project-level `.scion` directory (walking up from cwd)
3. Global `~/.scion` directory

**Key properties:**
- **Name**: Slugified from the parent directory containing `.scion`.
- **Git remote** (Hosted): Normalized remote URL used as a unique identifier for cross-broker project identity.
- **Default Runtime Broker** (Hosted): The broker used when creating agents without an explicit target.

Projects contain an `agents/` subdirectory (gitignored) that holds per-agent state, and a `templates/` directory for project-scoped template definitions.

### Agent

An **Agent** is an isolated container running an LLM harness. Each agent has:

| Component | Description |
| :--- | :--- |
| **Home directory** | Mounted at `/home/<user>` inside the container. Contains harness config, credentials, and a per-agent `agent-info.json`. |
| **Workspace** | Mounted at `/workspace`. Typically a dedicated git worktree to prevent merge conflicts between concurrent agents. |
| **Template** | The blueprint that seeded the agent's home directory and configuration. |
| **Harness** | The LLM-specific adapter (Claude, Gemini, OpenCode, Codex, or Generic). |

Agent identity varies by mode:

| Field | Solo Mode | Hosted Mode |
| :--- | :--- | :--- |
| `Name` | User-provided or auto-generated | User-provided or auto-generated |
| `ContainerID` | Assigned by the container runtime | Assigned by the container runtime |
| `ID` | Not used | UUID primary key in the Hub database |
| `Slug` | Not used | URL-safe identifier (unique per project) |

### Template

Templates are configuration blueprints for agents. They define:

- A `home/` directory tree to copy into the agent's home.
- A `scion-agent.json` (or `.yaml`) file specifying harness type, environment variables, volumes, command arguments, model overrides, container image, and resource requirements.

**Template chain**: Templates support inheritance via a `base` field. When resolving a template, Scion walks the chain and merges configurations bottom-up (base first, then overrides).

**Scopes (Hosted):** Templates can be scoped as `global`, `project`, or `user`, with visibility controls (`private`, `project`, `public`).

### Harness

A **Harness** encapsulates LLM-specific behavior behind a common interface (`api.Harness`):

```go
type Harness interface {
    Name() string
    DiscoverAuth(agentHome string) AuthConfig
    GetEnv(agentName, agentHome, unixUsername string, auth AuthConfig) map[string]string
    GetCommand(task string, resume bool, baseArgs []string) []string
    PropagateFiles(homeDir, unixUsername string, auth AuthConfig) error
    GetVolumes(unixUsername string, auth AuthConfig) []VolumeMount
    Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error
    GetInterruptKey() string
    // ... additional methods
}
```

**Supported harnesses:**

| Harness | Target Tool | Notes |
| :--- | :--- | :--- |
| `claude` | Claude Code | Anthropic API key auth |
| `gemini` | Gemini CLI | Google API key / OAuth / Vertex auth |
| `opencode` | OpenCode | OpenCode auth file |
| `codex` | Codex | Codex auth file |
| `generic` | Any CLI tool | Fallback adapter |

The harness factory (`harness.New(name)`) returns the appropriate implementation. Each harness handles:
- **Auth discovery**: Locating credentials on the host.
- **Environment injection**: Mapping credentials to container environment variables.
- **Command construction**: Building the correct CLI invocation (e.g., `claude --no-chrome --dangerously-skip-permissions <task>`).
- **Provisioning hooks**: Harness-specific setup during agent creation (e.g., writing config files).
- **Template seeding**: Populating default template directories from embedded files (`pkg/config/embeds/`).

### Runtime

The **Runtime** interface abstracts container lifecycle operations:

```go
type Runtime interface {
    Name() string
    Run(ctx context.Context, config RunConfig) (string, error)
    Stop(ctx context.Context, id string) error
    Delete(ctx context.Context, id string) error
    List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
    GetLogs(ctx context.Context, id string) (string, error)
    Attach(ctx context.Context, id string) error
    ImageExists(ctx context.Context, image string) (bool, error)
    PullImage(ctx context.Context, image string) error
    Sync(ctx context.Context, id string, direction SyncDirection) error
    Exec(ctx context.Context, id string, cmd []string) (string, error)
    GetWorkspacePath(ctx context.Context, id string) (string, error)
}
```

**Implementations:**

| Runtime | Platform | Selection |
| :--- | :--- | :--- |
| `AppleContainerRuntime` | macOS | Auto-detected when Apple `container` CLI is present |
| `DockerRuntime` | Linux / macOS / Windows | Default fallback; supports remote Docker hosts via `Host` config |
| `PodmanRuntime` | Linux / macOS | Daemonless/Rootless alternative; supports remote/machine execution |
| `KubernetesRuntime` | Any (via kubeconfig) | Runs agents as Kubernetes Pods; supports namespace isolation, resource specs, and workspace sync via `tar` snapshots |

**Runtime selection** is handled by the `GetRuntime` factory function, which resolves the runtime based on:
1. The active profile's `runtime` field in `settings.yaml`.
2. OS-level auto-detection (macOS with `container` CLI &rarr; Apple; Linux &rarr; Podman if available, else Docker).
3. Explicit override via CLI flags.

---

## Package Architecture

The Go codebase is organized into the following packages:

```
pkg/
├── api/             # Shared types: AgentInfo, ScionConfig, Harness, AuthConfig, etc.
├── agent/           # Agent lifecycle: Manager interface, provisioning, run, delete
├── agentcache/      # In-memory agent state caching
├── config/          # Settings, template resolution, path management, embeds
│   └── embeds/      # Embedded template files (go:embed)
├── harness/         # LLM-specific adapters (claude, gemini, opencode, codex, generic)
├── runtime/         # Container runtime abstraction (Docker, Apple, K8s)
├── hub/             # Hub API server: handlers, auth, control channel, metrics
├── hubclient/       # Go client library for the Hub REST API
├── hubsync/         # CLI-to-Hub synchronization logic
├── runtimebroker/   # Runtime Broker API server: handlers, heartbeat, auth
├── brokercredentials/ # Broker HMAC credential management
├── store/           # Persistence interface and models
│   └── sqlite/      # SQLite implementation of the Store interface
├── storage/         # Cloud object storage abstraction (GCS)
├── templatecache/   # Template download and caching for Runtime Brokers
├── transfer/        # Workspace transfer utilities (upload/download via GCS)
├── wsprotocol/      # WebSocket message types for Hub ↔ Broker communication
├── wsclient/        # WebSocket client utilities
├── k8s/             # Kubernetes client wrapper
├── credentials/     # Host credential discovery
├── daemon/          # Background daemon support
├── gcp/             # GCP-specific utilities
├── sciontool/       # Internal CLI status tool (used by agents)
├── util/            # Shared utilities (git, env expansion, file ops)
└── version/         # Build version info
```

### Dependency Flow

The dependency graph flows strictly downward:

```d2
cmd: cmd/ {
  tooltip: CLI commands - Cobra
}

agent: pkg/agent {
  tooltip: orchestration
}

config: pkg/config {
  tooltip: settings, templates, paths
}

harness: pkg/harness {
  tooltip: LLM adapters
}

runtime: pkg/runtime {
  tooltip: container lifecycle
}

api: pkg/api {
  tooltip: shared types
}

cmd -> agent
agent -> config
agent -> harness
agent -> runtime
runtime -> api
```

Hub and Runtime Broker servers have their own entry points but reuse the same `agent`, `runtime`, and `config` packages.

---

## Agent Lifecycle

### Solo Mode

```
scion start <name> --task "..." [--template claude] [--profile docker-local]
```

1. **Project resolution**: `config.GetResolvedProjectDir()` locates the `.scion` directory.
2. **Settings loading**: `config.LoadSettings()` reads `settings.yaml` from the project, merging with environment variable overrides.
3. **Provisioning** (`agent.ProvisionAgent`):
   a. Creates `agents/<name>/home/` and `agents/<name>/workspace/` directories.
   b. Resolves the template chain and copies home directory contents.
   c. Merges configuration: `template base → template → settings (harness/profile) → agent overrides`.
   d. Creates a git worktree at `agents/<name>/workspace/` on a new branch (slugified agent name).
   e. Runs harness-specific provisioning (`harness.Provision()`).
   f. Writes `scion-agent.json` and `agent-info.json`.
4. **Image resolution**: Resolves the container image from settings/template/CLI override. Pulls if not present.
5. **Container launch** (`runtime.Run`):
   a. Builds container run arguments (volumes, env vars, labels, resource limits).
   b. Mounts the agent home at `/home/<user>` and workspace at `/workspace`.
   c. If tmux is enabled, wraps the harness command in a tmux session named `scion`.
   d. Launches the container in detached mode.
6. **Status update**: Writes `agent-info.json` with status `running`.

### Hosted Mode

```
scion start <name> --task "..." --hub
```

1. **Hub sync**: The CLI registers/syncs the project with the Hub if not already registered.
2. **API call**: The CLI sends a `POST /api/v1/projects/{projectId}/agents` request to the Hub.
3. **Broker selection**: The Hub selects a Runtime Broker (explicit or project default).
4. **Environment resolution**: The Hub merges environment variables and secrets from all applicable scopes (user → project → broker).
5. **Template hydration**: The Hub resolves the template, attaches its content hash for broker-side caching.
6. **Dispatch**: The Hub dispatches the creation request to the selected Runtime Broker via:
   - **Direct HTTP** if the broker has a reachable endpoint.
   - **Control Channel** (WebSocket tunnel) if the broker is behind a NAT/firewall.
7. **Broker execution**: The Runtime Broker provisions and starts the agent using the same `agent.Manager` and `runtime.Runtime` code path as Solo mode.
8. **Status reporting**: The broker reports status back to the Hub via heartbeats and agent status updates.

---

## Hub Server

The Hub (`pkg/hub`) is a stateful API server that provides centralized management for the distributed architecture.

### Components

| Component | Responsibility |
| :--- | :--- |
| `Server` | HTTP server, route registration, middleware stack |
| `Store` | Persistence interface (currently backed by SQLite) |
| `ControlChannelManager` | Manages WebSocket connections from Runtime Brokers |
| `HTTPDispatcher` | Forwards agent lifecycle requests to brokers via HTTP |
| `Metrics` | Runtime metrics collection (agent counts, broker health) |
| `AuthMiddleware` | JWT-based user auth, dev auth, broker HMAC auth |

### API Surface

The Hub exposes a RESTful API under `/api/v1/`:

| Resource | Endpoints |
| :--- | :--- |
| **Agents** | `GET/POST /agents`, `GET/PUT/DELETE /agents/{id}`, `POST /agents/{id}/{action}` |
| **Projects** | `GET/POST /projects`, `POST /projects/register`, `GET/PUT/DELETE /projects/{id}`, nested agent/env/secret routes |
| **Runtime Brokers** | `GET/POST /runtime-brokers`, `GET/PUT/DELETE /runtime-brokers/{id}`, heartbeat, control channel |
| **Templates** | `GET/POST /templates`, `GET/PUT/DELETE /templates/{id}` |
| **Users** | `GET/POST /users`, `GET/PUT/DELETE /users/{id}` |
| **Auth** | Login, token, refresh, validate, logout, CLI OAuth, PATs |
| **Env Vars / Secrets** | CRUD for scoped environment variables and encrypted secrets |
| **Groups / Policies** | RBAC: groups with nested membership, policies with conditional bindings |

### Authentication

The Hub supports multiple authentication methods:

| Method | Use Case |
| :--- | :--- |
| **OAuth** | Production user authentication via external identity providers |
| **Dev Auth** | Development shortcut using a static token |
| **JWT (User)** | Issued after login; used for API calls |
| **JWT (Agent)** | Scoped tokens issued to agents for Hub API access from within containers |
| **PAT** | Programmatic access with `scion_pat_` prefixed tokens |
| **HMAC** | Runtime Broker authentication using shared secrets |

### Persistence (Store)

The `Store` interface (`pkg/store/store.go`) defines a comprehensive persistence contract composed of sub-interfaces:

- `AgentStore` &mdash; CRUD + status updates with optimistic locking (`StateVersion`)
- `ProjectStore` &mdash; CRUD + lookup by slug, git remote
- `RuntimeBrokerStore` &mdash; CRUD + heartbeat updates
- `TemplateStore` &mdash; CRUD with scope and harness filtering
- `UserStore` &mdash; CRUD with role and status filtering
- `ProjectProviderStore` &mdash; Project-to-broker relationship management
- `EnvVarStore` / `SecretStore` &mdash; Scoped key-value storage (encrypted for secrets)
- `GroupStore` / `PolicyStore` &mdash; RBAC with nested group support and policy bindings
- `APIKeyStore` / `BrokerSecretStore` &mdash; Authentication credential management

The current implementation uses **SQLite** (`pkg/store/sqlite/`). The interface is designed to support alternative backends (PostgreSQL, Firestore, etc.).

---

## Runtime Broker

The Runtime Broker (`pkg/runtimebroker`) is a compute node that executes agents on behalf of the Hub.

### Responsibilities

- Exposes a REST API for agent lifecycle operations (create, start, stop, delete, list, message, exec).
- Manages a local `agent.Manager` backed by a `runtime.Runtime`.
- Reports health via periodic **heartbeats** to the Hub.
- Maintains a **WebSocket control channel** for NAT traversal (the Hub tunnels HTTP requests through the WebSocket when direct connectivity is unavailable).
- Caches templates locally via `templatecache` to avoid repeated downloads.
- Authenticates Hub requests using HMAC shared secrets.
- Supports dynamic credential reload (watches for credential file changes).

### Communication with the Hub

```d2
hub: Hub
broker: Runtime Broker

hub -> broker: HTTP (direct) {
  tooltip: when broker is reachable
}
hub <-> broker: WebSocket {
  tooltip: control channel, always
}
```

The control channel uses a custom WebSocket protocol (`pkg/wsprotocol`) with the following message types:

| Type | Direction | Purpose |
| :--- | :--- | :--- |
| `connect` | Broker → Hub | Initiate connection with broker metadata |
| `connected` | Hub → Broker | Confirm connection |
| `request` | Hub → Broker | Tunnel an HTTP request through the WebSocket |
| `response` | Broker → Hub | Return the HTTP response |
| `stream_open/close` | Bidirectional | Open/close streams (PTY, logs, events) |
| `event` | Broker → Hub | Async events (heartbeat, agent status) |
| `ping/pong` | Bidirectional | Keepalive |

### Broker Registration Flow

1. Admin creates a broker record in the Hub: `POST /api/v1/runtime-brokers`.
2. Hub generates a short-lived **join token**: `POST /api/v1/brokers/join`.
3. Broker uses the join token to obtain HMAC credentials: `POST /api/v1/brokers/join` (with token).
4. Broker stores credentials locally (`~/.scion/broker-credentials.json`).
5. Broker authenticates subsequent requests using HMAC-SHA256 signatures.

---

## Configuration System

Configuration is managed by `pkg/config` and uses a layered resolution strategy.

### Settings File

Located at `.scion/settings.yaml` (YAML preferred) or `.scion/settings.json` (JSONC). Key sections:

```yaml
active_profile: docker-local
default_template: claude

hub:
  enabled: true
  endpoint: https://hub.example.com
  projectId: "uuid__slug"

runtimes:
  docker:
    host: ""          # Remote Docker host (optional)
    tmux: true
  kubernetes:
    namespace: scion-agents
    sync: tar

harnesses:
  claude:
    image: claude-code-sandbox:latest
    user: scion
  gemini:
    image: gemini-cli-sandbox:latest
    user: gemini

profiles:
  docker-local:
    runtime: docker
    resources:
      requests: { cpu: "500m", memory: "512Mi" }
    harness_overrides:
      claude:
        image: claude-code-sandbox:tmux
```

### Resolution Priority

Configuration values are resolved in this order (highest priority wins):

1. **CLI flags** (e.g., `--image`, `--profile`)
2. **Agent-level** `scion-agent.json` (per-agent overrides)
3. **Template chain** (merged bottom-up from base to leaf)
4. **Settings file** (profile → harness → runtime)
5. **Environment variables** (`SCION_` prefix for settings overrides; `$VAR` substitution in values)
6. **Embedded defaults** (`pkg/config/embeds/default_settings.yaml`)

### Workspace Strategy

Scion uses **git worktrees** for workspace isolation:

1. When an agent starts in a git repository, a worktree is created at `agents/<name>/workspace/` on a new branch.
2. The branch name defaults to the slugified agent name.
3. If a branch already exists and has a worktree, Scion reuses it (with a warning).
4. On deletion, the worktree and optionally the branch are cleaned up.
5. For non-git projects or explicit `--workspace` paths, the directory is bind-mounted directly.

---

## Web Frontend

The web dashboard (`web/`) provides a visual interface for Hosted mode operations.

| Layer | Technology | Location |
| :--- | :--- | :--- |
| Client SPA | Lit + TypeScript + Vite | `web/src/client/` |
| Server | Go (consolidated into the `scion` binary) | `pkg/hub/web.go` |

The server layer (enabled via `--enable-web`) provides:
- Static asset serving and SPA shell rendering.
- OAuth authentication and session management.
- SSE real-time event streaming via `pkg/hub/events.go`.
- API routing to the Hub.

---

## Security Model

### Container Isolation

Each agent runs in its own container with:
- A dedicated home directory (no shared state between agents).
- An isolated git worktree (no merge conflicts).
- Environment variables injected at container creation time (credentials are not written to disk in the project).

### Credential Flow

```d2
host: Host
cli: CLI {
  tooltip: discovers credentials (env vars, config files)
}
harness: Harness.DiscoverAuth()\nAuthProvider.GetAuthConfig()
container: Container {
  tooltip: Available as \$ANTHROPIC_API_KEY, etc.
}

host -> cli
cli -> harness
harness -> container: Injected as env vars\nat launch time
```

In Hosted mode, credentials can also be:
- Stored as encrypted **secrets** in the Hub (scoped to user, project, or broker).
- Resolved and injected by the Hub at dispatch time (`ResolvedEnv` in the create request).

### Project Security

When a project lives inside a git repository, Scion **requires** that `agents/` is listed in `.gitignore` to prevent accidental credential or state leakage:

```
security error: '<path>/agents/' must be in .gitignore when using a project-local project
```

### Hub Authentication Architecture

```d2
User -> Hub API: OAuth/DevAuth -> JWT (User Token)
Agent -> Hub API: JWT (Agent Token, scoped)
Broker -> Hub API: HMAC-SHA256 Signed Request
CLI -> Hub API: PAT (scion_pat_...)
```

---

## Data Flow: Agent Creation (Hosted)

The following sequence traces a complete agent creation through the Hosted architecture:

```d2
shape: sequence_diagram
CLI: CLI
Hub: Hub
Broker: Runtime Broker

CLI -> Hub: "POST /projects/{id}/agents"
Hub -> Hub: 1. Validate auth\n2. Resolve template\n3. Select broker\n4. Resolve env/secrets\n5. Create agent record (provisioning)
Hub -> Broker: POST /api/v1/agents\n(HTTP or via WS tunnel)
Broker -> Broker: 6. Hydrate template\n7. ProvisionAgent()\n8. Runtime.Run()
Broker -> Hub: AgentResponse
Hub -> Hub: 9. Update agent record (running)
Hub -> CLI: AgentInfo
Broker -> Hub: heartbeat (periodic)
```

---

## Observability

### Agent Status

Agents write status to a file inside the container (e.g., `/home/<user>/.gemini-status.json` for Gemini) which is read by the CLI:

| Status | Meaning |
| :--- | :--- |
| `STARTING` | Container is initializing |
| `THINKING` | LLM is processing |
| `EXECUTING` | Agent is running a tool/command |
| `WAITING_FOR_INPUT` | Human-in-the-loop required (`scion attach`) |
| `COMPLETED` | Task finished |
| `ERROR` | Unrecoverable failure |

### Hub Metrics

The Hub exposes a `/metrics` endpoint with runtime statistics:
- Connected broker count
- Active agent count
- Project count

### Logging

Both the Hub and Runtime Broker use structured logging via Go's `slog` package, with support for trace ID propagation (`X-Cloud-Trace-Context`).

---

## Key Design Decisions

### Shared Code Path

Solo and Hosted modes share the same `agent.Manager`, `runtime.Runtime`, and `harness.Harness` implementations. The Runtime Broker does not contain a separate agent management stack; it wraps the same `AgentManager` that the CLI uses locally.

### WebSocket Control Channel

The control channel enables the Hub to dispatch operations to brokers behind NATs or firewalls without requiring inbound connectivity. The broker initiates the WebSocket connection and the Hub tunnels HTTP requests through it.

### Template Inheritance

Templates use a chain-based merge strategy rather than flat overrides. This allows organizations to define a base template (e.g., common `.bashrc`, shared tooling) and layer harness-specific or project-specific customizations on top.

### Optimistic Locking

Agent records in the Hub use a `StateVersion` field for optimistic concurrency control. Updates that don't match the expected version are rejected with `ErrVersionConflict`, preventing lost updates from concurrent broker status reports.

### Filesystem as Source of Truth (Solo)

In Solo mode, the filesystem (`agents/<name>/scion-agent.json`, `agent-info.json`) is the only source of truth. There is no local database. This keeps Solo mode truly zero-config.
