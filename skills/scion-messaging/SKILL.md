---
name: scion-messaging
description: Teaches agents how to use the scion message command effectively. Use this for ANY agent type that needs to communicate with other agents or users. Covers recipient types, message timing, content best practices, and special message flags.
---

# Scion Messaging

## Overview

In a multi-agent orchestration environment, communication is the primary failure mode. Agent terminal output is invisible to everyone outside the container. The **only** way to communicate is via the `scion message` command. This skill codifies the patterns required for reliable, high-signal communication within the Scion ecosystem.

## When to Use

- When starting a task that requires coordination with other agents.
- When you need to provide a status update or ask a question to a user.
- When forwarding feedback or unblocking another agent.
- When you need to send literal keystrokes to an agent's terminal.
- When scheduling messages for the future.

**When NOT to use:** For internal cognitive work or logging that doesn't need to be seen by others. Never use messaging for banter or repetitive, low-signal status updates.

## Recipient Types

Choosing the right recipient is critical to avoid spam and ensure the message reaches the intended target.

- **`agent:<name>`**: Use this to message a specific agent by its name (e.g., `agent:tech-lead`).
- **`user:<email>`**: Use this to message a human user directly (e.g., `user:preston@example.com`).
- **`set[a,b,...]`**: Use this for group messaging to a specific list of agents/users (e.g., `set[tech-lead, editor]`).
- **`coordinator`**: (Convention) Usually refers to the agent managing the project.

**Anti-Pattern:** NEVER use `--broadcast`. It spams every agent in the project, wastes context windows, and is often ignored or causes confusion.

## Message Timing and Cadence

Effective communication requires balancing responsiveness with focus.

1.  **Immediate Acknowledgment**: When assigned a significant task, reply immediately to acknowledge receipt (e.g., "Got it, starting on the tech spec for X").
2.  **Milestone Reporting**: Report at significant milestones, not continuously. Don't spam "Still working..." messages.
3.  **No Silence**: If a task takes longer than expected, send a brief update before diving back in.
4.  **Simple Questions**: Gather all necessary info first, then ask clearly. Don't send a stream of consciousness.
5.  **Status Blocked**: When waiting for a reply or a scheduled event, use `sciontool status blocked "<reason>"` to signal you are intentionally waiting.

## Message Content Best Practices

Every message should move work forward. High-signal messages are functional and concrete.

- **Be Functional**: No banter, cheerleading, or "Ready to help!" filler.
- **Include Concrete Details**: Reference file paths, branch names, URLs, and specific error messages.
- **Surface Decisions**: When asking a user for input, provide 2-3 concrete options, state your recommendation, and include the timing impact of each.
- **Keep it Concise**: Focus on key findings and links rather than lengthy narratives.

## Channel and Thread Targeting

- **`--channel <name>`**: Use this to target a specific delivery channel (e.g., `telegram`, `gchat`, `web`).
- **`--thread-id <id>`**: Use this to reply within a specific project thread, ensuring continuity for the user.

## Special Message Flags

The `scion message` command provides powerful flags for advanced orchestration:

- **`--raw`**: Sends literal keystrokes to an agent's tmux terminal (e.g., `scion message agent:editor --raw "ENTER"`). Useful for unblocking interactive prompts.
- **`--wake`**: Resumes a suspended agent and delivers the message.
- **`--interrupt`**: Interrupts the target agent's current process before delivering the message (use with caution).
- **`--notify`**: Subscribes you to state-change notifications (e.g., completion, stall) for the target agent.
- **`--attach <file>`**: Attaches one or more files to the message.
- **`--in <delay>`**: Schedules a message for a relative delay (e.g., `--in 5m`).
- **`--at <time>`**: Schedules a message for an absolute time (e.g., `--at "2026-06-10 14:00"`).

## Agent-to-Agent Coordination Patterns

- **Coordinator Relay**: Workers generally communicate through the coordinator rather than directly with each other.
- **Context Sharding**: For long-running tasks, split the work into batches of ≤10 items. Have the agent message the coordinator after each batch to avoid context exhaustion.
- **Self-Callback Heartbeat**: For very long tasks, use `scion message --in` to send yourself a reminder to check in or provide a status update.

## Multi-User Communication

In projects with multiple users:
- Reply to each user independently.
- Do NOT notify other users when replying to a specific individual.
- Handle each user's requests within their own context.

## Anti-Patterns and Red Flags

- **Red Flag**: Using `--broadcast`.
- **Red Flag**: An agent goes silent for >30 minutes without a milestone update or "blocked" status.
- **Anti-Pattern**: Sending "I'm still here" or other low-signal filler messages.
- **Anti-Pattern**: Using `sleep` to wait for something; use `sciontool status blocked` instead.
- **Anti-Pattern**: Repeating the entire original brief in a follow-up message (exhausts context).

## Verification Checklist

- [ ] Does the message have a clear recipient (`agent:`, `user:`, or `set[]`)?
- [ ] Is the message functional and free of filler/banter?
- [ ] Does it include concrete references (paths, IDs, errors)?
- [ ] If a decision is needed, are concrete options and a recommendation provided?
- [ ] Is the message targeted to the correct channel or thread if applicable?
- [ ] For long tasks, has a milestone reporting cadence been established?
