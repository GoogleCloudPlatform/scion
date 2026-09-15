# ManagedAgent Design Document (Option B)

## 1. Overview

**Problem statement.** Scion currently runs AI coding agents exclusively in containers (Docker, Podman, Kubernetes, Apple Container, Cloud Run). Each container bundles a Runtime (container lifecycle) and a Harness (LLM CLI like Claude Code or Gemini CLI). A growing class of cloud services offer fully-managed agent execution where the model, tools, sandbox, and orchestration loop are handled server-side. Scion needs to support these managed backends so that `scion start`, `scion message`, `scion stop`, and other commands work seamlessly whether the agent runs in a local container or a remote cloud service.

**Design decision.** Option B: introduce `ManagedAgent` as a peer concept to the existing Runtime+Harness stack, not as a replacement. The Hub agent handlers are the live branching point: managed profiles dispatch directly to a `ManagedAgentBackend`, while container agents continue through the Runtime Broker. Container code is untouched.

**First backend.** Google Managed Agents API (Gemini API) at `generativelanguage.googleapis.com`. The Antigravity agent is the base agent.

**Scope.** This design covers the v1 managed agent integration: start, message, stop, delete, list, look, attach, and logs for managed agents. v1 targets repo-less use cases (research, exploration, standalone tasks). Repo-aware use cases (workspace sync, worktree branching) are deferred to v2.

**Future.** Option C (unified `ExecutionBackend` abstraction collapsing Runtime+Harness+ManagedAgent into one interface) will be pursued once a second managed-agent backend validates the abstraction shape.

---

## 2. Architecture

### 2.1 How ManagedAgent fits alongside Runtime+Harness

The current dispatch chain for container agents:

```
Template (scion-agent.yaml)
  -> AgentManager (implements Manager)
     -> Runtime (docker/podman/k8s/apple/cloudrun)
     -> Harness (claude/gemini/generic)
```

The live dispatch chain for managed agents:

```
Agent create request (profile: managed-agents)
  -> Hub agent handlers
     -> ManagedAgentBackend (interface)
        -> GoogleManagedAgentBackend (first impl)
```

The Hub detects the managed profile before broker dispatch and handles that lifecycle directly. Container agents continue through the existing `Manager` and Runtime Broker path.

### 2.2 Package layout

```
pkg/
  agent/
    manager.go              # Manager interface (UNCHANGED)
    run.go                  # AgentManager.Start (UNCHANGED)
  hub/
    handlers_managed_agents.go # Hub-direct managed backend selection and lifecycle
  managedagent/
    backend.go              # NEW: ManagedAgentBackend interface
    types.go                # NEW: Backend-agnostic types (InteractionStream, etc.)
    google/
      client.go             # NEW: Google API HTTP client
      backend.go            # NEW: GoogleManagedAgentBackend (implements ManagedAgentBackend)
      types.go              # NEW: Google API request/response types
      sse.go                # NEW: SSE event stream parser
  api/
    types.go                # MODIFIED: add ServiceConfig to ScionConfig
  config/
    templates.go            # MODIFIED: add service: field to template validation
```

### 2.3 Interface definitions

**ManagedAgentBackend** (new, in `pkg/managedagent/backend.go`):

```go
package managedagent

import (
    "context"
    "io"
)

// ManagedAgentBackend is the cloud-provider abstraction for managed agent services.
// Each backend (Google, LangChain, etc.) implements this interface.
type ManagedAgentBackend interface {
    // Name returns the backend identifier (e.g., "google", "langchain").
    Name() string

    // CreateAgent creates a persistent agent configuration on the cloud service.
    // Returns the cloud-assigned agent ID.
    CreateAgent(ctx context.Context, cfg CreateAgentConfig) (string, error)

    // DeleteAgent removes the agent configuration from the cloud service.
    DeleteAgent(ctx context.Context, cloudAgentID string) error

    // CreateInteraction starts a new interaction (turn) with the agent.
    // Returns an InteractionHandle for streaming results and tracking state.
    CreateInteraction(ctx context.Context, req InteractionRequest) (*InteractionHandle, error)

    // GetInteraction retrieves the current state of an interaction.
    GetInteraction(ctx context.Context, interactionID string) (*InteractionState, error)

    // CancelInteraction cancels a running interaction.
    CancelInteraction(ctx context.Context, interactionID string) error

    // StreamInteraction opens an SSE stream for a running or completed interaction.
    // If lastEventID is non-empty, resumes from that point.
    StreamInteraction(ctx context.Context, interactionID string, lastEventID string) (io.ReadCloser, error)
}

// CreateAgentConfig contains the parameters for creating a managed agent.
type CreateAgentConfig struct {
    ID                string            // Desired agent ID
    BaseAgent         string            // e.g., "antigravity-preview-05-2026"
    SystemInstruction string            // System prompt
    Description       string            // Human-readable description
    Tools             []ToolConfig      // Tool definitions
    Environment       *EnvironmentConfig // Base environment configuration
}

// InteractionRequest contains the parameters for creating an interaction.
type InteractionRequest struct {
    CloudAgentID          string              // Cloud-side agent ID
    Input                 string              // User message
    PreviousInteractionID string              // For multi-turn continuation
    EnvironmentID         string              // Reuse existing environment (empty = new)
    Environment           *EnvironmentConfig  // Environment config (if creating new)
    Stream                bool                // Enable SSE streaming
    Background            bool                // Run asynchronously
    Tools                 []ToolConfig        // Per-interaction tool overrides
    SystemInstruction     string              // Per-interaction system prompt override
}

// InteractionHandle represents a running or completed interaction.
type InteractionHandle struct {
    InteractionID string
    EnvironmentID string
    Status        InteractionStatus
    Steps         []Step
    OutputText    string
    Usage         *UsageInfo
    EventStream   io.ReadCloser // Non-nil when streaming
}

// InteractionState is the polled state of an interaction.
type InteractionState struct {
    InteractionID string
    Status        InteractionStatus
    Steps         []Step
    OutputText    string
    EnvironmentID string
    Usage         *UsageInfo
}

type InteractionStatus string

const (
    StatusInProgress     InteractionStatus = "in_progress"
    StatusRequiresAction InteractionStatus = "requires_action"
    StatusCompleted      InteractionStatus = "completed"
    StatusFailed         InteractionStatus = "failed"
    StatusCancelled      InteractionStatus = "cancelled"
    StatusIncomplete     InteractionStatus = "incomplete"
)

type Step struct {
    Type      string // "user_input", "model_output", "thought", "function_call", etc.
    Text      string
    Arguments string // For function_call steps
    ToolName  string // For function_call steps
}

type ToolConfig struct {
    Type       string                 // "code_execution", "google_search", "function", "mcp_server", "url_context"
    Name       string                 // For function tools
    Parameters map[string]interface{} // Tool-specific config
}

type EnvironmentConfig struct {
    Type    string         // "remote"
    Sources []SourceConfig // Git repos, GCS, inline content
    Network *NetworkConfig // Egress rules
}

type SourceConfig struct {
    Type   string // "repository", "gcs", "inline"
    URI    string
    Branch string
    Path   string
}

type NetworkConfig struct {
    Disabled  bool
    Allowlist []AllowlistEntry
}

type AllowlistEntry struct {
    Domain  string
    Headers map[string]string
}

type UsageInfo struct {
    TotalInputTokens  int
    TotalOutputTokens int
    TotalTokens       int
}
```

### 2.4 How execution branches

The Hub branches before broker dispatch. `handlers_agents_core.go` recognizes the
`managed-agents` profile and calls `managedAgentCreate`; the implementation in
`handlers_managed_agents.go` lazily constructs the configured backend and handles
managed lifecycle operations directly. All other profiles continue through the
ordinary Runtime Broker path.

---

## 3. Template Schema — Execution-Agnostic

### 3.1 Templates do NOT declare execution mode

**Key decision (Q7):** Templates define *what* an agent does, not *where/how* it runs. There is no `service:` field in the template YAML. The managed-vs-container decision is a **broker profile** selected at agent creation time (analogous to choosing Cloud Run vs GKE).

Templates remain unchanged from the existing schema. The same template can run on a container runtime or a managed agent service depending on the broker profile selected at `scion start` time.

### 3.2 Managed agent configuration lives in Scion settings

Managed agent backend configuration (provider, base agent model, API key) lives in Scion settings, not templates:

```yaml
# In Scion settings (not template YAML)
managed_agents:
  google:
    api_key: "<key>"           # Or resolved from Hub secrets
    base_agent: "antigravity-preview-05-2026"
    environment:
      sources:
        - type: repository
          uri: https://github.com/example/repo.git
          branch: main
      network:
        allowlist:
          - domain: api.github.com
```

### 3.3 Broker profile selection

The execution mode is selected as a broker profile during agent creation. The profile determines whether the agent runs in a container or on a managed service:

- `cloud-run` — container on Cloud Run
- `gke` — container on GKE
- `managed-agents` — Google Managed Agents API (Gemini)

The template is the same regardless of profile.

### 3.4 Example template YAML

A template that works on any execution backend:

```yaml
# scion-agent.yaml — execution-agnostic
agent_instructions: |
  You are a research assistant. Focus on analyzing code quality.
system_prompt: instructions.md
task: "Analyze the repository structure and produce a report."
max_duration: 30m
```

---

## 4. Agent Lifecycle

### 4.1 Start flow for managed agents

When an agent create request selects the `managed-agents` profile:

1. The Hub performs the normal request validation, authorization, and Agent record creation.
2. `handlers_agents_core.go` selects `managedAgentCreate` before broker dispatch.
3. `getManagedBackend` loads `managed_agents.google` from global settings and lazily constructs the backend.
4. The Hub sets the runtime to `managed:<provider>` and records the provider in Agent annotations.
5. If the request includes a task, the Hub creates the first interaction and records its interaction and environment IDs in the same Agent annotations.
6. The Hub persists the Agent record. No local agent directory or Runtime Broker is involved.

### 4.2 Message delivery flow

1. The Hub loads the Agent record and selects `managedAgentMessage` from its `managed:` runtime.
2. For interrupting messages, the current interaction is cancelled best-effort.
3. The Hub creates a new interaction using the previous interaction and environment IDs from Agent annotations.
4. The returned interaction and environment IDs replace those annotations and are persisted through the Agent store.

### 4.3 Status monitoring and mapping

`scion look` resolves the latest interaction ID from the stored Agent annotations,
fetches that interaction from the backend, and formats its steps and token usage.
Lifecycle phase remains Hub-owned Agent state.

### 4.4 Stop/Delete flows

**Stop**: Cancel the active interaction if it is still in progress, then update the Hub Agent phase to stopped. The remote environment remains reusable.

**Delete**: Stop best-effort, then continue through the Hub's normal Agent deletion path. There is no local managed-agent state directory to remove.

### 4.5 CLI command behavior for managed agents

**`scion look`**: Fetch latest interaction via `Backend.GetInteraction()`, format steps as structured text output with timestamps (decision Q6). Primary consumer is agent-to-agent observation:
```
[14:05:02] [thought] Analyzing the repository structure...
[14:05:08] [code_execution] $ find . -name "*.go" | head -20
[14:05:12] [model_output] The repository contains 45 Go source files...
[14:05:12] [status] completed (3,200 input tokens, 1,800 output tokens)
```

**`scion attach`**: Not supported for managed agents in v1 (decision Q2). Returns error directing user to `scion message` and `scion look`.

**`scion logs`**: Reads from GCP Cloud Logging (decision Q4). No local log files.

---

## 5. Google Backend Implementation

### 5.1 API client package design

`pkg/managedagent/google/client.go` -- thin HTTP client wrapping the Gemini API REST surface. Uses `net/http` with JSON marshaling directly (no official Go SDK dependency to avoid gRPC/proto transitive deps).

```go
package google

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

type Client struct {
    baseURL    string
    apiKey     string
    httpClient *http.Client
}

func NewClient(apiKey string) *Client { ... }

// Agent CRUD
func (c *Client) CreateAgent(ctx, *CreateAgentRequest) (*Agent, error) { ... }
func (c *Client) GetAgent(ctx, agentID string) (*Agent, error) { ... }
func (c *Client) DeleteAgent(ctx, agentID string) error { ... }
func (c *Client) ListAgents(ctx, pageSize int, pageToken string) (*ListAgentsResponse, error) { ... }

// Interaction lifecycle
func (c *Client) CreateInteraction(ctx, *CreateInteractionRequest) (*Interaction, error) { ... }
func (c *Client) CreateInteractionStream(ctx, *CreateInteractionRequest) (io.ReadCloser, error) { ... }
func (c *Client) GetInteraction(ctx, interactionID string) (*Interaction, error) { ... }
func (c *Client) CancelInteraction(ctx, interactionID string) error { ... }
func (c *Client) DeleteInteraction(ctx, interactionID string) error { ... }
```

Auth: `apiKey` set as `x-goog-api-key` header on every request.

### 5.2 Environment lifecycle

Environments are NOT a separate CRUD resource. They are created implicitly when an interaction uses `environment: "remote"` and reused by passing the returned `environment_id`. The Hub tracks the latest environment ID in the Agent record's annotations.

Google API lifecycle: auto-snapshot after ~15min idle, retained 7 days, then deleted. Scion does not manage this.

### 5.3 Auth handling

- API key lives in Scion settings (`managed_agents.google.api_key`) — NOT in templates (decision Q1)
- Resolution chain: Scion settings > Hub secret > environment variable
- No template-level credential configuration; templates are execution-agnostic

### 5.4 Multi-turn state tracking

Two independent chains:
- **Conversation**: `previous_interaction_id` links context
- **Environment**: `environment_id` links sandbox state

The Hub persists only the current provider state on the Agent record; it does
not maintain a separate local interaction-chain document:

```json
{
  "runtime": "managed:google",
  "annotations": {
    "scion.dev/cloud-provider": "google",
    "scion.dev/interaction-id": "int_xyz789",
    "scion.dev/environment-id": "env_def456"
  }
}
```

### 5.5 Function calling — NOT APPLICABLE

Managed agents are a pure server-side API (decision Q3). The cloud agent executes its own tools (code execution, search, etc.) internally within its sandbox. There is no client-side function execution. If the API ever surfaces a `requires_action` status, treat it as an unexpected state and log it.

---

## 6. Hub-Direct Integration

### 6.1 No broker routing for managed agents (Decision Q5)

Managed agent API calls go directly from the Hub to the cloud API, bypassing brokers entirely. Managed agents are a stateless API with no container lifecycle — the broker layer would be a pass-through adding latency with no value.

### 6.2 Hub dispatch

Flow:
1. Hub receives `POST /agents` from CLI/API with broker profile = `managed-agents`
2. Hub handles directly via its managed agent client interface (no broker delegation)
3. Hub calls cloud API (CreateAgent, CreateInteraction, etc.)
4. Hub tracks agent state and reports status to CLI

Reference: the `cloudrun-ha` branch on origin implements a similar hub-direct client interface pattern. Look for reusable patterns to factor out (e.g., client interface abstraction, error handling, retry logic).

### 6.3 Broker profile selection

Managed agents are selected as a broker profile during agent creation — analogous to choosing Cloud Run or GKE (decision Q7). The profile is a runtime/deployment decision, not a template-level declaration.

---

## 7. State Storage

### 7.1 Hub Agent record

Managed agents use the same Hub Agent store as container agents. Provider state is
kept in the record rather than a broker-local directory:

- `runtime`: `managed:<provider>`
- `scion.dev/cloud-provider` annotation: backend name
- `scion.dev/interaction-id` annotation: latest interaction
- `scion.dev/environment-id` annotation: reusable remote environment

### 7.2 Agent fields

| Field | Container Agent | Managed Agent |
|---|---|---|
| `ContainerID` | Docker/k8s ID | Empty |
| `Runtime` | "docker" / "podman" / "kubernetes" | "managed:google" |
| `Phase` | From container + sciontool hooks | Hub lifecycle state |
| `Activity` | From `.scion-status.json` | Hub lifecycle state |
| `Image` | Container image | Empty |

---

## 8. CLI Behavior Matrix

| Command | Container Agent | Managed Agent |
|---|---|---|
| `scion start <name> [task]` | Provision + container launch | Create cloud agent + first interaction |
| `scion create <name> [task]` | Provision only (no container) | Create cloud agent (no interaction) |
| `scion stop <name>` | Stop container | Cancel active interaction |
| `scion delete <name>` | Delete container + files | Stop interaction + delete Hub Agent record |
| `scion list` | Merge container + on-disk state | Read managed agents from the Hub Agent store |
| `scion message <name> <msg>` | tmux paste-buffer + send-keys | Create new interaction with message |
| `scion message --raw <name>` | tmux send-keys (control chars) | Error: "not supported for managed agents" |
| `scion message --broadcast` | Fan-out tmux delivery | Fan-out interaction creation |
| `scion look <name>` | tmux capture-pane | Fetch latest interaction, format as text |
| `scion attach <name>` | tmux attach-session | Error: "not supported for managed agents — use scion message and scion look" (deferred, Q2) |
| `scion logs <name>` | Read agent.log from container | Read from GCP Cloud Logging (Q4) |
| `scion resume <name>` | Restart container with --continue | New interaction with environment reuse |
| `scion sync <name>` | Workspace file sync | Not applicable (v1) |
| `scion suspend <name>` | Stop container, preserve state | Cancel interaction, preserve cloud agent |

---

## 9. Resolved Design Decisions

### Q1: API key storage — DECIDED: Scion settings only

API key lives in Scion settings (`managed_agents.google.api_key`), NOT in the template YAML. This is an infrastructure/credential concern (analogous to harness-config), not a template-level concern. Templates remain portable and credential-free.

### Q2: `scion attach` — DECIDED: Deferred for v1

`scion attach` is not supported for managed agents in v1. There is no SSH or tmux session to attach to. The primary UX is `scion message` + `scion look`. Both `message` and `look` currently use tmux internally for container agents — for managed agents, these are reimplemented as direct API calls (Message → create interaction, Look → fetch latest interaction). A web-based REPL-like interface is a potential future addition.

### Q3: Function calling — DECIDED: Not applicable

Managed agents are a pure server-side API. The cloud agent has its own sandbox and executes tools internally (code execution, search, etc.). There is no client-side function execution in this model. `requires_action` for local tool dispatch does not apply and is removed from the design. If the API ever surfaces a `requires_action` status, treat it as an unexpected state and log it.

### Q4: Logging — DECIDED: GCP Cloud Logging

Logging goes through GCP Cloud Logging with its own configurable retention policies. No local log files on hub or broker — avoids introducing statefulness. `scion logs` reads from Cloud Logging on demand.

### Q5: Broker routing — DECIDED: Hub-direct

Managed agent API calls go directly from the Hub to the cloud API, bypassing brokers entirely. Managed agents are a stateless API with no container lifecycle to manage — the broker layer would be a pass-through adding latency with no value. Managed agents are selected as a **broker profile** (like choosing Cloud Run or GKE) during agent creation. Reference: `cloudrun-ha` branch on origin has a similar hub-direct client interface pattern — look for reusable patterns to factor out.

### Q6: `scion look` output format — DECIDED: Structured with timestamps

Structured, readable format with timestamps on each step. Primary consumer is agent-to-agent observation (one agent understanding what another is doing). Timestamps are critical for this use case.

```
[14:05:02] [thought] Analyzing the repository structure...
[14:05:08] [code_execution] $ find . -name "*.go" | head -20
[14:05:12] [model_output] The repository contains 45 Go files...
[14:05:12] [status] completed (3,200 input / 1,800 output tokens)
```

### Q7: Template execution model — DECIDED: Templates are execution-agnostic

Templates do NOT declare whether the agent runs on a managed service or in a container. The `service:` field is removed from the template schema entirely. Templates define *what* the agent does (instructions, task, tools); the infrastructure layer decides *where/how* it runs. Managed-vs-container is a **broker profile** — analogous to choosing Cloud Run, GKE, or managed agents as the execution target. Hard error if any implementation leaks execution-mode concerns into the template.

### Q8: API client — DECIDED: Custom HTTP client

Custom `net/http` wrapper for v1. The API surface is small (~8 endpoints), auth is simple (API key header), and payloads are straightforward JSON. Avoids large Go SDK dependency tree and potential module conflicts. Migrate to official SDK later if it stabilizes and offers clear benefits.

---

## Appendix: Key Source Files for Implementation

| File | Role | Change |
|------|------|--------|
| `pkg/agent/manager.go` | Manager interface | Container manager contract |
| `pkg/api/types.go` | Core types | Add ManagedAgentSettings (in settings, not ScionConfig) |
| `pkg/agent/run.go` | AgentManager.Start | UNCHANGED |
| `pkg/hub/handlers_managed_agents.go` | Hub-direct managed backend routing | NEW |
| `pkg/managedagent/backend.go` | Backend interface | NEW |
| `pkg/managedagent/types.go` | Backend types | NEW |
| `pkg/managedagent/google/client.go` | Google HTTP client | NEW |
| `pkg/managedagent/google/backend.go` | Google backend impl | NEW |
| `pkg/managedagent/google/types.go` | Google API types | NEW |
| `pkg/managedagent/google/sse.go` | SSE parser | NEW |
| `pkg/agent/list.go` | Agent listing | Add managed agent scanning |
| `pkg/runtimebroker/handlers.go` | Broker dispatch | UNCHANGED (managed agents bypass broker, hub-direct) |
| `pkg/config/templates.go` | Template validation | UNCHANGED (templates are execution-agnostic) |
