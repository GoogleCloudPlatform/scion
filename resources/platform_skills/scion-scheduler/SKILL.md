---
name: scion-scheduler
description: >-
  Schedule one-shot and recurring events for agents using the Scion CLI.
  Covers self-callbacks, recurring cron schedules and time zones, scheduled
  in-container exec commands (--type exec), which schedules an agent can see,
  lifecycle management, and the blocked-wait pairing rule.
---

# Scion Scheduler

Schedule future events that deliver messages to agents or (when enabled) run
commands directly inside an agent's container — either one-shot (fire once at
a specific time) or recurring (fire on a cron schedule).
All schedules are **project-scoped** — an agent can only create and manage
schedules within its own project.

For full command syntax: `scion schedule --help` and its subcommands.

Use `scion schedule create` for every delayed message, including a simple
self-callback. The `--in` and `--at` flags of `scion message` are deprecated;
do not use them for callbacks.

## Waiting on external processes — the blocked-wait pairing rule

When waiting on an external, non-agent process (CI run, build job, deploy,
third-party API), calling `sciontool status blocked "..."` alone is **not
sufficient**. That command marks intentional waiting for the stall detector —
it does not poll anything or wake you when the external condition resolves.
Without a separate wake mechanism, you sit waiting and only resume if an
unrelated inbound message happens to arrive.

**Correct pattern:** schedule a self-callback, then set blocked status.

```bash
scion schedule create --non-interactive --type message \
  --agent "$(scion whoami --non-interactive --format json | jq -r .name)" \
  --message "Recheck CI status" --in 5m
sciontool status blocked "Waiting for CI run to complete"
```

The self-callback delivers the wake-up; `status blocked` prevents the stall
detector from flagging you during the wait. **Neither replaces the other.**

- `status blocked` alone → stall detector is happy, but you never wake up
- Self-callback alone → you wake up, but the stall detector may flag you first

`sciontool status blocked` alone **is** sufficient when waiting on another
agent or a scheduled event already tracked by the orchestration system —
those fire their own state-change notifications.

Use `blocked` for these specific waits, and always while your own background
job or process runs. When you have no further work until something new
arrives, signal `sciontool status idle "<reason>"` instead. If your only
pending event is a scheduled message hours away, `idle` is fine: the schedule
will wake you.

## Waking a suspended agent

An agent that Scion suspended because it was idle or completed is woken
by any scheduled message, without `--wake`. For any other suspended agent,
a scheduled message fails unless the schedule sets
`--wake` (on `create` or `create-recurring`). With `--wake` the fire resumes
the agent, then delivers. It needs permission to start the agent; without
it, or for a held agent, the fire is refused and nothing starts. A stopped
agent is not resumed. An agent can schedule its own wake-up this way only
if it holds lifecycle permission (the `full` agent role); with the
`baseline` or `readonly` role every wake fire is refused.

## Event schedules: one-shot vs recurring

### One-shot events

Fire once, then done. Specify timing with `--in` (relative delay: `30m`,
`2h`) or `--at` (absolute ISO 8601 timestamp, UTC).

### Recurring schedules

Fire on a cron expression (5-field: minute hour day-of-month month
day-of-week). Pass `--time-zone <IANA name>` (for example
`--time-zone America/Los_Angeles`) to evaluate it in that zone; a zoned
schedule keeps its local time across daylight saving changes. Without
`--time-zone` the expression is evaluated in UTC. Each recurring schedule
has a name, can be paused and resumed, and maintains execution history.

## Exec schedules (`--type exec`, experimental)

When a scheduled task only needs to run a deterministic command or script in a
running agent's container — with no message and no LLM turn — use
`--type exec` and pass `argv` after `--`:

```bash
# One-shot exec event
scion schedule create --non-interactive --type exec \
  --agent "$(scion whoami --non-interactive --format json | jq -r .name)" \
  --in 10m --timeout 60s -- /usr/local/bin/check-queue

# Recurring exec schedule (forwarded to Hub; requires Hub recurring exec support)
scion schedule create-recurring --non-interactive --type exec \
  --name hourly-probe --cron "0 * * * *" \
  --agent "$(scion whoami --non-interactive --format json | jq -r .name)" \
  --timeout 60s -- /usr/local/bin/probe
```

- **Prerequisites:** `hub.conduit` and `hub.exec_schedules` must be enabled on
  the Hub (check `scion whoami --full`), the target agent must have been
  launched or restarted with `hub.exec_schedules` on (otherwise fires record
  `skipped: agent executor unavailable`), agent clocks must be
  NTP-synchronised, and you need `agent.attach` permission on the target
  (for an agent caller, the `full` role in the same delegation chain;
  `baseline` and `readonly` are refused).
- **Execution environment:** runs directly via `execve` (no implicit shell —
  pass `-- sh -c '...'` if shell syntax is needed) as user `scion`, with cwd
  `/workspace` (else home), the harness shell's launch environment, and
  `stdin` `/dev/null`.
- **Timeouts and outcomes:** `--timeout` accepts whole-second durations from
  `1s` to `60m` (`3600s`, default `60s`). Exit `0` records `fired`; non-zero
  records `failed` (`exit status N`); timeout kills the process group
  (`SIGTERM`, then `SIGKILL` after 5s). Non-running targets are `skipped`
  (`--wake` is not supported for `exec`).
- **No output stored:** stdout and stderr are drained and discarded in the
  container. `scion schedule get` and `scion schedule history` show
  `execSummary` (`outcome`, `exitCode`, `durationMs`, `outputBytes`) and
  redact `argv` (`<redacted, N args, sha256:...>`, a SHA-256 of the command
  (`argv`)) unless the reader holds live `agent.attach`. Write logs to a file
  or send failure alerts with `scion message` from your script.
- **Long-running work (>60m) and `flock`:** run jobs up to 60m directly under
  `--timeout`. For work exceeding 60m, detach with `setsid` and guard mutual
  exclusion with `flock -n /tmp/<job>.lock` because detached descendants
  escape process-group tracking.

## Message-to-orchestrator pattern

To create agents on a timer, message a long-lived orchestrator rather than
dispatching agents directly from the schedule. The orchestrator receives the
message and creates/manages task agents as needed — keeping agent lifecycle
owned by something that can reason about it.

## Which schedules you can see

A user with list permission sees all of the project's schedules. An agent
sees and manages only the schedules and events it created (whose recorded
initiator is that agent): `list` omits the others, and `get`, `history`,
`pause`, `resume`, `cancel` and `delete` refuse them. An empty list therefore does not show that
a schedule is absent. To check another agent's schedule, ask that agent, or
a user, to read it back.

## Lifecycle management

| Action | Command |
|---|---|
| List your events and schedules (all, for a user) | `scion schedule list` |
| List only recurring | `scion schedule list --show recurring` |
| List only one-shot | `scion schedule list --show events` |
| Inspect | `scion schedule get <id>` |
| Cancel a one-shot event | `scion schedule cancel <id>` |
| Pause a recurring schedule | `scion schedule pause <id>` |
| Resume a paused schedule | `scion schedule resume <id>` |
| Delete a recurring schedule | `scion schedule delete <id>` |
| View execution history | `scion schedule history <id>` |

## Gotchas

- **Cron defaults to UTC.** For a local time, pass `--time-zone` rather than
  converting to UTC; a fixed UTC time shifts by an hour against local time
  across DST changes. Zone prefixes in the expression (`CRON_TZ=`, `TZ=`) are
  not supported.
- **`cancel` vs `pause` vs `delete`:** `cancel` is for one-shot events only.
  `pause` and `delete` are for recurring schedules only. Using the wrong command
  on the wrong type will result in an error.
- **Schedule names must be unique** within the project, including names of
  schedules you cannot see. A name conflict can come from another agent's
  schedule.
- **Clean up recurring schedules** when the task they serve is complete —
  they fire indefinitely until paused or deleted.
- **Check your existing schedules** with `scion schedule list` before
  creating new ones — duplicate schedules deliver duplicate messages. The
  list shows only your own (see above).
