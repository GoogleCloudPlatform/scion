---
title: Scheduling & Scheduled Events
description: Schedule recurring or one-shot future events to deliver messages or run tasks within your Scion project.
---

Scion includes a robust, project-scoped scheduling system that allows you to trigger future events. You can schedule a message to be delivered to an agent at a specific time (one-shot) or repeatedly on a calendar schedule (recurring).

All schedules are **project-scoped**. An agent can only create, list, and manage schedules within its own project.

---

## Two Ways to Schedule

There are two primary ways to schedule future activities in Scion. They solve different problems:

| | `scion schedule create --in/--at` (one-shot) | `scion schedule create-recurring` |
|---|---|---|
| **Nature** | Fire-once scheduled event | Durable, recurring event |
| **Manageable** | ✅ List, inspect, cancel | ✅ List, inspect, pause, resume, delete, history |
| **Visibility** | ✅ Any agent in the project can see it via `list` | ✅ Any agent in the project can see it via `list` |
| **Recurrence** | ❌ No | ✅ Yes (`--cron`) |

### When to use which:
* **Use a one-shot `scion schedule create --in`** for simple self-callbacks or quick, one-off delays within an agent's session (e.g., "ping me in 5 minutes to check CI status").
* **Use `scion schedule create-recurring`** when the event is recurring, when other agents may need to inspect or modify the schedule, when the wait outlives the current agent session, or when you need execution history and failure tracking.

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
Schedules are evaluated in **UTC (Coordinated Universal Time) only**. A cron expression cannot carry a timezone, and there is no timezone setting — convert from your local timezone to UTC before writing the expression. A fixed UTC time shifts by an hour against local time across daylight-saving changes.

An expression that starts with a `CRON_TZ=` or `TZ=` prefix is rejected with `400`. Schedules created with such a prefix before this rule were paused when the Hub upgraded, with a warning in the Hub log. To bring one back, edit its expression to UTC and resume it; a prefixed schedule cannot be resumed or enabled as is.
:::

For how Scion handles times and zones elsewhere (API timestamps, display zone, agent `TZ`), see [Times and Timezones](/scion/reference/times-and-timezones/).

### Schedules paused by the Hub

The Hub pauses a recurring schedule on its own in two cases, and records why in the schedule's status reason (`statusReason` in the API). `scion schedule list` shows it after the status, for example `paused (creator_deleted)`, `scion schedule get` prints it on a `Reason:` line, and the Web schedule list shows it under the status badge.

| Reason | Cause | To bring it back |
| :--- | :--- | :--- |
| `creator_deleted` | The agent the schedule runs under was deleted, so no fire could be authorized. | Resume it. Resuming records your authority, and the schedule then runs as you. |
| `cron_zone_unsupported` | The cron expression carries a `CRON_TZ=` or `TZ=` prefix. | Edit the expression to UTC, then resume it. |

A schedule you pause yourself has no status reason. Any pause, resume or status edit by a user clears the reason.

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
scion schedule create --non-interactive --type message \
  --agent "$(scion whoami --non-interactive --format json | jq -r .name)" \
  --message "Recheck CI build status" \
  --in 5m

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

*Tip: All 6 schedule subcommands (`get`, `cancel`, `pause`, `resume`, `delete`, `history`) support client-side prefix matching. You can provide a truncated schedule ID (e.g. `scion schedule cancel a1b2`) instead of the full ID.*

---

## Gotchas & Best Practices

* **Unique Names**: Schedule names must be unique within the project.
* **Cleanup obligation**: Recurring schedules fire indefinitely until paused or deleted. Always delete them when the task or project they serve is completed.
* **Check before creating**: Run `scion schedule list` to check for existing schedules before creating new ones. Duplicate schedules will deliver duplicate messages.
* **Command mismatch**: `cancel` is strictly for one-shot events. `pause` and `delete` are strictly for recurring schedules. Using the wrong command on a schedule type will return an error.
* **Event retention**: The Hub deletes every scheduled event that is no longer pending, including the fires listed by `scion schedule history`, 7 days after it completed (fired, expired or failed), or 7 days after it was created if it never fired (for example, a cancelled event), whatever its soft-delete retention. Fire and failure outcomes are also written to the Hub log.

---

## Security & Authorization

To ensure platform security and isolate team activities, schedules and scheduled events are protected by project-scoped role permissions:

- **Role-Based Access**: Each request on a project's schedules or scheduled events is checked against the caller's `scheduled_event` permission for that action (`read`, `list`, `create`, `update` or `delete`) in the project, granted through the caller's roles (see **Project-Scoped Role Permissions** below). A caller without the permission gets `403`. An agent needs the `project:read` scope and reaches only its own project's schedules and scheduled events; no `scheduled_event` role permission is checked for it, but creating, updating or resuming still passes the event type's target checks.
- **Scheduled Agent Identity**: An agent created by a schedule is attributed to the schedule's creator: `CreatorName` is set to the creator's agent name or user email, as with manual creation. It also receives the project's default GCP identity. The same service-account authorization checks as manual agent creation run against the creator, and agent creation fails if they do not pass.
- **Token Authoring**: A [user access token](/scion/hosted/user/personal-access-tokens/) can create a scheduled event or recurring schedule with `scheduled_event:create`, and update, re-target, pause or resume a schedule with `scheduled_event:update`, on a project or hub token whose boundary contains the project. It must also pass the same target checks as a signed-in request for that event type. The revision records the token, and each fire runs within that token's permission ceiling and boundary. A fire fails once that token is revoked, deleted or has expired, so a recurring schedule authored with a token stops firing at the token's expiry; pause and resume it with a sign-in or a newer token to record new authority. This does not cover messages scheduled in a chat conversation, which still refuse every user access token.
- **Authority recorded per revision (`dispatch_agent`)**: Creating a `dispatch_agent` event or schedule, and any update, re-target or resume that changes what a future dispatch does, records an *authorization revision*: who made the request, which credential they used, and that credential's permission ceiling at the time. Each fire runs under the latest revision. When the event fires, the creator's principal must still be valid: a user principal must still be active and hold `agent.create` in the project, or an agent principal must still exist in the project, and an access token the revision was recorded with must not be revoked, deleted or expired. If any check fails, that fire is recorded as failed. For a recurring schedule whose recorded authority is an agent, deleting that agent pauses the schedule with `statusReason` `creator_deleted` (and the evaluator pauses it as a backstop if it comes due; see [Schedules paused by the Hub](#schedules-paused-by-the-hub)). Pausing, deleting and edits that change only metadata write no revision.
- **Scheduled Message Authority**: A scheduled message is sent under the authority recorded by the last request that created the event or schedule, changed what, where or when it sends, or resumed or re-enabled it — not under the original creator's identity. When the message fires, that principal must be active and admitted to the project, an access token it was recorded with must be live, and the send must pass the same messaging rules as a direct send. For a recurring schedule whose recorded authority is an agent, deleting that agent pauses the schedule with `statusReason` `creator_deleted` (see [Schedules paused by the Hub](#schedules-paused-by-the-hub)). A request to create, change or resume a message event or schedule with a credential whose authority cannot be recorded is denied with a 403, as for an agent-dispatch schedule, and nothing is written. A message event or schedule without recorded authority (written before authority was recorded) does not fire, and its event records `schedule authority not recorded; pause and resume the schedule, or recreate the event`. Pausing and resuming the schedule, or recreating the event, records the caller's authority.
- **Project-Scoped Role Permissions**: Scheduled event permissions come from project-scoped roles. The built-in `project-member` role carries `scheduled_event.create`, `scheduled_event.list` and `scheduled_event.read`, and `project-admin` also carries `scheduled_event.update`. `project-owner` carries all five, including `scheduled_event.delete`, and so does the hub-wide `hub-admin` role. Cancelling or deleting an event or schedule needs `scheduled_event.delete` for every user caller, signed in or with a token, including the user who created it, so a `project-member` can create events but not cancel or delete them. An agent is not checked against this permission (see **Role-Based Access** above).
- **Required Permissions**: To perform scheduler actions, the caller's token must have the appropriate permission in the project scope:
  - **Creating/Scheduling**: Requires `scheduled_event.create`
  - **Listing/Viewing**: Requires `scheduled_event.list` and `scheduled_event.read`
  - **Modifying/Pausing**: Requires `scheduled_event.update`
  - **Deleting/Cancelling**: Requires `scheduled_event.delete`

:::caution[Schedules created before authorization revisions]
A `dispatch_agent` event or schedule created before the Hub recorded authorization revisions has none, so each fire fails with `schedule authority not recorded; pause and resume the schedule, or recreate the event`. Resuming a paused schedule records a new revision for the caller who resumes it:

```bash
scion schedule pause <id-or-name>
```

```bash
scion schedule resume <id-or-name>
```

Recreate a one-shot event instead.
:::
