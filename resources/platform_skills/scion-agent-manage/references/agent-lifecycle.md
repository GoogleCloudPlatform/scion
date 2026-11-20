# Agent Lifecycle

When to delete, suspend, or stop an agent, and who may authorize it.

## Upgrading in place instead of replacing

If a long-lived agent (lead, coordinator, architect) needs a newer template, image, or
harness config but should keep its identity, conversations, and lineage, use
`scion reincarnate <agent>` (or `scion reincarnate` from inside the agent, to migrate
itself) instead of delete-and-recreate; see its `--help` for the contract.
Patch flags (`--model`, `--image`, `--role`, `--service-account`, `--thinking-level`,
`--harness-auth`, and `--harness`, which deletes and rebuilds the agent home) change that
setting on the new generation; `reincarnate` refreshes the agent's existing template and
has no flag to switch to a different template (switching templates requires deleting and
recreating at a safe boundary).
Reincarnating another agent needs permission to delegate its role, but the agent keeps
its existing delegator (and so its authority) unless you change its role with `--role`.
An agent denied with `ceiling_unrecorded` (for example, it cannot assign a service
account after a hub upgrade) has a delegation chain without recorded provenance. Ask an
authorized user to reincarnate it, or to recreate it directly: a user's reincarnation
re-records the agent's delegation with that user as delegator, even without a role
change. Reincarnating it yourself, or from another agent, keeps the chain and does not
clear the denial. Run `--dry-run` first to catch unintended model or config fallback
drift, and move any needed container-local `/tmp/` files to persistent storage beforehand
(in-place container recreation wipes `/tmp/`, while keeping the agent home unless
`--harness` or `--broker` rebuilds it). Because reincarnation returns before the
replacement container finishes starting, verify that the agent reaches `running` (rather
than `error`) and confirm a resumed-work receipt; this proves runtime continuity, whereas
`resolvedVersion` in `~/.scion/resolved-skills.json` inside the container verifies
installed `gh://` skill revisions when a task depends on newly merged guidance
(`--dry-run` does not show `gh://` skill commit changes).

### Moving to another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves an agent to another Runtime Broker,
keeping its identity and its workspace (uncommitted work included). Check first with
`--dry-run`; it changes nothing and prints every eligibility check.

- **Requirement:** both brokers mount the **same NFS export** and the agent's workspace
  is on it. The workspace is never copied or re-cloned; a move that does not meet this
  is refused before anything changes. Both brokers must support agent move and be
  online (an offline source is refused).
- **What carries over:** the workspace on the export, the agent ID, slug and lineage.
  The agent home is regenerated on the target and harness session history is not
  carried, so write a full handoff (`--handoff-file`), as for any reincarnation.
- **Moving yourself:** allowed only to a broker that already serves your project
  (otherwise `409`); an agent with a GCP passthrough identity cannot move itself
  (`403`: ask a user). Follow the five-line contract in `--help`. The CLI does not set
  your status; it only prints that the container will be stopped shortly, so do nothing
  after the call.
- **Moving another agent (as an agent):** for example, a coordinator moving its child.
  You need lifecycle permission on that agent. The target must already serve the
  project, and must auto-provide unless your scopes include `project:agent:create`.
  An agent never links a new broker to the project; a broker you cannot see returns
  `404`.
- **Moving another agent (as a user):** reaching a broker that does not serve the
  project yet needs a signed-in user with project update, plus broker read and broker
  dispatch unless the broker auto-provides. A user access token can only reach brokers
  that auto-provide.
- **Failure:** the move runs in the background after the request is accepted, so a
  failure shows as the agent in the `error` phase with the reason in its status
  message. A failure after the agent is assigned to the target and before the new
  generation is running rolls the move back: the agent is left stopped on the source,
  in the `error` phase, with its workspace intact. Start it or retry. If the target's start may have left a container,
  the agent stays on the target in the `error` phase instead.

## Default: delete when done

`scion delete <name> --non-interactive` frees system resources.  **Delete is the
default disposition for a completed agent.** Add `--preserve-branch` to keep the branch
for later review — but the flag does not push; confirm the branch is on the remote first.

When an agent must remain available for follow-up — such as an unanswered human question,
open PR review, or an idle lead between assignments — and its harness supports session
resume, ensure its state and handoff are durable and use `scion suspend <name>` (under the
same creator/supervisor authority) to stop the container while keeping the harness session
resumable. Suspension releases the active broker and project capacity slot without closing
open human questions; human silence is not closure. An agent you suspend receives nothing
until someone with lifecycle permission on it (normally you) resumes it: messages without
`--wake` fail (`scion-messaging`), and its own schedules fire only if they set `--wake`
(`scion-scheduler`). Before suspending, confirm the agent has read any notice you sent; a
message sent moments earlier can still fail delivery. Keep an agent running while it waits
on a delivery from a peer that cannot wake it. `scion stop` ends the session (the next
`scion resume` begins a fresh session) and is justified only for short, time-boxed terminal or
incident inspection when resume is unsupported or unwanted; do not leave agents stopped
indefinitely.

> **Deleting an agent is safe because its deliverable is an artifact** — commits pushed
> to the remote, files written to a shared volume the container's deletion cannot reach,
> PRs opened. These survive deletion.
> A commit that was never pushed is local to the container and dies with it — committed
> is not the same as pushed. Terminal logs do not survive either, and should not need
> to — they are not the audit trail.

## Who may authorize deletion

| Agent role | Deleted by | When |
|---|---|---|
| Task-bounded agent — clear start and end | The agent's creator/supervisor | Once output is accepted and verified |
| Agent with an open human question | The agent's creator/supervisor | **Only after all questions to humans are answered** and the conversation is explicitly done |
| Lead — the first agent on a project or workstream, an agent coordinating that project or workstream, or an agent explicitly designated by a human as its lead | **Only on explicit human instruction naming the workstream** | It carries continuity from the initial request through closure, including non-code and non-repository projects |
| Delegated supervisor with a bounded assignment, not a lead | Its creator or authorized supervisor, subject to the assigned workflow; a human where that workflow requires it | After its deliverable is accepted, verified and durable, its subtree is torn down, and its human questions are resolved |

### Hierarchy teardown: bottom-up deletion

**In general, do not delete agents you did not create.** When tearing down a
hierarchy, deletion happens bottom-up — each level confirms its subtree is torn
down before the level above deletes it:

1. The parent learns work is complete; notifies the orchestrator below.
2. The orchestrator deletes its completed task-bounded agents.
3. The orchestrator confirms its subtree is torn down.
4. The parent deletes the orchestrator.

Apply the role-based authority table above at each step — the orchestrator's
category (bounded worker vs. free-standing lead) determines whether its creator
can delete it directly or explicit human instruction is required.

### Rules

1. **Completion of a task is not completion of an agent.** A completion signal means the
   task is done, not that the agent should be deleted. The user may want follow-up work.

2. **An agent with an unanswered question to a human is not complete.** "Design complete"
   does not mean "conversation complete," and silence on a pending question is not closure.
   Before deleting any agent, ask: *has this agent raised open questions to the user?* If
   yes, do not delete (suspending an idle resume-capable agent after durable state is
   written is fine while waiting, and still keeps the question open).

3. **An agent's own readiness signal is not permission.** An agent saying it is ready for
   cleanup does not constitute user permission to delete it. Only a human can authorize
   deletion of leads.

4. **"Clean up the agents" means task-bounded agents.** When a human says "clean up
   agents" or "check with leads about cleanup," they mean completed **task-bounded**
   agents. They do **not** mean delete the leads themselves.

   | Human phrase | Means |
   |---|---|
   | "Clean up agents" | Delete completed **workers** only |
   | "Close down the X workstream" | Delete the lead for **that named** project |
   | "Check with leads about cleanup" | Ask leads which of **their sub-agents** are safe to delete |
   | A lead's own readiness signal | **Never** authorizes deleting that lead |

5. **A notification is not proof of completion.** State-change notifications fire on
   sub-task transitions, not only on full-task completion. Before treating an agent as
   done, verify with `scion look <agent>` and confirm the artifact it owed — commit
   pushed, file written, PR opened. Acting on the notification alone causes premature
   follow-up dispatch against a state that does not yet exist. For long-lived children
   whose routine `IDLE` transitions create noisy wakeups, narrow the auto-subscription
   to drop `IDLE` while keeping `WAITING_FOR_INPUT` and terminal or exception states:
   `scion notifications update <subscription-id> --triggers COMPLETED,WAITING_FOR_INPUT,LIMITS_EXCEEDED,STALLED,SUSPENDED,ERROR,HARNESS_ERROR,HALTED`
   (use the full `id` from `scion notifications subscriptions --json`, matching `agentId`
   to the child's ID from `scion list --format json`).

## Anti-patterns

- **Deleting an agent immediately on completion signal.** Wait for explicit confirmation
  or apply the role-based rules above.
- **Interpreting "clean up" as permission to delete leads.** It never is, unless the
  human names the specific workstream being closed.
- **Leaving agents stopped indefinitely as an audit trail.** Stopped and suspended agents
  release active broker and project capacity slots, but stopped agents still retain
  workspaces, clutter project listings, and lose their active session (restarting them
  begins a fresh session and requires an available capacity slot). Time-box operational
  stops: write durable findings and handoffs while the agent is still running, then delete
  completed workers (or `scion suspend` resume-capable agents that legitimately await
  follow-up).
- **Deleting an agent with unpushed work.** Always verify work is pushed to the
  remote (not just committed locally) or written to a shared volume before deletion.
