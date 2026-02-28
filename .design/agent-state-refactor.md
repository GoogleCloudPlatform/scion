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
│     idle | thinking | executing | waiting_for_input |               │
│     completed | limits_exceeded | stalled                           │
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
| `deleted` | (soft-delete flag, not a phase) |
| `restored` | (restore clears soft-delete, sets `stopped`) |

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

        ┌────────┐
        │stalled │  (set by platform when no events received within timeout)
        └────────┘
```

**Values**: `idle`, `thinking`, `executing`, `waiting_for_input`, `completed`, `limits_exceeded`, `stalled`

**Rules**:
- Activity is only set/meaningful when `phase = running`
- When phase transitions away from `running`, activity is cleared (set to empty)
- When phase transitions to `running`, activity defaults to `idle`
- **Sticky activities** (`waiting_for_input`, `completed`, `limits_exceeded`) resist being overwritten by `idle` or other transient states. They are only cleared by "new work" events (`prompt-submit`, `agent-start`, `session-start`)
- `stalled` is set by the platform (hub/broker) when no heartbeat or event has been received within a configurable timeout. It is cleared by any event from the agent.

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
    ActivityCompleted       Activity = "completed"
    ActivityLimitsExceeded  Activity = "limits_exceeded"
    ActivityStalled         Activity = "stalled"
)

// IsStickyActivity returns true if the activity resists being overwritten
// by normal event-driven updates.
func (a Activity) IsSticky() bool {
    switch a {
    case ActivityWaitingForInput, ActivityCompleted, ActivityLimitsExceeded:
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
  | 'completed'
  | 'limits_exceeded'
  | 'stalled';

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
| `notification` | (none) | → `waiting_for_input` (sets sticky) |
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

- **Trigger conditions** are expressed as activity values: `completed`, `waiting_for_input`, `limits_exceeded`, `stalled`
- **Notification subscriptions** store `triggerActivities` (renamed from `triggerStatuses`)
- The normalization issue (UPPERCASE vs lowercase) is resolved since everything uses lowercase activity values

## UI Impact

### Status Badge

The status badge component can be simplified. Instead of a flat `StatusType` with 17+ values, it renders based on phase and activity:

- **Non-running phases**: Show phase directly (provisioning → pulsing yellow, stopped → gray, error → red)
- **Running + activity**: Show activity (idle → green, thinking → blue pulse, executing → blue pulse with tool name, waiting_for_input → amber, completed → green checkmark)

### Terminal Availability

Terminal availability logic becomes clearer:

```typescript
function isTerminalAvailable(phase: AgentPhase): boolean {
  return phase === 'running' || phase === 'stopping';
}
```

No need to reason about which "status" values imply a running container.

### Agent List/Dashboard

The dashboard can show richer information:
- Phase badge (lifecycle indicator)
- Activity indicator (what the agent is doing now)
- Tool name tooltip when executing
- Task summary as secondary text

## Stalled Detection

A new `stalled` activity is introduced for agents that haven't reported events within a configurable timeout. This is set by the platform, not the agent itself:

- **Detection**: The Hub checks `lastSeen` during heartbeat processing. If `lastSeen` is older than a configured threshold (e.g., 5 minutes) and `phase = running` and `activity` is not a terminal sticky state (`completed`, `limits_exceeded`), the Hub sets `activity = stalled`.
- **Recovery**: Any event from the agent clears `stalled` and sets the appropriate activity.
- **Notification**: `stalled` can be a notification trigger, enabling users to investigate hung agents.

This replaces the previously discussed but unimplemented "stale/stalled detection" from the notifications design.

## Implementation Plan

### Phase 1: Define Canonical Types

1. Create `pkg/agent/state/state.go` with the canonical `Phase`, `Activity`, `Detail`, and `AgentState` types
2. Add `DisplayStatus()` for backward-compatible flat status
3. Add validation functions (`Phase.IsValid()`, `Activity.IsValid()`, `Activity.IsSticky()`)
4. Add tests for the state model

### Phase 2: Refactor Sciontool (Container-Side)

1. Update `pkg/sciontool/hooks/types.go` to import and use `pkg/agent/state` types
2. Refactor `StatusHandler` to write `phase` + `activity` to `agent-info.json`
3. Refactor `HubHandler` to send structured `phase`/`activity` updates
4. Update `pkg/sciontool/hub/client.go` to use canonical types
5. Remove the duplicate `AgentState` and `AgentStatus` type definitions

### Phase 3: Refactor Hub and Store

1. Add `phase`, `activity`, `tool_name` columns to the agents table
2. Update `AgentStatusUpdate` struct to accept `Phase`/`Activity`/`Detail`
3. Update `UpdateAgentStatus()` to write new columns
4. Compute `status` (flat) from `phase`+`activity` for backward compat
5. Update SSE event payloads to include `phase`/`activity`/`detail`
6. Update the ent schema to match (or fully replace ent status enum with the new phase enum)

### Phase 4: Refactor Runtime Broker

1. Update `pkg/runtimebroker/types.go` to use canonical types
2. Update heartbeat payload to report `phase` instead of `status`
3. Update heartbeat handler to map container status → phase

### Phase 5: Refactor Web Frontend

1. Update `web/src/shared/types.ts` with `AgentPhase`, `AgentActivity`, `AgentDetail`
2. Update state manager to handle structured state deltas
3. Update status badge to render phase + activity
4. Update terminal availability check
5. Update agent detail page, agent list, dashboard

### Phase 6: Cleanup

1. Remove all duplicate status constant definitions across the codebase
2. Remove the deprecated flat `status` field from the API (major version bump, or keep as computed)
3. Update notification subscriptions to use `triggerActivities`
4. Update design docs to reference the new model

## Backward Compatibility

The `status` field is retained as a computed convenience:

```go
func (s AgentState) DisplayStatus() string {
    if s.Phase == PhaseRunning && s.Activity != "" {
        return string(s.Activity)
    }
    return string(s.Phase)
}
```

This means:
- Existing API consumers that read `status` continue to work
- The value they see becomes more precise (e.g., `thinking` instead of `busy`, `executing` instead of `busy`)
- New API consumers can use `phase`/`activity`/`detail` for richer state information
- The `busy` value is retired — consumers see the actual activity

## Open Questions

1. **Should `cloning` be a sub-phase of `provisioning`?** Currently `cloning` is a distinct lifecycle step, but it only happens during provisioning. It could be modeled as `phase: provisioning, detail.message: "Cloning repository"` instead of a separate phase. Keeping it separate provides better visibility.

2. **Should `stalled` be an activity or a separate flag?** Making it an activity means it overwrites the last known activity (`thinking`, `executing`). A separate `isStalled` boolean would preserve the last activity. The trade-off is complexity vs information preservation. Recommendation: keep as activity for simplicity, preserve last activity in `detail.message`.

3. **Granularity of `thinking` vs `executing`**: Claude Code doesn't distinguish model calls from tool calls as cleanly as Gemini does. The `model-start`/`model-end` events from Gemini give true LLM API call boundaries, while Claude fires `prompt-submit` → (`tool-start`/`tool-end`)* → `agent-end`. Should `thinking` specifically mean "LLM is generating" or more broadly "agent is processing"? Recommendation: `thinking` = agent turn is active but no tool is running; `executing` = a tool is actively running.

4. **Soft-delete representation**: `deleted`/`restored` are currently status values. In the new model, soft-delete is a separate concern (a `deletedAt` timestamp), not a phase. This is already partially implemented. Confirm this approach.
