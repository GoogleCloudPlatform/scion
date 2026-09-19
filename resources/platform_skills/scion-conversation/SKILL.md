---
name: scion-conversation
description: >-
  How to use the scion conversation command for managing conversations.
  Covers listing, creating, viewing, participant management, and message retrieval.
  Complements the scion-messaging skill.
---

# Scion Conversation

## Overview

Use `scion conversation` to inspect and manage conversation metadata,
participants, and message history. Conversation commands require Hub mode.

This command does not send messages. Use `scion message` to write to a
conversation; see the `scion-messaging` skill for sending and reply-routing
guidance.

## When to Use

- See which conversations you participate in.
- Create a group conversation for multi-agent coordination.
- Read message history or catch up on recent activity.
- List or add participants, or leave a conversation.
- Retrieve a specific message by ID.

**When NOT to use:** Do not use `scion conversation` to send messages. Use
`scion message` for writing into conversations. Avoid creating a conversation
for a one-off direct message; send it with `scion message @<agent-name>` instead.

## Conversation References

Conversation subcommands accept three reference formats:

- **`conv:<uuid>`**: Direct conversation ID. This is the most common form and
  always works for a conversation you can access.
- **`@<agent-name>`**: The direct-message conversation with an agent.
- **`#<thread-name>`**: A named thread conversation.

Bare UUIDs are not accepted. Always include the `conv:` prefix when addressing
a conversation by ID.

## Functional Capabilities

**Reading:** `list`, `get`, `messages`, `catch-up`, and `get-message` let you
discover accessible conversations, inspect their metadata, and read all,
recent, or individual messages. Prefer structured output when another command
or agent will consume the result.

**Administration:** `create`, `join`, `leave`, `participants`, and
`set-default` create coordination spaces and manage their membership and
default agent. A conversation name is the positional argument to `create`, not
a `--title` flag. `join` is not idempotent: adding an existing participant
returns HTTP 409, so check `participants` first when membership is uncertain.

Run `scion conversation --help` for the full command reference and
`scion conversation <subcommand> --help` for authoritative usage, arguments,
and flags for an individual subcommand.

## Common Patterns

### Reply in the original conversation

Read the inbound envelope's `conversation.id`, add the `conv:` prefix, and send
the reply with `scion message`:

```bash
scion message conv:a1b2c3d4-... "Reply in the original conversation"
```

Do not use `scion conversation` to send the reply. See the `scion-messaging`
skill for the complete routing rules.

### Create a coordination space

```bash
scion conversation create "project-x coordination" --json
```

Capture the returned ID and share it as `conv:<id>` with participants.

### Catch up and manage membership

Use `catch-up` to review recent activity without rereading the full history.
Before adding a participant whose membership is uncertain, inspect
`participants`; call `join` only when they are absent.

## Relationship to `scion message`

The commands are complementary:

| Goal | Command |
|---|---|
| Send into a conversation | `scion message conv:<id> "text"` |
| Read conversation history | `scion conversation messages conv:<id>` |
| Read one message | `scion conversation get-message conv:<id> <message-id>` |
| Inspect or administer a conversation | `scion conversation get`, `participants`, `join`, or `leave` |

Use the `scion-messaging` skill for writing and reply routing. Use this skill
for reading and conversation administration.

## Anti-Patterns

- Passing a bare UUID instead of a `conv:<uuid>` reference; the command rejects
  bare UUIDs.
- Trying to send through `scion conversation`; use `scion message` instead.
- Treating `scion conversation join` as idempotent; an existing participant
  produces HTTP 409.
- Creating a conversation for a one-off message; use `scion message @<agent>`
  for a direct message.
