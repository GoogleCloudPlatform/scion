# Suspend/Resume: Distinct Agent Lifecycle Semantics

## Status
**Design** | May 2026

## Problem

The current agent lifecycle conflates two distinct user intents under `stop` + `resume`:

1. **"Pause this agent for now, I'll come back to it later"** — The agent's container is stopped, but the user expects to pick up exactly where they left off. The harness session, conversation history, and working state should all be preserved. This is the common case when a user is done for the day or switching contexts.

2. **"This agent is done / broken, I want a fresh start"** — The agent should be fully terminated. When restarted, it gets a clean harness session. Working files (worktree, home dir) may be preserved for continuity, but the LLM session starts fresh.

Today, `stop` always means "stop the container" and `resume` always means "restart the container with the harness resume flag." There is no way for the user to express which intent they have, and there is no way for the platform to optimize behavior based on intent (e.g., keeping container state warm for suspended agents, or clearing harness history for stopped-then-restarted agents).

### Current Behavior

```
scion stop <agent>    → container stopped, phase=stopped, local config status="stopped"
scion resume <agent>  → container restarted with resume=true flag, always attempts to restore harness session
```

The `resume` command always passes `resume=true` to `RunAgent()`, which sets `opts.Resume = true`. The agent manager then calls `Start()` which:
1. Finds and deletes any existing (stopped) container
2. Re-provisions and starts a new container
3. Passes `--resume` to the harness (Claude Code's `--continue` flag)

This means `resume` always tries to restore the harness session regardless of whether the agent was gracefully paused or forcefully terminated after an error.

### Issues

1. **No semantic distinction**: `stop` is used for both "pause" and "terminate" — the user has no way to signal intent.
2. **Resume is always optimistic**: `resume` always tries to restore the harness session, even when the agent was stopped due to an error or the user wants a fresh start.
3. **No state preservation optimization**: Since `stop` always fully stops the container, there's no opportunity to keep container state warm for quick resume.
4. **Confusing UX for `start` vs `resume`**: Users must remember to use `resume` (not `start`) to continue an agent's session. Using `start` on a stopped agent deletes it and creates a fresh one.

## Design: Suspend vs Stop

Introduce `suspend` as a new agent lifecycle action that explicitly means "pause for later resume." `stop` retains its current meaning of "terminate."

### New Phase: `suspended`

Add `PhaseSuspended` to the agent state machine. This is a new lifecycle phase between `running` and `stopped`:

```
                    ┌──────────┐
                    │ created  │
                    └────┬─────┘
                         │ provision
                    ┌────▼─────────┐
                    │ provisioning │
                    └────┬─────────┘
                         │
                    ┌────▼─────┐
                    │ running  │
                    └──┬───┬───┘
                       │   │
          suspend ─────┘   └───── stop (graceful)
                       │         │
                  ┌────▼──────┐  │
                  │ suspended │  │
                  └────┬──────┘  │
                       │    ┌────▼─────┐
                       │    │ stopping │
                       │    └────┬─────┘
                       │         │
                       │    ┌────▼─────┐
                       └───►│ stopped  │
                            └──────────┘
```

**Transitions**:
- `running` → `suspended`: Agent is paused. Container is stopped, but the phase indicates intent to resume.
- `suspended` → `running`: Agent is resumed. Container is restarted with harness resume semantics.
- `suspended` → `stopped`: Agent is explicitly stopped after being suspended (user changed their mind). Can also happen via `stop --all`.
- `running` → `stopping` → `stopped`: Current stop flow, unchanged.

### Behavioral Differences

| Aspect | `suspend` | `stop` |
|--------|-----------|--------|
| Phase set to | `suspended` | `stopped` |
| Container | Stopped | Stopped |
| Harness session preserved | Yes (intent) | No (fresh start expected) |
| `resume` behavior | Restart with `--resume`/`--continue` | Restart fresh (no resume flag) |
| `start` behavior | Implicit resume (prints "Resuming...") | Delete + recreate |
| Notification trigger | No | No |
| Shows in `list` as | suspended | stopped |
| `stop --all` includes | Yes (transitions to stopped) | N/A |

### Key Design Decision: Implicit Resume from Start

Both `start` and `resume` check the agent's current phase:

- **`suspended`** → Restart with harness resume flag (`--continue` for Claude, `--resume` for Gemini). This preserves the LLM session. **`start` automatically detects this and prints "Resuming agent '...'..."**
- **`stopped`** → Restart without the resume flag. The harness starts a fresh session. Working files (worktree, home directory) are still present, but the LLM conversation starts clean.

The `start` command is now the primary way to resume a suspended agent — no need for users to remember a separate `resume` command. The `resume` command remains as an explicit alias.

### Key Design Decision: Suspend Requires Resume Support

The `suspend` command validates that the agent's harness supports session resume before proceeding. Harnesses that do not support resume (e.g., `generic`) will return an error directing the user to use `stop` instead. This prevents agents from entering a `suspended` state that cannot be meaningfully resumed.

The `Resume` capability is declared in `HarnessAdvancedCapabilities` alongside other capability fields. Built-in harnesses (`claude`, `gemini`, `opencode`, `codex`) all declare `SupportYes`. The `generic` harness declares `SupportNo`. Declarative and container-script harnesses can set the `Resume` field in their `config.yaml` capabilities block.

## Implementation

### 1. State Machine (`pkg/agent/state/state.go`)

Add `PhaseSuspended`:

```go
const (
    PhaseCreated      Phase = "created"
    PhaseProvisioning Phase = "provisioning"
    PhaseCloning      Phase = "cloning"
    PhaseStarting     Phase = "starting"
    PhaseRunning      Phase = "running"
    PhaseSuspended    Phase = "suspended"    // NEW
    PhaseStopping     Phase = "stopping"
    PhaseStopped      Phase = "stopped"
    PhaseError        Phase = "error"
)
```

Update `allPhases` and validation.

### 2. Ent Schema (`pkg/ent/schema/agent.go`)

Add "suspended" to the status enum:

```go
field.Enum("status").
    Values("created", "provisioning", "cloning", "starting", "running", "suspended", "stopping", "stopped", "error").
    Default("created"),
```

Regenerate ent code to update `pkg/ent/agent/agent.go` validators.

### 3. API Action (`pkg/api/agent_actions.go`)

Add `AgentActionSuspend`:

```go
const (
    AgentActionSuspend = "suspend"
)
```

Add to `RuntimeBrokerAgentActionMethod` mapping.

### 4. CLI Command (`cmd/suspend.go`)

New `scion suspend <agent>` command, modeled after `cmd/stop.go`:
- Local mode: stops container, sets phase to `suspended` (not `stopped`)
- Hub mode: dispatches suspend action to hub
- Supports `--all` flag to suspend all running agents
- Does NOT support `--rm` (suspend implies intent to resume)

### 5. Hub Handler Update (`pkg/hub/handlers.go`)

Add `AgentActionSuspend` to the lifecycle action handler:

```go
case api.AgentActionSuspend:
    newPhase = string(state.PhaseSuspended)
    if dispatcher != nil && agent.RuntimeBrokerID != "" {
        s.syncWorkspaceOnStop(ctx, agent)
        dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
    }
```

The dispatcher calls the same `StopAgent` on the broker — the container operation is identical. The difference is purely in the phase recorded by the hub.

### 6. Resume Update (`cmd/common.go` RunAgent)

`RunAgent` always checks the saved phase to determine resume behavior, regardless of
whether it was called from `start` (resume=false) or `resume` (resume=true):

```go
savedPhase := agent.GetSavedPhase(agentName, grovePath)
if savedPhase == "suspended" {
    effectiveResume = true   // implicit resume from start
} else if resume && savedPhase == "stopped" {
    effectiveResume = false  // explicit resume of stopped agent → fresh session
}
```

For hub mode, `startAgentViaHub()` queries the agent's phase before creating. If the
agent is suspended, it sets `Resume: true` in the create request and prints "Resuming...".
The hub already resumes suspended agents in-place regardless of the Resume flag
(`handleExistingAgent` checks `existingAgent.Phase == "suspended"`).

### 7. Hubclient (`pkg/hubclient/agents.go`)

Add `Suspend` method:

```go
func (s *agentService) Suspend(ctx context.Context, agentID string) error {
    resp, err := s.c.transport.Post(ctx, s.agentPath(agentID)+"/suspend", nil, nil)
    if err != nil {
        return err
    }
    return apiclient.CheckResponse(resp)
}
```

### 8. Broker Handler (`pkg/runtimebroker/handlers.go`)

Add "suspend" to the agent action router. The broker-side implementation is identical to stop (it calls `mgr.Stop()`) — the semantic difference is recorded at the hub level.

### 9. CLI Mode (`cmd/cli_mode.go`)

Add "suspend" to `agentAllowed` for agent mode access.

### 10. Stop --all Update

`stop --all` should also suspend→stop any suspended agents (transition from `suspended` to `stopped`).

## Migration / Backward Compatibility

- The new `suspended` phase is additive — no existing data needs migration.
- Existing `stopped` agents continue to work as before.
- The `resume` command's behavior change (no harness resume for stopped agents) is intentional and a UX improvement. Users who want session preservation should use `suspend` instead of `stop`.
- The ent schema change adds a new enum value, which is a non-breaking database change.

## Future Considerations

- **Container hibernation**: When runtimes support checkpoint/restore (CRIU), `suspend` could preserve container memory state for instant resume. The phase distinction makes this possible without API changes.
- **Auto-suspend**: Agents that have been idle for a configurable period could be auto-suspended rather than left running, reducing resource consumption while preserving session state.
- **Cost optimization**: Suspended agents clearly signal that compute resources can be reclaimed, while the platform knows to preserve session state for quick restore.
