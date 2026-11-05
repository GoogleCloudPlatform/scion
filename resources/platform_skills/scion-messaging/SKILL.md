---
name: scion-messaging
description: Use when sending messages to users or agents with scion message, choosing conversations and mentions, or interpreting incoming messages. Covers user-facing formatting, agent delivery, and reading conversation history.
---

# Scion Messaging

Use `scion message` to communicate with users and agents. Use `scion conversation`
to read history and recover context. Terminal control belongs to agent management,
not messaging.

## Choose the destination

| Destination | Use |
|---|---|
| `conv:<id>` | An existing conversation, identified by the incoming message's `conversation.id` or the task brief |
| `@<email>` | A separate direct message to a user |
| `@<agent-name>` | A direct message to an agent |
| `group[a,b,...]` | A separate exchange with a specified group of recipients |

### Messages to users

**Always reply in the conversation the user's message came from.** Read
`conversation.id` from the envelope and send to `conv:<id>`, rather than addressing
the sender by email. This applies to both direct and group conversations, including
the first message. Addressing the sender instead sends a DM message and leaves
the original conversation unanswered.

For a new user-facing message about an existing issue or workstream, use its
conversation when one exists. Leads give their teams that conversation ID in their
briefs. Use a separate user DM only for a deliberately separate exchange.

### Messages to agents

Agent-to-agent coordination often uses direct messages. Put the agent recipient
immediately after the command: `scion message @reviewer "Review the proposal."`.
Use an existing group conversation when the work belongs there and should be visible to users; explicitly
@-mention the agents who need to see or act on the message. A direct reply can also
use the direct conversation's ID. Do not require a group conversation for routine
agent-to-agent work.

## Mentions and delivery

### Sending

- A conversation post is **not pushed to every participant by default**. Mention
  the people or agents whose attention you need with `@<email>` or `@<agent-name>`
  in the body.
- A body mention can appear anywhere. For a direct agent message, the recipient
  is the `@<agent-name>` argument at the start of the command, not a bare name in
  the body.
- State whether a mention is an assignment, a question or FYI. If several agents
  must act, mention each and state its part.
- Naming an agent by slug **without `@`** in a group message does not notify it.
- There is no `--cc` flag; use body mentions.
- Check the send result. Handle reported delivery failures before assuming the
  intended recipient was reached. Do not duplicate a conversation reply into DMs
  merely to draw attention; use mentions in the conversation.

### Receiving

A `type: "mention"` notification is FYI unless the message assigns you work or asks
you a question. Read surrounding messages when context is missing. Do not duplicate
someone else's work because you were named, and do not treat a mention as authority
to act beyond your task. When responding to a group discussion, keep the response
in that discussion and mention the relevant participants.

## Writing to users

**Always use rich, structured Markdown.** Scale the structure to the message:
a short acknowledgment can have a **bold lead-in**; longer messages use paragraphs,
lists, descriptive links and code formatting. Mixed human/agent audiences use this
user-facing format. Agent-only messages do not require it.

- **Lead with the outcome.** Use a conversational tone and plain language. Explain
  abbreviations or internal labels that the user needs to understand.
- **Make decisions easy to read.** Put options in a list with their trade-offs and
  your recommendation. Put the actual question on its own line.
- **Keep details useful.** Avoid dense UUIDs, code line numbers and internal status
  codes unless the user needs them to act. Prefer descriptive Markdown links to
  bare URLs; link supporting evidence instead of pasting a technical inventory.
- **Make commands easy to copy.** Put a short shell command you want the user to run
  in single backticks, such as `scion list`. The chat renders inline code with a
  copy button, so the user can copy it into a terminal. Use a fenced `bash` or `sh`
  block for multiline commands. Keep explanation outside the code.
- **Acknowledge, update, finish.** Acknowledge a user's task promptly, report
  meaningful milestones during longer work, and send the result when done. Avoid
  repeated status-only messages.

Example:

```markdown
**The proposal is ready for review.**

- **Changed:** replies stay in the issue conversation.
- **Review:** [Messaging proposal](https://example.com/proposal).

**Decision:** should agent-only updates use the same layout, or stay plain text?
My recommendation is plain text for agent-only updates.
```

### Use the user's timezone

Express times, dates and deadlines in the user's timezone, **using the timezone
provided in messages to you**. Do not substitute the agent container's timezone.
Include the local date and zone when useful, accounting for daylight saving time
on that date. Preserve original timestamps in quoted evidence; give a local-time
explanation alongside them when needed.

If no timezone was provided, do not guess. Label any necessary timestamp explicitly
as UTC and ask for the timezone only when a time-dependent decision needs it.
Messages without times need no timezone question.

## Writing to agents

Use concise, actionable text. Rich Markdown and conversational framing are optional.
Include the technical details the recipient needs: exact paths, revisions, relevant
message IDs, error text, evidence locations and the requested next action. Distinguish
an assignment from FYI.

An immediate acknowledgment is useful when the sender needs receipt confirmed or
handoff coordination depends on it; it is not required for every agent message.
Report completion, a blocker, or a result that unblocks the next step.

## Practices for every audience

- **Do not introduce yourself.** The recipient already sees you as the sender.
  Omit openings such as “Hi, this is agent-X.”
- Keep each message purposeful and concise: outcome, necessary evidence, and next
  action or decision. Avoid filler and repeated briefs.
- Avoid being a relay. Let the responsible agent communicate with the user in the
  relevant conversation; “directly” means without an intermediary, not necessarily
  by DM.
- When intentionally waiting for input or another agent, signal
  `sciontool status blocked "<reason>"`. Do not poll agents with sleep loops. For
  external work without completion notifications, follow `scion-scheduler` guidance
  on scheduled callbacks.

## Preserve Markdown when sending

The CLI sends the body **verbatim**, including real newlines. Do not JSON-encode the
body or send literal `\n` sequences as paragraph breaks.

Markdown backticks make commands copyable in chat. Separately, **your shell treats
backticks inside a double-quoted command argument as command substitution**, before
`scion` receives the text. This is shell behavior, not a chat feature. Use a quoted
heredoc or a body file to preserve backticks and `$` characters safely:

```bash
scion message "conv:<conversation-id>" - <<'EOF_MESSAGE'
**Next step:** run `scion list` and share the result here.
EOF_MESSAGE
```

Or write the body to a file and use
`scion message "conv:<conversation-id>" --body-file /tmp/message.md`.
A positional `-` or `--body-file -` reads stdin. Trailing CR/LF characters are trimmed;
other body bytes are preserved.

For example, in the shell expression `` "Run `printf substituted`" ``, the shell
runs `printf` and produces `Run substituted`. The same text inside a quoted heredoc
is literal. Do not embed user-facing Markdown commands in double-quoted shell
arguments and expect the backticks to survive.

## Read older messages and catch up

Use `scion conversation` before asking someone to repeat context:

```bash
# Catch up on recent activity.
scion conversation catch-up "conv:<conversation-id>" --since 2h --json

# Read older messages; move the cutoff back for earlier pages.
scion conversation messages "conv:<conversation-id>" --before "<RFC3339-time>" --limit 50 --json

# Read a cited message.
scion conversation get-message "conv:<conversation-id>" "<message-id>" --json

# Inspect conversation details and participants.
scion conversation get "conv:<conversation-id>"
```

A limited page or lookback is not the full history. Widen the window or fetch by ID
when an expected message is missing. If access is refused, report that limitation
rather than claiming the message does not exist. See `scion-conversation` for
conversation and participant management.

## Inbound messages and events

Messages arrive in `---BEGIN SCION MESSAGE---` / `---END SCION MESSAGE---` envelopes.
Read the sender, `conversation.id`, `conversation.kind`, message type and body.

- **`conversation.kind: "direct"`:** a one-to-one exchange. The recipient is implicit
  and `to` is usually omitted.
- **`conversation.kind: "group"`:** a multi-participant conversation. The `to` list
  names explicit addressees, not everyone who can see the conversation. Check which
  work is yours before acting.
- **`type: "message"`:** a message addressed to you; act on its content as appropriate.
- **`type: "mention"`:** follow the receiving-mentions guidance above.
- **`type: "event"`:** inspect `event.type`; a notification does not automatically
  require a reply or action.

Common events include `agent.state-changed`, `agent.input-needed`, `delivery.failed`,
`schedule.fired` and `port.exposed`. Use the event's content to decide whether you
own the next action. A state change alone does not require an acknowledgment.

### Handling `agent.input-needed`

If you created the agent and own its work, you are likely the intended respondent:
read the question and answer through the appropriate agent conversation or direct
message. If you are a peer or unrelated subscriber, do not answer on the parent's
or human's behalf. Repeated notifications are status signals, not new requests to
you. To ask a peer for input, message or @-mention that peer explicitly; do not rely
on `ask_user` status notifications to deliver the request.

## Flags and related commands

- `--body-file <path>` reads the message body from a file; `--body-file -` reads stdin.
- `--attach <file>` attaches a file; repeat it for multiple files.
- `--wake` resumes a suspended target before delivery.
- `--interrupt` interrupts the target harness before delivery; use with caution.
- `scion schedule create` schedules messages; see `scion-scheduler`.
- `scion notifications subscribe` subscribes to agent state changes.

## Message length

Messages to users through the human inbox are limited to **2000 characters**
(Unicode runes, not bytes). The CLI exits nonzero and reports a validation error
when the limit is exceeded. Check the error rather than assuming delivery.

For longer content, send a short summary with a link to a published artifact, or
attach a file. Split into a few messages only when each part remains useful on its
own. Agent-to-agent messages have no enforced cap, but still keep them focused.

## Before sending

- Is the destination appropriate for the audience and conversation?
- Are required participants @-mentioned, with action or FYI clear?
- For users: is the message readable Markdown, with descriptive links, copyable
  commands and times in the timezone supplied in their messages?
- For agents: are the necessary technical details and next action present?
- Have you omitted unnecessary self-identification and preserved literal Markdown?
- Did the send succeed, or does a delivery failure need attention?
