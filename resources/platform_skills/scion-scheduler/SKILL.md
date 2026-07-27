---
name: scion-scheduler
description: >-
  Schedule one-shot and recurring events for agents using the Scion CLI.
  Covers when to use scheduled events vs inline message delays, recurring
  cron schedules, lifecycle management, and the blocked-wait pairing rule.
---

# Scion Scheduler

Schedule future events that deliver messages to agents — either one-shot
(fire once at a specific time) or recurring (fire on a cron schedule).
All schedules are **project-scoped** — an agent can only create and manage
schedules within its own project.

For full command syntax: `scion schedule --help` and its subcommands.

## Two ways to schedule — when to use which

Scion has two scheduling mechanisms. They solve different problems:

| | `scion message --in/--at` | `scion schedule create` |
|---|---|---|
| Nature | fire-and-forget delayed send | durable, project-scoped event |
| Manageable | no handle once sent | list, inspect, cancel, pause, resume, delete, history |
| Visibility | only sender knows it exists | any agent in the project can `scion schedule list` |
| Recurrence | no | yes (`create-recurring --cron`) |

**Use `scion message --in`** for a simple self-callback during your own wait —
you need a poke, not a managed object.

**Use `scion schedule create`** when any of these apply:
- The event is recurring
- Another agent may need to inspect, cancel, or take over the schedule
- The wait outlives the current agent's session
- You need execution history or failure tracking

## Waiting on external processes — the blocked-wait pairing rule

When waiting on an external, non-agent process (CI run, build job, deploy,
third-party API), calling `sciontool status blocked "..."` alone is **not
sufficient**. That command marks intentional waiting for the stall detector —
it does not poll anything or wake you when the external condition resolves.
Without a separate wake mechanism, you go fully idle and only resume if an
unrelated inbound message happens to arrive.

**Correct pattern:** schedule a self-callback, then set blocked status.

```
scion message --in 5m agent:<self> "recheck CI status"
sciontool status blocked "Waiting for CI run to complete"
```

The self-callback delivers the wake-up; `status blocked` prevents the stall
detector from flagging you during the wait. **Neither replaces the other.**

- `status blocked` alone → stall detector is happy, but you never wake up
- Self-callback alone → you wake up, but the stall detector may flag you first

`sciontool status blocked` alone **is** sufficient when waiting on another
agent or a scheduled event already tracked by the orchestration system —
those fire their own state-change notifications.

## Two event types

### One-shot events

Fire once, then done. Specify timing with `--in` (relative delay: `30m`,
`2h`) or `--at` (absolute ISO 8601 timestamp, UTC).

### Recurring schedules

Fire on a cron expression (5-field: minute hour day-of-month month
day-of-week, **UTC**). Each recurring schedule has a name, can be paused
and resumed, and maintains execution history.

## Message-to-orchestrator pattern

To create agents on a timer, message a long-lived orchestrator rather than
dispatching agents directly from the schedule. The orchestrator receives the
message and creates/manages task agents as needed — keeping agent lifecycle
owned by something that can reason about it.

## Lifecycle management

| Action | Command |
|---|---|
| List all events and schedules | `scion schedule list` |
| List only recurring | `scion schedule list --show recurring` |
| List only one-shot | `scion schedule list --show events` |
| Inspect | `scion schedule get <id>` |
| Cancel a one-shot event | `scion schedule cancel <id>` |
| Pause a recurring schedule | `scion schedule pause <id>` |
| Resume a paused schedule | `scion schedule resume <id>` |
| Delete a recurring schedule | `scion schedule delete <id>` |
| View execution history | `scion schedule history <id>` |

## Gotchas

- **Cron is UTC.** There is no timezone configuration — convert from local
  time before writing the expression.
- **`cancel` vs `pause` vs `delete`:** `cancel` is for one-shot events only.
  `pause` and `delete` are for recurring schedules only. Using the wrong one
  on the wrong type errors.
- **Schedule names must be unique** within the project.
- **Clean up recurring schedules** when the task they serve is complete —
  they fire indefinitely until paused or deleted.
- **Check existing schedules** with `scion schedule list` before creating
  new ones — duplicate schedules deliver duplicate messages.
