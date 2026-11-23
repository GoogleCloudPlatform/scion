---
title: Receiving Replies from Scion Agents
description: Receive low-latency replies from Scion agents in a third-party coding agent or script using scion conversation watch.
---

This guide is for non-Scion agents and scripts (for example Claude Code, Gemini CLI, or an automation runner on your machine) that send messages to Scion agents with `scion message` and need to receive replies without polling on a fixed timer.

`scion conversation watch` uses server-side long-polling (`GET /api/v1/conversations/{id}/watch`, gated by the [`hub.conversation_watch`](/scion/reference/experiments/#server-experiments-with-notes) experiment, on by default) to deliver new messages with low latency while keeping the cursor entirely on the client.

:::note[Host-only command]
`scion conversation watch` is only available on the host CLI — it is not exposed inside Scion agent containers, because Scion agents already receive push delivery.
:::

## 1. Create a scoped user access token

A third-party agent authenticates as a Hub user using a [user access token](/scion/hosted/user/personal-access-tokens/) (`scion_pat_*`) passed in `SCION_HUB_TOKEN`. See [Using the scion CLI from a coding agent](/scion/hosted/user/coding-agent-cli/) for running the CLI without a stored interactive login.

Authorization on `/watch` is identical to `GET /api/v1/conversations/{id}/messages`:

| Flow | Required UAT scopes |
| --- | --- |
| Send to an agent and watch the direct message (DM) | `project:read`, `project:list`, `agent:message`, `agent:read`, `inbox:read` |
| Watch a DM (read-only) | `project:read`, `project:list`, `agent:read`, `inbox:read` |
| Watch a project group conversation (read-only) | `project:read`, `project:list` |

Create a minimal token scoped to one project from your signed-in shell:

```bash
scion hub token create \
  --project my-project \
  --name "external-agent-watch" \
  --scopes project:read,project:list,agent:message,agent:read,inbox:read \
  --expires 30d
```

Export the token in the third-party agent's environment (with no stored interactive login in `HOME`):

```bash
export SCION_HUB_TOKEN="scion_pat_..."
```

### Credentials on a local workstation server

Against a hosted Hub, use a user access token (`SCION_HUB_TOKEN`) or an interactive OAuth session (`scion hub auth login`). Delegated agent tokens and federation tokens are refused.

Against a local workstation server (`scion server start`), both `SCION_DEV_TOKEN` (from `~/.scion/dev-token`) and a user JWT (`SCION_HUB_TOKEN`) work for watching both direct messages and group conversations.

## 2. The one-shot send → watch → cursor loop

For a third-party agent that runs shell commands between reasoning turns, use `--once` with `--json`. The canonical reply path is the direct message (DM) conversation between your Hub user and the Scion agent.

### Step 1: Send the message and capture IDs

```bash
SEND=$(scion message reviewer "Please review PR #42." --format json)
CONV_ID=$(echo "$SEND" | jq -r '.conversation_id')
MSG_ID=$(echo "$SEND" | jq -r '.message_id')
```

### Step 2: Wait for the reply with `--after-message`

Pass `--after-message "$MSG_ID"` on the first watch call. This closes the race between sending and starting the watch: even if the agent replies before `watch` connects, the reply has a higher sequence number than `$MSG_ID` and is returned immediately.

```bash
REPLY=$(scion conversation watch "conv:${CONV_ID}" \
  --once \
  --after-message "$MSG_ID" \
  --timeout 2m \
  --json)
EXIT_CODE=$?
```

When a reply arrives, `watch --once --json` exits `0` and prints a compact JSON object on stdout:

```json
{
  "events": [
    {
      "eventId": "msg:ac6205ec-d1c7-444f-88ba-a4d760c89377",
      "type": "conversation.message.created",
      "occurredAt": "2026-11-23T14:28:28.487062151Z",
      "cursor": "djEuOTFjOTQzM2EtOTQ4ZC00YjAyLTgxNTYtYmMwM2NlYTllNjNlLjQ",
      "resourceUri": "scion://conversations/91c9433a-948d-4b02-8156-bc03cea9e63e/messages/ac6205ec-d1c7-444f-88ba-a4d760c89377",
      "data": {
        "id": "ac6205ec-d1c7-444f-88ba-a4d760c89377",
        "sender": "agent:reviewer",
        "recipient": "user:alice@example.com",
        "msg": "Looks good, two small nits..."
      }
    }
  ],
  "nextCursor": "djEuOTFjOTQzM2EtOTQ4ZC00YjAyLTgxNTYtYmMwM2NlYTllNjNlLjQ",
  "hasMore": false,
  "truncated": false,
  "timedOut": false,
  "retryAfterMs": 0
}
```

### Step 3: Loop with `--cursor`

Save `nextCursor` and pass it back on every subsequent watch call. `nextCursor` is **always** returned — even when `--timeout` expires with no messages (`exit 124`, `"timedOut": true`, `"events": []`) — so you never lose your place or re-receive older messages:

```bash
CURSOR=$(echo "$REPLY" | jq -r '.nextCursor')

# Later, wait for the next message without duplicates or gaps:
NEXT=$(scion conversation watch "conv:${CONV_ID}" \
  --once \
  --cursor "$CURSOR" \
  --timeout 2m \
  --json)
```

If `hasMore` is `true`, additional matching messages are already available; call `watch` again immediately with `nextCursor` to drain them. If `truncated` is `true`, one or more messages between your previous cursor and the returned event were deleted (for example by retention).

## 3. Stream mode

Omit `--once` to stay attached and receive messages continuously:

```bash
# Plain text (next-cursor printed to stderr when the command exits)
scion conversation watch @reviewer

# NDJSON: one compact Event object per line, flushed per event
scion conversation watch "conv:${CONV_ID}" --json
```

In `--json` stream mode, each message is emitted as one compact `Event` JSON line. When a server poll times out with no new messages, the CLI writes a cursor-only heartbeat line so consumers can checkpoint their position:

```json
{"type":"cursor","nextCursor":"djEuOTFjOTQzM2EtOTQ4ZC00YjAyLTgxNTYtYmMwM2NlYTllNjNlLjQ"}
```

Pass `--timeout <duration>` in stream mode to bound total run time and exit `0` when the budget elapses.

## 4. Filters and starting position

Use `--filter` to control which messages wake the watch:

- `others` *(default)*: delivers every message not sent by the caller. In a DM, this delivers every reply from the agent while ignoring your own sends.
- `all`: delivers all messages in the conversation, including your own sends.
- `addressed`: for a user, `addressed` matches messages not sent by you that were sent explicitly to you as `recipient`, or where you have a `message_addressees` row (no production path writes user addressee rows today). Body `@`-mentions of a user in a group thread do not match `addressed`, so use `--filter others` to follow a group thread as a user.

When neither `--cursor` nor `--after-message` is provided:

- Default (`start=head`): waits only for messages committed after the watch starts.
- `--from-start` (`start=beginning`): replays retained history from the first message in the conversation, respecting `--filter` and `--limit`.

## 5. Exit codes

| Exit code | Meaning | Recommended action |
| --- | --- | --- |
| `0` | `--once` delivered at least one message, or stream mode ended normally (timeout or signal). | Process output and save `nextCursor`. |
| `124` | `--once` reached `--timeout` with no matching messages. Under `--json`, a valid timeout object with `"timedOut": true` and `nextCursor` is still printed on stdout. | Save `nextCursor` and retry if you want to keep waiting. |
| `3` | Cursor expired (`410 Gone`, for example a cursor ahead of the conversation head after a database restore). | Restart with `--after-message <id>` or without `--cursor`. |
| `130` | Interrupted (`SIGINT` / `SIGTERM`) in `--once` mode. | Stop or resume from your last saved cursor. |
| `1` | Error (authentication, `403` authorization, `404` missing conversation or `hub.conversation_watch` disabled, invalid flags/cursor, or exhausted network retries). | Inspect stderr and fix credentials, scopes, or arguments. |

See [`scion conversation watch`](/scion/reference/cli/#scion-conversation-watch) for the complete flag reference.
