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
- When you need to send literal keystrokes to an agent's terminal (via `scion keys`).
- When scheduling messages for the future (via `scion schedule create`).

**When NOT to use:** For internal cognitive work or logging that doesn't need to be seen by others. Never use messaging for banter or repetitive, low-signal status updates.

## Recipient Types

Choosing the right recipient is critical to avoid spam and ensure the message reaches the intended target.

- **`@<agent-name>`** (preferred): The preferred way to message a specific agent (e.g., `scion message @tech-lead "..."`). This addresses the agent's conversation directly.
- **`@<email>`**: Send a global DM to a user by email address (e.g., `scion message @preston@example.com "..."`).
- **`agent:<name>`** (legacy): Explicit agent addressing by name. Still works but `@<agent-name>` is preferred.
- **`<agent-name>`** (legacy): Bare agent name, equivalent to `agent:<name>`. Still works but `@<agent-name>` is preferred.
- **`user:<name>`**: Send to a user's inbox (Hub mode only).
- **`group[a,b,...]`**: Group messaging to a specific list of recipients (Hub mode only).
- **`conv:<uuid>`**: Address a conversation by ID. **Not yet supported — currently errors.**
- **`#<thread>`**: Address a named thread. **Not yet supported — currently errors.**
- **`coordinator`**: (Convention) Usually refers to the agent managing the project.

**Anti-Pattern:** Do not use `scion broadcast` for routine communication. It sends to every agent in the project, wastes context windows, and is often ignored or causes confusion. Broadcasting is now a separate command (`scion broadcast`); the old `--broadcast` flag on `scion message` has been removed.

## Message Timing and Cadence

Effective communication requires balancing responsiveness with focus.

1.  **Immediate Acknowledgment**: When assigned a significant task, reply immediately to acknowledge receipt (e.g., "Got it, starting on the tech spec for X").
2.  **Milestone Reporting**: Report at significant milestones, not continuously. Don't spam "Still working..." messages.
3.  **No Silence**: If a task takes longer than expected, send a brief update before diving back in.
4.  **Simple Questions**: Gather all necessary info first, then ask clearly. Don't send a stream of consciousness.
5.  **Status Blocked**: When waiting for a reply or a scheduled event, use `sciontool status blocked "<reason>"` to signal you are intentionally waiting.

## Message Formatting

The `scion message` CLI delivers the body argument **verbatim** — it performs no escape expansion, no markdown rendering, and no character substitution. Whatever bytes you pass are exactly what the recipient sees.

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
- **Confirm receipt, then report completion.** When you receive a task, respond immediately to confirm you got it. Then report again when the work is done. Don't leave a user wondering whether their message was received.
- **Include Concrete Details**: Reference file paths, branch names, URLs, and specific error messages.
- **Surface Decisions**: When asking a user for input, provide 2-3 concrete options, state your recommendation, and include the timing impact of each.
- **Keep it Concise**: Focus on key findings and links rather than lengthy narratives.

## Channel and Thread Targeting

- **`--channel <name>`** (deprecated): Targets a specific delivery channel. This flag triggers a deprecation warning if used; prefer `@` conversation addressing instead.
- **`--thread-id <id>`** (deprecated): Replies within a specific project thread. This flag triggers a deprecation warning if used; prefer `@` conversation addressing instead.

## Special Message Flags

The `scion message` command provides the following flags:

- **`--wake`**: Resumes a suspended agent before delivering the message.
- **`--interrupt`**: Interrupts the target agent's harness before sending the message (use with caution).
- **`--attach <file>`**: Attaches one or more file paths to the message. Repeatable.
- **`--visibility <level>`**: Sets message visibility: `normal`, `verbose`, or `full`. Controls how the message appears in conversation views with different density filters.

**Capabilities that moved to separate commands:**
- **Raw keystrokes**: Use `scion keys` to send literal keystrokes to an agent's tmux terminal (replaces the old `--raw` flag).
- **Scheduled messages**: Use `scion schedule create` to schedule messages for future delivery (replaces the old `--in` and `--at` flags). See the `scion-scheduler` skill.
- **Broadcasting**: Use `scion broadcast` to send to all agents in a project, or `scion broadcast --all` for global broadcast (replaces the old `--broadcast` flag).
- **Notifications**: Use `scion notifications subscribe` to subscribe to agent state changes (replaces the old `--notify` flag).

**Deprecated flags still accepted (with warnings):**
- `--cc`: Carbon-copy additional agents. Use `group[...]` addressing or body `@mentions` instead.
- `--plain`: Mark for plain-text delivery. Will be removed in a future release.
- `--channel` and `--thread-id`: See "Channel and Thread Targeting" above.

## Agent-to-Agent Coordination Patterns

- **Coordinator Relay**: Workers generally communicate through the coordinator rather than directly with each other. This guidance may be set by the coordinator.
- **Avoid being a relay.** If an agent needs to communicate something to a user, have them message the user directly rather than relaying through you. Relay adds latency, risks reframing the message in transit, and wastes context.
- **Self-Callback Heartbeat**: For very long external tasks, use `scion schedule create` to send yourself a reminder to check on the process or provide a status update. (during long blocked periods)

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
- **`system`** — a hub-generated operational notice (e.g. scheduled event fired, port auto-exposed, message delivery failed). Read for situational awareness; no reply needed. Check `metadata.system_category` for the specific category.

**Note:** The messaging system is transitioning to a conversation-based model where messages carry a `conversation_id` and are addressed to conversations rather than agents directly. During this transition, inbound messages continue to arrive with the type fields described above, and agents should continue to discriminate on the `type` field as documented. New fields such as `conversation_id` may appear in message metadata but are not yet required for correct agent behavior.

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

- **Red Flag**: Using `scion broadcast` for routine communication (the old `--broadcast` flag on `scion message` has been removed).
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
