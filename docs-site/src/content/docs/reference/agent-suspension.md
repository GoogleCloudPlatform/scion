---
title: Agent Suspension When a Member Leaves a Project
description: What the Hub does with a user's agents when that user's access to a project ends, how held agents are stopped, and how they are resumed.
---

When a user's access to a project ends, the Hub suspends the agents that user
started in the project, together with every agent those agents created. A
suspended agent keeps its workspace and state. It does not run, send
messages, fire schedules or create agents until it is resumed. In this
release a hub admin resumes it by lifting its hold (see
[Resuming](#resuming)); resume by a project owner is planned.

## When it applies

Access to a project ends when the user no longer has project membership or a
system role that applies to the project. Every way that can happen is
covered:

- a project member binding is removed, or changed to a role without access;
- all of a member's project roles are removed;
- a user is removed from a group (or a group is deleted) that gave them
  project access;
- a project role binding expires;
- a hub-level role that gave access is removed.

A user who keeps access through another binding, a group or a system role is
not affected.

## What happens

1. **Live refusal.** From the moment access ends, every capability the
   user's agents use is refused: starting or waking, minting agent
   credentials, reading project secrets and environment variables, sending
   messages, creating agents, firing their schedules, and minting GitHub or
   GCP tokens. This check runs on every request and does not wait for
   anything else.
2. **Hold.** The Hub records a *hold* on each agent rooted at the user in the
   project, including agents created by those agents, agents started by
   their schedules, and soft-deleted agents (a later restore brings them back
   suspended). In the same transaction the agents' Hub credentials are
   revoked. Their run intent is set to stopped right after the transaction
   commits; the hold already refuses every start in the meantime. Each hold
   is recorded in the audit log with the actor who removed the member, the
   trigger and the correlation ID.
3. **Stop.** The Hub then stops each running container. The agent's phase
   moves to `suspended` (or `stopped` when its harness cannot resume) only
   once the runtime broker confirms the stop.

Holds survive restarts, broker status reports, reincarnation attempts and
re-adding the user to the project. Hard-deleting an agent removes its holds.

### When the runtime broker is unreachable

The hold and the credential revoke are committed even when the broker cannot
be reached. The Hub retries the stop every minute until it is confirmed, logs
each failed attempt at warning level ("stopping a held agent failed"), and
writes an `agent_hold_stop_dispatched` audit record per attempt. The agent's
phase stays as it was until the stop succeeds.

While the container is still running, its Hub credentials are already
refused. External tokens that were delivered to the container before the
hold (GitHub App installation tokens, GCP service account access tokens)
remain valid until the stop is confirmed or the token expires, which is at
most one hour after it was issued. No new external token is issued for a
held agent.

## Seeing suspended agents

`GET /api/v1/agents/{id}` and agent list responses include a `suspension`
field while an agent is held:

```json
"suspension": { "held": true, "since": "2026-10-07T10:00:00Z" }
```

The field carries no reason. The reason, root user, trigger and actor are in
the audit log (`agent_hold_set` records).

Starting, restarting, waking or reincarnating a held agent returns
`409 conflict` with "This agent is suspended. A hub admin can lift the hold."
See [Resuming](#resuming).

## Resuming

Re-adding the user to the project does not resume their agents on its own.

In this release a hub admin lifts the hold; resume by a project owner is
planned. A hub admin can lift the holds of an agent with
`POST /api/v1/agents/{id}/hold/lift` (session credential). The lift is
accepted only when every user the agent's holds name is active and admitted
to the project again; otherwise it returns `409 conflict`. The lift clears
the holds and records an `agent_hold_cleared` audit record. It does not start
the agent; start it as usual afterwards. Agent credentials can never lift a
hold.

## Background processing and upgrade

Each membership change writes a durable work item in the same transaction.
For project membership and group changes the Hub processes it right after
the change commits; for hub-level role changes it is picked up by the
background reconciler within a minute. The reconciler (every minute) also
retries anything that did not finish, and retries pending container stops
from one Hub replica at a time. Every five
minutes it also looks for project role bindings that expired, and every hour
(and once at startup) it runs a full sweep over all agents. Only the full
sweep can be put in report-only mode (see
[Upgrading: report-only first boot](#upgrading-report-only-first-boot));
nothing can be turned off.

On upgrade, the first sweep also handles users whose access ended before the
upgrade. Before it writes any hold, every sweep logs one line,
`membership standing sweep: measured before holding`, with:

| Field | Meaning |
| :--- | :--- |
| `agents_to_hold` | Agents rooted at a user who is no longer admitted, not yet held. |
| `not_admitted_pairs` | (user, project) pairs whose user is no longer admitted. |
| `walks_incomplete` | Pairs whose count is a lower bound because the tree is larger than one pass. |
| `agents_unresolved` | Live agents whose chain does not resolve to a user (no user they can be traced to, or a broken, deleted or too-deep link). They cannot be held and are refused live everywhere. |
| `lookups_failed` | Agents or pairs skipped because a lookup failed. They are retried on the next sweep; the other pairs are processed normally. |
| `first_sweep_since_start` | `true` for the first sweep after the Hub process started. |

Agents are traced to their root user through delegation records, and, for
agents created before those records existed, through their owner, ancestry
or creator. An agent up to eleven delegation steps below its root user is
traced normally; agents twelve or more steps below are refused live and held
by the sweep.

The full sweep reads every agent and the links above it once an hour, so
its cost grows with the number of agents (and the depth of their chains);
the pending-stop retry reads each agent that has an active hold once a
minute. On large hubs, expect these reads in the database load.

A very deep agent tree (more than 32 levels) is held down to that depth; the
remainder is refused live, and the work item is logged at error level and
recorded with a `membership_loss_parked` audit record.

## Upgrading: report-only first boot

**What the first boot does.** On a Hub upgraded from a release without this
enforcement, the first full sweep runs on the scheduler's first tick after
start, 0 to 30 seconds after the Hub starts. Without report-only mode it holds
every agent whose root user is not admitted to the agent's project (for
example a user removed from the project before the upgrade). It also revokes
the agent's credentials and stops its container. Clearing those holds then
needs the user to be admitted again and a hub admin
[hold lift](#resuming) for each agent.

**Turning report-only on before upgrading.** Set
`server.hub.membership_sweep_report_only` before the new binary starts for
the first time, in `settings.yaml`:

```yaml
server:
  hub:
    membership_sweep_report_only: true
```

or in the environment with `SCION_SERVER_HUB_MEMBERSHIPSWEEPREPORTONLY=true`.
The setting is read at startup only. On a replicated Hub, set it on every
replica: the sweep runs on whichever replica takes its lock. When it is on,
the Hub logs a warning at startup that the sweep is in report-only mode. Every
sweep (once at startup and then hourly) then reports instead of holding: it
places no hold, revokes no credential and stops no agent. Only the sweep
reports instead of enforcing. Membership changes made after the upgrade (a
member removed, a role binding changed or expired) still hold the affected
agents as described above.

**Reading the would-hold list.** Each report-only sweep writes one log line,
and one audit record, per agent it would hold:

- Log line (level `WARN`):
  `membership standing sweep (report-only): would hold agent and stop it if running`
  with `agent_id`, `project_id`, `root_user_id`, `reason`, `walk_incomplete`
  and `sweep_id`. The sweep ends with
  `membership standing sweep (report-only): no agent held or stopped` and the
  count in `would_hold`. The `measured before holding` line above also carries
  `report_only=true`.
- Audit record: mutation type `agent_hold_would_set`, target type `agent`,
  target ID the agent's ID, actor `system`/`hub`. The after summary holds
  `project_id`, `root_user_id`, `reason`, `walk_incomplete` and
  `"report_only": true`. All the records of one sweep share a correlation ID
  (the `sweep_id` in the log). The records are in the `mutation_audits` table
  of the Hub database, for example:

  ```sql
  SELECT timestamp, target_id, after_summary, correlation_id
  FROM mutation_audits
  WHERE mutation_type = 'agent_hold_would_set'
  ORDER BY timestamp DESC;
  ```

`reason` is `root_user_not_admitted`: the agent's root user (the user it is
traced to, as described above) is not admitted to the agent's project, or no
longer exists. `walk_incomplete: true` means that user's tree in that project
is larger than one pass, so the user may root more agents than are listed.
Agents that are already held are not listed again.

For each listed agent, either admit its root user to the project again (the
agent then drops off the list at the next sweep), or accept that it will be
held. A deleted user cannot be admitted again: their agents will be held.

**Switching enforcement on.** Remove the setting or set it to `false`
(unset `SCION_SERVER_HUB_MEMBERSHIPSWEEPREPORTONLY`), then restart the Hub
(every replica). The sweep at that start holds and stops the agents still on
the list. Report-only mode is meant for the upgrade window: while it is on,
agents whose root user lost access before the upgrade are not held, revoked
or stopped. The [live refusal](#what-happens) does not depend on the sweep:
those agents' requests (credentials, secrets, messages, agent creates,
schedules, external tokens) are still refused while their containers run.
