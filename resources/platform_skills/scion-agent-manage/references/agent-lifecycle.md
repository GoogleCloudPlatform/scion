# Agent Lifecycle

When to delete an agent, when to stop one, and who may authorize it.

## Default: delete when done

`scion delete <name> --non-interactive` frees the broker slot. `scion list` silently
truncates at 50 agents — stopped agents count against that ceiling. **Delete is the
default disposition for a completed agent.** Use `--preserve-branch` when the agent's
branch may still need review.

`scion stop` is justified only when you need the agent's terminal state within the
current work phase — for example, to inspect logs before deciding whether output was
accepted. Time-box it; do not leave agents stopped indefinitely.

> **Deleting an agent is safe because its deliverable is an artifact** — files committed
> to the repo, designs written to the scratchpad, findings in a shared document. These
> survive deletion. Terminal logs do not, and should not need to — they are not the
> audit trail.

## Who may authorize deletion

| Agent role | Deleted by | When |
|---|---|---|
| Developer, reviewer — clear start and end | The agent's creator/supervisor | Once output is accepted and verified |
| Investigator, architect — may hold an open question | The agent's creator/supervisor | **Only after all questions to humans are answered** and the conversation is explicitly done |
| Engineering manager, coordinator, project lead | **Only on explicit human instruction naming the workstream** | Human says "close down the X workstream" or equivalent |

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

## Anti-patterns

- **Deleting an agent immediately on completion signal.** Wait for explicit confirmation
  or apply the role-based rules above.
- **Interpreting "clean up" as permission to delete leads.** It never is, unless the
  human names the specific workstream being closed.
- **Leaving agents stopped for audit trail.** Commit findings to files instead. Stopped
  agents consume broker slots and count against the 50-agent list ceiling.
- **Deleting an agent with uncommitted work.** Always verify work is committed or
  preserved before deletion.
