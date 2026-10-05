---
title: API Reference
description: Hub and Runtime Broker REST/WebSocket specifications.
---

The Scion ecosystem exposes several APIs for coordination, management, and observability. This reference provides an overview of the primary resource types and communication patterns.

## Hub API

The Scion Hub provides a RESTful API (mostly JSON) for managing the state of the system.

### Authentication
Most endpoints require a `Bearer` token in the `Authorization` header.
- **User Tokens**: Obtained via OAuth or Dev Auth.
- **Agent Tokens**: Issued to agents at startup for state reporting.
- **Broker Tokens**: Used for broker-to-hub communication, often combined with HMAC request signing.

### Pagination

List endpoints for templates, harness configs and groups return an opaque `nextCursor`. Cursors are encrypted and bound to the endpoint, the filter and the caller that received them, so pass them back unchanged to the same endpoint with the same filter. A cursor that is malformed, tampered with, reused in a different context, or sealed with a key the Hub no longer holds (for example after a key rotation) is rejected with a uniform `400 Bad Request` and error code `invalid_cursor`; restart the listing from the first page.

### Core Resources

#### Agents (`/api/v1/agents`)
- `GET /`: List agents (filterable by project, user, phase). Add `sort=created` or `sort=updated` for a server-sorted, keyset-paged view with the same `dir`, `limit` (clamped to 500), `cursor`, `fit` and `stats` parameters and the same 2,000-agent ceiling as the project agent list's sorted mode below. Without `sort` the response is unchanged. Add `view=compact` for slimmer items: each agent carries only its identity, template, project, status, labels, lineage, message mode, creator name and timestamps, plus `_capabilities` and `_messageability`, and no `appliedConfig`. The result has the same agents in the same order, with the same cursors, counts and other response fields as the default `view=full`. An empty `view` is the same as no `view`. With `sort`, a non-empty `view` other than `full` or `compact` is rejected with `400`; without `sort`, any value other than `compact` is ignored.
- `POST /`: Dispatch a new agent.
- `POST /stop-all`: Stop every running agent across all projects. Requires a user identity holding `agent.stop_all` on the hub; a user without it gets `403`. For the project-scoped variant, see `POST /api/v1/projects/:id/agents/stop-all` below.
- `GET /:id`: Get detailed agent state (phase, activity, detail). An agent can always read its own record with its agent token.
- `POST /:id/suspend`: Suspend a running agent, preserving its harness session for a later resume. Sets the phase to `suspended`. Requires a harness that supports session resume.
- `POST /:id/start`, `POST /:id/restart`: Start/restart an agent. Starting a `suspended` agent resumes (continues) its harness session; starting a `stopped` or `error` agent runs a fresh session. To continue the interrupted session of an `error` agent instead, send `{"forceResume": true}` as the `start` body (best effort). `forceResume` has no effect in other phases.
- `POST /:id/reincarnate`: Migrate the agent to a new generation with the same ID and slug and a freshly resolved config (see [`scion reincarnate`](/scion/reference/cli/#scion-reincarnate)). Body: `handoff` (optional text for the new generation's first task, max 256 KiB) and `dryRun`. Returns `202 Accepted` with the pending plan, or `200 OK` with the plan only for a dry run. The migration runs in the background. Requires `agent.lifecycle`; returns `400` for agents in worktree-per-agent projects. Also available as `POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/reincarnate`.
- `POST /:id/message`: Deliver a message to an agent. Body: `message` (plain text) or `structured_message`, plus optional flags `plain` (deliver the text without the Scion message envelope), `interrupt`, `notify`, and `wake`. Top-level `plain` applies with either body form; with `structured_message`, it is merged onto it. The whole body is capped at 2 MiB, checked before authorization (`413 payload_too_large`, no `operation_id`). Raw keystroke delivery through this route has been removed: see "Retired `raw` field" under `/:id/keys`.
- `POST /:id/keys`: Send literal terminal input to an agent's tmux session — the replacement for the removed `message`/`raw` keystroke path. Also available project-scoped as `POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/keys`. The whole HTTP body is capped at 32 KiB, rejected with `413` before anything else is parsed. Body: `{"keys": "<string>"}` only — unknown fields, duplicate `keys`, a non-string/missing/empty value, an embedded NUL byte, invalid UTF-8 or an unpaired UTF-16 surrogate escape, and a value over 4096 UTF-8 bytes are all rejected (`400`/`413`). The string is delivered verbatim as a single `tmux send-keys` argument: a recognized tmux key name (`Enter`, `Escape`, `C-c`, arrow names, etc.) is interpreted only on an exact whole-string match; anything else — including text containing spaces — is typed literally, character by character. There is no sequence syntax: `"Up Up Enter"` is eleven literal characters, not three key presses — issue separate calls for separate key presses. An empty string is rejected outright and never becomes `Enter`; whitespace-only input is valid and preserved exactly.

  Response: `{"status": "dispatched", "operation_id": "<uuid>", "agent_id": "<uuid>"}`. A `200` means the Runtime Broker acknowledged terminal injection, not that the harness consumed or acted on it. `operation_id` correlates audit and error records; it is not an idempotency key, and there is no `202`/queued state. Errors use the standard Hub error envelope with `code` set to the machine outcome (`invalid_request`, `payload_too_large`, `keys_denied`, `not_found`, `agent_not_running`, `terminal_not_ready`, `cross_project_keys_unsupported`, `keys_unsupported`, `keys_rate_limited` with a `Retry-After` header, `keys_unavailable`, or `keys_outcome_unknown`), or a generic `500` for an unexpected internal failure (which still carries `operation_id`, since validation already succeeded by the time one could occur); `details.operation_id` is present on every outcome decided inside the handler, from target resolution onward — everything except `unauthorized` (decided by auth middleware before any handler runs), the two validation failures above, and, on the project-scoped route specifically, its shared project-resolution `404` (decided before any keys-specific code runs at all, for every action on that route, not only keys).

  **Audit.** Every request that reaches the keys handler — accepted, denied, or malformed — writes content-free records to the Hub instance's structured log: timestamp, `operation_id` (when one exists), actor type and ID, source and target project, target agent, credential kind and ID, the route (`keys`), the input's byte length, the decision code, and duration. Every request admitted to dispatch writes two records — an admission record before dispatch and an outcome record after, whatever that outcome turns out to be, including an ambiguous `keys_outcome_unknown`; a request refused before dispatch writes only the single outcome record. Anything decided before the keys handler runs at all — a `401` from the shared auth middleware, a `405` for a non-`POST` method, and, on the project-scoped route, its shared project-resolution `404` — is **not** keys-audited: do not expect unauthenticated probing, a wrong HTTP method, or an unresolvable project to appear in this log. A validation-stage record (before an `operation_id` exists) reports the raw request body's byte length under a separate field instead of the decoded `keys` length — reported as `0` when the body exceeded the 32 KiB cap or could not be read, since no byte count is known in that case either. An unexpected internal failure is recorded with an `error_class` field instead of a decision code, since it isn't one of the machine outcomes above. The record never contains the `keys` value itself, any preview or hash of it, or terminal output. This is a log line emitted per Hub instance, not a queryable audit store or API.

  **Immediate-route limitations.** This operation never wakes, starts, or queues anything — it either reaches the target's terminal right now or it fails: a stopped or suspended target answers `409 agent_not_running` rather than being started; a running target without a ready terminal session answers `409 terminal_not_ready`. `503 keys_unavailable` means dispatch definitely did not start at all — an offline Runtime Broker, no immediate synchronous route to it (including a cross-Hub forward with no direct transport), or an admission window (30s by default) that already expired before the call could begin; nothing is queued for later delivery, so a caller may reasonably resend after confirming the route is fixed. `502`/`504 keys_outcome_unknown` is the opposite case — the call may have reached the terminal and partially or fully executed before the failure — and must never be retried automatically or read as confirmation of delivery.

  Authorization mirrors `agent.attach`, not message-mode authorization: a human session needs to own the target agent or hold `agent.attach` on it (closed/`none` message mode does not block it). No built-in project role grants `agent.attach`, so being a project owner, admin, or member is not by itself enough; an agent credential needs `ScopeAgentLifecycle`, the same project as the target, and live attach authority on the target via the relationship evaluator — same-project alone is not sufficient for an agent caller. Cross-project disclosure differs by route, matching the contract's documented trade-off: on the **project-scoped** route, an authenticated agent's project is compared against the URL's project *before* any target lookup, so a mismatch answers `422 cross_project_keys_unsupported` without ever revealing whether a same-slug agent exists there; on the **top-level** route, the target is resolved first (the same order every other action on that route already uses), so a foreign *existing* agent answers `422` while a *nonexistent* one answers `404` — a disclosure this route's other lifecycle actions already have today, not a new one. This `422` on the top-level route is conditional on the agent already holding the lifecycle scope checked above: an agent lacking it is refused with `403 keys_denied` before the project comparison ever runs, regardless of whether the target is foreign or same-project. The project-scoped route's `422`, by contrast, does not depend on scope — it is decided purely on the project mismatch, before any scope or target check. A human operator with live cross-project permissions is not blocked on either route; the cross-project refusal applies only to authenticated agent identities. No conversation, message row, SSE event, observer/mention fan-out, or notification is created for any outcome. Requests are rate-limited per authenticated principal+project (5 req/s, burst 10) and per target agent (10 req/s, burst 20), Hub-instance-local; local mode has no Hub quota. Delivery is single-attempt end-to-end — no SDK retry, redirect replay, or routing fallback — so a caller must not resend a call whose outcome is unknown. A managed-runtime target or a Runtime Broker without the keys route answers `422 keys_unsupported`; this is never downgraded to an ordinary message send.

  **Retired `raw` field (migrating from `raw`).** Raw keystroke delivery through messages has been removed; use this `/:id/keys` operation (or `scion keys`) instead. A request that still carries the retired `raw` field is rejected with `422 raw_input_removed` before anything happens — no message or conversation row, no dispatch, no event, no notification. This applies to both spellings (top-level `raw` and nested `structured_message.raw`, or `message.raw` on the broker inbound routes), to every value including `false` and `null`, and to malformed values. It applies on these routes:
  - `POST /api/v1/agents/:id/message` and `POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/message`;
  - `POST /api/v1/projects/:projectId/broadcast` (body capped at 2 MiB, `413 payload_too_large`);
  - `POST /api/v1/broker/inbound` and `POST /api/v1/broker/inbound/routed` (Message Broker plugins; body capped at 2 MiB, `413 payload_too_large`), checked before topic validation and sender resolution;
  - the advanced `payload` JSON of scheduled events and recurring schedules.

  The error `message` names `scion keys`, and `details` carries `operation_id`, `ingress`, and `replacement`, the generic `/keys` route template to use instead (`POST /api/v1/projects/{projectId}/agents/{agentIdOrSlug}/keys` on the project-scoped message route, `POST /api/v1/agents/{id}/keys` elsewhere). The response never names the resolved target, because the field is rejected before message authorization. Each rejection writes a content-free audit line with the route value `message_raw_removed`. Plain, normal, and interrupt messages are unaffected, and historical message rows are not rewritten.
- `PATCH /:id`: Update an agent's mutable fields: `name`, `labels`, `annotations`, `taskSummary`, `config`, and `gcp_identity`. Send `stateVersion` for optimistic locking (`409` on mismatch). `config` (for example `model`, `image`, `env`, `thinking_level`) is accepted only for agents in the `created` or `stopped` phase and takes effect on the next start, because starting a stopped agent recreates its container from the applied config. Otherwise it returns `409`. A `model` value is resolved through the harness's model aliases, and fields the agent's harness does not support return a validation error. `gcp_identity` can be changed only in the `created` phase. `explicitTimezone` pins the agent's container timezone to an IANA zone name, or unpins it with `""`; an invalid name returns `400`. It is accepted in any phase except on a deleted agent (`409`) and applies at the next start (a running container keeps its `TZ`, and the response carries a warning saying so). A `TZ` key in `config.env` is ignored with a warning; use `explicitTimezone` instead. The response is the agent plus `resolvedTimezone` (the `TZ` its next start will get, `""` when none is sent) and `timezoneSource`, which names the step of the [agent timezone chain](/scion/reference/times-and-timezones/#agent-tz-hub-dispatched-agents) that supplied it: `explicit`, `legacy` (adopted from a `TZ` an older Hub stored in the agent's env), `user`, `project`, `hub`, `broker`, `progeny`, `hub-default`, or `none`. The project-scoped `PATCH /api/v1/projects/:id/agents/:agentId` applies the same rules.
- `DELETE /:id`: Stop and remove an agent.
- `GET /:id/logs`: Stream agent logs (WebSocket).
- `GET /:id/pty`: Interactive terminal (WebSocket). The Hub relays the stream to the agent's Runtime Broker and sends keepalive pings every 30 seconds, so a dead peer is detected instead of leaving the connection hanging. See [PTY close codes](#pty-close-codes).

There is no separate resume endpoint: resuming is the **start** action applied to a `suspended` agent. A `suspended` agent is also resumed automatically when a message is delivered to it with the `wake` option set.

Agent creation and start are rejected with `429 Too Many Requests` (`quota_exceeded`) when the target runtime broker is at its `max_agents_per_broker` limit (see [Admin](#admin-apiv1admin)).

Agent responses no longer include a `visibility` field. Access is determined by scope and grants only.

Agent state uses a layered model:
- **Phase**: Lifecycle stage (`created`, `provisioning`, `cloning`, `starting`, `running`, `stopping`, `stopped`), plus `suspended` (paused for resume) and `error` (the agent crashed — restartable).
- **Activity**: Runtime activity within the `running` phase (`working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `stalled`, `offline`). Note: `offline` occurs when an agent heartbeat has not been heard for some time, often due to an expired auth token that the agent failed to refresh; `stalled` flags a live-but-hung agent and can trigger auto-suspend. (A crash surfaces as the `error` phase, not as an activity.)
- **Detail**: Freeform context (tool name, message, task summary).

#### Agent Self-Service (`/api/v1/agent`)
Endpoints an agent calls on its own behalf. They authenticate with the agent token, sent in the `X-Scion-Agent-Token` header (which `sciontool` uses) or as an `Authorization: Bearer` token, and take no agent ID in the URL: the Hub derives the agent's identity from the token.

- `POST /secrets`: Fetch several secret values in one call. Body: `{"keys": ["KEY_A", "KEY_B"]}` (1 to 100 keys, 64 KB body limit). Returns `{"secrets": [...]}`, one entry per requested key with `key`, `status` (`ok`, `not_found`, or `entitled_but_unavailable`), `value` (only when `ok`) and `error`. Per-key failures do not fail the request; a request the agent is not authorized for at all returns `403`. Values are scoped to the agent's project. `sciontool init` uses this to fetch the keys listed in `SCION_SECRET_KEYS` at startup.

#### Projects (`/api/v1/projects`)

The legacy `/api/v1/groves` aliases have been removed. Requests to `/api/v1/groves` or any path under it now return `404 Not Found`; use `/api/v1/projects`.

- `GET /`: List projects you have access to.
- `POST /`: Create a project without a Runtime Broker. Body: `name` (required), optional `slug`, `gitRemote` and `labels`, and `workspaceMode`. Omit `gitRemote` for a Hub-managed project without git.
  - `workspaceMode` is the [workspace sharing mode](/scion/local/workspaces-and-sharing/#setting-the-mode-on-a-hub-project): `shared`, `per-agent` or `worktree-per-agent`. With a `gitRemote`, `per-agent` means clone-per-agent. Without one, it means empty-per-agent: each agent gets a private directory that starts empty. Omitted means the existing default (shared for a project without git).
  - The mode can only be set at create time. Unknown values, and `worktree-per-agent` without a `gitRemote`, are rejected with `400`. It is stored in the server-owned `scion.dev/workspace-mode` label. A `labels` entry for that key must match `workspaceMode` (else `400`), and is dropped when `workspaceMode` is omitted. A later update that tries to change the label is rejected with `400`.
  - For an empty-per-agent project, creating or starting an agent on a Runtime Broker that does not advertise the `emptyPerAgentWorkspace` capability fails with `412 Precondition Failed`. An agent create that names a `workspace` path is rejected with `400`, and workspace files sent with it are ignored with a warning in the response's `warnings`. Reincarnating such an agent is rejected with `400`. A Runtime Broker whose default runtime is Cloud Run or Substrate reports this capability as absent from its first heartbeat onward, so creating an empty-per-agent agent there returns `412`. Until that heartbeat, the agent can still be dispatched, and the Cloud Run or Substrate runtime rejects it with an error. The capability reflects only the broker's default runtime, so an agent that uses a Cloud Run or Substrate runtime profile on a broker whose default runtime is neither gets no `412` and fails at agent start. The `cloudrun-sandbox` runtime supports the mode.
- `POST /register`: Register or link a project repository. If the request resolves to an existing project, the caller needs update access to that project; without it, the request is rejected before anything changes. The same check applies to creating a project that resolves to an existing one and to linking a provider.
- `GET /:id`: Get project metadata and statistics.
- `PATCH /:id`: Update a project. The updatable fields are `name`, `slug`, `labels` and `defaultRuntimeBrokerId`; an omitted or empty field is left unchanged, and `labels` replaces the whole map. The owner changes only through `POST /:id/transfer-ownership`: an `ownerId` in a `PATCH` body is ignored. The response is the updated project.
- `GET /:id/secrets`: Manage environment secrets for the project.
- `GET /:id/agents`: List the project's agents. Query: `phase`, `label` (repeatable `key=value`), `runtimeBrokerId`, `includeDeleted=true`, `limit` (default and maximum 500), and `cursor`. The response carries `agents`, `totalCount`, `serverTime`, and, when more results exist, `nextCursor`. Cursors are bound to the project, the filter, and the caller that received them; pass them back unchanged with the same query. A cursor that fails this check is rejected with `400`.
  - **Sorted mode** (`sort=created` or `sort=updated`): returns a stable, server-sorted order by creation time or last update, newest first (`dir=desc`, the default) or oldest first (`dir=asc`). Any other `sort` value is rejected with `400`. Optional `fit` (1–500, at least `limit`, not valid with `cursor`) asks for the whole readable set in one response when the project has at most `fit` agents; the response then reports `complete: true`, or `complete: false` with the first page and a `nextCursor`. `stats=1` adds a `stats` block (`total`, `running`, and `agents` as `[id, phase]` pairs) computed over the filter with `phase` ignored. The response echoes `sort` and `dir`. Sorted mode serves at most 2,000 agents: a request whose filter matches more is refused with `422` and error code `sorted_view_unavailable` (`details.reason: too_many_candidates`), so retry without `sort`. The page size can be narrower than `limit` for large projects. An agent token can use sorted mode on its own project.
  - **Compact view** (`view=compact`): returns the same slimmer items as `GET /api/v1/agents?view=compact`, without `appliedConfig`. This endpoint does not set `_messageability`, in either view. Membership, order, cursors, counts, `complete` and `stats` are the same as in the default `view=full`. An empty `view` is the same as no `view`. In sorted mode, a non-empty `view` other than `full` or `compact` is rejected with `400`; otherwise any value other than `compact` is ignored.
- `GET /:id/providers`: List the Runtime Brokers that provide compute for the project. Each provider reports broker-wide capacity: `agentLimit` (the effective `max_agents_per_broker` limit; omitted when the broker is unlimited or no limit applies), `agentCount` (running agents on that broker from any project; omitted when no limit is configured), and `agentLimitSource` (which precedence step produced `agentLimit`: `broker`, `entitlement`, `hub_default`, `unlimited`, or `not_enforced`). When `agentLimitSource` is `not_enforced`, the hub-wide "enforce broker agent quotas" switch is off: `agentLimit` is informational only — it is still the resolved cap, but agent creates on that broker are not rejected against it. `scion hub projects info` shows these as `(agents: count/limit)`, with `(not enforced)` appended when the switch is off.
- `GET /:id/settings/resolved`: Get project settings indicating whether a Hub default exists per-setting (non-admin gated).
- `GET /:id/members`: List the project's role bindings. With `groupBy=principal`, returns one item per principal (`principalType`, `principalId`, `principalDisplayName`, `builtInRoleName`, and its `bindings`), and `limit`/`offset`/`totalCount` count principals rather than bindings. Each binding carries `roleKind` (`builtin` or `custom`).
- `PUT /:id/members/principals/:type/:principalId`: Set a principal's whole project role set in one transaction. `:type` is `user`, `agent`, or `group`; a user can be addressed by ID or email. Body: `roleDefinitionIds` (at most one built-in membership role plus any custom project roles), optional `expectedRoleDefinitionIds` (a precondition: `409 membership_changed` if the principal's current roles differ; an empty list means "not yet a member"), and optional `notBefore`/`expiresAt` for newly created bindings. Returns the principal's grouped membership with `changed`; `201` when the principal was not a member before. An empty set is refused with `400 empty_role_set` (use `DELETE`), and an unknown, non-project, or second built-in role with `400 invalid_role_set`. Requires `project.manage` and an interactive user identity (agent tokens and User Access Tokens get `403 credential_insufficient`). Guards: the last active direct user owner cannot be removed or demoted (`409 last_owner`); built-in role changes follow the owner/admin governance rules (`403 target_role_protected`); creating or removing a custom role requires a direct project owner (or a caller with no project role who holds hub `role_binding` authority) and the usual `CanDelegate` ceiling; and a custom role that itself carries a `role_binding.*` permission cannot be granted through this endpoint by anyone.
- `DELETE /:id/members/principals/:type/:principalId`: Remove every project binding the principal holds, under the same guards. `404` if the principal holds none.
- `GET /:id/members/assignable-roles`: List the project-scoped roles with `grantable` for the caller, and, when not grantable, the `reason`, `denialCode`, and `details` the `PUT` above would return. Requires `project.manage`.
- `POST /:id/agents/stop-all`: Stop the project's running agents. Authorized from role bindings: holders of `agent.stop_all` on the project (project owners and admins, and hub admins) stop every agent (`scope: all`); other members with a built-in project role, direct or group-derived, stop only the agents they own and may operate (`scope: own`); anyone else, including a caller holding only a custom project role, gets `403`. The hub-wide `POST /api/v1/agents/stop-all` requires `agent.stop_all` on the hub.
- `POST /:id/transfer-ownership`: Body `{"newOwnerId": "<user ID or email>"}`. Grants the new owner `project-owner`, downgrades the caller to member, and moves the project's `ownerId` to the new owner in one transaction.
- `POST /:id/clone`: Deep-copy settings, labels, env vars, skills, hooks, harness configs, and templates to a new project with rollback protection. Supports an optional `gitRemote` field in the request body to override the source project's git repository (carrying configurations over while using a different repository).

#### Runtime Brokers (`/api/v1/brokers`)
- `GET /`: List registered runtime brokers.
- `POST /`: Register a new compute node, or re-mint its join token. Requires `broker.create` (see [Broker Registration Permission](/scion/hosted/ha/runtime-broker/#broker-registration-permission)). The caller becomes the broker's owner. Re-registering an existing broker requires ownership (see [Broker Ownership](/scion/hosted/ha/runtime-broker/#broker-ownership)).
- `POST /join`: Complete the two-phase broker registration.
- `GET /:id`: Get broker status and capacity.

#### Broker Settings (`/api/v1/runtime-brokers/:id/settings`)

A general per-broker settings mechanism. `maxAgents`, a per-broker override of the `max_agents_per_broker` cap, is the first registered key.

- `GET /:id/settings`: Read the broker's stored settings document plus the effective (resolved) value for each key. Requires `broker.read`. `404` if the broker doesn't exist. If the broker has no settings row yet, `settings` is `{}` and `revision` is `0`.
- `PUT /:id/settings`: Replace the settings document. Body: `{"settings": {"maxAgents": 30}, "expectedRevision": 3}`. This is a full replace, not a merge: a key omitted from `settings` (or sent as `null`) is cleared back to "inherit". Each key's own permission gates writing it — `maxAgents` requires `quota.update` — checked only against keys whose value actually changes, so re-sending the current document needs no permission at all.

Response shape (both verbs):

```json
{
  "brokerId": "…",
  "settings": { "maxAgents": 30 },
  "effective": {
    "maxAgents": {
      "value": 30,
      "source": "broker",
      "count": 7,
      "inherited": { "value": 100, "source": "hub_default" }
    }
  },
  "revision": 3,
  "updatedBy": "admin@example.com",
  "updated": "2026-01-01T00:00:00Z",
  "_capabilities": { "update": true }
}
```

`effective.maxAgents.source` is one of `broker` (this broker's own setting), `entitlement` (an entitlement binding), `hub_default` (the limit definition's default value), or `unlimited` (no limit definition exists, or no quota service is configured); it is `null`/`""` only if resolution itself fails. `inherited` reports what the value and source would be if the broker's own setting were cleared, so the UI can always show what "use the default" means without having to clear it first to find out. `count` is the current active-reservation count against the same limit `Reserve` counts.

Status codes: `400` for an unknown key or an invalid value (`maxAgents` must be `>= 0`; `0` means unlimited); `403` if the caller lacks the permission a changed key requires; `404` if the broker doesn't exist; `409` on a stale `expectedRevision` (the response body carries the current record under `current`, same shape as a normal `GET`).

**Precedence for `max_agents_per_broker`** (most specific wins): a broker's own `maxAgents` setting, if set, is the effective limit — it can be lower than the hub-wide default *and* lower than any system-scoped entitlement binding. Otherwise, the existing entitlement-engine resolution applies: bindings (most generous wins), falling back to the limit definition's hub-wide default value. In both layers, `0` means unlimited.

**Migration from entitlement bindings.** Earlier releases had no per-broker settings API, so operators worked around it with a broker-scoped entitlement binding on `max_agents_per_broker` — either a `system_default` binding with a non-empty `subjectId`, or a user binding whose `subjectId` is set to the broker's own ID (the "user-subject hack" — see [`ptone/scion#2063`](https://github.com/ptone/scion/issues/2063)). On upgrade, a one-shot migration copies **every** broker-scoped binding on this limit into a `maxAgents` setting (`0` if any binding was `0`, otherwise the largest value), regardless of subject — including bindings the entitlement engine never actually enforced (a `system_default` row needed an *empty* subject to be picked up, so a non-empty-subject row was previously a silent no-op; a user binding scoped to the broker but owned by some other user was likewise never matched). Sweeping these up anyway restores what the operator evidently intended when they scoped a binding to that broker. **This means an upgrade can newly impose, or tighten, a broker's effective cap** for a broker that previously had no effective per-broker limit at all (or was really being capped by a more generous system-scoped binding, since the engine used to merge broker- and system-scoped bindings with "most generous wins" — the migrated setting no longer merges with anything). The migration is attributed as `updatedBy: "migration:ptone/scion#2061"`, and it only fills in brokers that don't already have a `maxAgents` setting.
It never deletes the old bindings — they are shadowed by the new setting per the precedence above, and their IDs are logged at migration time so an operator can find and remove them. **Removing them matters**: if the migrated `maxAgents` setting is later cleared ("use hub default"), the precedence rule falls through to the entitlement engine, and any leftover binding the entitlement engine matches (a user binding with `subjectId` equal to the broker ID, or a `system_default` binding with an empty subject — i.e. the shapes the migration actually inherited enforcement from) becomes live again. The never-enforced shapes described above stay inert either way. Clearing the setting does not by itself restore the hub-wide default if a matching binding is still there. Legacy broker-scoped bindings can still be deleted (`DELETE /entitlements/:id`) or read normally; they just can't be created fresh or edited while staying broker-scoped — see the 400 below.
Because of this, creating a **new** broker-scoped binding on `max_agents_per_broker`, or editing an existing one while keeping it broker-scoped (`POST` on `/limits/:id/entitlements`, or `PUT` on `/entitlements/:id` with `scopeType: "broker"`), now returns `400` with the message `per-broker agent caps are set via PUT /api/v1/runtime-brokers/{id}/settings`. System-scoped bindings for this limit are unaffected and continue to work as the hub-wide override.

#### Chat Attachments (`/api/v1/chat/attachments`)
- `POST /`: Upload one or more files (`multipart/form-data`, field `files`, optional `project_id`). Max 10 files, 10 MB each. Text files containing unusual control characters (e.g., vertical tab `0x0B`) are supported and correctly identified as text.
- `GET /:id`: Download a stored attachment. Responses carry `X-Content-Type-Options: nosniff`, and `Content-Disposition: inline` only for image types — everything else is served as an `attachment`.

Uploads are accepted or refused **per file**, and the response reports both outcomes:

```json
{
  "attachments": [{ "id": "…", "name": "compose.yaml", "mime": "text/plain", "size": 34, "url": "/api/v1/chat/attachments/…" }],
  "failures": [
    { "name": "setup.sh", "error": "dangerous file extension: .sh" },
    { "name": "notes.html", "error": "files with a .html extension are not accepted" }
  ]
}
```

`size` is the stored file's length in bytes — the example assumes a 34-byte `compose.yaml` — while `id` and `url` are elided here because both are assigned per upload.

Status codes:
- `201 Created` — **at least one** file was stored. `failures` may be non-empty. Previously a single bad file failed the whole batch with `400` and stored nothing; clients that treat `201` as "all files stored" must now read `failures`, or they will drop files silently.
- `400 Bad Request` — nothing was stored and the caller can fix it (blocked extension, unaccepted type, oversized file).
- `500 Internal Server Error` — nothing was stored and the failure was server-side.

The stored MIME type is derived from the file's content plus its extension; the `Content-Type` a client declares on the part is ignored. Executable extensions (`.exe`, `.sh`, `.js`, `.ps1`, and their peers) and markup extensions (`.html`, `.svg`, and their peers) are refused whatever the content is.

#### Experiments (`/api/v1/experiments`)
- `GET /`: Return the resolved hub-wide experiment map, `{"experiments": {"<name>": true|false}}`. Available to any authenticated identity (user, agent, or Runtime Broker token). The web client fetches it at boot. Admins change values through [`/api/v1/admin/experiments`](#admin-apiv1admin).

#### Templates (`/api/v1/templates`)
- `GET /`: List available agent templates. The authorized list validator caps list requests at a maximum limit of **100** templates per page (default is 50). Requests specifying a `limit` query parameter greater than 100 will fail with HTTP 400 Bad Request.
- `POST /`: Upload a new template or version.

#### Auth (`/api/v1/auth`)
- `GET /scopes`: Dynamically discover all available User Access Token (UAT) scopes and their descriptions. With `projectId`, each scope also reports `eligible` and, when not eligible, `eligibilityReason`, for the caller in that project. A project the caller cannot access and a nonexistent project are denied identically. Minting a token (`POST /api/v1/auth/tokens`) re-checks every requested scope against the caller's live authority and refuses an ineligible one with `403 scope_violation`.

#### Users (`/api/v1/users`)
- `GET /`: List users (admin only).
- `GET /:id`: Get user details and capabilities.
- `PATCH /:id`: Update user attributes. Accepted fields: `displayName`, `role`, `status`, and `preferences`; any other field is rejected with `400`. `preferences` is merged per key onto the stored preferences rather than replacing them: a key that is omitted is left unchanged, an empty string clears it, and unknown keys are ignored. Preference keys are `defaultTemplate`, `defaultProfile`, `theme`, and `timezone`. `timezone` is the user's display timezone, an IANA zone name such as `Europe/Berlin`; empty means Auto (the browser's zone). An invalid zone returns `400`. `/auth/me` also returns the caller's preferences.
- `GET /me/terminal-workspace`, `PUT /me/terminal-workspace`: Read or save the caller's open terminal list for the web Terminal Workspace. The document is `{"agentIds": [...], "frontmostAgentId": "<uuid>|null"}`, with up to 32 canonical agent UUIDs; the response adds `revision`, `updatedAt`, and `pruned`. `PUT` replaces the saved document (last writer wins). `GET` drops agents that no longer exist or that the caller can no longer attach to, and reports how many were dropped in `pruned`. Requires an interactive web session or a dev credential; there is no admin access to another user's list.
- `DELETE /:id`: Delete a user. Returns `409 last_owner` (with `details.projects`) if the user is the only active owner of any project, and `409 conflict` if a concurrent grant or role change to the user's role bindings commits before the delete's cascade (retry; a concurrent revoke does not abort the delete); on success all of the user's role bindings are removed too (see [Deleting users](/scion/hosted/single-node/auth/#deleting-users)).
- `POST /:id/revoke-sessions`: Revoke all active sessions for a user (admin only). Increments the user's session generation counter, causing every existing cookie-based session to be invalidated on the next request. The affected user is forced to re-authenticate.

#### Admin (`/api/v1/admin`)
- `GET /roles`: List Role Definitions.
- `POST /roles`, `PUT /roles/:id`, `DELETE /roles/:id`: Manage Role Definitions (requires appropriate administrative capabilities). Note that `updateRoleDefinition` includes a `CanDelegate` check to prevent privilege escalation.
- `GET /role-bindings`: List Role Bindings (paginated).
- `POST /role-bindings`, `PUT /role-bindings/:id`, `DELETE /role-bindings/:id`: Manage Role Bindings.
- `GET /limits`: List Limit Definitions.
- `GET /limits/:id`, `PUT /limits/:id`: Inspect or update a Limit Definition.
- `GET /entitlements/:id`: Inspect an Entitlement Binding.
- `GET /gcp-quota`: View GCP quota status.
- `GET /experiments`, `PUT /experiments`, `DELETE /experiments`: Manage hub-wide experiment overrides (requires `hub.experiments.update`). `GET` lists every registered experiment with its `default`, stored `override`, and resolved `enabled` value, plus the section `revision`. `PUT` body: `{"overrides": {"<name>": true|false|null}, "expected_revision": N}`. `null` removes an override, omitted names are unchanged, and unregistered names return `400`. A stale revision returns `409` (`revision_conflict`). `DELETE` resets all overrides to registry defaults (body: `expected_revision`, or `confirm_reset_malformed: true` if the stored section is malformed). There is no admin UI yet.
- `GET /messaging/divergence`: View a read-only snapshot of migration divergence counters and metadata for the conversation model transition (requires `hub.diagnostics.read` permission).

The Hub seeds a `max_agents_per_broker` limit (default **100**) that caps how many agents can be running on a single runtime broker. It is checked before an agent is created, and again when an agent is started, resumed, or restarted. Only running agents count: stop, suspend, and exit release an agent's slot. The Hub reconciles stale reservations at startup and hourly. This default is a single hub-wide value shared by every broker on the hub; to override it for one broker, set that broker's `maxAgents` setting instead — see [Broker Settings](#broker-settings-apiv1runtime-brokersidsettings) above.

To change the hub-wide value, `PUT /limits/:id` on the `max_agents_per_broker` limit definition. `PUT` replaces the whole definition, so send the current `name`, `resourceType`, `unit`, and `description` (for example, from `GET /limits/:id`) along with the new `defaultValue` — omitting `description` clears it. For this system-seeded limit, `name`, `resourceType`, and `unit` must be sent unchanged; changing any of them returns `403`. This is also the recommended step after deploying a single-node Cloud Run hub — see the Cloud Run operator docs for the recommended value for that tier.

The Quota System API enforces fail-closed limits. Route guards strictly separate read and write permissions, preventing arbitrary modification of system limits.

## Runtime Broker API

The Runtime Broker exposes a local API (usually on port 9800) for agent execution and management.

### Control Channel (WebSocket)
Brokers maintain a persistent outbound WebSocket connection to the Hub. The Hub uses this tunnel to send commands (e.g., `CreateAgent`) to brokers that might be behind NAT.

### Local Endpoints
- `GET /healthz`: Basic liveness and readiness check. In multi-node or hosted setups, if a reverse proxy (like GFE) intercepts this endpoint and returns a non-JSON body, the client detects this and returns a precise error naming the likely cause (rather than a generic JSON-decoding failure) to assist with troubleshooting.
- `POST /api/v1/agents`: (Internal) The Hub dispatches agents to this endpoint.
- `GET /api/v1/agents/:id/attach`: (WebSocket) Provides a terminal stream for interactive sessions.

### PTY close codes

The WebSocket close frame that ends a terminal attach carries a code that tells the client why the attach ended. The hop that knows the cause picks the code, and every later hop passes it through unchanged. The Runtime Broker classifies the cause the same way on every runtime. The close frame also carries a machine-readable `snake_case` reason that names the specific cause. A code can carry several reasons, so clients should decide whether to retry from the code and treat the reason as diagnostic detail.

| Code | Meaning | Reasons | Client should |
| :--- | :--- | :--- | :--- |
| `1000` | Clean detach. The tmux session still exists. | None (empty reason) | Not retry |
| `4404` | The Runtime Broker cannot find the agent or its container. | `agent_not_found` | Not retry |
| `4410` | The tmux session is gone (agent exited, container stopped or removed). | `session_ended` (the container still exists), `container_removed` (the container is gone too), `agent_stopped` (the attach never reached a tmux session because the container is definitively stopped) | Not retry |
| `4503` | A hop behind this one is temporarily unavailable (Hub-to-broker control channel dropped, stream failed to open, exec transport dropped while the session is still alive, tmux session not ready yet, container runtime lookup failed). | From the Hub: `broker_disconnected`, `stream_open_failed`, `broker_write_failed`. From the Runtime Broker: `runtime_stream_dropped`, `session_not_ready`, `lookup_unavailable`, `runtime_unavailable` | Retry |
| `1006` | Connection dropped without a close frame. The client library generates this code; it is never sent. | None | Retry |
| `1011` | Unexpected server error, or the Runtime Broker could not check whether the tmux session survived. | `internal_error`, `client_read_failed`, `client_write_failed`, `probe_failed` | Retry |

`4401`, `4403`, and `4504` are reserved. Authentication and permission failures currently surface as HTTP `401`/`403` on the handshake.

## System Health Endpoints (Hub)
- `GET /healthz`: Basic liveness check. If a reverse proxy intercepts this with a non-JSON response, the client gracefully falls back to `/health`. Always returns HTTP `200`; the response body's top-level `status` field reports `healthy` or `degraded`, with the failing check(s) named under `checks`. When this process runs a co-located (embedded) runtime broker, the co-located broker check reports `healthy`, `unhealthy: registration failed` (the fixed public value — see the server log for the underlying error), or `unhealthy: registration pending`; `status` goes `degraded` while it is anything but healthy. A failed registration is not retried, so this needs a broker configuration fix and a server restart, not a wait. **Path differs by deployment shape**: on a standalone Hub (no web server, e.g. port `9810`), it is `checks.colocated_broker` at the top level. On the combined single-node workstation setup (the default — the web server answers `/healthz` on its own port and nests the Hub's health under `hub`, per `CompositeHealthResponse`), it is `hub.checks.colocated_broker`.
- `GET /readyz`: Readiness check verifying database connectivity and, when a non-`local` workspace storage backend is configured, that its mount is available. Kubernetes and Cloud Run readiness probes must target this endpoint rather than `/healthz`, which always returns `200`. `/readyz` is intentionally unaffected by the co-located broker check above.
- `GET /health`: Legacy/alternative liveness check endpoint.

## Communication Patterns

### State Reporting
Agents use the `sciontool` utility to report their state back to the Hub via the `POST /api/v1/agents/:id/status` endpoint. State updates include the agent's current phase, activity, and contextual detail (e.g., which tool is executing). This happens at high frequency during task execution.

### Log Streaming
Logs are collected by the Runtime Broker and can be streamed in two ways:
1. **Real-time**: Streamed via WebSocket from the Broker to the Hub, then to the Dashboard/CLI.
2. **Persistent**: Batched and uploaded to a storage backend (like GCS) after agent completion.