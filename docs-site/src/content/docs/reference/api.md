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
- `GET /`: List agents (filterable by project, user, phase).
- `POST /`: Dispatch a new agent.
- `GET /:id`: Get detailed agent state (phase, activity, detail). An agent can always read its own record with its agent token.
- `POST /:id/suspend`: Suspend a running agent, preserving its harness session for a later resume. Sets the phase to `suspended`. Requires a harness that supports session resume.
- `POST /:id/start`, `POST /:id/restart`: Start/restart an agent. Starting a `suspended` agent resumes (continues) its harness session; starting a `stopped` or `error` agent runs a fresh session. To continue the interrupted session of an `error` agent instead, send `{"forceResume": true}` as the `start` body (best effort). `forceResume` has no effect in other phases.
- `POST /:id/reincarnate`: Migrate the agent to a new generation with the same ID and slug and a freshly resolved config (see [`scion reincarnate`](/scion/reference/cli/#scion-reincarnate)). Body: `handoff` (optional text for the new generation's first task, max 256 KiB) and `dryRun`. Returns `202 Accepted` with the pending plan, or `200 OK` with the plan only for a dry run. The migration runs in the background. Requires `agent.lifecycle`; returns `400` for agents in worktree-per-agent projects. Also available as `POST /api/v1/projects/:projectId/agents/:agentIdOrSlug/reincarnate`.
- `POST /:id/message`: Deliver a message to an agent. Body: `message` (plain text) or `structured_message`, plus optional flags `raw` (send the text as raw keystrokes with no envelope and no trailing Enter), `plain` (deliver the text without the Scion message envelope), `interrupt`, `notify`, and `wake`. Top-level `raw` and `plain` apply with either body form; with `structured_message`, they are merged onto it.
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

#### Projects (`/api/v1/projects`)

The legacy `/api/v1/groves` aliases have been removed. Requests to `/api/v1/groves` or any path under it now return `404 Not Found`; use `/api/v1/projects`.

- `GET /`: List projects you have access to.
- `POST /register`: Register or link a project repository. If the request resolves to an existing project, the caller needs update access to that project; without it, the request is rejected before anything changes. The same check applies to creating a project that resolves to an existing one and to linking a provider.
- `GET /:id`: Get project metadata and statistics.
- `GET /:id/secrets`: Manage environment secrets for the project.
- `GET /:id/providers`: List the Runtime Brokers that provide compute for the project. Each provider reports broker-wide capacity: `agentLimit` (the effective `max_agents_per_broker` limit; omitted when the broker is unlimited or no limit applies), `agentCount` (running agents on that broker from any project; omitted when no limit is configured), and `agentLimitSource` (which precedence step produced `agentLimit`: `broker`, `entitlement`, `hub_default`, `unlimited`, or `not_enforced`). When `agentLimitSource` is `not_enforced`, the hub-wide "enforce broker agent quotas" switch is off: `agentLimit` is informational only — it is still the resolved cap, but agent creates on that broker are not rejected against it. `scion hub projects info` shows these as `(agents: count/limit)`, with `(not enforced)` appended when the switch is off.
- `GET /:id/settings/resolved`: Get project settings indicating whether a Hub default exists per-setting (non-admin gated).
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

#### Templates (`/api/v1/templates`)
- `GET /`: List available agent templates. The authorized list validator caps list requests at a maximum limit of **100** templates per page (default is 50). Requests specifying a `limit` query parameter greater than 100 will fail with HTTP 400 Bad Request.
- `POST /`: Upload a new template or version.

#### Auth (`/api/v1/auth`)
- `GET /scopes`: Dynamically discover all available User Access Token (UAT) scopes and their descriptions.

#### Users (`/api/v1/users`)
- `GET /`: List users (admin only).
- `GET /:id`: Get user details and capabilities.
- `PATCH /:id`: Update user attributes (e.g., role).
- `DELETE /:id`: Delete a user.
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