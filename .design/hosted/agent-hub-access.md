# Agent-to-Hub Access: Sub-Agent Creation and Management

## Status
**Proposed**

## 1. Overview

This document specifies how an agent running inside a container can use the `scion` CLI (or Hub API directly) to create, start, and manage peer agents within its own grove. This enables a "lead agent" pattern where a coordinating agent can spawn specialized sub-agents to divide work, without requiring user-level credentials inside the container.

### Motivation

A common orchestration pattern is for a "lead" or "planner" agent to decompose a task and delegate sub-tasks to specialized agents. Today, only users (via CLI or web) can create and start agents. Enabling agents to do this through the existing Hub API unlocks autonomous multi-agent workflows.

### Goals

1. **Scoped Delegation** - Agents can create and manage other agents, but only within their own grove.
2. **Minimal Surface** - Reuse existing auth infrastructure (agent JWT tokens and scopes) with minimal new code.
3. **No New Binaries** - The `scion` CLI is already present in agent containers; it just needs the Hub to accept its token for these operations.
4. **Security Boundaries** - Agents cannot escalate beyond their grove, cannot affect agents in other groves, and cannot grant sub-agents more scopes than they possess.

### Non-Goals

- Cross-grove agent creation (agents spawning work in other projects).
- Granting agents user-level identity (agents remain agents, not users).
- Local-mode (non-Hub) nested agent creation (requires Docker socket access, out of scope).
- Recursive depth limits (can be addressed later if needed).

---

## 2. Current State

### 2.1 What's Already in Place

| Capability | Status | Notes |
|---|---|---|
| `scion` binary in container | Available | Built into base image at `/usr/local/bin/scion` |
| `SCION_HUB_URL` env var | Available | Injected by Runtime Broker at container start |
| `SCION_HUB_TOKEN` env var | Available | Agent JWT injected by Runtime Broker |
| `SCION_AGENT_ID` env var | Available | Agent's own ID |
| `.scion` grove directory | Available | Mounted at `/repo-root/.scion` via repo root mount |
| Git remote URL | Available | `.git` is mounted; `git remote get-url origin` works |
| Grove resolution | Works | `config.FindProjectRoot()` walks up from cwd to find `.scion` |

### 2.2 What Blocks It

**Agent token scopes are too narrow.** Tokens are issued with only `agent:status:update` (`pkg/hub/server.go:548`). The existing scopes are:

| Scope | Purpose |
|---|---|
| `agent:status:update` | Update own status |
| `agent:log:append` | Append own logs |
| `grove:secret:read` | Read grove secrets |

**Handler auth checks require `UserIdentity`.** Key handlers explicitly reject non-user callers:

- `handleAgentAction` (`pkg/hub/handlers.go:686-693`) — gates `start`/`stop`/`restart` behind `GetUserIdentityFromContext() != nil`.
- `handleAgentByID` (`pkg/hub/handlers.go:522`) — gates workspace operations behind user identity.
- `createAgent` (`pkg/hub/handlers.go:216`) — no explicit user gate, but no agent-aware grove-scoping either.

---

## 3. Design

### 3.1 New Agent Token Scopes

Add two new scopes to `pkg/hub/agenttoken.go`:

```go
const (
    // Existing scopes
    ScopeAgentStatusUpdate AgentTokenScope = "agent:status:update"
    ScopeAgentLogAppend    AgentTokenScope = "agent:log:append"
    ScopeGroveSecretRead   AgentTokenScope = "grove:secret:read"

    // New scopes for agent-to-hub management
    ScopeAgentCreate    AgentTokenScope = "grove:agent:create"    // Create agents in same grove
    ScopeAgentLifecycle AgentTokenScope = "grove:agent:lifecycle"  // Start/stop/restart agents in same grove
)
```

> **Note:** The `grove:` prefix on the new scopes signals that these are grove-scoped operations, consistent with the naming foreshadowed in `sciontool-auth.md` §4.2 which listed `grove:agent:create` as a future scope.

### 3.2 Scope Grant Configuration

Not all agents should be able to spawn sub-agents. The scopes should be opt-in, controlled through agent or grove configuration.

#### Option A: Agent Template Property (Recommended)

Add an `allowHubAccess` (or `hubScopes`) field to agent templates:

```yaml
# In agent template or .scion/agents/<name>/config.yaml
hub_access:
  scopes:
    - grove:agent:create
    - grove:agent:lifecycle
```

When the Hub provisions an agent whose template includes these scopes, they are included in the JWT.

#### Option B: Grove-Level Default

A grove-level setting that grants all agents in the grove the ability to create sub-agents:

```yaml
# In .scion/settings.yaml or grove config on Hub
grove:
  agent_hub_scopes:
    - grove:agent:create
    - grove:agent:lifecycle
```

#### Decision

Option A (agent template property) is the chosen approach. It provides finer control and follows the principle of least privilege.

> **Future Direction:** This scope-grant mechanism is an interim solution. It will be superseded by a comprehensive permissions and policy system (see [permissions-design.md](auth/permissions-design.md)) that provides unified authorization across all principal types. Implementation code should include comments noting this forward dependency.

### 3.3 Token Generation Changes

**File:** `pkg/hub/server.go` — `GenerateAgentToken()` method

Currently:
```go
return tokenService.GenerateAgentToken(agentID, groveID, []AgentTokenScope{ScopeAgentStatusUpdate})
```

Updated to merge configured scopes:

```go
scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
if agentConfig.HubAccess != nil {
    scopes = append(scopes, agentConfig.HubAccess.Scopes...)
}
return tokenService.GenerateAgentToken(agentID, groveID, scopes)
```

### 3.4 Handler Authorization Changes

> **Implementation Note:** The inline authorization checks described below are appropriate for the initial implementation. When implemented, the code should include comments indicating that these checks are candidates for extraction into common authorization middleware that evaluates requests against a policy engine (see [permissions-design.md](auth/permissions-design.md)). This will avoid scattering authorization logic across individual handlers as the system grows.

#### 3.4.1 `createAgent` (`pkg/hub/handlers.go:216`)

Add agent identity handling after request validation:

```go
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    // ... existing request parsing and validation ...

    // Authorization: accept user OR agent with grove:agent:create scope
    if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
        if !agentIdent.HasScope(ScopeAgentCreate) {
            Forbidden(w, "missing required scope: grove:agent:create")
            return
        }
        // Agents can only create agents within their own grove
        if req.GroveID != agentIdent.GroveID() {
            Forbidden(w, "agents can only create agents in their own grove")
            return
        }
    }
    // If neither user nor authorized agent, the unified auth middleware
    // already rejected the request as unauthenticated.

    // ... rest of handler ...
}
```

#### 3.4.2 `handleAgentAction` (`pkg/hub/handlers.go:686-693`)

Relax the user-only gate for lifecycle actions:

```go
// Current:
if action != "status" {
    if GetUserIdentityFromContext(r.Context()) == nil {
        writeError(w, http.StatusForbidden, ...)
        return
    }
}

// Updated:
if action != "status" {
    user := GetUserIdentityFromContext(r.Context())
    agentIdent := GetAgentIdentityFromContext(r.Context())

    if user == nil && agentIdent == nil {
        Unauthorized(w)
        return
    }

    if user == nil && agentIdent != nil {
        if !agentIdent.HasScope(ScopeAgentLifecycle) {
            Forbidden(w, "missing required scope: grove:agent:lifecycle")
            return
        }
        // Verify target agent is in the same grove
        targetAgent, err := s.store.GetAgent(r.Context(), id)
        if err != nil {
            writeErrorFromErr(w, err, "")
            return
        }
        if targetAgent.GroveID != agentIdent.GroveID() {
            Forbidden(w, "agents can only manage agents in their own grove")
            return
        }
    }
}
```

#### 3.4.3 `handleAgentByID` (`pkg/hub/handlers.go:522`)

Same pattern — allow agents with appropriate scopes to read sibling agent details (needed so the creating agent can poll status or check if an agent already exists):

```go
// For GET (read) operations: allow agents within same grove
// For workspace operations: continue requiring user auth
```

### 3.5 CLI Behavior Inside Containers

The `scion` CLI already supports Hub mode and reads from environment variables. From inside a container, the following would work without CLI changes:

```bash
# Agent's environment already has:
# SCION_HUB_URL=https://hub.example.com
# SCION_HUB_TOKEN=<agent-jwt>

# Create and start a sub-agent
scion start code-reviewer "Review the changes in src/auth/ for security issues"

# The CLI will:
# 1. Find .scion at /repo-root/.scion (grove resolution works)
# 2. Read SCION_HUB_URL and SCION_HUB_TOKEN from env
# 3. Resolve grove ID via git remote + Hub API lookup
# 4. POST to Hub API to create/start the agent
# 5. Hub dispatches to an available Runtime Broker
```

#### CLI Authentication Path

The `scion` CLI resolves auth in this order (`cmd/hub.go`):

1. Settings-based token (`settings.Hub.Token`)
2. OAuth credentials (`~/.scion/credentials.json`)
3. Dev token (`SCION_DEV_TOKEN`)
4. Auto-dev-auth fallback

In a container, none of these are present. The `SCION_HUB_TOKEN` env var is consumed by `sciontool`, but the `scion` CLI needs to also pick it up. This requires a small addition to the CLI's auth resolution to check `SCION_HUB_TOKEN` as a bearer token source.

**File:** `cmd/hub.go` — hub client initialization

Add a step in the auth resolution chain:

```go
// Check for agent-mode token (when running inside a container)
if token := os.Getenv("SCION_HUB_TOKEN"); token != "" {
    opts = append(opts, hubclient.WithBearerToken(token))
}
```

---

## 4. Security Considerations

### 4.1 Grove Isolation

All agent-initiated operations MUST be constrained to the agent's own grove. The `grove_id` claim in the JWT provides the boundary. Every handler that accepts agent identity must verify the target resource belongs to the same grove.

> **Future Enhancement:** Grove-level isolation is the baseline boundary. A more granular per-agent authorization model — where policies control which specific agents a given agent can create, start, or interact with — is planned as part of the broader permissions system. The initial scope-based checks here should be structured to make this transition straightforward.

### 4.2 Scope Ceiling

Sub-agents created by an agent should NOT receive broader hub-access scopes than the parent agent holds. The provisioning logic should intersect the requested scopes with the creating agent's scopes (or omit hub-access scopes entirely for sub-agents unless explicitly configured).

### 4.3 Token Lifetime

Agent tokens currently have a 24-hour lifetime. For lead agents that spawn long-running sub-agents, this is sufficient since the sub-agent receives its own independent token from the Hub during provisioning. The lead agent's token only needs to be valid at creation time.

### 4.4 Audit Trail

All agent-initiated create/start/stop operations should be logged with the originating agent's ID (from `AgentIdentity.ID()`) so that the chain of delegation is traceable.

### 4.5 Rate Limiting

Consider limiting the number of agents that an agent can create within a time window to prevent runaway agent proliferation. This can be enforced at the Hub handler level using the agent's identity.

---

## 5. Container Context Summary

For reference, the following context is available inside an agent container that enables this feature:

| Resource | Container Path | Source |
|---|---|---|
| `scion` binary | `/usr/local/bin/scion` | Built into container image |
| `sciontool` binary | `/usr/local/bin/sciontool` | Built into container image |
| Grove directory | `/repo-root/.scion` | Mounted from host repo root |
| Git directory | `/repo-root/.git` | Mounted from host repo root |
| Hub URL | `$SCION_HUB_URL` | Env var injected by broker |
| Hub token | `$SCION_HUB_TOKEN` | Env var injected by broker |
| Agent ID | `$SCION_AGENT_ID` | Env var injected by broker |
| Agent slug | `$SCION_AGENT_SLUG` | Env var injected by broker |
| Broker name | `$SCION_BROKER_NAME` | Env var injected by broker |

---

## 6. Implementation Plan

### Phase 1: Scope and Token Changes
1. Add `ScopeAgentCreate` and `ScopeAgentLifecycle` constants to `pkg/hub/agenttoken.go`.
2. Add `HubAccess` configuration field to agent templates / config model.
3. Update `GenerateAgentToken` call in `pkg/hub/server.go` to include configured scopes.
4. Add tests for new scope validation.

### Phase 2: Handler Updates
5. Update `createAgent` in `pkg/hub/handlers.go` to accept agent identity with scope + grove check.
6. Update `handleAgentAction` in `pkg/hub/handlers.go` to accept agent identity for lifecycle actions.
7. Update `handleAgentByID` for read access by sibling agents.
8. Add handler tests covering agent-as-caller scenarios.

### Phase 3: CLI Integration
9. Update `cmd/hub.go` auth resolution to check `SCION_HUB_TOKEN` env var.
10. End-to-end test: agent inside container calls `scion start` to create a sub-agent.

---

## 7. Files Affected

| File | Change |
|---|---|
| `pkg/hub/agenttoken.go` | Add `ScopeAgentCreate`, `ScopeAgentLifecycle` scope constants |
| `pkg/hub/agenttoken_test.go` | Tests for new scopes |
| `pkg/hub/server.go` | Conditional scope inclusion in `GenerateAgentToken` call |
| `pkg/hub/handlers.go` | Auth checks in `createAgent`, `handleAgentAction`, `handleAgentByID` |
| `pkg/hub/handlers_test.go` | Handler tests for agent-as-caller |
| `cmd/hub.go` | Auth resolution: add `SCION_HUB_TOKEN` env var check |
| Agent template/config schema | `hub_access.scopes` field |

---

## Related Documents

- [Agent Authentication (sciontool-auth.md)](auth/sciontool-auth.md) — Current agent token design; §4.2 foreshadows `grove:agent:create`.
- [Authentication Overview (auth-overview.md)](auth/auth-overview.md) — Identity model and token types.
- [Server Auth Design (server-auth-design.md)](auth/server-auth-design.md) — Unified auth middleware.
- [Permissions Design (permissions-design.md)](auth/permissions-design.md) — Hub permission model.
- [Multi-Broker (multi-broker.md)](multi-broker.md) — Broker selection during agent creation.
