# Lifecycle Hooks — Admin Guide

**Status**: Shipped (M1–M6 complete; HA de-duplication is implemented)

## Overview

Lifecycle hooks are Hub-side, admin-authored automation rules that fire an HTTP
or webhook action when an agent crosses an **authoritative phase transition**.
They run asynchronously after the transition is committed — hook execution never
blocks or fails the transition itself.

Typical use cases:

- **Register / deregister** agents with an internal service registry (Consul,
  internal catalog) on start and stop.
- **Notify** an external system (Slack, PagerDuty, custom dashboard) when an
  agent enters an error state.
- **Trigger** downstream workflows (CI pipelines, cleanup jobs) on agent
  lifecycle events.

## Triggers

A hook fires on exactly one of these authoritative phase transitions:

| Trigger       | Fires when                                      |
|---------------|--------------------------------------------------|
| `running`     | Agent transitions to the running phase            |
| `suspended`   | Agent transitions to the suspended phase          |
| `stopped`     | Agent transitions to the stopped phase            |
| `error`       | Agent transitions to the error phase              |

Only *transitions* fire hooks. Repeated publications of the same phase (e.g.
heartbeats) are de-duplicated and do not re-fire.

## Admin CRUD API

All endpoints are under `/api/v1/admin/lifecycle-hooks` and require the
**hub-admin** role (`Authorization: Bearer <admin-token>`).

### Create a hook

```
POST /api/v1/admin/lifecycle-hooks
Content-Type: application/json

{
  "name": "register-agent",
  "scopeType": "hub",
  "trigger": "running",
  "action": {
    "type": "http",
    "method": "POST",
    "url": "https://registry.internal/v1/agents/${AGENT_ID}",
    "headers": { "Content-Type": "application/json" },
    "body": "{\"agent\":\"${AGENT_ID}\",\"project\":\"${PROJECT_ID}\"}",
    "onError": "retry",
    "timeoutSeconds": 10,
    "allowedUntrustedVars": []
  },
  "executionIdentity": "<managed-sa-record-id>",
  "enabled": true
}
```

Returns `201 Created` with the full hook object including `id` and
`stateVersion`.

### List hooks

```
GET /api/v1/admin/lifecycle-hooks
GET /api/v1/admin/lifecycle-hooks?trigger=running
GET /api/v1/admin/lifecycle-hooks?enabled=true
```

Returns `200 OK` with `{ "items": [...], "totalCount": N }`.

### Get a hook

```
GET /api/v1/admin/lifecycle-hooks/{id}
```

Returns `200 OK` with the hook object, or `404 Not Found`.

### Update a hook

```
PUT /api/v1/admin/lifecycle-hooks/{id}
Content-Type: application/json

{
  "name": "register-agent-v2",
  "trigger": "running",
  "action": { ... },
  "executionIdentity": "<managed-sa-record-id>",
  "enabled": true,
  "stateVersion": 1
}
```

Uses **optimistic locking**: the `stateVersion` field must match the current
version in the database. Returns `409 Conflict` on mismatch. The `scopeType` is
immutable after creation.

### Delete a hook

```
DELETE /api/v1/admin/lifecycle-hooks/{id}
```

Returns `204 No Content`, or `404 Not Found`.

## Action Types

### `http` — Authenticated service call

The `http` action type makes an authenticated HTTP request using a managed GCP
service account for bearer token injection. It is designed for calling internal
or GCP-hosted services.

**Requirements:**

- The URL **must** use HTTPS.
- An `executionIdentity` **must** be specified — this is the record ID (UUID)
  of a managed GCP service account that has been verified and is in-scope for
  the hook.
- The executor resolves the SA record to an email, impersonates it to obtain a
  bearer token, and injects the token as `Authorization: Bearer <token>`.
- Auth headers are injected *after* template rendering — they **never** come
  from hook variables.

### `webhook` — Unauthenticated POST

The `webhook` action type sends an unauthenticated HTTP request. The webhook
URL is expected to carry its own authentication (e.g. a token in the path or
query string).

**Constraints:**

- No `Authorization` header is attached.
- No `executionIdentity` is allowed — webhooks run without impersonation.
- Auth headers **must not** be set in the action's `headers` map.

## Execution Identity

The `executionIdentity` field references the **record ID** (UUID) of a managed
GCP service account (`/api/v1/admin/gcp-service-accounts/{id}`). The SA must
be:

1. **Verified** — its `verified` status is `true` (impersonation was
   successfully tested).
2. **In-scope** — the SA's scope includes the resources the hook will access.

At execution time, the executor resolves the record ID to the SA email, then
uses GCP IAM impersonation to generate a short-lived access token. This token
is attached as a bearer token to `http`-type requests only.

## Variable Substitution and Trust Model

Hook actions use `${VAR_NAME}` syntax for variable substitution. Variables are
classified into two trust classes:

### Trusted variables (hub-controlled)

These values come from authoritative hub data and are substituted verbatim.
They may appear in the URL, headers, and body.

| Variable         | Source                          |
|------------------|---------------------------------|
| `HOOK_ID`        | Hook record ID                  |
| `HOOK_NAME`      | Hook name                       |
| `TRIGGER`        | Trigger that fired (`running`, etc.) |
| `PROJECT_ID`     | Agent's project ID              |
| `PROJECT_NAME`   | Agent's project name            |
| `AGENT_ID`       | Agent record ID                 |
| `AGENT_SLUG`     | Agent slug (hub-controlled)     |
| `SA_EMAIL`       | Resolved SA email               |

### Untrusted variables (agent/LLM-derived)

These values originate from agent-controlled data (potentially LLM-generated)
and are subject to strict encoding rules.

| Variable         | Source                          |
|------------------|---------------------------------|
| `AGENT_NAME`     | Agent display name              |
| `TASK_SUMMARY`   | Agent task summary              |
| `AGENT_STATUS`   | Agent phase string              |
| `ERROR_MSG`      | Agent error message             |

**Security rules for untrusted variables:**

1. Untrusted variables are **never** allowed in the URL host, path, or query
   parameters (prevents URL injection).
2. Untrusted variables are **never** allowed in headers (prevents header
   injection).
3. Untrusted variables are allowed **only in the body**, and only if
   explicitly listed in `action.allowedUntrustedVars`.
4. When substituted in the body, untrusted values are **JSON-escaped**
   (quotes, backslashes, control characters are escaped) to prevent JSON
   injection.
5. The admin must consciously opt-in each untrusted variable — this prevents
   an agent-controlled value from being substituted under the service
   account's authority.

## Error Handling

### `onError` policy

| Value     | Behavior                                                    |
|-----------|-------------------------------------------------------------|
| `log`     | (Default) Single attempt. Failure is logged; no retry.       |
| `retry`   | Up to 3 attempts with exponential backoff (500ms, 1s, 2s).  |

- **4xx responses are non-retryable** — they indicate a client error and are
  never retried, even with `onError: retry`.
- **5xx responses and network errors** are retryable.
- After all attempts are exhausted, the error is logged. Hook failures never
  propagate to the agent transition.

### Timeout

Each action has a per-attempt `timeoutSeconds` (max 30 seconds, default 10).
The timeout applies independently to each retry attempt.

## SSRF Protection

The executor enforces multiple layers of SSRF (Server-Side Request Forgery)
protection:

- **IP blocking**: Connections to loopback (`127.0.0.0/8`, `::1`) and
  link-local (`169.254.0.0/16`, `fe80::/10`) addresses are blocked at the
  dialer level. The dialer resolves the hostname, selects the first
  non-blocked IP, and dials that specific IP — closing the DNS-rebinding
  TOCTOU window.
- **RFC1918 allowed**: Private addresses (`10/8`, `172.16/12`, `192.168/16`)
  are intentionally allowed for internal service registries.
- **Redirect blocking**: All HTTP redirects are blocked to prevent SSRF via
  redirect chains.

## Audit Behavior

Every hook execution attempt generates an audit event with the following
metadata:

- Hook ID, hook name, trigger, agent ID
- Execution identity (SA email or record ID)
- Action type (`http` or `webhook`)
- HTTP method
- **Host only** (never the full URL, which may contain path-based tokens)
- Success/failure, HTTP status code, failure reason
- Latency (milliseconds)
- Attempt number

**Security invariants for audit:**

- **Response bodies** are never recorded.
- **Authorization header values** (bearer tokens) are never recorded.
- **Full URLs** are never recorded (only the host portion).

## Selector

A hook's `selector` controls which agents it applies to. If the selector is
`null` or empty, the hook matches **all agents**.

| Selector field | Matches against      |
|----------------|----------------------|
| `projectId`    | Agent's project ID   |
| `template`     | Agent's template     |

When both fields are set, both must match (AND logic).

## HA De-Duplication

Cross-instance HA de-duplication is **implemented**. The evaluator
automatically selects the appropriate deduplication strategy based on the
configured database backend:

- **Postgres (production/HA)**: The evaluator detects the
  `PostgresEventPublisher` broadcast type and auto-selects a **durable
  store-backed CAS (compare-and-set) deduper**. This ensures exactly-once hook
  firing across multiple Hub instances. Each instance receives every agent
  status event via Postgres `NOTIFY`, but only the instance that wins the
  atomic CAS on the `lifecycle_hook_agent_phase` table fires the hook. The CAS
  uses `SELECT … FOR UPDATE` row locking to serialise concurrent attempts.

- **SQLite (single-instance/dev)**: An **in-memory deduper** is used. Since
  SQLite deployments are single-instance, there is no cross-instance
  contention. The in-memory map is seeded from the store on evaluator startup
  to survive evaluator restarts within the same process.

Terminal phases (`stopped`, `error`) automatically prune their deduper entries
to prevent unbounded growth.

## Example: Register / Deregister Flow

A common pattern is to register an agent with a service registry when it
starts and deregister it when it stops.

**Register hook** (fires on `running`):

```json
{
  "name": "register-agent",
  "scopeType": "hub",
  "trigger": "running",
  "action": {
    "type": "http",
    "method": "POST",
    "url": "https://registry.internal/v1/agents/${AGENT_ID}",
    "headers": { "Content-Type": "application/json" },
    "body": "{\"agentId\":\"${AGENT_ID}\",\"projectId\":\"${PROJECT_ID}\",\"slug\":\"${AGENT_SLUG}\"}",
    "onError": "retry",
    "timeoutSeconds": 10
  },
  "executionIdentity": "<sa-record-id>",
  "enabled": true
}
```

**Deregister hook** (fires on `stopped`):

```json
{
  "name": "deregister-agent",
  "scopeType": "hub",
  "trigger": "stopped",
  "action": {
    "type": "http",
    "method": "DELETE",
    "url": "https://registry.internal/v1/agents/${AGENT_ID}",
    "headers": { "Content-Type": "application/json" },
    "onError": "retry",
    "timeoutSeconds": 10
  },
  "executionIdentity": "<sa-record-id>",
  "enabled": true
}
```

You may also add deregister hooks for the `suspended` and `error` triggers to
ensure agents are removed from the registry in all terminal/inactive states.
