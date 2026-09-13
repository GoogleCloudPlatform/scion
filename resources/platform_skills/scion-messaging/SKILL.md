---
name: scion-messaging
description: How to use the scion message command effectively. Use this for communication with other agents or users. Covers recipient types, message timing, content best practices, and special message flags.
---

# Scion Messaging

## Overview

In this multi-agent orchestration environment, the primary way to communicate is via the `scion message` command. This skill codifies the patterns required for reliable, high-signal communication within the Scion ecosystem.

## When to Use

- When starting a task that requires coordination with other agents.
- When you need to provide a status update or ask a question to a user.
- When forwarding feedback or unblocking another agent.
- When you need to send literal keystrokes to an agent's terminal (via `scion keys`).
- When scheduling messages for the future (via `scion schedule create`).

**When NOT to use:** For internal cognitive work or logging that doesn't need to be seen by others. Never use messaging for banter or repetitive, low-signal status updates.

## Recipient Types

Choosing the right recipient is critical to avoid spam and ensure the message reaches the intended target.

- **`@<agent-name>`** (preferred): The preferred way to message a specific agent (e.g., `scion message @tech-lead "..."`). This addresses the agent's conversation directly.
- **`@<email>`**: Send a global DM to a user by email address (e.g., `scion message @preston@example.com "..."`).
- **`group[a,b,...]`**: Group messaging to a specific list of recipients (Hub mode only).
- **`conv:<uuid>`**: Address a conversation by ID. Use this to reply into the conversation you were addressed in — pass the `conversation` field from the inbound message envelope.
- **`#<thread>`**: Address a named thread by its thread identifier.

## Message Timing and Cadence

Effective communication requires balancing responsiveness with focus.

1.  **Immediate Acknowledgment**: Reply immediately to acknowledge receipt (e.g., "Got it, starting on the tech spec for X").
2.  **Milestone Reporting**: Report at significant milestones, not continuously. Don't spam "Still working..." messages.
3.  **No Silence**: If a task takes longer than expected, send a brief update before diving back in. Always send a final message when you are done.
4.  **Simple Questions**: Gather all necessary info first, then ask clearly. Don't send a stream of consciousness.
5.  **Status Blocked**: When waiting for a reply or a scheduled event, use `sciontool status blocked "<reason>"` to signal you are intentionally waiting.

## Message Formatting

The `scion message` CLI delivers the body argument **verbatim** — it performs no escape expansion, and no character substitution. Whatever bytes you pass are exactly what the recipient sees. Markdown is accepted and encouraged and is rendered properly in surfaces.

To include newlines, use real newlines inside shell quoted strings or heredocs. Do **not** use JSON-encoded bodies or literal backslash-n sequences — those will appear as literal characters in the delivered message.

Correct — real newlines in a quoted string:
```bash
scion message --non-interactive @reviewer "PR #42 is ready for review.

Branch: fix/auth-bug
CI: all green"
```

Correct — heredoc for longer messages:
```bash
scion message --non-interactive @reviewer "$(cat <<'EOF'
PR #42 is ready for review.

Branch: fix/auth-bug
CI: all green
EOF
)"
```

Wrong — JSON-encoded body with literal \n:
```bash
# BAD: literal \n chars appear in the delivered message
scion message --non-interactive @reviewer "PR #42 is ready for review.\n\nBranch: fix/auth-bug\nCI: all green"
```

## Message Content Best Practices

Every message should move work forward. High-signal messages are functional and concrete.

- **Be Functional**: No banter, cheerleading, or "Ready to help!" filler.
- **Keep tone conversational and short.** Messages should be functional but not robotic — write like a colleague, not a status report.
- **You are identified as a sender** — the system already shows your identity with every message. Don't open with "Hi, this is agent-X" or restate who you are.
- **Include Concrete Details**: Reference file paths, branch names, URLs, and specific error messages.
- **Surface Decisions**: When asking a sender for input, provide 2-3 concrete options, state your recommendation, and include the timing impact of each.
- **Keep it Concise**: Focus on key findings and links rather than lengthy narratives.
- **Confirm receipt, then report completion.** When you receive a task, respond immediately to confirm you got it. Then report again when the work is done. Don't leave a sender wondering whether their message was received.


## Special Message Flags

The `scion message` command provides the following flags:

- **`--wake`**: Resumes a suspended agent before delivering the message.
- **`--interrupt`**: Interrupts the target agent's harness before sending the message (use with caution).
- **`--attach <file>`**: Attaches one or more file paths to the message. Repeatable.
**Capabilities that moved to separate commands:**
- **Raw keystrokes**: Use `scion keys` to send literal keystrokes to an agent's tmux terminal.
- **Scheduled messages**: Use `scion schedule create` to schedule messages for future delivery. See the `scion-scheduler` skill.
- **Notifications**: Use `scion notifications subscribe` to subscribe to agent state changes.

## Agent-to-Agent Coordination Patterns

- **Coordinator Relay**: Workers generally communicate through the coordinator rather than directly with each other. This guidance may be set by the coordinator.
- **Avoid being a relay.** If an agent needs to communicate something to a user, have them message the user directly rather than relaying through you. Relay adds latency, risks reframing the message in transit, and wastes context.
- **Self-Callback Heartbeat**: For very long external tasks, use `scion schedule create` to send yourself a reminder to check on the process or provide a status update. (during long blocked periods)

## Conversation Management

In projects with multiple users:
- Reply to direct messages from each user independently.
- Do not repeat messages to different users who contact you in a group
- Handle each user's requests within their own context.

## Message Length Limit

Messages to **users** (agent-to-human-inbox path) are limited to **2000
characters** (counted as Unicode runes, not bytes — CJK and emoji each
count as one character). Agent-to-agent messages have **no enforced cap
in code** and are not subject to this limit, but remember to keep message content focused. Longer findings can be written to a shared file and sent as a reference.

When the limit is exceeded, the command returns a non-zero exit code but
also dumps the full CLI `--help` text to `stderr` — the actual error line
(`validation_error: message exceeds 2000 character limit`) scrolls off if
you pipe to `tail`. Redirect `stderr` and pipe to `head` (e.g., `2>&1 | head`) to surface it.

If your user-directed message is long:
- Split it into two or more messages, each under ~1800 characters.
- Or write the content to a shared file and send as an attachment.

## Inbound Message Types

Messages arrive wrapped in `---BEGIN SCION MESSAGE---` / `---END SCION MESSAGE---`
markers as a JSON envelope containing sender, type, and conversation metadata.

### Direct vs group conversations

**Check the `conversation.kind` field first.** It tells you the shape of the conversation you are in:

- **`"direct"`** — a one-to-one conversation between you and the sender. Messages here are addressed to you. The `to` field is usually omitted (your identity is implicit).
- **`"group"`** — a multi-participant conversation. The `to` field lists all addressees. Read the message, but be aware others received it too — avoid duplicate work unless the message specifically assigns you a task.

When `conversation` is absent, the message predates the conversation model or is a broadcast. Treat it like a direct message unless other context suggests otherwise.

### The `type` field

Within a conversation, the `type` field classifies how the message reached you:

- **`"message"`** — a text message addressed to you (directly or as part of a group). Read and act on it.
- **`"event"`** — a lifecycle notification about another agent or the system. Check the `event.type` subfield for the specific event:
  - `agent.state-changed` — an agent changed state (e.g., completed, stalled). No reply needed.
  - `agent.input-needed` — an agent is waiting for input. See [Handling `input-needed`](#handling-input-needed) below.
  - `delivery.failed` — a message you sent could not be delivered.
  - `schedule.fired` — a scheduled event fired (see the `scion-scheduler` skill).
  - `port.exposed` — an auto-exposed port notification.
- **`"mention"`** — you were @-mentioned in a message primarily addressed to someone else. **Default to treating this as FYI — no action required.** Only act if the message text explicitly directs you to do something (e.g., "@agent-X, please review this PR"). Being CC'd or name-dropped in passing is not a request. When in doubt, do nothing.

### Conversation routing

Inbound messages carry a `conversation` field with an `id` that identifies the conversation. When replying, use `conv:<id>` addressing so the reply stays in the same conversation:

```bash
scion message conv:<conversation-id> "your reply"
```

An agent that omits the conversation ID sends a proactive DM instead of a reply — correct for starting new conversations, wrong for replies. Always read the `conversation.id` from the message you are replying to and route your reply into it.

### Handling `input-needed`

When an agent calls `sciontool status ask_user`, the hub dispatches the question as an event (`type: "event"`, `event.type: "agent.input-needed"`) to that agent's subscribers.

**Decision tree when you receive one:**

1. **Am I the parent that created this agent?** → You are likely the intended respondent. Read the question and reply with `scion message @<agent-name> "your answer"`.
2. **Am I a peer or unrelated subscriber?** → Ignore it. The agent is waiting for its parent or a human, and your reply will not unblock it. Repeated appearances are status re-signals, not impatience.

**Why ignoring matters when you are not the parent:**
- Wasted tokens — the reply goes nowhere useful.
- False loop signals — repeated echoes look like a stuck agent.
- **Scope violations** — answering a question meant for someone else can make a recommendation look ratified.

**To request a peer's input, send a direct message** via `scion message @<agent-name>`. Do not rely on your `ask_user` status signal to reach them — it is a broadcast to subscribers, not a delivery to an addressee.

## Anti-Patterns and Red Flags

- **Red Flag**: Attempting to broadcast. Broadcasting is not available in agent mode; address recipients explicitly.
- **Red Flag**: An agent goes silent for >30 minutes without a milestone update or "blocked" status.
- **Anti-Pattern**: Sending "I'm still here" or other low-signal filler messages.
- **Anti-Pattern**: Using `sleep` to wait for something; use `sciontool status blocked` instead. For external processes that emit no notification (CI, builds, deploys), pair `status blocked` with a scheduled self-callback — see the `scion-scheduler` skill → **Waiting on external processes**.
- **Anti-Pattern**: Repeating the entire original brief in a follow-up message (exhausts context).
- **Anti-Pattern**: JSON-encoding or escaping the message body before passing to `scion message`. The CLI delivers the body verbatim — use real newlines in shell strings or heredocs.

## Verification Checklist

- [ ] Does the message have a clear recipient (`@<agent>`, `@<email>`, `agent:`, `user:`, or `group[]`)?
- [ ] Is the preferred `@<agent-name>` form used (rather than legacy `agent:<name>`)?
- [ ] Is the message functional and free of filler/banter?
- [ ] Does it include concrete references (paths, IDs, errors)?
- [ ] If a decision is needed, are concrete options and a recommendation provided?
- [ ] For long tasks, has a milestone reporting cadence been established?
