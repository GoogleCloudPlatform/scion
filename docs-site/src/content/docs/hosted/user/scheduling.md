---
title: Scheduling & Scheduled Events
description: Schedule recurring or one-shot future events to deliver messages or run tasks within your Scion project.
---

Scion includes a robust, project-scoped scheduling system that allows you to trigger future events. You can schedule a message to be delivered to an agent—either at a specific time (one-shot) or repeatedly on a calendar schedule (recurring)—or, when the `hub.exec_schedules` and `hub.conduit` experiments are enabled, schedule a one-shot or recurring command to run directly in an agent's container without an LLM turn (`--type exec`).

All schedules are **project-scoped**. An agent can only create, list, and manage schedules within its own project.

---

## Two Ways to Schedule

There are two primary ways to schedule future activities in Scion. They solve different problems:

| | `scion schedule create --in/--at` (one-shot) | `scion schedule create-recurring` |
|---|---|---|
| **Nature** | Fire-once scheduled event | Durable, recurring event |
| **Manageable** | ✅ List, inspect, cancel | ✅ List, inspect, pause, resume, delete, history |
| **Visibility** | ✅ Users with `scheduled_event` list permission in the project; for an agent caller, only events whose recorded authority is that agent | ✅ Users with `scheduled_event` list permission in the project; for an agent caller, only schedules whose recorded authority is that agent |
| **Recurrence** | ❌ No | ✅ Yes (`--cron`) |

### When to use which:
* **Use a one-shot `scion schedule create --in`** for simple self-callbacks or quick, one-off delays within an agent's session (e.g., "ping me in 5 minutes to check CI status").
* **Use `scion schedule create-recurring`** when the event is recurring, when the wait outlives the current agent session, or when you need execution history and failure tracking.

---

## One-Shot Events

One-shot events fire exactly once and are then cleaned up. You can specify the timing in two ways:
* **Relative Delay (`--in`)**: Specify a duration such as `15m` (minutes), `2h` (hours), or `1d` (days).
* **Absolute Timestamp (`--at`)**: Specify an absolute ISO 8601 timestamp in UTC (e.g., `2026-08-03T14:00:00Z`).

### Creating a One-Shot Schedule via CLI

```bash
scion schedule create \
  --type message \
  --agent "deploy-agent" \
  --message "Recheck system health" \
  --in 15m
```

When the `hub.exec_schedules` and `hub.conduit` [experiments](/scion/reference/experiments/#server-experiments-with-notes) are enabled (both off by default), `scion schedule create --type exec` (or `POST /api/v1/projects/{projectId}/scheduled-events` with `eventType: "exec"`) can also run a non-interactive command inside an agent's container over Conduit without sending a message or starting an LLM turn; see [Exec Schedules (Experimental)](#exec-schedules-experimental).

---

## Recurring Schedules

Recurring schedules fire repeatedly on a **5-field cron expression** (Minute, Hour, Day of Month, Month, Day of Week). 

### Time zones

By default a cron expression is evaluated in **UTC**. A schedule can instead carry an IANA **time zone** (for example `America/Los_Angeles`): the expression is then matched against local wall-clock time in that zone, so a schedule keeps its local run time across daylight saving changes instead of shifting by an hour. Set it with `--time-zone` on `scion schedule create-recurring`, the **Time zone** field in the Web schedule form, or `timeZone` in the API. An empty zone, or `UTC`, means UTC. Next-run times in the API are always UTC instants; the CLI and Web show them in your display zone.

Daylight saving changes create local times that do not exist or that happen twice. For a schedule with a zone:

- A matching local time that is **skipped** when the clocks go forward (for example 02:30 on the night clocks jump from 02:00 to 03:00) runs **once, at the moment of the change** (03:00 local). The day is not lost, and several matches inside the skipped hour run once.
- A matching local time that **happens twice** when the clocks go back (for example 01:30) runs **once, at its first occurrence**. A schedule that runs more often than hourly (for example every 15 minutes) therefore does not run again during the repeated hour.
- If the next run is computed during the repeated hour after the first occurrence has passed (for example when you create, edit or resume the schedule then), the second occurrence is used: the next run is always in the future.

An unknown zone name, `Local`, or a name with leading or trailing spaces is refused with `400`. Changing a schedule's zone recomputes its next run and, like changing its cron expression, records your authority for the schedule.

:::caution[Downgrading the Hub]
A Hub version from before schedule time zones ignores the zone and evaluates every schedule in UTC. Clear the zones (or convert those schedules' expressions to UTC) before downgrading.
:::

A zone cannot be written inside the expression: an expression that starts with a `CRON_TZ=` or `TZ=` prefix is rejected with `400`. Schedules created with such a prefix by an older Hub were paused when the Hub upgraded, with a warning in the Hub log. To bring one back, remove the prefix, set the schedule's time zone instead, and resume it; a prefixed schedule cannot be resumed or enabled as is.

For how Scion handles times and zones elsewhere (API timestamps, display zone, agent `TZ`), see [Times and Timezones](/scion/reference/times-and-timezones/).

### Schedules paused by the Hub

The Hub pauses a recurring schedule on its own in three cases, and records why in the schedule's status reason (`statusReason` in the API). `scion schedule list` shows it after the status, for example `paused (creator_deleted)`, `scion schedule get` prints it on a `Reason:` line, and the Web schedule list shows it under the status badge.

| Reason | Cause | To bring it back |
| :--- | :--- | :--- |
| `creator_deleted` | The agent or user the schedule runs under was deleted, or that user is being deleted, so no fire could be authorized. | Resume it. Resuming records your authority, and the schedule then runs as you. |
| `cron_zone_unsupported` | The cron expression carries a `CRON_TZ=` or `TZ=` prefix. | Remove the prefix and set the schedule's time zone instead, then resume it. |
| `target_deleted` | The target agent of a recurring `exec` schedule was deleted (checked before `creator_deleted`, so a self-targeted `exec` schedule records `target_deleted`). | Edit the schedule via `PATCH` to name a live target agent together with a replacement `command` (or `payload`), then resume it (`scion schedule resume <id>` or `POST .../resume`, or include `"status": "active"` in the `PATCH`). Resuming without re-targeting first fails with `404` because the stored target no longer exists. |

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

To run at a local time in a time zone, add `--time-zone`; this one runs at 19:00 Pacific time all year:

```bash
scion schedule create-recurring \
  --name "evening-report" \
  --cron "0 19 * * *" \
  --time-zone America/Los_Angeles \
  --type message \
  --agent "report-agent" \
  --message "Post the daily report"
```

`scion schedule get` shows the zone on a `Zone:` line (`UTC` when none is set), and `scion schedule list` shows it after the expression when it is not UTC. The CLI sets the zone only at creation; change it later in the Web schedule form or with `PATCH` and `timeZone` (an empty string sets it back to UTC).

### Waking a suspended agent

By default a scheduled message is not delivered to a suspended agent: the fire is recorded as failed. Add `--wake` to `scion schedule create` or `create-recurring` (the **Wake agent** option in the Web forms, or `"wake": true` in the API request or message payload) to resume the agent first and then deliver the message. The agent can suspend itself again afterwards, or be auto-suspended. `wake` applies to message events only; it is refused (`400`) on a `dispatch_agent` event or schedule.

```bash
scion schedule create-recurring \
  --name "morning-check" \
  --cron "0 8 * * *" \
  --agent "watcher" \
  --message "Check the overnight builds" \
  --wake
```

- **Permission**: a fire that resumes the agent needs the permission to start it (`agent.lifecycle`), as `scion message --wake` does, checked under the schedule's recorded authority (see **Scheduled Message Authority** below). The permission ceiling recorded with that authority must also include it, so a schedule an agent created without the permission does not wake anyone after that agent is given broader rights. Without it, the fire is refused (`scheduled_message_denied: message delivery refused`), and nothing is started or delivered. An agent that is already running needs no extra permission.
- **Refusals**: a held agent is never woken (the fire is refused), and an agent that is migrating (`scion reincarnate`) fails as without `--wake`. A stopped, errored or halted agent is not resumed: the fire fails with `failed to wake agent <name>: ...`, the same reason `scion message --wake` gives. Any refusal of the start itself (a delete in progress, capacity, broker placement) fails the fire the same way, and nothing is delivered.
- **Start already in progress**: if the agent is already starting, or another start of it (or a stop) is in progress, when the message fires, the fire does not start it again: it waits for that start to bring the agent up and then delivers. If the agent does not become ready, the fire fails with `target agent was already starting or stopping and did not become ready; message not delivered`.
- **Status while running**: a wake fire shows status `firing` until its delivery outcome is written (`fired`, `expired` or `failed`). If the Hub stops before the outcome can be written, recovery marks the event `failed` with `hub stopped before the wake fire completed` (and counts the error on the parent recurring schedule).
- **Auto-wake under `lifecycle.idle_suspend`**: when the [`lifecycle.idle_suspend`](/scion/reference/experiments/#server-experiments-with-notes) experiment is enabled, a scheduled message to an agent the Hub suspended (`suspendReason` `idle` or `stalled`) resumes it automatically even without `--wake` and without requiring `agent.lifecycle` on the schedule's authority. `--wake` is still required to wake an agent a user suspended (`suspendReason` `user`) or while `lifecycle.idle_suspend` is disabled.
- [Scheduled chat messages](/scion/hosted/user/scheduled-send/) are separate from the project scheduler and have their own opt-in **Wake the agent if it is suspended** option for the primary recipient.

---

## Exec Schedules (Experimental)

When a one-shot event or recurring schedule only needs to run a deterministic command or script inside an agent's container—without waking an LLM or consuming a model turn—use `--type exec`. Scheduled exec dispatches the command (`argv`) over the target agent's Conduit session to `sciontool` inside the container.

### Prerequisites

- **Hub experiments**: Both `hub.conduit` and `hub.exec_schedules` must be enabled on the Hub (see [Experiments](/scion/reference/experiments/#server-experiments-with-notes)).
- **Agents launched or restarted with the flag**: `sciontool` registers and advertises the `exec-start` and `exec-status` control routes only when the agent started with `hub.exec_schedules` enabled in its launch environment. Agents started before the experiment was enabled do not advertise those routes until restarted; fires targeting them record status `skipped` (`agent executor unavailable`).
- **NTP-synchronised clocks**: `sciontool` enforces a fail-closed start deadline (`not_after` plus a 10-second clock tolerance). Agent hosts and containers must run NTP-synchronised clocks; clock skew beyond the 10-second tolerance causes starts to fail closed (`start expired (check agent clock)`) rather than run late.
- **Supported runtimes**: Container runtimes with Conduit sessions are supported. Managed agent runtimes (`managed:*`) are refused with `422` at authoring (`create`, `PATCH`, and `resume`) and `skipped` at fire time.

### Creating One-Shot and Recurring Exec Schedules

Pass the command and its arguments after `--`. Everything after `--` is sent as an `argv` array (`1`–`64` arguments, non-empty `argv[0]`, no NUL bytes, at most `4,096` bytes per argument and `16,384` bytes total) and executed directly with `execve`—**no shell is invoked** unless you name one explicitly (for example `-- sh -c '...'`).

- **One-shot (`scion schedule create`)**: requires `--in` or `--at`, `--type exec`, `--agent <name>`, optional `--timeout <duration>` (`1s`–`60m`, whole seconds, default `60s`), and `-- <argv...>` (`POST /api/v1/projects/{projectId}/scheduled-events` with `eventType: "exec"`, `fireIn` or `fireAt`, `agentId` or `agentName`, `command`, and optional `timeoutSeconds`, or raw `payload`).
- **Recurring (`scion schedule create-recurring`)**: requires `--name <name>`, `--cron "<5-field>"`, optional `--time-zone <IANA>` (default `UTC`), `--type exec`, `--agent <name>`, optional `--timeout <duration>` (`1s`–`60m`, whole seconds, default `60s`), and `-- <argv...>` (`POST /api/v1/projects/{projectId}/schedules` with `eventType: "exec"`, `name`, `cronExpr`, optional `timeZone`, `agentId` or `agentName`, `command`, and optional `timeoutSeconds`, or raw `payload`). `--message`, `--interrupt`, and `--wake` are rejected with `--type exec`.

```bash
# One-shot exec event in 10 minutes (default 60s timeout)
scion schedule create \
  --type exec \
  --agent "worker" \
  --in 10m \
  --timeout 60s \
  -- /usr/local/bin/check-queue --strict

# Recurring exec schedule every 6 hours in UTC (55m timeout)
scion schedule create-recurring \
  --name "alt-image-pull" \
  --cron "0 */6 * * *" \
  --time-zone UTC \
  --type exec \
  --agent "ops-runner" \
  --timeout 55m \
  -- /usr/local/bin/pull-images.sh
```

In the Web UI, the project's **Recurring Schedules** and **Scheduled Events** forms also offer the `exec` event type while `hub.exec_schedules` is enabled, with an attachable-agent picker, per-argument `argv` inputs, optional timeout, and run-summary detail and history views.

### Execution Environment and Output

- **User and groups**: Runs as the container's Conduit PTY user (`scion`) with its supplementary groups, never with PID 1 credentials (if privilege drop cannot be established, the run fails closed).
- **Working directory**: `/workspace` if it exists, otherwise the `scion` user's home directory.
- **Environment**: The same environment `sciontool` gives the harness shell in the agent's tmux session at launch, so scheduled commands see the same `PATH`, workspace variables, and credentials as commands typed in the agent's shell.
- **Standard streams and no stored output**: `stdin` is `/dev/null`. `stdout` and `stderr` are continuously drained and discarded inside the container—**no command output is stored or returned** in the database, API, CLI, or Hub/broker logs.
- **Run summary (`execSummary`)**: Once an exec event's run is admitted, `ScheduledEventResponse` (`GET /scheduled-events`, `GET /scheduled-events/{id}`, and `GET /schedules/{id}/history`) includes `execSummary` with `outcome`, `exitCode`, `durationMs`, `outputBytes`, `startedAt`, and `finishedAt` (never `command`, `commandSha256`, or command output). `scion schedule get <event-id>` prints `Outcome`, `Exit Code`, `Duration`, and `Output`; `scion schedule get <schedule-id>` prints `Last Outcome` and `Last Exit Code` from the latest history entry; and `scion schedule history <schedule-id>` renders `STATUS`, `OUTCOME`, `EXIT`, `DURATION`, `OUTPUT`, `FIRED AT`, and `ERROR`. Jobs that need readable logs or failure alerts should write their own log file (for example in `/workspace` or a shared volume) and, if needed, send alerts from the script with `scion message`.

### Timeouts, Outcomes, Overlap, and Accounting

- **Timeouts**: `--timeout` accepts whole-second durations from `1s` up to `60m` (`3600s`), defaulting to `60s` when omitted. Each command runs in its own process group. On timeout, `sciontool` sends `SIGTERM` to the process group and `SIGKILL` 5 seconds later, recording event status `failed` (`timed out after Ns`, `outcome: timeout`).
- **Exit codes**: Exit code `0` records `fired` (`outcome: ok`). Any non-zero exit code records `failed` with `exit status N` (`outcome: nonzero`).
- **Target state**: The target agent must be `running` when the event fires. Stopped, suspended, or reincarnating targets are **not** woken (`--wake` is not supported with `--type exec`); the fire records `skipped` (`target not running`). Fires delayed more than 10 minutes past their scheduled time also record `skipped` (`missed window`). A **held** target is refused as `failed` (`exec refused`).
- **Recurring overlap lease**: Recurring `exec` fires run detached from the schedule evaluator (`nextRunAt` advances before dispatch, so slow starts or hour-long runs never delay other due schedules). While a previous run of the same recurring schedule is still running, a new due fire is `skipped` with `previous run in progress`—enforced both at the Hub via a per-schedule database overlap lease (an unfinished run row for that `scheduleId` with an unexpired `leaseUntil = startedAt + timeout + 120s`) and inside the container by `sciontool` while the previous run's main process is still alive (`409` `overlap`).
- **Recurring run accounting**: Each completed fire of a recurring `exec` schedule updates its counters and last-run fields atomically with the run/event outcome:
  - `runCount` (`run_count`) increments by `1` on every completed fire (`success`, `skipped`, or `error`).
  - `errorCount` (`error_count`) increments by `1` only on failed fires (non-zero exit, timeout, `abandoned`, start/dispatch failure, fire-time `exec refused`, shutdown abort, or unadmitted crash recovery); `skipped` fires and exit-`0` fires do not increment `errorCount`.
  - `lastRunAt`, `lastRunStatus` (`success`, `skipped`, or `error`), and `lastRunError` (cleared on `success`; set to the skip or failure reason otherwise) advance monotonically whenever a run's timestamp is newer than `lastRunAt`.
- **Container concurrency and authoring quotas**: At most **2 live scheduled exec runs per container** (`skipped` with `capacity`). Authoring quotas cap active exec items—pending one-shot `exec` events plus active recurring `exec` schedules—at `20` per target agent and `200` per project (`429` `quota_exceeded` when exceeded; updating or resuming an active schedule excludes itself from the count).

### Long-Running Work and `flock` Mutual Exclusion

Tracked runs support timeouts up to **60 minutes** (`--timeout 60m`), which is the preferred mode whenever a job fits within an hour because event status, schedule `errorCount`, overlap leases, and exit codes track the real job directly.

If a job can legitimately run longer than 60 minutes, have a short wrapper script launch the work detached in the background (`setsid ... </dev/null >>/workspace/job.log 2>&1 &`). When the main process exits normally, `sciontool` records the outcome immediately and does **not** kill backgrounded children.

Because detached or daemonized descendants (`setsid`, double-fork) escape process-group tracking on timeout and release the per-schedule overlap lease as soon as the wrapper exits—allowing a later fire to overlap while the background job is still active—**guard detached jobs (and any script touching shared external resources) with `flock -n`**:

```bash
#!/usr/bin/env bash
set -euo pipefail

exec 9>/tmp/nightly-sync.lock
if ! flock -n 9; then
  # Previous run still holds fd 9; exit cleanly without overlapping.
  exit 0
fi

/usr/local/bin/nightly-sync >>/workspace/nightly-sync.log 2>&1
```

### Permissions, Updates (`PATCH` / `resume`), Target Deletion, and Redaction

- **Live `agent.attach` authority**: Creating or resuming an `exec` event or schedule requires `scheduled_event.create` (or `scheduled_event.update` on `resume`/`PATCH`) in the project **and** a user or agent that may attach to the agent (`agent.attach`—for example its owner or an ancestor, a role that grants `agent.attach`, or an agent with the `full` role in the same delegation chain; user access tokens also require the `agent:attach` scope, and delegated agent tokens are refused). That authority is re-checked on every fire against the resolved `agentId` under the recorded authorization revision and frozen permission ceiling; if revoked, the fire is refused (`failed` with `exec refused`).
- **Target deletion (`target_deleted`)**: Deleting an agent cancels its pending one-shot `exec` events (matched strictly by resolved `agentId`) and pauses every active recurring `exec` schedule targeting that `agentId` with `statusReason: "target_deleted"` (see [Schedules paused by the Hub](#schedules-paused-by-the-hub)).
- **Resuming (`POST .../resume` / `scion schedule resume`)**: Resuming a paused recurring `exec` schedule requires `hub.exec_schedules` and `hub.conduit` enabled and re-authorizes the caller with live `agent.attach` on the stored target agent, clearing `statusReason`, recomputing `nextRunAt`, checking active quotas, and recording a new authorization revision and ceiling. A schedule paused with `target_deleted` cannot be resumed until it has been re-targeted via `PATCH` (`404` while the stored target is deleted).
- **Updating (`PATCH /api/v1/projects/{projectId}/schedules/{id}`)**: Only user callers with `scheduled_event.update` may `PATCH` schedule definitions (`403` for agent callers). `eventType` **cannot** be changed to or from `exec` (`400`, `eventType cannot be changed to or from exec`), and `PATCH` on an `exec` schedule is refused while `hub.exec_schedules` or `hub.conduit` is off (`pause` and `delete` remain operable). Any `PATCH` on an `exec` schedule that supplies `payload` or exec convenience fields (`agentId`, `agentName`, `command`, `timeoutSeconds`) or changes future dispatch (`cronExpr`, `timeZone`, `payload`, or enabling `status: "active"`) requires live `agent.attach` on the **stored** target agent (when that target still exists) as well as live `agent.attach` on the effective target, and records a new authorization revision and ceiling; metadata-only edits (`name`, or pausing via `status: "paused"`) do not re-attribute. When the previous target agent was **deleted**, re-targeting to another `agentId` or `agentName` requires supplying an explicit replacement `command` or `payload` (`400`, `retargeting an exec schedule whose previous target was deleted requires command or payload`).
- **Command (`argv`) redaction and `commandSha256`**: Every `/scheduled-events`, `/schedules` (including `history`, `create`, `PATCH`, `pause`, and `resume`), and `/admin/scheduler` response redacts `payload` fail-closed unless the reading caller's current credential holds live `agent.attach` on the stored target agent at read time (`/admin/scheduler` always redacts, and the creator is **not** exempt if they later lose attach permission or read with a token lacking `agent:attach`). Redacted responses omit `command` from `payload` and return `{"agentId":...,"agentName":...,"timeoutSeconds":...,"commandRedacted":true,"argc":N,"commandSha256":"..."}`, which `scion schedule get` displays on `Command:` as `<redacted, N args, sha256:...>`. `commandSha256` matches `agentcontrol.ExecArgvDigest`: the lowercase hex SHA-256 digest of the canonical UTF-8 JSON array encoding of `command` (`argv`), serialized with compact separators, **no HTML escaping** (`SetEscapeHTML(false)`, so `&`, `<`, and `>` stay literal UTF-8 rather than `\u0026`, `\u003c`, or `\u003e`), and the trailing newline trimmed (`nil` treated as `[]`). The same digest is recorded as `command_sha256` on exec audit events (`execSummary` itself carries only `outcome`, `exitCode`, `durationMs`, `outputBytes`, `startedAt`, and `finishedAt`).

### Documented Limitations

- **No in-flight cancellation**: Cancelling an event, pausing or deleting a schedule, revoking authority, or placing a hold on the target stops future fires from starting, but does not cancel an exec run already admitted in the container; that run continues until it exits or reaches its local timeout.
- **Outcome recovery when `hub.conduit` is turned off**: Turning `hub.exec_schedules` off stops new starts while `exec-status` recovery for already-admitted runs continues. Turning `hub.conduit` off also refuses new or reconnecting agent Conduit sessions, so outcome recovery continues **only** over agent sessions that stay connected; if the session drops (or the Hub restarts while `hub.conduit` is off), the agent cannot reconnect and the run ends `abandoned` when its lease (`timeout + 120s`) expires.

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
* **Event retention**: The Hub deletes every completed scheduled event (excluding `pending` and `firing` events), including the fires listed by `scion schedule history`, 7 days after it completed (fired, expired or failed), or 7 days after it was created if it never fired (for example, a cancelled event), whatever its soft-delete retention. Fire and failure outcomes are also written to the Hub log.

---

## Security & Authorization

To ensure platform security and isolate team activities, schedules and scheduled events are protected by project-scoped role permissions:

- **Role-Based Access**: Each request on a project's schedules or scheduled events is checked against the caller's `scheduled_event` permission for that action (`read`, `list`, `create`, `update` or `delete`) in the project, granted through the caller's roles (see **Project-Scoped Role Permissions** below). A caller without the permission gets `403`. An agent needs the `project:read` scope and reaches only its own project's schedules and scheduled events; within that project, an agent caller can list, inspect, view history for, pause, resume, cancel, and delete **only** the schedules and scheduled events whose current recorded authority (`initiatorPrincipalKind` `agent`, `initiatorPrincipalId`) is that agent (a schedule or event with no recorded principal is held by no agent, and any other answers `403`), and cannot edit schedule definitions (`PATCH` returns `403`). Creating or resuming still passes the event type's target checks.
- **Scheduled Agent Identity**: An agent created by a `dispatch_agent` one-shot event or recurring schedule is created as the revision's recorded principal would create it interactively: `OwnerID` and `Ancestry` are derived from that principal (`[user_id]` for a user principal; the parent agent's ID as `OwnerID` and `parent.Ancestry + [parent.ID]` for an agent principal), `CreatorName` is set to the principal's user email or agent name, and the agent inherits the project's default settings (profile, harness-config, harness-auth, model, thinking level, limits, resources, auto-expose, and default GCP identity, checked against the principal's service-account authorization). Its agent role comes from the default role ladder (`scion.io/default-agent-role`, then Hub `agent_defaults.default_agent_role`, then `full` for user principals; the parent agent's role for agent principals), capped by `scion.io/max-agent-role` and the revision's recorded permission ceiling. If the recorded permission ceiling admits no usable role in `{full, baseline, readonly}`, the fire fails (without silently falling back to `none`) with `no usable role for the recorded authority; set noAuth: true or re-authorize the schedule`. Setting `"noAuth": true` (via the create convenience field, not combined with a raw `payload`, or by replacing the schedule's `payload` via `PATCH`, which records a new authorization revision) explicitly selects role `none` and `NoAuth: true`, launching a credential-less agent under any frozen ceiling without resolving secrets or running environment gathering. Otherwise, when the resolved role is above `none`, the agent launches with `NoAuth: false`: the Hub resolves environment variables and secrets from Hub storage under the principal (owner rules for a user principal, progeny rules for an agent principal) and dispatches creation to the Runtime Broker with environment gathering, satisfying `as_needed` entries on the second pass; if required environment variables remain missing, the create is rolled back and the fire is recorded as failed with `missing required environment variables (N)`. Owned scheduled agents block deleting their owner user until removed, while scheduled agents created before this change keep their stored `NoAuth: true` and `none` role across `scion reincarnate`. On a `dispatch_agent` one-shot event or recurring schedule, `template` (when set) must be a single-path-component template name or a template ID (`400`, `template must be a template name or ID`); at fire time, a project default template (`scion.io/default-template`) in another form fails the dispatch with `the project's default template setting must be a template name or ID`, while a Hub default template (`agent_defaults.default_template`) in another form is ignored with a warning.
- **Scheduled Agent Runtime Broker**: A `dispatch_agent` fire picks its Runtime Broker through the same [broker selection](/scion/hosted/ha/multi-broker/#broker-selection) as a manual agent creation that names no broker, checked against the schedule's recorded authority: the project default first, with no fallback when it is offline or the creator may not use it. When no Runtime Broker is selected, the fire fails before any agent is created. The recorded error (in `scion schedule history`) gives the reason manual creation gives, followed by what to do instead, because a schedule cannot name a Runtime Broker: set the project's default Runtime Broker to an online one the schedule's creator may use; if a default is set, bring it online, give the creator use of it, or change it; if the project has no Runtime Broker, link one and make it the default. A project with several Runtime Brokers and no default refuses every scheduled fire until a default is set.
- **Token Authoring**: A [user access token](/scion/hosted/user/personal-access-tokens/) can create a scheduled event or recurring schedule with `scheduled_event:create`, and update, re-target, pause or resume a schedule with `scheduled_event:update`, on a project or hub token whose boundary contains the project. It must also pass the same target checks as a signed-in request for that event type. The revision records the token, and each fire runs within that token's permission ceiling and boundary. A fire fails once that token is revoked, deleted or has expired, so a recurring schedule authored with a token stops firing at the token's expiry; pause and resume it with a sign-in or a newer token to record new authority. This does not cover messages scheduled in a chat conversation, which still refuse every user access token.
- **Authority recorded per revision (`dispatch_agent`)**: Creating a `dispatch_agent` event or schedule, and any update, re-target or resume that changes what a future dispatch does, records an *authorization revision*: who made the request, which credential they used, and that credential's permission ceiling at the time. Each fire runs under the latest revision. When the event fires, the creator's principal must be valid at that time: a user principal must be active and hold `agent.create` in the project, or an agent principal must exist in the project, and an access token the revision was recorded with must not be revoked, deleted or expired. If any check fails, that fire is recorded as failed. For a recurring schedule whose recorded authority is an agent, deleting that agent pauses the schedule with `statusReason` `creator_deleted`, and deleting a user does the same for every schedule whose recorded authority is that user, in any project (and the evaluator pauses such a schedule as a backstop if it comes due; see [Schedules paused by the Hub](#schedules-paused-by-the-hub)). Pausing, deleting and edits that change only metadata write no revision.
- **Scheduled Message Authority**: A scheduled message is sent under the authority recorded by the last request that created the event or schedule, changed what, where or when it sends, or resumed or re-enabled it — not under the original creator's identity. When the message fires, that principal must be active and admitted to the project, an access token it was recorded with must be live, and the send must pass the same messaging rules as a direct send. For a recurring schedule whose recorded authority is an agent or a user, deleting that agent or user pauses the schedule with `statusReason` `creator_deleted` (see [Schedules paused by the Hub](#schedules-paused-by-the-hub)). A request to create, change or resume a message event or schedule with a credential whose authority cannot be recorded is denied with a 403, as for an agent-dispatch schedule, and nothing is written. A message event or schedule without recorded authority (written before authority was recorded) does not fire, and its event records `schedule authority not recorded; pause and resume the schedule, or recreate the event`. Pausing and resuming the schedule, or recreating the event, records the caller's authority.
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
