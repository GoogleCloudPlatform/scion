---
name: agent-status-signals
description: >-
  Status signaling protocol for all Scion agents. Covers the three mandatory signals
  (ask_user, blocked, task_completed) that keep the orchestration system informed of
  agent state. Every agent template should reference this skill.
---

# Agent Status Signals

Every Scion agent must signal its state to the orchestration system using `sciontool status`. These signals prevent false stall detection, enable notification routing, and keep users informed.

## Waiting for Input

Before asking a user or coordinator a question, signal that you are waiting:

```bash
sciontool status ask_user "<question>"
```

Then proceed to ask the question via `scion message`.

## Blocked (Intentionally Waiting)

When you are intentionally waiting for something — such as a child agent to complete, a scheduled event, or a user reply — signal that you are blocked:

```bash
sciontool status blocked "<reason>"
```

Example: `sciontool status blocked "Waiting for agent deploy-frontend to complete"`

This prevents the system from falsely marking you as stalled. The status clears automatically when you resume work (e.g. when you receive a message or start a new task).

**Important:** An agent that has completed its current work and is waiting for the next assignment must call `sciontool status blocked` — not go idle. Idle triggers stall detection; blocked tells the system it is intentionally waiting.

## Completing Your Task

Once you have completed your task, summarize and report back as you normally would, then signal completion:

```bash
sciontool status task_completed "<task title>"
```

Do not follow this with "What would you like to do now?" or similar — just stop.
