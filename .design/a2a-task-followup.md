# A2A Bridge: Follow-Up Messages on Existing Tasks

**Status:** Implementing
**Created:** 2026-06-05
**Related:** [a2a-bridge-design.md](./a2a-bridge-design.md), [a2a-multi-turn-lifecycle.md](./a2a-multi-turn-lifecycle.md)

---

## 1. Problem

The A2A protocol supports multi-turn conversations via `contextId` (session) and
`taskId` (turn). A client should be able to:

1. Send an initial `message/send` → get a taskID back
2. Wait for the agent to respond (possibly with `input-required`)
3. Send a follow-up `message/send` with the same `taskID` → continue the conversation

Currently, `SendMessage` always creates a new task, even when `taskID` is provided
in the params. The `SendMessageParams` struct already has a `TaskID` field, but
it's unused in the dispatch logic.

## 2. Design

### Current behavior

```
message/send {taskId: "abc"} → ignored, creates new task → new taskID returned
```

### New behavior

```
message/send {taskId: "abc"} → look up task "abc" → verify not terminal →
  resolve agent from task → send message to agent → return existing task
```

### Changes

**bridge.go — SendMessage():**
At the top of the function, before creating a new task:
1. Check if `taskID` is non-empty (from `SendMessageParams.TaskID`)
2. If set, look up the task from the store
3. Verify the task belongs to the requesting project/agent (authorization)
4. Verify the task is not in a terminal state
5. Resolve the agent from the task's stored `AgentID`
6. Send the message to the agent
7. Update task state to `working` if it was `input-required`
8. Return the existing task (not a new one)

### Context vs Task

The A2A spec uses `contextId` to group related tasks into a session, and `taskId`
to identify individual turns. For follow-ups:
- `taskID` provided → continue that specific task/turn
- Only `contextID` provided → new task within the existing session (current behavior works)
- Neither → new context + new task (current behavior works)

## 3. Testing

- Test: message/send with valid taskID routes to same agent
- Test: message/send with terminal-state taskID returns error
- Test: message/send with unknown taskID returns error
- Test: message/send with taskID from different project returns error
- Test: task state transitions from input-required to working on follow-up

## 4. Scope

- In: Follow-up routing via taskID in SendMessage
- Out: Multi-turn lifecycle changes (PR 1), capability updates (PR 3)
