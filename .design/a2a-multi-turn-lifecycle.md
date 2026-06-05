# A2A Bridge: Multi-Turn Task Lifecycle

**Status:** Implementing
**Created:** 2026-06-05
**Related:** [a2a-bridge-design.md](./a2a-bridge-design.md)

---

## 1. Problem

The A2A bridge treats the first content message from a Scion agent as the final
response and immediately marks the task as `completed`, closing all streaming and
push notification subscriptions. This breaks multi-turn agent interactions where:

- An agent asks a clarifying question (`waiting_for_input` → user replies → agent continues)
- An agent sends progress updates before the final answer
- An agent emits interim artifacts during a long-running task

The bridge's TODO at `bridge.go:633` explicitly acknowledges this: *"treats any
non-state-change message as a terminal response."*

## 2. Design

### Current behavior (bridge.go dispatchToActiveTask)

```
Agent content message → mark task completed → broadcast final event → close subscriptions
Agent state-change    → map to A2A state → broadcast → close if terminal
```

### New behavior

```
Agent content message → broadcast as status update with message → keep task alive
Agent state-change    → map to A2A state → broadcast → close only if terminal
```

The key insight: **task lifecycle should be driven by agent state changes, not by
content messages.** Content messages are data within a turn; state changes are
lifecycle events. The bridge already correctly handles state changes — it just
needs to stop prematurely closing on content.

### State mapping (unchanged, already correct)

| Scion Activity | A2A Task State | Terminal? |
|---|---|---|
| WORKING / THINKING / EXECUTING | working | No |
| WAITING_FOR_INPUT | input-required | No |
| COMPLETED | completed | Yes |
| ERROR / STALLED / LIMITS_EXCEEDED | failed | Yes |

### Changes required

**bridge.go — dispatchToActiveTask():**
Replace the `else` branch (lines ~633-675) that auto-completes on first content.
Instead:
1. Translate the message to A2A format
2. Broadcast as a `StatusUpdate` with state=working and the message attached
3. Broadcast any artifacts
4. Do NOT update task state or close subscriptions

**stream.go — StreamEvent:**
No changes needed. `StatusUpdate` already supports carrying a message payload with
`Final: false`.

**translate.go:**
No changes needed. `MapActivityToTaskState` already maps `WAITING_FOR_INPUT` →
`input-required` correctly.

## 3. Testing

- Test: agent sends content message → task stays in `working` state
- Test: agent sends two content messages → both are broadcast, task still alive
- Test: agent sends content then state-change to `completed` → task completes
- Test: agent goes `waiting_for_input` → task transitions to `input-required`
- Regression: single-turn blocking mode still works (waiter receives first content)

## 4. Scope

- In: dispatchToActiveTask content handling
- Out: follow-up message routing (PR 2), capability advertisement (PR 3)
