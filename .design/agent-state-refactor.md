# Agent State Refactor: Unified State Model

## Status
**Design** | February 2026

## Problem

Agent state is currently defined in **five separate locations** with overlapping but inconsistent sets of values, different casing conventions, and no shared taxonomy. This makes it difficult to reason about what state an agent is in, leads to bugs when new states are added in one place but not others, and prevents the Hub from presenting a coherent view of agent status to the UI and API consumers.

### Current State Definitions

| Location | Type | Count | Casing | Example Values |
|---|---|---|---|---|
| `pkg/sciontool/hooks/types.go` | `AgentState` | 11 | UPPERCASE | `IDLE`, `THINKING`, `EXECUTING`, `WAITING_FOR_INPUT` |
| `pkg/store/models.go` | untyped `string` | 13 | lowercase | `busy`, `idle`, `waiting_for_input`, `deleted`, `restored` |
| `pkg/runtimebroker/types.go` | untyped `string` | 9 | lowercase | `created`, `starting`, `running`, `stopping` |
| `pkg/sciontool/hub/client.go` | `AgentStatus` | 14 | lowercase | `busy`, `idle`, `shutting_down`, `limits_exceeded` |
| `pkg/ent/agent/agent.go` | `Status` enum | 5 | lowercase | `pending`, `provisioning`, `running`, `stopped`, `error` |
| `web/src/shared/types.ts` | `AgentStatus` union | 9 | lowercase | `running`, `idle`, `busy`, `waiting_for_input`, `completed` |
| `web/src/components/shared/status-badge.ts` | `StatusType` union | 17 | lowercase | generic UI types, not agent-specific |

### Key Issues

1. **Conflated concerns**: Lifecycle state (created → provisioning → running → stopped), activity state (idle, busy, thinking, executing), and agent-reported state (waiting_for_input, completed, limits_exceeded) are flattened into a single `status` field with no formal taxonomy.

2. **Case mismatch**: The container-side sciontool uses UPPERCASE (`THINKING`, `EXECUTING`), while everything Hub-side uses lowercase (`busy`, `idle`). The Hub handler translates between them ad-hoc.

3. **Ent schema drift**: The ent ORM schema only validates 5 status values (`pending`, `provisioning`, `running`, `stopped`, `error`), but the SQLite store bypasses ent for status updates via raw SQL, allowing 13+ values to be stored without validation.

4. **Semantic ambiguity**: `running` means "container is up" in the lifecycle sense, but also serves as the parent of `idle`/`busy`/`thinking`/`executing` activity states. The Hub stores `idle` or `busy` in the same `status` column that also holds `provisioning` or `stopped` — mixing categories.

5. **Undocumented state machine**: The sticky-state logic (`WAITING_FOR_INPUT`, `COMPLETED`, `LIMITS_EXCEEDED` resist being overwritten) is implemented in code but not formalized in any model or design doc. Transition rules differ between the local status handler and the hub handler.

6. **Missing states**: Design docs reference `terminated` but it's never implemented. The `stalled` concept (agent hasn't produced events within a timeout) has no state representation. `starting` exists in the broker but not the store or ent. `shutting_down` exists in the hub client but not the store.

7. **Lost granularity**: The sciontool captures rich state like `EXECUTING (Bash)` or `THINKING`, but the Hub collapses these to just `busy`. The UI cannot distinguish between an agent that's thinking vs executing a tool vs waiting for an LLM API response.

## Proposal: Layered State Model

Replace the flat `status` string with a structured, layered model that separates orthogonal concerns while maintaining a single source of truth.

### Core Principle: Three Orthogonal Dimensions

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Agent State Model                            │
│                                                                     │
│  1. PHASE (lifecycle)     Where is the agent in its lifecycle?      │
│     created → provisioning → starting → running → stopping →       │
│     stopped → error                                                 │
│                                                                     │
│  2. ACTIVITY (runtime)    What is the running agent doing?          │
│     idle | thinking | executing | waiting_for_input | blocked |     │
│     completed | limits_exceeded | stalled | offline                 │
│     (only meaningful when phase = running)                          │
│                                                                     │
│  3. DETAIL (context)      What specifically? (optional metadata)    │
│     tool_name, message, task_summary                                │
│     (extensible per-harness, not enumerated)                        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 1. Phase (Lifecycle State)

The **phase** represents where the agent is in its infrastructure lifecycle. This is controlled by the platform (broker, hub, container runtime) — not by the LLM agent itself.

```
                    ┌──────────┐
                    │ created  │  Agent record exists, no container yet
                    └────┬─────┘
                         │ provision
                    ┌────▼─────────┐
              ┌─────│ provisioning │  Container being built/configured
              │     └────┬─────────┘
              │          │ clone (if git workspace)
              │     ┌────▼─────┐
              │     │ cloning  │  Git workspace being prepared
              │     └────┬─────┘
              │          │ start
              │     ┌────▼─────┐
              │     │ starting │  Container starting, pre-start hooks
              │     └────┬─────┘
              │          │ ready
              │     ┌────▼─────┐
              │     │ running  │  Container up, agent process active
              │     └────┬─────┘
              │          │ stop (graceful)
              │     ┌────▼─────┐
              │     │ stopping │  Shutdown in progress
              │     └────┬─────┘
              │          │
              │     ┌────▼─────┐
              └────►│ stopped  │  Clean shutdown
                    └────┬─────┘
                         │ restart → starting
                         │
                    ┌────▼─────┐
                    │  error   │  Unrecoverable failure at any point
                    └──────────┘
```

**Values**: `created`, `provisioning`, `cloning`, `starting`, `running`, `stopping`, `stopped`, `error`

**Rules**:
- Phase is set by platform operations (broker commands, heartbeats, container events)
- Only `running` allows an `activity` value to be meaningful
- Transitioning to a non-running phase clears the activity
- `error` can be reached from any phase

**Mapping from current implementation**:
| Current | New Phase |
|---|---|
| `created` | `created` |
| `pending` | `created` (rename — "pending" is ambiguous) |
| `provisioning` | `provisioning` |
| `cloning` | `cloning` |
| `starting` | `starting` |
| `running` | `running` |
| `stopping` | `stopping` |
| `stopped` | `stopped` |
| `error` | `error` |
| `deleted` | (soft-delete flag via `deletedAt` timestamp, not a phase) |
| `restored` | (not a state — restore clears soft-delete, agent returns to prior phase) |

### 2. Activity (Runtime State)

The **activity** represents what the agent is doing while it's running. This is reported by the agent process itself (via sciontool hooks) and only has meaning when `phase = running`.

```
                         ┌──────┐
              ┌──────────│ idle │◄──────────────────┐
              │          └──┬───┘                    │
              │             │ prompt-submit /        │
              │             │ agent-start            │
              │          ┌──▼──────┐                 │
              │          │thinking │                 │
              │          └──┬──────┘                 │
              │             │ tool-start             │ tool-end /
              │          ┌──▼───────┐                │ agent-end /
              │          │executing │────────────────┘ model-end
              │          └──────────┘
              │
              │ notification /
              │ ask_user / ExitPlanMode
              │
              ▼
        ┌─────────────────┐
        │waiting_for_input│──── prompt-submit ──► thinking
        └─────────────────┘         (sticky)

        ┌─────────┐
        │completed│──── prompt-submit / session-start ──► thinking
        └─────────┘         (sticky)

        ┌────────────────┐
        │limits_exceeded │──── prompt-submit / session-start ──► thinking
        └────────────────┘         (sticky)

        ┌─────────┐
        │blocked  │──── prompt-submit / session-start / message ──► thinking
        └─────────┘         (sticky — agent-set, prevents false stalled)

        ┌────────┐
        │stalled │  (set by platform when heartbeat present but no activity events)
        └────────┘

        ┌─────────┐
        │offline  │  (set by platform when no heartbeat — broker may be disconnected)
        └─────────┘
```

**Values**: `idle`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `stalled`, `offline`


**Rules**:
- Activity is only set/meaningful when `phase = running`
- When phase transitions away from `running`, activity is cleared (set to empty)
- When phase transitions to `running`, activity defaults to `idle`
- **Sticky activities** (`waiting_for_input`, `blocked`, `completed`, `limits_exceeded`) resist being overwritten by `idle` or other transient states. They are only cleared by "new work" events (`prompt-submit`, `agent-start`, `session-start`)
- **`blocked`** is set by the agent itself (via `sciontool status blocked`) when it is intentionally waiting for an expected event — typically a child agent completing its task, or a scheduled event. Unlike `stalled`, `blocked` is a proactive declaration by the agent. It prevents the platform from falsely marking the agent as `stalled`. It is **not** a notification trigger. Cleared by any new work event from the agent.
- **`stalled`** is set by the platform when the sciontool heartbeat is still being received but no activity events have arrived within a configurable timeout. The agent process is alive but appears hung. Agents that have set `blocked` are excluded from stalled detection. Cleared by any activity event from the agent.
- **`offline`** is set by the platform when neither activity events nor the sciontool heartbeat have been received. The agent may still be running on a disconnected broker, so it could be doing work — the platform is simply blind to its current activity. The UI should prominently display the existing `lastHeartbeat` timestamp alongside the `offline` badge so users can gauge how long connectivity has been lost. Cleared by any event or heartbeat from the agent.

**Mapping from current implementation**:
| Current (sciontool UPPERCASE) | New Activity |
|---|---|
| `IDLE` | `idle` |
| `THINKING` | `thinking` |
| `EXECUTING` | `executing` |
| `WAITING_FOR_INPUT` | `waiting_for_input` |
| `COMPLETED` | `completed` |
| `LIMITS_EXCEEDED` | `limits_exceeded` |
| `STARTING` | (maps to phase `starting`, not activity) |
| `INITIALIZING` | (maps to phase `starting`, not activity) |
| `SHUTTING_DOWN` | (maps to phase `stopping`, not activity) |
| `EXITED` | (maps to phase `stopped`, not activity) |
| `ERROR` | (maps to phase `error`, not activity) |

| Current (Hub lowercase) | New Activity |
|---|---|
| `busy` | `thinking` or `executing` (see detail for disambiguation) |
| `idle` | `idle` |
| `waiting_for_input` | `waiting_for_input` |
| `completed` | `completed` |
| `limits_exceeded` | `limits_exceeded` |

### 3. Detail (Context Metadata)

The **detail** provides freeform context about the current activity. This is where harness-specific information lives — tool names, messages, task summaries. It is not enumerated and is always optional.

```go
type AgentDetail struct {
    ToolName    string `json:"toolName,omitempty"`    // Currently executing tool (e.g., "Bash", "Read")
    Message     string `json:"message,omitempty"`     // Human-readable description
    TaskSummary string `json:"taskSummary,omitempty"` // Current task being worked on
}
```

**Rules**:
- Detail is cleared when activity changes (except `message` may persist across transitions)
- `toolName` is only set when `activity = executing`
- `taskSummary` persists across activity changes (it describes the overall task, not the current step)
- Harness-specific metadata (e.g., Claude's tool input/output) is captured in telemetry, not in state detail

## Unified Data Model

### Go Types (Single Package)

All agent state types should live in a single shared package (`pkg/agent/state` or within `pkg/api/types.go`) to prevent the current duplication:

```go
package state

// Phase represents the infrastructure lifecycle phase of an agent.
type Phase string

const (
    PhaseCreated      Phase = "created"
    PhaseProvisioning Phase = "provisioning"
    PhaseCloning      Phase = "cloning"
    PhaseStarting     Phase = "starting"
    PhaseRunning      Phase = "running"
    PhaseStopping     Phase = "stopping"
    PhaseStopped      Phase = "stopped"
    PhaseError        Phase = "error"
)

// Activity represents what a running agent is doing.
// Only meaningful when Phase = PhaseRunning.
type Activity string

const (
    ActivityIdle            Activity = "idle"
    ActivityThinking        Activity = "thinking"
    ActivityExecuting       Activity = "executing"
    ActivityWaitingForInput Activity = "waiting_for_input"
    ActivityBlocked         Activity = "blocked"
    ActivityCompleted       Activity = "completed"
    ActivityLimitsExceeded  Activity = "limits_exceeded"
    ActivityStalled         Activity = "stalled"
    ActivityOffline        Activity = "offline"
)

// IsStickyActivity returns true if the activity resists being overwritten
// by normal event-driven updates.
func (a Activity) IsSticky() bool {
    switch a {
    case ActivityWaitingForInput, ActivityBlocked, ActivityCompleted, ActivityLimitsExceeded:
        return true
    }
    return false
}

// Detail provides freeform context about the current activity.
type Detail struct {
    ToolName    string `json:"toolName,omitempty"`
    Message     string `json:"message,omitempty"`
    TaskSummary string `json:"taskSummary,omitempty"`
}

// AgentState is the complete state representation.
type AgentState struct {
    Phase    Phase    `json:"phase"`
    Activity Activity `json:"activity,omitempty"` // Only when Phase = running
    Detail   Detail   `json:"detail,omitempty"`
}

// DisplayStatus returns a single human-readable status string for backward
// compatibility and simple display. This collapses the layered model back
// to a flat string when needed (e.g., CLI output, simple badges).
func (s AgentState) DisplayStatus() string {
    if s.Phase == PhaseRunning && s.Activity != "" {
        return string(s.Activity)
    }
    return string(s.Phase)
}
```

### Database Schema

The store model gains explicit fields for phase, activity, and detail, replacing the ambiguous single `status` field:

```sql
-- Agents table (key state columns)
ALTER TABLE agents ADD COLUMN phase TEXT NOT NULL DEFAULT 'created';
ALTER TABLE agents ADD COLUMN activity TEXT DEFAULT '';
ALTER TABLE agents ADD COLUMN tool_name TEXT DEFAULT '';
-- Existing columns retained:
-- status → deprecated, computed from phase+activity for backward compat
-- message, task_summary, container_status, runtime_state, connection_state
```

During migration, the existing `status` column is retained as a computed/denormalized field for backward compatibility with API consumers that haven't updated.

### API Representation

```json
{
  "id": "uuid",
  "name": "my-agent",
  "phase": "running",
  "activity": "executing",
  "detail": {
    "toolName": "Bash",
    "message": "Running tests",
    "taskSummary": "Implement auth module"
  },
  "status": "executing",
  "containerStatus": "Up 2 hours",
  "connectionState": "connected"
}
```

The `status` field is retained as a computed convenience field: `DisplayStatus()` — returns the activity if running, otherwise the phase. This provides backward compatibility for existing API consumers and simple UI badges.

### TypeScript Types

```typescript
export type AgentPhase =
  | 'created'
  | 'provisioning'
  | 'cloning'
  | 'starting'
  | 'running'
  | 'stopping'
  | 'stopped'
  | 'error';

export type AgentActivity =
  | 'idle'
  | 'thinking'
  | 'executing'
  | 'waiting_for_input'
  | 'blocked'
  | 'completed'
  | 'limits_exceeded'
  | 'stalled'
  | 'offline';

export interface AgentDetail {
  toolName?: string;
  message?: string;
  taskSummary?: string;
}

export interface Agent {
  id: string;
  name: string;
  phase: AgentPhase;
  activity?: AgentActivity;
  detail?: AgentDetail;
  status: string; // Computed: activity ?? phase (backward compat)
  // ...
}
```

### SSE Event Payloads

SSE events currently send a flat `{ status, containerStatus }` payload. The new model enriches this:

```json
{
  "subject": "grove.{groveId}.agent.status",
  "data": {
    "agentId": "uuid",
    "groveId": "uuid",
    "phase": "running",
    "activity": "executing",
    "detail": {
      "toolName": "Bash",
      "message": "Running tests"
    },
    "status": "executing"
  }
}
```

## Sciontool ↔ Hub Translation

The sciontool hooks inside the container continue to produce normalized events. The translation to the layered model happens at two points:

### 1. Local Status Handler (agent-info.json)

The `StatusHandler` writes structured state to `agent-info.json`:

```json
{
  "phase": "running",
  "activity": "executing",
  "toolName": "Bash",
  "message": "Running tests"
}
```

The existing `eventToState()` mapping splits into phase and activity:

| Event | Phase Effect | Activity Effect |
|---|---|---|
| `pre-start` | → `starting` | clear |
| `post-start` | → `running` | → `idle` |
| `session-start` | (none) | → `idle` (clears sticky) |
| `prompt-submit` | (none) | → `thinking` (clears sticky) |
| `agent-start` | (none) | → `thinking` (clears sticky) |
| `model-start` | (none) | → `thinking` (respects sticky) |
| `model-end` | (none) | → `idle` (respects sticky) |
| `tool-start` | (none) | → `executing` + set toolName (respects sticky*) |
| `tool-end` | (none) | → `idle` + clear toolName (respects sticky) |
| `agent-end` | (none) | → `idle` (respects sticky) |
| `notification` | (none) | → `waiting_for_input` (sets sticky; keeps `limits_exceeded`, `crashed` and an agent-set wait) |
| `pre-stop` | → `stopping` | clear |
| `session-end` | → `stopped` | clear |

*\*Tool-start clears `waiting_for_input` (user responded) but preserves `completed`/`limits_exceeded`.*

### 2. Hub Handler (Status Reports)

The `HubHandler` maps the local state model to Hub status updates:

| Local State | Hub Update |
|---|---|
| phase=`starting` | `phase: starting` |
| phase=`running`, activity=`idle` | `activity: idle` |
| phase=`running`, activity=`thinking` | `activity: thinking` |
| phase=`running`, activity=`executing` | `activity: executing`, `detail.toolName: X` |
| phase=`running`, activity=`waiting_for_input` | `activity: waiting_for_input` |
| phase=`running`, activity=`completed` | `activity: completed` |
| phase=`stopping` | `phase: stopping` |
| phase=`stopped` | `phase: stopped` |

The Hub handler no longer needs to collapse `thinking`/`executing` into `busy` — it reports the actual activity.

## Notification Integration

The notification system currently triggers on status values like `COMPLETED`, `WAITING_FOR_INPUT`, `LIMITS_EXCEEDED`. Under the new model:

- **Trigger conditions** are expressed as activity values: `completed`, `waiting_for_input`, `limits_exceeded`, `stalled`, `offline` (note: `blocked` is intentionally **not** a notification trigger — it represents expected idle time, not an anomaly)
- **Notification subscriptions** store `triggerActivities` (renamed from `triggerStatuses`)
- The normalization issue (UPPERCASE vs lowercase) is resolved since everything uses lowercase activity values

## UI Impact

### Status Badge

The status badge component can be simplified. Instead of a flat `StatusType` with 17+ values, it renders based on phase and activity:

- **Non-running phases**: Show phase directly (provisioning → pulsing yellow, stopped → gray, error → red)
- **Running + activity**: Show activity (idle → green, thinking → blue pulse, executing → blue pulse with tool name, waiting_for_input → amber, completed → green checkmark)

Each badge should include a tooltip-style popover with a human-readable description of the current state (e.g., "Agent is waiting for user input" or "Agent is executing the Bash tool"). This helps users who are unfamiliar with the state model understand what's happening.

### Terminal Availability

Terminal availability logic becomes clearer:

```typescript
function isTerminalAvailable(phase: AgentPhase): boolean {
  return phase === 'running' || phase === 'stopping';
}
```

No need to reason about which "status" values imply a running container.

**CLI `attach` fix**: The CLI `attach` command currently has a broken pre-check that rejects agents with `status: idle` even when the container is running. Under the new model, `attach` should check `phase` (not activity/status), which eliminates this bug — an agent with `phase: running, activity: idle` is clearly attachable.

### Agent List/Dashboard

The dashboard can show richer information:
- Phase badge (lifecycle indicator)
- Activity indicator (what the agent is doing now)
- Tool name tooltip when executing
- Task summary as secondary text

## Stalled and Offline Detection

Two platform-set activities are introduced for agents whose state cannot be determined through normal event flow. Both are set by the platform (not by the agent itself) and are implemented as scheduled jobs via the existing scheduler system.

### Stalled (heartbeat present, no activity)

An agent is `stalled` when the sciontool heartbeat is still being received (the process is alive) but no activity events have arrived within a configurable timeout. This typically means the agent process is hung or blocked.

- **Detection**: A scheduled job checks `lastActivityEvent` for all agents with `phase = running`. If the timestamp exceeds the configured threshold (default: 5 minutes, configurable as a global Hub server setting) and a recent heartbeat has been received, and `activity` is not a terminal sticky state (`completed`, `limits_exceeded`), the scheduler sets `activity = stalled`.
- **Recovery**: Any activity event from the agent clears `stalled` and sets the appropriate activity.
- **Notification**: `stalled` is a notification trigger, enabling users to investigate hung agents.

### Offline (no heartbeat, broker may be disconnected)

An agent is `offline` when neither activity events nor the sciontool heartbeat have been received. The agent may still be running and doing work on a broker that has become disconnected — the platform is simply blind to its current activity.

- **Detection**: A scheduled job checks `lastHeartbeat` for all agents with `phase = running`. If the timestamp exceeds the heartbeat timeout threshold and `activity` is not a terminal sticky state, the scheduler sets `activity = offline`.
- **Refinement**: The system may additionally check broker connectivity status. If the broker itself is unreachable, this strengthens the `offline` classification. If the broker is connected but the agent has no heartbeat, this may indicate the agent process crashed — but the system does **not** auto-escalate to `phase = error` to avoid false positives (see Resolved Design Decisions). This is noted as a potential future improvement once heartbeat reliability is proven.
- **Recovery**: Any event or heartbeat from the agent clears `offline` and restores the appropriate activity. Notably, any non-heartbeat event (activity update, tool event, etc.) also clears `offline` immediately — if the platform received a real event from the agent, it is clearly not offline regardless of heartbeat state.
- **Flapping**: No hysteresis is applied to heartbeat-based transitions. Intermittent heartbeats (e.g., network flapping) may cause rapid transitions between `stalled` and `offline`. This is acceptable for now; hysteresis-based debouncing is a potential future improvement if flapping proves disruptive in practice.
- **Notification**: `offline` is a notification trigger, enabling users to investigate connectivity issues.
- **UI**: The `offline` badge should prominently display the existing `lastHeartbeat` timestamp (e.g., "Offline — last seen 12 minutes ago") so users can assess severity and decide whether to investigate. No new API field is needed — the existing heartbeat timestamp already serves this purpose.

This replaces the previously discussed but unimplemented "stale/stalled detection" from the notifications design.

## Implementation Plan

### Phase 1: Define Canonical Types ✅

1. ~~Create `pkg/agent/state/state.go` with the canonical `Phase`, `Activity`, `Detail`, and `AgentState` types~~
2. ~~Add `DisplayStatus()` for backward-compatible flat status~~
3. ~~Add validation functions (`Phase.IsValid()`, `Activity.IsValid()`, `Activity.IsSticky()`)~~
4. ~~Add tests for the state model~~

### Phase 2: Refactor Sciontool (Container-Side) ✅

1. ~~Update `pkg/sciontool/hooks/types.go` to import and use `pkg/agent/state` types~~
2. ~~Refactor `StatusHandler` to write `phase` + `activity` to `agent-info.json`~~
3. ~~Refactor `HubHandler` to send structured `phase`/`activity` updates~~
4. ~~Update `pkg/sciontool/hub/client.go` to use canonical types~~
5. ~~Remove the duplicate `AgentState` and `AgentStatus` type definitions~~

### Phase 3: Refactor Hub and Store ✅

1. ~~Add `phase`, `activity`, `tool_name` columns to the agents table~~
2. ~~Update `AgentStatusUpdate` struct to accept `Phase`/`Activity`/`Detail`~~
3. ~~Update `UpdateAgentStatus()` to write new columns~~
4. ~~Compute `status` (flat) from `phase`+`activity` for backward compat~~
5. ~~Update SSE event payloads to include `phase`/`activity`/`detail`~~
6. ~~Update the ent schema to match (or fully replace ent status enum with the new phase enum)~~
7. ~~Rename `MarkStaleAgentsUndetermined` → `MarkStaleAgentsOffline` (uses phase/activity)~~
8. ~~Update broker heartbeat handler to set phase alongside status~~
9. ~~Update notification system to match on activity when available~~
10. ~~Add `AgentDetail` to API types and `ToAPI()` conversion~~

### Phase 4: Refactor Runtime Broker ✅

1. ~~Update `pkg/runtimebroker/types.go` to use canonical types~~
2. ~~Update heartbeat payload to report `phase`/`activity` alongside `status`~~
3. ~~Update hub heartbeat handler to prefer structured `phase`/`activity` fields~~
4. ~~Update `pkg/agent/list.go` to propagate Phase/Activity from agent-info.json~~
5. ~~Remove duplicate `AgentStatus*` constants from runtimebroker~~

### Phase 5: Refactor Web Frontend ✅

1. ~~Update `web/src/shared/types.ts` with `AgentPhase`, `AgentActivity`, `AgentDetail`~~
2. ~~Update state manager to handle structured state deltas (not needed — delta merge already spreads all fields)~~
3. ~~Update status badge to render phase + activity~~
4. ~~Update terminal availability check~~
5. ~~Update agent detail page, agent list, grove detail, broker detail, terminal, agent-create~~

### Phase 6: Cleanup and Documentation ✅

1. ✅ Remove all duplicate status constant definitions across the codebase
2. ✅ Remove the deprecated flat `status` field from the API and database
3. ✅ Update notification subscriptions to use `triggerActivities`
4. ✅ Update design docs to reference the new model
5. ✅ Update the docs-site with the new state model

## Backward Compatibility

The flat `status` field has been fully removed. All consumers now use the layered `phase`/`activity`/`detail` model exclusively. The `DisplayStatus()` helper remains available for cases where a single display string is needed (shows activity when running, phase otherwise).

## Resolved Design Decisions

1. **`cloning` is a separate phase** (not a sub-phase of `provisioning`). This keeps `provisioning` purely about the container, while `cloning` covers git workspace preparation. Both are visible as distinct lifecycle steps.

2. **`stalled` is an activity** (not a separate boolean flag). When an agent becomes stalled, the last known activity is preserved in `detail.message` for diagnostic context.

3. **`thinking` means the LLM is actively processing/generating.** This is the narrow definition — `thinking` maps to the LLM API call itself, not to the broader "agent turn is active" concept. For Claude Code, which doesn't fire `model-start`/`model-end` events, `thinking` is the default activity between `prompt-submit`/`agent-start` and the first `tool-start`, and between `tool-end` and the next `tool-start` or `agent-end`.

4. **Soft-delete is not a phase.** `deleted` and `restored` are not lifecycle states. Soft-delete uses a `deletedAt` timestamp. Restoring an agent clears the soft-delete flag and returns the agent to its prior phase (typically `stopped`). The `restored` value is removed entirely — it was always just the act of returning to a normal state.

5. **`offline` (not `undetermined`) for missing heartbeat/events.** The activity is named `offline` because it's the most intuitive term for users. While the agent *could* still be running on a disconnected broker, `offline` clearly signals "we can't see it" — the existing `lastHeartbeat` timestamp displayed alongside in the UI gives users the context to judge severity. Alternatives considered: `undetermined` (too vague to act on), `unreachable` (implies a network problem specifically).

6. **No auto-escalation from `offline` to `error`.** Even when a broker is confirmed connected but an agent on it has no heartbeat, the system does not automatically set `phase = error`. Auto-escalation risks false positives if the heartbeat mechanism itself has issues. This is noted as a potential future improvement once heartbeat reliability is proven across deployments and edge cases are better understood.

7. **Stalled/offline detection runs via the scheduler system.** Detection jobs are first-class scheduled jobs in the existing scheduler, not ad-hoc goroutine timers. This provides consistency with other periodic platform tasks, configurability (threshold tuning), and observability (job execution logs). The scheduler must be operational for detection to function.

8. **Claude Code `thinking` inference is acceptable.** Since Claude Code doesn't emit `model-start`/`model-end` events, `thinking` is inferred from the absence of tool execution during an active agent turn. This is reliable enough for now. If Claude Code adds native model lifecycle events in the future, the inference logic can be simplified to use them directly.

9. **`lastSeen` uses the existing heartbeat timestamp.** The existing `lastHeartbeat` timestamp already recorded in the agent state serves as the `lastSeen` value. No new API field is needed — the current UI already displays a valid "last seen" indicator from this data. The `offline` badge simply renders the existing timestamp more prominently.

10. **`stalled` threshold is a global server config setting.** The stalled detection threshold is a global Hub server configuration value (not per-grove or per-agent), with a default of 5 minutes. This keeps configuration simple and consistent. If per-agent tuning proves necessary in the future, it can be added as an override.

11. **Heartbeat flapping is allowed; any agent event clears `offline`.** No hysteresis is applied to heartbeat-based state transitions — the system allows rapid flapping between `stalled` and `offline` if heartbeats arrive intermittently. This is acceptable for now. Importantly, any non-heartbeat event from an agent (activity update, tool event, etc.) clears `offline` status immediately, even if heartbeats are still missing. The reasoning: if we received an event from the agent, it is clearly not offline regardless of heartbeat state. Implementation should include code comments noting that hysteresis-based debouncing is a potential future improvement if flapping proves disruptive in practice.

12. **No fallback for scheduler-based detection.** Stalled/offline detection relies exclusively on the scheduler system. No in-process fallback timer is implemented. The scheduler will have its own internal health check as part of the overall Hub health status. Chaining separate health-check mechanisms adds complexity without proportional benefit at this stage.