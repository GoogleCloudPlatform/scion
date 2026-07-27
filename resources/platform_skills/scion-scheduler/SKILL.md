---
name: scion-scheduler
description: >-
  Schedule one-shot and recurring events for agents using the Scion CLI.
  Covers when to use scheduled events vs inline message delays, recurring
  cron schedules, and lifecycle management.
---

# Scion Scheduler

Schedule future events that deliver messages to agents — either one-shot
(fire once at a specific time) or recurring (fire on a cron schedule).

For full command syntax: `scion schedule --help` and its subcommands.

## When to use scheduling

| Need | Use |
|---|---|
| Self-reminder during a blocked wait | `scion message --in <delay> agent:<self> "check status"` — lightweight, inline |
| Delayed message to another agent | `scion schedule create --in <delay> --agent <name> --message "..."` |
| Recurring operational task | `scion schedule create-recurring --cron "..." --name <name> --agent <name> --message "..."` |
| Wake an agent at an absolute time | `scion schedule create --at <ISO-8601> --agent <name> --message "..."` |

**Use `scion message --in`** for simple self-callbacks during a single wait.
**Use `scion schedule`** when you need visibility, cancellation, or recurrence —
scheduled events are tracked by the Hub and can be listed, inspected, and cancelled.

## Two event types

### One-shot events

Fire once, then done. Specify timing with `--in` (relative delay: `30m`, `2h`) or
`--at` (absolute ISO 8601 timestamp).

### Recurring schedules

Fire on a cron expression (5-field: minute hour day-of-month month day-of-week, **UTC**).
Each recurring schedule has a name, can be paused and resumed, and maintains an
execution history.

## Lifecycle management

| Action | Command |
|---|---|
| List all events and schedules | `scion schedule list` |
| List only recurring schedules | `scion schedule list --show recurring` |
| Inspect a specific event or schedule | `scion schedule get <id>` |
| Cancel a pending one-shot event | `scion schedule cancel <id>` |
| Pause a recurring schedule | `scion schedule pause <id>` |
| Resume a paused schedule | `scion schedule resume <id>` |
| Delete a recurring schedule | `scion schedule delete <id>` |
| View execution history | `scion schedule history <id>` |

## Operational guidance

- **Always use `--non-interactive`** on any `scion` command — this applies to
  scheduling just as to all other CLI operations.
- **Clean up recurring schedules** when the task they serve is complete. Unlike
  one-shot events, recurring schedules fire indefinitely until paused or deleted.
- **Check existing schedules** with `scion schedule list` before creating new
  ones — duplicate schedules deliver duplicate messages.
- **Cron expressions are UTC.** Account for timezone offset when scheduling
  time-sensitive recurring tasks.
- **Pair with `sciontool status blocked`** when scheduling a self-callback.
  The schedule delivers the wake-up message; `status blocked` prevents the stall
  detector from flagging you during the wait. Neither replaces the other.
