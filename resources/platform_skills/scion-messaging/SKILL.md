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
- **`group[a,b,...]`**: Use this for group messaging to a specific list of agents/users (e.g., `group[tech-lead, editor]`).
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
- **Keep tone conversational and short.** Messages should be functional but not robotic — write like a colleague, not a status report.
- **You are identified as a sender** — the system already shows your identity with every message. Don't open with "Hi, this is agent-X" or restate who you are.
- **Confirm receipt, then report completion.** When you receive a task, respond immediately to confirm you got it. Then report again when the work is done. Don't leave a user wondering whether their message was received.
- **Include Concrete Details**: Reference file paths, branch names, URLs, and specific error messages.
- **Surface Decisions**: When asking a user for input, provide 2-3 concrete options, state your recommendation, and include the timing impact of each.
- **Keep it Concise**: Focus on key findings and links rather than lengthy narratives.

## Channel and Thread Targeting

- **`--channel <name>`**: Use this to target a specific delivery channel (e.g., `telegram`, `discord`, `web`).
- **`--thread-id <id>`**: Use this to reply within a specific project thread, ensuring continuity for the user.

## Special Message Flags

The `scion message` command provides powerful flags for advanced orchestration:

- **`--raw`**: Sends literal keystrokes to an agent's tmux terminal (e.g., `scion message agent:editor --raw "ENTER"`). Useful for unblocking interactive prompts.
- **`--wake`**: Resumes a suspended agent and delivers the message.
- **`--interrupt`**: Interrupts the target agent's current process before delivering the message (use with caution).
- **`--notify`**: Subscribes you to state-change notifications (e.g., completion, stall) for the target agent. Note: `--notify` on `scion start` is **deprecated** — agents that create other agents are automatically subscribed via creation ancestry. Use `--notify` on `scion message` only when subscribing to an agent you did not create.
- **`--attach <file>`**: Attaches one or more files to the message.
- **`--in <delay>`**: Schedules a message for a relative delay (e.g., `--in 5m`).
- **`--at <time>`**: Schedules a message for an absolute time (e.g., `--at "2026-06-10 14:00"`).

## Agent-to-Agent Coordination Patterns

- **Coordinator Relay**: Workers generally communicate through the coordinator rather than directly with each other. This guidance may be set by the coordinator.
- **Avoid being a relay.** If an agent needs to communicate something to a user, have them message the user directly rather than relaying through you. Relay adds latency, risks reframing the message in transit, and wastes context.
- **Self-Callback Heartbeat**: For very long external tasks, use `scion message --in` to send yourself a reminder to check on the process or provide a status update. (during long blocked periods)

## Multi-User Communication

In projects with multiple users:
- Reply to each user independently.
- Do NOT notify other users when replying to a specific individual.
- Handle each user's requests within their own context.

## Message Length Limit

Messages to **users** (agent-to-human-inbox path) are limited to **2000
characters** (counted as Unicode runes, not bytes — CJK and emoji each
count as one character). Agent-to-agent messages have **no enforced cap
in code** and are not subject to this limit.

When the limit is exceeded, the command returns a non-zero exit code but
also dumps the full CLI `--help` text to `stderr` — the actual error line
(`validation_error: message exceeds 2000 character limit`) scrolls off if
you pipe to `tail`. Redirect `stderr` and pipe to `head` (e.g., `2>&1 | head`) to surface it.

If your user-directed message is long:
- Split it into two or more messages, each under ~1800 characters.
- Or write the content to a shared file and send a short message with the
  file path.

## Inbound Message Types

Messages arrive wrapped in `---BEGIN SCION MESSAGE---` / `---END SCION MESSAGE---`
markers and include sender and type metadata.

**Check the `type` field before replying.** The type tells you whether a message
is addressed to you or is a notification about another agent.

- **`instruction`** — addressed to you. Read and act on it.
- **`state-change`** — a notification that an agent changed state (e.g., completed, stalled). No reply needed.
- **`input-needed`** — an agent is waiting for input. See below.
- **`mention`** — you were CC'd or mentioned in a message primarily directed at someone else. Treat as FYI — no action needed unless the message text clearly directs you to do something.
- **`group-set`** — a user @-mentioned multiple agents (not `@all`). Read and act on it like an `instruction`.

### Handling `input-needed`

When an agent calls `sciontool status ask_user`, the question text is embedded
in a notification dispatched to that agent's **subscribers** (including any
agent that created it). The message arrives as
`"<name> is WAITING_FOR_INPUT: <question>"` with type `input-needed`.

**If you are the parent agent that created the waiting agent**, you may be the
intended respondent — the child may be asking you for a decision or input as
part of your coordination. Use `scion message agent:<name>` to reply.

**If you are a peer or unrelated subscriber**, do not answer. The agent is
likely waiting for a human or its parent, and your reply will not unblock it.
Repeated appearances are status re-signals, not impatience.

Answering `input-needed` messages you are not responsible for causes:
- Wasted tokens — the reply goes nowhere useful.
- False loop signals — repeated echoes look like a stuck agent.
- **Scope violations** — answering a question meant for someone else can make a recommendation look ratified.

**To request a peer's input, send an `instruction`** via `scion message
agent:<name>`. Do not rely on your `ask_user` status signal to reach them — it
is a broadcast to subscribers, not a delivery to an addressee.

## Anti-Patterns and Red Flags

- **Red Flag**: Using `--broadcast`.
- **Red Flag**: An agent goes silent for >30 minutes without a milestone update or "blocked" status.
- **Anti-Pattern**: Sending "I'm still here" or other low-signal filler messages.
- **Anti-Pattern**: Using `sleep` to wait for something; use `sciontool status blocked` instead. For external processes that emit no notification (CI, builds, deploys), pair `status blocked` with a scheduled self-callback — see the `scion-scheduler` skill → **Waiting on external processes**.
- **Anti-Pattern**: Repeating the entire original brief in a follow-up message (exhausts context).

## Verification Checklist

- [ ] Does the message have a clear recipient (`agent:`, `user:`, or `group[]`)?
- [ ] Is the message functional and free of filler/banter?
- [ ] Does it include concrete references (paths, IDs, errors)?
- [ ] If a decision is needed, are concrete options and a recommendation provided?
- [ ] Is the message targeted to the correct channel or thread if applicable?
- [ ] For long tasks, has a milestone reporting cadence been established?
