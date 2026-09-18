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

## Subcommand Reference

### List conversations

```bash
scion conversation list
scion conversation list --kind group --surface native --project <project-id> --limit 20
scion conversation list --json
```

`list` shows conversations you participate in. Filter with `--kind`,
`--surface`, or `--project`; control the result count with `--limit`; and use
`--json` for machine-readable output.

### Get conversation details

```bash
scion conversation get conv:a1b2c3d4-...
scion conversation get @my-agent --json
```

`get <conv-ref>` shows conversation metadata, kind, surface, and participants.

### Create a group conversation

```bash
scion conversation create "project-x coordination"
scion conversation create "project-x coordination" --project <project-id> --json
```

The name is the positional argument, not a `--title` flag. Use `--project` to
select a project or `--json` to capture the created conversation details.

### View message history

```bash
scion conversation messages conv:a1b2c3d4-...
scion conversation messages conv:a1b2c3d4-... --limit 50
scion conversation messages conv:a1b2c3d4-... --after 2026-09-18T10:00:00Z --json
scion conversation messages conv:a1b2c3d4-... --before 2026-09-18T12:00:00Z
```

`messages <conv-ref>` reads messages in a conversation. Use `--limit` to bound
the results and `--before` or `--after` with RFC3339 timestamps to navigate
history. Use `--json` for machine-readable output.

### Catch up on recent messages

```bash
scion conversation catch-up conv:a1b2c3d4-...
scion conversation catch-up conv:a1b2c3d4-... --since 30m --json
```

`catch-up <conv-ref>` shows messages from the last hour by default. Use
`--since` to select another duration and `--json` for machine-readable output.

### List participants

```bash
scion conversation participants conv:a1b2c3d4-...
scion conversation participants conv:a1b2c3d4-... --json
```

`participants <conv-ref>` lists the conversation's participants.

### Add a participant

```bash
scion conversation join conv:a1b2c3d4-... agent <agent-id>
scion conversation join conv:a1b2c3d4-... user <user-id>
```

`join <conv-ref> <principal-kind> <principal-id>` requires all three arguments.
The principal kind must be `agent` or `user`. This command is not idempotent:
it returns HTTP 409 if the participant already exists. Check participants
before adding someone when duplicate membership is possible.

### Leave a conversation

```bash
scion conversation leave conv:a1b2c3d4-...
```

`leave <conv-ref>` removes the caller from the conversation.

### Get a specific message

```bash
scion conversation get-message conv:a1b2c3d4-... <message-id>
scion conversation get-message conv:a1b2c3d4-... <message-id> --json
```

`get-message <conv-ref> <message-id>` requires both arguments and reads one
message. Use `--json` for machine-readable output.

### Set the default agent

```bash
scion conversation set-default conv:a1b2c3d4-... <agent-id>
```

`set-default <conv-ref> <agent-id>` sets the conversation's default agent.

## Common Patterns

### Reply to an inbound message

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

Capture the returned ID and share it as `conv:<id>` with the participating
agents.

### Monitor a conversation

```bash
scion conversation catch-up conv:a1b2c3d4-...
```

### Check before adding a participant

```bash
scion conversation participants conv:a1b2c3d4-...
scion conversation join conv:a1b2c3d4-... agent <agent-id>
```

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
