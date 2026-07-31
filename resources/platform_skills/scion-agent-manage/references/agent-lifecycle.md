# Agent Lifecycle

When to delete an agent, when to stop one, and who may authorize it.

## Default: delete when done

`scion delete <name> --non-interactive` frees system resources.  **Delete is the
default disposition for a completed agent.** Add `--preserve-branch` to keep the branch
for later review — but the flag does not push; confirm the branch is on the remote first.

`scion stop` is justified only when you need the agent's terminal state within the
current work phase — for example, to inspect logs before deciding whether output was
accepted. Time-box it; do not leave agents stopped indefinitely.

> **Deleting an agent is safe because its deliverable is an artifact** — commits pushed
> to the remote, files written to a shared volume the container's deletion cannot reach,
> PRs opened. These survive deletion.
> A commit that was never pushed is local to the container and dies with it — committed
> is not the same as pushed. Terminal logs do not survive either, and should not need
> to — they are not the audit trail.

## Who may authorize deletion

| Agent role | Deleted by | When |
|---|---|---|
| Developer, reviewer — clear start and end | The agent's creator/supervisor | Once output is accepted and verified |
| Investigator, architect — may hold an open question | The agent's creator/supervisor | **Only after all questions to humans are answered** and the conversation is explicitly done |
| Project initiator — the first agent on a project, whatever role it started as | **Only on explicit human instruction** | It is the project's continuity point from first research through closure; its starting role does not govern its lifespan |
| Engineering manager, coordinator, project lead | **Only on explicit human instruction naming the workstream** | Human says "close down the X workstream" or equivalent |

### Hierarchy teardown: bottom-up deletion

**In general, do not delete agents you did not create.** When tearing down a
hierarchy, deletion happens bottom-up — each level confirms its subtree is torn
down before the level above deletes it:

1. The parent learns work is complete; notifies the orchestrator below.
2. The orchestrator deletes its workers (developers, reviewers).
3. The orchestrator confirms its subtree is torn down.
4. The parent deletes the orchestrator.

Apply the role-based authority table above at each step — the orchestrator's
category (bounded worker vs. free-standing lead) determines whether its creator
can delete it directly or explicit human instruction is required.

### Rules

1. **Completion of a task is not completion of an agent.** A completion signal means the
   task is done, not that the agent should be deleted. The user may want follow-up work.

2. **An agent with an unanswered question to a human is not complete.** "Design complete"
   does not mean "conversation complete." Before deleting any agent, ask: *has this agent
   raised open questions to the user?* If yes, do not delete.

3. **An agent's own readiness signal is not permission.** An agent saying it is ready for
   cleanup does not constitute user permission to delete it. Only a human can authorize
   deletion of leads and initiators.

4. **"Clean up the agents" means workers.** When a human says "clean up agents" or "check
   with leads about cleanup," they mean completed **worker** agents (developers, reviewers,
   investigators). They do **not** mean delete the leads themselves.

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
   follow-up dispatch against a state that does not yet exist.

## Anti-patterns

- **Deleting an agent immediately on completion signal.** Wait for explicit confirmation
  or apply the role-based rules above.
- **Interpreting "clean up" as permission to delete leads.** It never is, unless the
  human names the specific workstream being closed.
- **Leaving agents stopped indefinitely as an audit trail.** Terminal logs have value
  during an active workstream, but stopped agents consume system resources and count
  against the 50-agent ceiling. Time-box it: GC stopped agents at milestone boundaries
  and commit durable findings to files.
- **Deleting an agent with unpushed work.** Always verify work is pushed to the
  remote (not just committed locally) or written to a shared volume before deletion.
