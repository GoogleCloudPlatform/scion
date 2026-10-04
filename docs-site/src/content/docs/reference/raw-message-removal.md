---
title: Migrating from raw message delivery
description: Raw keystroke delivery through messages has been removed. What changed, what callers now see, and how to move to scion keys.
---

Scion used to let a message carry a `raw` flag that sent its text to the agent's terminal as
literal keystrokes instead of as a message. That path has been removed. Keystrokes now go only
through the dedicated keys operation: `scion keys` on the CLI, or `POST .../keys` on the Hub API.
Ordinary, `plain` and `interrupt` messages are unchanged.

The removal was a direct cutover. There is no compatibility window, no fallback, and no automatic
retry: a client that still sends `raw` is refused, and nothing is delivered.

## What callers now see

| Caller | Before | Now |
| --- | --- | --- |
| `scion message --raw <agent> <keys>` | Sent keystrokes | Fails in argument validation, before any request is sent, with guidance naming `scion keys`. This applies to any value of the flag, any target form and both local and Hub mode. |
| Hub `POST /api/v1/agents/:id/message` or `POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/message` with `raw` or `structured_message.raw` | Sent keystrokes | `422 raw_input_removed` |
| Hub `POST /api/v1/projects/:projectId/broadcast` with `raw` or `structured_message.raw` | Rejected with an older code | `422 raw_input_removed` |
| Message Broker plugins: `POST /api/v1/broker/inbound` or `/inbound/routed` with `raw` or `message.raw` | Rejected with an older code | `422 raw_input_removed`, checked before topic validation and sender resolution |
| A scheduled event or recurring schedule whose advanced `payload` JSON contains `raw` | Rejected with an older code | `422 raw_input_removed`; nothing is stored |
| Runtime Broker `/message` (Hub-to-broker only) with `raw` or `structured_message.raw` | Sent keystrokes | `422 raw_input_removed`; nothing reaches the agent |

The field is rejected whatever its value (`true`, `false`, `null`, a wrong type or a malformed
value) and however it is capitalized. A rejected request has no side effects: no message or
conversation row, no dispatch, no event, no notification, and no rate-limit charge.

The error uses the standard Hub error envelope. `code` is `raw_input_removed`, the message names
`scion keys`, and `details` carries:

- `operation_id`, for correlating the audit record;
- `ingress`, naming the route that refused the request;
- `replacement`, the generic keys route (`POST /api/v1/agents/{id}/keys`, or the project-scoped
  form for the project-scoped message route). It never names the resolved target.

Each refusal writes one content-free audit line (`route=message_raw_removed`, with an `ingress`
field) to the Hub's structured log. Search for it to find callers that still send `raw`. The line
never contains the message text.

On the two agent `/message` routes the caller must be authenticated and the target is looked up
before the `raw` check, so an unknown target still answers `404`. Once the target resolves, `raw`
is refused with `422` before message authorization runs, whatever the caller's project or scope.
The former bridge codes (`raw_combination_unsupported`) and the `unsupported_capability` reasons
for raw (`raw_plain_conflict`, `raw_broadcast_unsupported`, `raw_scheduling_unsupported`,
`raw_broker_ingress_unsupported`) are no longer returned.

## Moving to `scion keys`

Replace each `scion message --raw` call with `scion keys`, one key or literal string per call:

```bash
scion keys my-agent "Enter"
```

For the API, send `{"keys": "<string>"}` to `POST /api/v1/agents/:id/keys` or
`POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/keys`. See the
[API reference](/scion/reference/api/#agents-apiv1agents) for the full body rules and outcome
codes, and the [CLI reference](/scion/reference/cli/) for `scion keys`.

Differences to plan for:

- **Authorization.** Keys are authorized like `agent.attach`, not by message mode. A human needs to
  own the target or hold `agent.attach` on it; no built-in project role grants that. An agent needs
  `ScopeAgentLifecycle`, the same project as the target, and live attach authority on the target.
  Message-only authority is not enough, and a closed message mode does not block keys.
- **Cross-project agent callers.** An agent can never send keys to another project's agent. The
  error depends on the route and on the agent's scope:
  - project-scoped route: `422 cross_project_keys_unsupported`, decided on the project mismatch
    alone, before any scope check or target lookup;
  - top-level route, agent holds `ScopeAgentLifecycle`: `422 cross_project_keys_unsupported` for
    an existing foreign agent, `404` for a nonexistent one;
  - top-level route, agent lacks that scope: `403 keys_denied`, before the project comparison.

  A human operator with cross-project permissions is not blocked.
- **Immediate delivery only.** Keys never wake, start or queue an agent. A stopped or suspended
  target answers `409 agent_not_running`; a running target without a ready terminal answers
  `409 terminal_not_ready`.
- **No retries.** Delivery is single-attempt. `503 keys_unavailable` means dispatch never started
  and may be resent once the route is fixed. `502`/`504 keys_outcome_unknown` means the keys may
  have arrived; check with `scion look` before resending.
- **No history.** Keys create no message, conversation, event or notification, only a
  content-free audit record.
- **No sequence syntax.** `"Up Up Enter"` is typed as eleven literal characters. Send separate
  calls for separate key presses.

## Version requirements

Hub, Runtime Broker and CLI must all be on a release that includes the keys operation. An older
Hub answers `scion keys` with its own `404`, which the CLI reports as `hub_unsupported`; it never
falls back to the message path. Against an older Runtime Broker, a current Hub answers
`422 keys_unsupported`. Upgrade both rather than looking for a workaround.

Message Broker plugins need no change unless they relied on the `raw` field. Field 11 (`raw`) of
the plugin `StructuredMessage` is reserved and can never be reused, so a plugin built against an
older definition still decodes messages, and the Hub never sets the field.

Historical message rows that recorded `raw` are not rewritten. They read back as ordinary messages.
