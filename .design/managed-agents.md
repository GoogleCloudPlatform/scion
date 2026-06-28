# ManagedAgent Design Document (Option B)

## 1. Overview

**Problem statement.** Scion currently runs AI coding agents exclusively in containers (Docker, Podman, Kubernetes, Apple Container, Cloud Run). Each container bundles a Runtime (container lifecycle) and a Harness (LLM CLI like Claude Code or Gemini CLI). A growing class of cloud services offer fully-managed agent execution where the model, tools, sandbox, and orchestration loop are handled server-side. Scion needs to support these managed backends so that `scion start`, `scion message`, `scion stop`, and other commands work seamlessly whether the agent runs in a local container or a remote cloud service.

**Design decision.** Option B: introduce `ManagedAgent` as a peer concept to the existing Runtime+Harness stack, not as a replacement. The `Manager` interface (`pkg/agent/manager.go`) is the branching point. A new `ManagedAgentManager` implements the same `Manager` interface but delegates to a cloud API client instead of a Runtime+Harness pair. Container code is untouched.

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

The new dispatch chain for managed agents:

```
Template (scion-agent.yaml with service: field)
  -> ManagedAgentManager (implements Manager)
     -> ManagedAgentBackend (interface)
        -> GoogleManagedAgentBackend (first impl)
```

Both `AgentManager` and `ManagedAgentManager` implement the same `Manager` interface defined in `pkg/agent/manager.go`. CLI commands and the broker both talk to `Manager` -- they do not need to know whether the agent is containerized or managed.

### 2.2 Package layout

```
pkg/
  agent/
    manager.go              # Manager interface (UNCHANGED)
    run.go                  # AgentManager.Start (UNCHANGED)
    managed_manager.go      # NEW: ManagedAgentManager struct + Manager impl
    managed_state.go        # NEW: ManagedAgentState persistence
    manager_factory.go      # NEW: NewManagerForConfig factory
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

**ManagedAgentManager** (new, in `pkg/agent/managed_manager.go`):

```go
package agent

import (
    "context"

    "github.com/GoogleCloudPlatform/scion/pkg/api"
    "github.com/GoogleCloudPlatform/scion/pkg/managedagent"
)

// ManagedAgentManager implements the Manager interface for cloud-managed agents.
type ManagedAgentManager struct {
    Backend   managedagent.ManagedAgentBackend
    stateDir  string // Base directory for managed agent state files
}

func NewManagedAgentManager(backend managedagent.ManagedAgentBackend, stateDir string) Manager {
    return &ManagedAgentManager{
        Backend:  backend,
        stateDir: stateDir,
    }
}

// All Manager interface methods implemented by delegating to Backend.
// See Section 4 for detailed lifecycle flows.

func (m *ManagedAgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) { ... }
func (m *ManagedAgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) { ... }
func (m *ManagedAgentManager) Stop(ctx context.Context, agentID string, projectPath string) error { ... }
func (m *ManagedAgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) { ... }
func (m *ManagedAgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) { ... }
func (m *ManagedAgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error { ... }
func (m *ManagedAgentManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error { ... }
func (m *ManagedAgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) { ... }
func (m *ManagedAgentManager) Close() { ... }
```

### 2.4 How the Manager interface branches

The branching happens at construction time, not dispatch time. A factory function inspects the resolved template config:

```go
// pkg/agent/manager_factory.go

package agent

import (
    "github.com/GoogleCloudPlatform/scion/pkg/api"
    "github.com/GoogleCloudPlatform/scion/pkg/managedagent"
    "github.com/GoogleCloudPlatform/scion/pkg/managedagent/google"
    "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// NewManagerForProfile returns the appropriate Manager implementation
// based on the broker profile selected at agent creation time.
// Managed agent config (API key, base agent) comes from Scion settings, not templates.
func NewManagerForProfile(rt runtime.Runtime, profile string, settings *api.Settings, stateDir string) Manager {
    switch profile {
    case "managed-agents":
        cfg := settings.ManagedAgents.Google
        backend := google.NewBackend(google.BackendConfig{
            APIKey:    cfg.APIKey,
            BaseAgent: cfg.BaseAgent,
        })
        return NewManagedAgentManager(backend, stateDir)
    default:
        return NewManager(rt)
    }
}
```

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

When `ManagedAgentManager.Start()` is called:

1. **Project resolution** -- same as container path: `config.GetResolvedProjectDir(opts.ProjectPath)`.
2. **Duplicate check** -- scan local state directory for existing managed agent with same slug.
3. **Template/config resolution** -- load template chain, merge configs (same as container path up to merge).
4. **Detect managed agent** -- broker profile is `managed-agents` (selected at creation time, not from template).
5. **Resolve auth** -- resolve API key from Scion settings (`managed_agents.google.api_key`), Hub secrets, or environment variable.
6. **Create cloud agent** -- call `Backend.CreateAgent()` with system instruction, tools, and environment config. Synchronous call. Receives `cloudAgentID`.
7. **Create first interaction** -- if `opts.Task` is non-empty, call `Backend.CreateInteraction()` with the task as input and `Stream: true`.
8. **Persist local state** -- write `managed-agent-state.json` to state directory. Write `agent-info.json` with phase=running, activity=working.
9. **Return AgentInfo** -- populated with runtime set to `"managed:google"`.

Container path steps 5-12 (image resolution, harness resolution, container auth, image pull, env construction, container launch, verify) are all skipped.

### 4.2 Message delivery flow

1. **Find agent** -- look up managed agent state from local state directory by slug.
2. **Load cloud state** -- read `managed-agent-state.json` for `cloudAgentID`, `latestEnvironmentID`, `latestInteractionID`.
3. **Create new interaction** -- call `Backend.CreateInteraction()` with:
   - `CloudAgentID`: from state
   - `Input`: the message
   - `PreviousInteractionID`: from state (chains conversation)
   - `EnvironmentID`: from state (preserves sandbox)
   - `Stream: true`
4. **Update state** -- write new `interactionID` and `environmentID` to state file. Update `agent-info.json` with activity=working.
5. **Monitor completion** -- goroutine reads SSE stream, updates `agent-info.json` on completion.

`MessageRaw()` returns an error for managed agents (no tmux).

### 4.3 Status monitoring and mapping

| Google Interaction Status | Scion Phase | Scion Activity |
|---|---|---|
| (agent created, no interaction yet) | running | waiting_for_input |
| `in_progress` | running | working |
| `requires_action` (unexpected) | running | error (log warning) |
| `completed` | running | completed |
| `failed` | error | crashed |
| `cancelled` | stopped | -- |
| `incomplete` | running | limits_exceeded |

SSE event-level mapping:

| SSE Event | Scion Activity |
|---|---|
| `step.start` type=thought | thinking |
| `step.start` type=model_output | working |
| `step.start` type=function_call | executing |
| `step.start` type=code_execution_call | executing |
| `interaction.completed` | completed |
| `interaction.status_update` requires_action | error (log warning, unexpected) |

### 4.4 Stop/Delete flows

**Stop**: Cancel active interaction if `in_progress`. Cloud agent and environment persist (resumable). Update `agent-info.json` phase=stopped.

**Delete**: Stop (as above), then `Backend.DeleteAgent()`, then remove local state directory if `deleteFiles` is true. Return `(false, nil)` -- no branch to delete.

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

Environments are NOT a separate CRUD resource. Created implicitly when an interaction uses `environment: "remote"`, reused by passing the returned `environment_id`. The `ManagedAgentManager` tracks `latestEnvironmentID` in state.

Google API lifecycle: auto-snapshot after ~15min idle, retained 7 days, then deleted. Scion does not manage this.

### 5.3 Auth handling

- API key lives in Scion settings (`managed_agents.google.api_key`) — NOT in templates (decision Q1)
- Resolution chain: Scion settings > Hub secret > environment variable
- No template-level credential configuration; templates are execution-agnostic

### 5.4 Multi-turn state tracking

Two independent chains:
- **Conversation**: `previous_interaction_id` links context
- **Environment**: `environment_id` links sandbox state

```json
{
  "cloud_agent_id": "agent_abc123",
  "latest_interaction_id": "int_xyz789",
  "latest_environment_id": "env_def456",
  "interaction_chain": ["int_001", "int_002", "int_xyz789"],
  "created_at": "2026-06-28T10:00:00Z",
  "updated_at": "2026-06-28T10:05:00Z"
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

### 7.1 Per-agent state

```go
type ManagedAgentState struct {
    CloudAgentID        string   `json:"cloud_agent_id"`
    CloudProvider       string   `json:"cloud_provider"`
    LatestInteractionID string   `json:"latest_interaction_id,omitempty"`
    LatestEnvironmentID string   `json:"latest_environment_id,omitempty"`
    InteractionChain    []string `json:"interaction_chain,omitempty"`
    LastStatus          string   `json:"last_status,omitempty"`
    CreatedAt           string   `json:"created_at"`
    UpdatedAt           string   `json:"updated_at"`
    APIKeyRef           string   `json:"api_key_ref,omitempty"`
}
```

### 7.2 Directory layout

```
<projectDir>/agents/<agent-name>/
  scion-agent.json          # Template config (same as container agents)
  managed-agent-state.json  # NEW: cloud-side state tracking
  home/
    agent-info.json          # Status (same format as container agents)
    # No local agent.log — logs go to GCP Cloud Logging (decision Q4)
```

### 7.3 Integration with agent-info.json

Same schema, different field values:

| Field | Container Agent | Managed Agent |
|---|---|---|
| `ContainerID` | Docker/k8s ID | Empty |
| `Runtime` | "docker" / "podman" / "kubernetes" | "managed:google" |
| `Phase` | From container + sciontool hooks | From interaction status |
| `Activity` | From `.scion-status.json` | From SSE event mapping |
| `Image` | Container image | Empty |

---

## 8. CLI Behavior Matrix

| Command | Container Agent | Managed Agent |
|---|---|---|
| `scion start <name> [task]` | Provision + container launch | Create cloud agent + first interaction |
| `scion create <name> [task]` | Provision only (no container) | Create cloud agent (no interaction) |
| `scion stop <name>` | Stop container | Cancel active interaction |
| `scion delete <name>` | Delete container + files | Delete cloud agent + local state |
| `scion list` | Merge container + on-disk state | Merge managed state from agent dirs |
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

## 9. Migration Path to Option C

### 9.1 Refactoring to ExecutionBackend

Option C introduces a unified interface:

```go
type ExecutionBackend interface {
    Name() string
    Start(ctx context.Context, cfg BackendConfig) (AgentHandle, error)
    Stop(ctx context.Context, handle AgentHandle) error
    Delete(ctx context.Context, handle AgentHandle) error
    SendMessage(ctx context.Context, handle AgentHandle, msg string) error
    GetOutput(ctx context.Context, handle AgentHandle) (string, error)
    StreamOutput(ctx context.Context, handle AgentHandle) (io.ReadCloser, error)
    GetStatus(ctx context.Context, handle AgentHandle) (Phase, Activity, error)
}
```

### 9.2 What stays stable

- **`Manager` interface** -- the public API for CLI and broker. Internal impl switches from "Manager holds Runtime" to "Manager holds ExecutionBackend", but callers unaffected.
- **`ScionConfig.Service` field** -- template schema is stable.
- **`ManagedAgentState` persistence** -- state file format and directory layout are stable.
- **`agent-info.json`** -- status format is stable.

### 9.3 What changes

- **`ManagedAgentBackend`** -- absorbed into `ExecutionBackend`.
- **`ManagedAgentManager`** -- absorbed into unified `Manager` implementation.
- **`AgentManager`** -- also absorbed; Runtime+Harness wrapped in `ContainerExecutionBackend`.
- **Package layout** -- `pkg/managedagent/` may move to `pkg/backend/managed/`.

Key insight: Option B's `ManagedAgentBackend` is a preview of `ExecutionBackend`'s shape, because managed agents naturally surface the right abstraction level (message-in, output-out, status query). The Option C refactor mostly consists of wrapping existing Runtime+Harness into the same shape.

---

## 10. Resolved Design Decisions

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
| `pkg/agent/manager.go` | Manager interface | Add factory function |
| `pkg/api/types.go` | Core types | Add ManagedAgentSettings (in settings, not ScionConfig) |
| `pkg/agent/run.go` | AgentManager.Start | UNCHANGED |
| `pkg/agent/managed_manager.go` | ManagedAgentManager | NEW |
| `pkg/agent/managed_state.go` | State persistence | NEW |
| `pkg/agent/manager_factory.go` | Manager factory | NEW |
| `pkg/managedagent/backend.go` | Backend interface | NEW |
| `pkg/managedagent/types.go` | Backend types | NEW |
| `pkg/managedagent/google/client.go` | Google HTTP client | NEW |
| `pkg/managedagent/google/backend.go` | Google backend impl | NEW |
| `pkg/managedagent/google/types.go` | Google API types | NEW |
| `pkg/managedagent/google/sse.go` | SSE parser | NEW |
| `pkg/agent/list.go` | Agent listing | Add managed agent scanning |
| `pkg/runtimebroker/handlers.go` | Broker dispatch | UNCHANGED (managed agents bypass broker, hub-direct) |
| `pkg/config/templates.go` | Template validation | UNCHANGED (templates are execution-agnostic) |
