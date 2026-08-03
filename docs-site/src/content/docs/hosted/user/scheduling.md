---
title: Scheduling & Scheduled Events
description: Schedule recurring or one-shot future events to deliver messages or run tasks within your Scion project.
---

Scion includes a robust, project-scoped scheduling system that allows you to trigger future events. You can schedule a message to be delivered to an agent at a specific time (one-shot) or repeatedly on a calendar schedule (recurring).

All schedules are **project-scoped**. An agent can only create, list, and manage schedules within its own project.

---

## Two Ways to Schedule

There are two primary ways to schedule future activities in Scion. They solve different problems:

| | `scion message --in/--at` | `scion schedule create / create-recurring` |
|---|---|---|
| **Nature** | Fire-and-forget delayed message | Durable, project-scoped event |
| **Manageable** | ❌ No handle once sent | ✅ List, inspect, cancel, pause, resume, delete, history |
| **Visibility** | Only sender knows it exists | ✅ Any agent in the project can see it via `list` |
| **Recurrence** | ❌ No | ✅ Yes (`--cron`) |

### When to use which:
* **Use `scion message --in`** for simple self-callbacks or quick, one-off delays within an agent's session (e.g., "ping me in 5 minutes to check CI status").
* **Use `scion schedule`** when the event is recurring, when other agents may need to inspect or modify the schedule, when the wait outlives the current agent session, or when you need execution history and failure tracking.

---

## One-Shot Events

One-shot events fire exactly once and are then cleaned up. You can specify the timing in two ways:
* **Relative Delay (`--in`)**: Specify a duration such as `15m` (minutes), `2h` (hours), or `1d` (days).
* **Absolute Timestamp (`--at`)**: Specify an absolute ISO 8601 timestamp in UTC (e.g., `2026-08-03T14:00:00Z`).

### Creating a One-Shot Schedule via CLI

```bash
scion schedule create \
  --name "one-shot-check" \
  --type message \
  --agent "deploy-agent" \
  --message "Recheck system health" \
  --in 15m
```

---

## Recurring Schedules

Recurring schedules fire repeatedly on a **5-field cron expression** (Minute, Hour, Day of Month, Month, Day of Week). 

:::caution[Cron is UTC]
Schedules evaluate using **UTC (Coordinated Universal Time)**. There is no local timezone configuration — convert from your local timezone to UTC before writing the cron expression.
:::

### Creating a Recurring Schedule via CLI

```bash
scion schedule create-recurring \
  --name "nightly-cleanup" \
  --cron "0 2 * * *" \
  --type message \
  --agent "cleanup-agent" \
  --message "Run database vacuum and log compression"
```

---

## Patterns and Recipes

### 1. Self-Scheduling (The Whoami Recipe)

An agent can schedule a message to itself. To do this, resolve your own agent name first:

```bash
scion schedule create --non-interactive --type message \
  --agent "$(scion whoami --non-interactive --format json | jq -r .name)" \
  --message "Check if migration completed" \
  --in 15m
```

### 2. Waiting on External Processes (The Blocked-Wait Pairing Rule)

When an agent needs to wait on an external, non-agent process (such as a CI/CD build, cloud deployment, or a third-party API):

1. Calling `sciontool status blocked "..."` alone **is not sufficient**. This command only updates your status to satisfy the stall detector — it does not poll or wake the agent up.
2. You must pair `status blocked` with a scheduled self-callback to act as a wake-up signal.

```bash
# 1. Schedule a self-callback message in 5 minutes
scion message --in 5m agent:$(scion whoami --non-interactive --format json | jq -r .name) "Recheck CI build status"

# 2. Set your status to blocked to avoid the stall detector
sciontool status blocked "Waiting for CI run 9428 to complete"
```

* **`status blocked` alone** → Stall detector is happy, but you go idle forever because nothing wakes you up.
* **Self-callback alone** → You wake up, but the stall detector may flag you as stalled during the 5-minute silent window.
* **Both together** → Safe, reliable asynchronous polling.

:::note
You do **not** need a scheduled callback when waiting on another Scion agent or a native platform-tracked event, as those trigger automatic state-change notifications that wake you up.
:::

### 3. Message-to-Orchestrator Pattern

To orchestrate agents on a timer (for example, starting a test runner every hour), message a long-lived **orchestrator agent** rather than scheduling direct agent starts. The orchestrator agent receives the scheduled message and creates/dispatches task agents as needed — keeping agent lifecycle owned by an agent that can reason about failures and state.

---

## Lifecycle Management Commands

Use these commands to manage schedules and events in your project:

| Action | Command |
|---|---|
| **List all events and schedules** | `scion schedule list` |
| **List only recurring schedules** | `scion schedule list --show recurring` |
| **List only one-shot events** | `scion schedule list --show events` |
| **Inspect a schedule** | `scion schedule get <id-or-name>` |
| **Cancel a one-shot event** | `scion schedule cancel <id>` |
| **Pause a recurring schedule** | `scion schedule pause <id-or-name>` |
| **Resume a recurring schedule** | `scion schedule resume <id-or-name>` |
| **Delete a recurring schedule** | `scion schedule delete <id-or-name>` |
| **View execution history** | `scion schedule history <id-or-name>` |

---

## Gotchas & Best Practices

* **Unique Names**: Schedule names must be unique within the project.
* **Cleanup obligation**: Recurring schedules fire indefinitely until paused or deleted. Always delete them when the task or project they serve is completed.
* **Check before creating**: Run `scion schedule list` to check for existing schedules before creating new ones. Duplicate schedules will deliver duplicate messages.
* **Command mismatch**: `cancel` is strictly for one-shot events. `pause` and `delete` are strictly for recurring schedules. Using the wrong command on a schedule type will return an error.
