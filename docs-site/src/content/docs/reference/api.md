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

### Core Resources

#### Agents (`/api/v1/agents`)
- `GET /`: List agents (filterable by project, user, phase).
- `POST /`: Dispatch a new agent.
- `GET /:id`: Get detailed agent state (phase, activity, detail).
- `POST /:id/suspend`: Suspend a running agent, preserving its harness session for a later resume. Sets the phase to `suspended`. Requires a harness that supports session resume.
- `POST /:id/start`, `POST /:id/restart`: Start/restart an agent. Starting a `suspended` agent resumes (continues) its harness session; starting a `stopped` or `error` agent runs a fresh session.
- `DELETE /:id`: Stop and remove an agent.
- `GET /:id/logs`: Stream agent logs (WebSocket).

There is no separate resume endpoint: resuming is the **start** action applied to a `suspended` agent. A `suspended` agent is also resumed automatically when a message is delivered to it with the `wake` option set.

Agent state uses a layered model:
- **Phase**: Lifecycle stage (`created`, `provisioning`, `cloning`, `starting`, `running`, `stopping`, `stopped`), plus `suspended` (paused for resume) and `error` (the agent crashed — restartable).
- **Activity**: Runtime activity within the `running` phase (`working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `stalled`, `offline`). Note: `offline` occurs when an agent heartbeat has not been heard for some time, often due to an expired auth token that the agent failed to refresh; `stalled` flags a live-but-hung agent and can trigger auto-suspend. (A crash surfaces as the `error` phase, not as an activity.)
- **Detail**: Freeform context (tool name, message, task summary).

#### Projects (`/api/v1/projects`)
- `GET /`: List projects you have access to.
- `POST /register`: Register or link a project repository.
- `GET /:id`: Get project metadata and statistics.
- `GET /:id/secrets`: Manage environment secrets for the project.
- `GET /:id/settings/resolved`: Get project settings indicating whether a Hub default exists per-setting (non-admin gated).
- `POST /:id/clone`: Deep-copy settings, labels, env vars, skills, hooks, harness configs, and templates to a new project with rollback protection. Supports an optional `gitRemote` field in the request body to override the source project's git repository (carrying configurations over while using a different repository).

#### Runtime Brokers (`/api/v1/brokers`)
- `GET /`: List registered runtime brokers.
- `POST /register`: Register a new compute node.
- `POST /join`: Complete the two-phase broker registration.
- `GET /:id`: Get broker status and capacity.

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
- `GET /`: List available agent templates.
- `POST /`: Upload a new template or version.

#### Auth (`/api/v1/auth`)
- `GET /scopes`: Dynamically discover all available User Access Token (UAT) scopes and their descriptions.

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

The Quota System API enforces fail-closed limits. Route guards strictly separate read and write permissions, preventing arbitrary modification of system limits.

## Runtime Broker API

The Runtime Broker exposes a local API (usually on port 9800) for agent execution and management.

### Control Channel (WebSocket)
Brokers maintain a persistent outbound WebSocket connection to the Hub. The Hub uses this tunnel to send commands (e.g., `CreateAgent`) to brokers that might be behind NAT.

### Local Endpoints
- `GET /healthz`: Basic liveness and readiness check. In multi-node or hosted setups, if a reverse proxy (like GFE) intercepts this endpoint and returns a non-JSON body, the client detects this and returns a precise error naming the likely cause (rather than a generic JSON-decoding failure) to assist with troubleshooting.
- `POST /api/v1/agents`: (Internal) The Hub dispatches agents to this endpoint.
- `GET /api/v1/agents/:id/attach`: (WebSocket) Provides a terminal stream for interactive sessions.

## System Health Endpoints (Hub)
- `GET /healthz`: Basic liveness check. If a reverse proxy intercepts this with a non-JSON response, the client gracefully falls back to `/health`.
- `GET /readyz`: Readiness check verifying database connectivity and, when a non-`local` workspace storage backend is configured, that its mount is available. Kubernetes and Cloud Run readiness probes must target this endpoint rather than `/healthz`, which always returns `200`.
- `GET /health`: Legacy/alternative liveness check endpoint.

## Communication Patterns

### State Reporting
Agents use the `sciontool` utility to report their state back to the Hub via the `POST /api/v1/agents/:id/status` endpoint. State updates include the agent's current phase, activity, and contextual detail (e.g., which tool is executing). This happens at high frequency during task execution.

### Log Streaming
Logs are collected by the Runtime Broker and can be streamed in two ways:
1. **Real-time**: Streamed via WebSocket from the Broker to the Hub, then to the Dashboard/CLI.
2. **Persistent**: Batched and uploaded to a storage backend (like GCS) after agent completion.