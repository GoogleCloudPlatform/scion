# Design: Agent Limits (max_turns & max_duration)

## 1. Overview

Agents need configurable limits on how long they run and how many LLM turns they consume. These limits act as safety guardrails preventing runaway agents from consuming unbounded resources. When a limit is exceeded, the agent must transition to a distinct `LIMITS_EXCEEDED` state with clear logging, then exit cleanly.

**Scope:** This document covers the enforcement of `max_turns` and `max_duration` settings, the new `LIMITS_EXCEEDED` state, and the container exit behavior when limits are hit.

## 2. Current State

### Configuration

Both settings are already defined in the configuration layer:

- **Schema** (`agent-v1.schema.json`): `max_turns` (integer, min 1) and `max_duration` (string, pattern `^[0-9]+(s|m|h)$`), both gated on `schema_version: "1"`.
- **Go struct** (`pkg/api/types.go`): `ScionConfig.MaxTurns` and `ScionConfig.MaxDuration` with a `ParseMaxDuration()` helper.
- **Environment injection** (`pkg/agent/run.go:280-286`): Both are injected into the container as `SCION_MAX_TURNS` and `SCION_MAX_DURATION`.

### Duration Enforcement (Host-Side, Incomplete)

A rudimentary duration timer exists in `pkg/agent/run.go:542-558`. It spawns a goroutine on the host that calls `rt.Stop()` after the configured duration. This approach has significant drawbacks:

1. **No status update** -- the agent's state is not set before the container is killed, so the exit reason is opaque to users and the Hub.
2. **No agent-side logging** -- nothing is written to `agent.log` explaining why the agent stopped.
3. **No Hub notification** -- the Hub sees the agent transition from `running`/`idle` directly to `stopped`, with no indication that a limit was the cause.
4. **Host-only** -- only works when the host process is alive; does not work in hosted/Kubernetes environments where the runtime broker may not keep the goroutine running.

### Turn Enforcement (Not Implemented)

No turn counting or enforcement exists. The session parser (`pkg/sciontool/hooks/session/parser.go`) tracks `TurnCount` for metrics, but this is a post-hoc analysis tool, not a live enforcement mechanism.

## 3. Design

### 3.1. Principle: Enforce Inside the Container

All limit enforcement moves into `sciontool`, which runs as PID 1 inside the container. This is the correct enforcement point because:

- **sciontool is already the supervisor**: It manages the child process lifecycle, handles signals, and controls container exit. Limit enforcement is a natural extension of this role.
- **sciontool already receives hook events**: The harness (Claude Code, Gemini CLI) sends events to `sciontool hook` on every turn, tool call, and session event. Turn counting piggybacks on this existing event stream.
- **sciontool already manages status**: It writes `agent-info.json`, logs to `agent.log`, and reports to the Hub. Limit-exceeded reporting uses the same channels.
- **Works everywhere**: Inside the container, enforcement is runtime-agnostic. It works identically on Docker, Kubernetes, and Apple Virtualization.

The existing host-side `startDurationTimer` in `pkg/agent/run.go` should be removed once sciontool enforcement is in place.

### 3.2. New Agent State: `LIMITS_EXCEEDED`

A new terminal state is added alongside `COMPLETED` and `ERROR`:

```go
// In pkg/sciontool/hooks/types.go
StateLimitsExceeded AgentState = "LIMITS_EXCEEDED"
```

```go
// In pkg/sciontool/hub/client.go
StatusLimitsExceeded AgentStatus = "limits_exceeded"
```

This state is **sticky** (like `COMPLETED`), meaning subsequent events from the dying harness process cannot overwrite it. The `isStickyStatus` function in `handlers/status.go` must include `LIMITS_EXCEEDED`.

### 3.3. Duration Enforcement

#### Mechanism

When `sciontool init` starts, it reads `SCION_MAX_DURATION` from the environment and starts an internal timer. When the timer fires:

1. Log the event to `agent.log`.
2. Set agent status to `LIMITS_EXCEEDED` in `agent-info.json`.
3. Report `limits_exceeded` to the Hub (if configured) with a descriptive message.
4. Send `SIGTERM` to the child process group (the harness).
5. Wait for the configured grace period (same as normal shutdown).
6. If the child has not exited, send `SIGKILL`.
7. `sciontool` exits with a **distinct exit code** (see Section 3.5).

#### Timer Start Point

The timer starts when the child process (harness) is successfully launched -- after `post-start` hooks complete. This means `max_duration` measures the agent's active working time, not container boot overhead. The duration timer resets on resume (each run gets a fresh budget).

#### Implementation Location

The duration timer lives in `cmd/sciontool/commands/init.go`, integrated into the existing `runInit` flow. It is implemented as an additional case in the `select` that currently waits on `exitChan`:

```go
// Pseudocode for the wait loop in runInit()
var durationTimer <-chan time.Time
if maxDur := parseDurationEnv(); maxDur > 0 {
    t := time.NewTimer(maxDur)
    defer t.Stop()
    durationTimer = t.C
}

select {
case result := <-exitChan:
    // Normal child exit (existing behavior)
case <-durationTimer:
    // Max duration exceeded -- initiate limit-exceeded shutdown
    handleLimitsExceeded("duration", fmt.Sprintf("max_duration of %s exceeded", os.Getenv("SCION_MAX_DURATION")))
}
```

### 3.4. Turn and Model Call Enforcement

#### What Counts as a Turn vs a Model Call

Two separate counters are maintained, each with its own configurable limit:

- **Turns** (`max_turns`): Counted on `agent-end` events. One "turn" is one full cycle of the LLM receiving input and producing a response, which may include many tool calls and sub-agent spawns. This controls **autonomy scope** -- how many high-level actions the agent can take.
- **Model calls** (`max_model_calls`): Counted on `model-end` events. Each `model-end` corresponds to a single LLM API inference call. This controls **cost** -- directly correlating with API usage.

Both counters are tracked independently. If either limit is hit, the agent transitions to `LIMITS_EXCEEDED`. For harnesses that don't emit `agent-end` events consistently, the `model-end` counter provides a reliable fallback enforcement mechanism.

#### Resume Behavior

All limit counters (turn count, model call count, and duration timer) reset when an agent is resumed. Each "run" gets a fresh budget. This matches the expected usage pattern where each resume typically brings a new task or continuation of work with renewed resource allocation.

#### Mechanism

Turn and model call counting is implemented as a new handler registered in the hook event pipeline. Since `sciontool hook` is invoked as a separate process for each event (it is not a long-running daemon), the counts must be persisted to disk between invocations.

**Limit state file**: `~/agent-limits.json`

```json
{
  "turn_count": 17,
  "model_call_count": 42,
  "max_turns": 50,
  "max_model_calls": 200,
  "started_at": "2026-02-22T10:30:00Z"
}
```

When a count-incrementing event is received and the count meets or exceeds the corresponding limit:

1. Set agent status to `LIMITS_EXCEEDED` in `agent-info.json`.
2. Write a clear log entry to `agent.log`.
3. Signal PID 1 (sciontool init): write the trigger file `/tmp/scion-limits-exceeded` holding the limit message, then send `SIGUSR1` as a fallback.
4. Report the updated counts to the Hub (if configured).

The hook does not report `limits_exceeded` to the Hub itself. Init does that when it sees the trigger file or `SIGUSR1`, using the message from the trigger file (ptone/scion#3610). The hook runs under the harness's hook timeout (antigravity 10s, opencode and grok-build 5s), so its Hub calls share one per-process budget (`hookHubBudget`, 3s), and the local actions above come before any Hub call so an unreachable Hub never delays shutdown. The calls share the budget in order, so a slow earlier call (the status update) can use it up and the later ones (counts, session-end metrics) are then dropped; this is accepted because `limits_exceeded` and the final stop status come from init.

#### Signaling the Init Process

The `sciontool hook` process needs to tell the init process (PID 1) to begin shutdown. This is done by sending `SIGUSR1` to PID 1. The init process registers a `SIGUSR1` handler that initiates the same limit-exceeded shutdown sequence used for duration limits.

The reason for using a signal rather than having the hook process directly kill the harness: the init process owns the child lifecycle and the graceful shutdown sequence. The hook process should only request shutdown, not perform it.

```
┌──────────────────┐     SIGUSR1      ┌──────────────────┐
│  sciontool hook   │ ──────────────▶  │  sciontool init   │
│  (turn counter)   │                  │  (PID 1)          │
│                   │                  │                    │
│  Detects limit    │                  │  Receives signal   │
│  Sets status      │                  │  Logs reason       │
│  Logs event       │                  │  SIGTERM → child   │
│  Trigger file +   │                  │  Reports to Hub    │
│  SIGUSR1          │                  │  Waits grace, exits│
└──────────────────┘                  └──────────────────┘
```

#### Initialization

On `pre-start` or `post-start`, the init process initializes `agent-limits.json` with the configured values from the environment, setting `turn_count` to 0. This ensures the state file exists before the first hook event arrives.

### 3.5. Exit Codes

When sciontool exits due to a limit being exceeded, it uses a distinct exit code so that the host-side orchestrator (scion CLI or runtime broker) can distinguish limit-exceeded exits from normal completion or errors:

| Exit Code | Meaning |
|-----------|---------|
| 0         | Normal exit (harness exited successfully) |
| 1         | Error (harness crashed or sciontool error) |
| 10        | Limits exceeded (max_turns or max_duration) |

The exit code is propagated through the container runtime and is available to the host via `scion list` or the Hub API.

### 3.6. Logging

All limit-related events produce structured log entries in `agent.log` using the existing `log` package. The log entries must be unambiguous about what happened and why.

#### When Duration Limit is Hit

```
2026-02-22T14:30:00Z [sciontool] [INFO] [LIMITS_EXCEEDED] Agent stopped: max_duration of 2h exceeded (started at 2026-02-22 12:30:00)
```

#### When Turn Limit is Hit

```
2026-02-22T14:30:00Z [sciontool] [INFO] [LIMITS_EXCEEDED] Agent stopped: max_turns of 50 exceeded (completed 50 turns)
```

#### When Neither Limit is Configured

No limit-related logging occurs. The agent runs until the harness exits naturally.

### 3.7. Hub Reporting

When reporting to the Hub, the status update includes both the new status and a descriptive message:

```go
hubClient.UpdateStatus(ctx, hub.StatusUpdate{
    Status:  hub.StatusLimitsExceeded,
    Message: "max_duration of 2h exceeded",
})
```

This allows the Hub UI and API consumers to display the reason the agent stopped. A new `ReportLimitsExceeded` convenience method is added to the Hub client alongside the existing `ReportError`, `ReportStopped`, etc.

Limit status is also incorporated into the sciontool heartbeat/status update cycle to the Hub. The separate `agent-limits.json` file stores the raw counters locally, but the limit-exceeded status is communicated to the Hub via the standard status update mechanism (same endpoint used by heartbeats and other status changes).

### 3.8. Interaction with Existing States

The `LIMITS_EXCEEDED` state has specific interactions with the status system:

- **Sticky**: Once set, it cannot be overwritten by normal event-driven updates (same as `COMPLETED`). This prevents the harness's dying events from overwriting the limit status.
- **Overrides `COMPLETED`**: If a harness happens to report task completion in the same moment a limit fires, `LIMITS_EXCEEDED` takes priority. The limit is the authoritative reason for shutdown.
- **Does not override `ERROR`**: If the agent is already in an error state, the limit status is not applied. The original error is more important to preserve.
- **Hub shutdown sequence**: After `LIMITS_EXCEEDED` is set and the child exits, the normal shutdown sequence (`shutting_down` → `stopped`) still runs on the Hub side. The `LIMITS_EXCEEDED` status is preserved in `agent-info.json` for the local `scion list` display.

## 4. Implementation Plan

### Phase 1: State and Status Infrastructure ✓

1. ✓ Add `StateLimitsExceeded` to `pkg/sciontool/hooks/types.go`.
2. ✓ Add `StatusLimitsExceeded` to `pkg/sciontool/hub/client.go` with a `ReportLimitsExceeded` method.
3. ✓ Update `isStickyStatus` in `handlers/status.go` to include `LIMITS_EXCEEDED`.
4. ✓ Update hub handler's tool-start check to also skip `LIMITS_EXCEEDED` (not just `COMPLETED`).
5. ✓ Add a `sciontool status limits_exceeded` subcommand (for manual testing and potential future use by custom harnesses).

### Phase 2: Duration Enforcement in sciontool init

1. In `cmd/sciontool/commands/init.go`, read `SCION_MAX_DURATION` and start a timer after post-start hooks complete.
2. Add `SIGUSR1` handler to `init.go` that triggers the limit-exceeded shutdown path.
3. Implement `handleLimitsExceeded(limitType, message string)` that performs the status update, logging, Hub reporting, and child termination sequence.
4. Exit with code 10 on limit-exceeded shutdown.

### Phase 3: Turn and Model Call Enforcement via Hook Handler

1. Create `pkg/sciontool/hooks/handlers/limits.go` containing a `LimitsHandler` that:
   - Reads `SCION_MAX_TURNS` and `SCION_MAX_MODEL_CALLS` from the environment on construction.
   - Maintains turn count and model call count in `~/agent-limits.json`.
   - Increments turn count on `agent-end` events.
   - Increments model call count on `model-end` events.
   - When either limit is reached: updates status, logs, writes the trigger file and sends `SIGUSR1` to PID 1. Init reports `limits_exceeded` to the Hub (see §3.4).
2. Register `LimitsHandler` in the hook event pipeline in `cmd/sciontool/commands/hook.go`.
3. Initialize `agent-limits.json` during `post-start` in `init.go` (counters reset on each start/resume).

### Phase 4: Remove Host-Side Timer

1. Remove `startDurationTimer` from `pkg/agent/run.go`.
2. Remove the call site in the `Run` function.
3. The env var injection (`SCION_MAX_TURNS`, `SCION_MAX_DURATION`) remains -- these are how the configuration reaches sciontool.

### Phase 5: Host-Side Status Display

1. Update `scion list` / `scion look` to recognize and display the `LIMITS_EXCEEDED` state clearly (distinct from `COMPLETED` or `ERROR`).
2. Update the Hub UI (if applicable) to display limit-exceeded agents with appropriate messaging.

## 5. Configuration Examples

### In scion-agent.yaml (Template)

```yaml
schema_version: "1"
max_turns: 100
max_model_calls: 500
max_duration: "4h"
```

### Per-Agent Override

```yaml
# .scion/agents/my-agent/scion-agent.yaml
schema_version: "1"
max_turns: 25
max_model_calls: 100
max_duration: "30m"
```

### No Limits (Default)

When none of `max_turns`, `max_model_calls`, or `max_duration` is configured, no enforcement occurs. The agent runs until the harness exits naturally or is stopped manually.

## 6. Testing Strategy

### Unit Tests

- **LimitsHandler**: Test turn counting, model call counting, file persistence, limit detection, and SIGUSR1 signaling (mock the signal send).
- **Status updates**: Verify `LIMITS_EXCEEDED` is sticky and interacts correctly with other states.
- **Duration parsing**: Verify `SCION_MAX_DURATION` env var is parsed correctly for various formats.
- **Exit codes**: Verify sciontool exits with code 10 on limit-exceeded.

### Integration Tests

- **Duration limit**: Start an agent with `max_duration: "5s"`, verify it stops with `LIMITS_EXCEEDED` status and exit code 10.
- **Turn limit**: Start an agent with `max_turns: 3`, verify it stops after 3 turns with correct status.
- **Model call limit**: Start an agent with `max_model_calls: 10`, verify it stops after 10 model calls with correct status.
- **No limits**: Start an agent with no limits configured, verify it runs and exits normally.
- **Hub reporting**: With a mock Hub endpoint, verify the `limits_exceeded` status is reported with the correct message.
- **Resume reset**: Start an agent, let it accumulate counts, stop it, resume it, and verify counters reset to 0.

### Manual Verification

- `scion start --max-duration 1m <agent>` → agent stops after 1 minute, `scion list` shows `LIMITS_EXCEEDED`.
- `scion start --max-turns 5 <agent>` → agent stops after 5 turns, `agent.log` contains the limits-exceeded entry.
- `scion start --max-model-calls 20 <agent>` → agent stops after 20 model API calls.
- `scion look <agent>` shows the limit-exceeded reason clearly.

## 7. Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Hook process crashes before sending SIGUSR1 (turn limit not enforced) | Duration limit acts as a backstop. Both limits should be configured for defense-in-depth. |
| Race between turn limit and duration limit firing simultaneously | The `handleLimitsExceeded` function is idempotent -- multiple calls result in the same outcome. The first to set `LIMITS_EXCEEDED` wins (sticky status). |
| `agent-limits.json` gets corrupted | Use atomic writes (write-to-temp + rename), matching the pattern used by `agent-info.json`. |
| Harness doesn't emit `agent-end` events consistently | The `max_model_calls` limit (counted on `model-end`) provides an independent enforcement mechanism. Both limits should be configured for defense-in-depth. |
| Removing host-side timer before sciontool enforcement is deployed | Phase 4 (removal) depends on Phase 2 and 3 being deployed first. Both approaches can coexist temporarily -- the host-side timer acts as a fallback. |

## 8. Resolved Decisions

### RD1: What counts as a "turn" — both `agent-end` and `model-end`

Both counters are exposed as separate configurable limits: `max_turns` (counted on `agent-end`) and `max_model_calls` (counted on `model-end`). `max_turns` controls autonomy scope (coarser, user-facing concept of "the agent did one thing"), while `max_model_calls` controls cost (finer, directly correlates with API usage). If either limit is hit, the agent transitions to `LIMITS_EXCEEDED`.

### RD2: Resume behavior — all limits reset

All limit counters (turn count, model call count, and duration timer) reset when an agent is resumed. Each "run" gets a fresh budget. This is the simplest approach and matches the expected usage pattern where each resume typically brings a new task.

### RD3: Turn state storage — separate file with Hub heartbeat inclusion

Limit state is stored in a separate `~/agent-limits.json` file to avoid lock contention between the status handler and limits handler. The limit-exceeded status is communicated to the Hub via the standard status update mechanism (same endpoint used by heartbeats and other status changes), so the Hub always has visibility into limit state.

### RD4: Exit code — 10 confirmed

Exit code `10` is used for limit-exceeded exits. Codes 1-9 are generally safe for application use and 10 is unlikely to collide with container runtime conventions (Docker uses 125-127) or harness exit codes.

### RD5: Graceful vs immediate — immediate for v1

Immediate termination via `SIGUSR1` → `SIGTERM` is used for the initial implementation. Graceful enforcement (completing the current response before stopping) is deferred as a future enhancement if mid-response termination causes problems with uncommitted work.
