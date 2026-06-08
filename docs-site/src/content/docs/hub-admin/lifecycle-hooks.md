---
title: Lifecycle Hooks
description: Hub-side, admin-authored automation that fires HTTP or webhook actions on agent phase transitions.
---

Lifecycle hooks are Hub-side, admin-authored automation rules that fire an HTTP
or webhook action when an agent crosses an **authoritative phase transition**.
They are stored in the Hub database, managed entirely through the admin API, and
run **outside the agent container** — there is no in-container scripting and no
code is executed inside the agent's runtime.

Hooks run asynchronously *after* a phase transition has been committed. Hook
execution never blocks, delays, or fails the transition itself: if a hook errors
or times out, the agent's lifecycle proceeds unaffected.

## When to use lifecycle hooks

Lifecycle hooks implement an admission- and policy-webhook pattern at the Hub
level. Reach for them when an external system needs to react to agent lifecycle
events:

- **Register / deregister** agents with an internal service registry (Consul, an
  internal catalog) when they start and stop.
- **Notify** an external system (Slack, PagerDuty, a custom dashboard) when an
  agent enters an error state.
- **Trigger** downstream workflows (CI pipelines, cleanup jobs) on agent
  lifecycle events.

The motivating case is registry integration: register an agent on `running`, and
deregister it on `stopped`, `suspended`, or `error`.

## Triggers

A hook fires on exactly one of these authoritative phase transitions:

| Trigger     | Fires when                                |
|-------------|-------------------------------------------|
| `running`   | Agent transitions to the running phase    |
| `suspended` | Agent transitions to the suspended phase  |
| `stopped`   | Agent transitions to the stopped phase    |
| `error`     | Agent transitions to the error phase      |

Only *transitions* fire hooks. Repeated publications of the same phase (for
example, heartbeats) are de-duplicated and do not re-fire.

## Admin CRUD API

All endpoints live under `/api/v1/admin/lifecycle-hooks` and require the
**hub-admin** role (`Authorization: Bearer <admin-token>`).

### Create a hook

```http
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

Returns `201 Created` with the full hook object, including its `id` and
`stateVersion`.

The `scopeType` field is `hub` in v1 and defaults to `hub` when omitted. A
`project` scope is **reserved for a future release** and is not usable yet — to
narrow which agents a hook applies to, use the [selector](#selector) instead. See
[Scope vs. selector](#scope-vs-selector).

### List hooks

```http
GET /api/v1/admin/lifecycle-hooks
GET /api/v1/admin/lifecycle-hooks?trigger=running
GET /api/v1/admin/lifecycle-hooks?enabled=true
```

Returns `200 OK` with `{ "items": [...], "totalCount": N }`.

### Get a hook

```http
GET /api/v1/admin/lifecycle-hooks/{id}
```

Returns `200 OK` with the hook object, or `404 Not Found`.

### Update a hook

```http
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

Updates use **optimistic locking**: the `stateVersion` field must match the
current version in the database, or the request returns `409 Conflict`. The
`scopeType` field is immutable after creation.

### Delete a hook

```http
DELETE /api/v1/admin/lifecycle-hooks/{id}
```

Returns `204 No Content`, or `404 Not Found`.

## Action types

### `http` — authenticated service call

The `http` action makes an authenticated HTTP request using a managed GCP
service account for bearer-token injection. It is designed for calling internal
or GCP-hosted services.

- The URL **must** use HTTPS.
- An `executionIdentity` **must** be specified — the record ID (UUID) of a
  managed GCP service account that has been verified and is in scope for the
  hook.
- The executor resolves the SA record to an email, impersonates it to obtain a
  short-lived bearer token, and injects the token as `Authorization: Bearer
  <token>`.
- Auth headers are injected *after* template rendering — they **never** come from
  hook variables.

### `webhook` — unauthenticated POST

The `webhook` action sends an unauthenticated HTTP request. The webhook URL is
expected to carry its own authentication (for example, a token in the path or
query string).

- No `Authorization` header is attached.
- No `executionIdentity` is allowed — webhooks run without impersonation.
- Auth headers **must not** be set in the action's `headers` map.

## Execution identity

The `executionIdentity` field references the **record ID** (UUID) of a managed
GCP service account (`/api/v1/admin/gcp-service-accounts/{id}`). The service
account must be:

1. **Verified** — its `verified` status is `true` (impersonation has been
   successfully tested).
2. **In scope** — the SA's scope includes the resources the hook will access.

At execution time, the executor resolves the record ID to the SA email, then uses
GCP IAM impersonation to generate a short-lived access token. This token is
attached as a bearer token to `http`-type requests only.

## Scope vs. selector

Two fields determine where a hook applies, and they are not interchangeable:

- **`scopeType`** is the hook's ownership scope. In v1 the only supported value is
  `hub` (a hub-wide hook), and it defaults to `hub` when omitted. A `project`
  scope is **reserved for a future release**: the create/update API will validate
  `scopeType: "project"` against the schema (and require a `scopeId`), but
  project-scoped selection is **not wired as a v1 capability**. Keep all hooks
  hub-scoped.
- **`selector`** is the active v1 mechanism for targeting a subset of agents.
  Use it — not scope — when a hook should apply only to certain projects or
  templates.

## Selector

A hook's `selector` controls which agents it applies to. If the selector is
`null` or empty, the hook matches **all agents**.

| Selector field | Matches against    |
|----------------|--------------------|
| `projectId`    | Agent's project ID |
| `template`     | Agent's template   |

When both fields are set, both must match (AND logic).

## Variable substitution and trust model

Hook actions use `${VAR_NAME}` syntax for variable substitution. Variables fall
into two trust classes, and the distinction is the core of the security model.

### Trusted variables (hub-controlled)

These values come from authoritative Hub data and are substituted verbatim. They
may appear in the URL, headers, and body.

| Variable       | Source                               |
|----------------|--------------------------------------|
| `HOOK_ID`      | Hook record ID                       |
| `HOOK_NAME`    | Hook name                            |
| `TRIGGER`      | Trigger that fired (`running`, etc.) |
| `PROJECT_ID`   | Agent's project ID                   |
| `PROJECT_NAME` | Agent's project name                 |
| `AGENT_ID`     | Agent record ID                      |
| `AGENT_SLUG`   | Agent slug (hub-controlled)          |
| `SA_EMAIL`     | Resolved SA email                    |

### Untrusted variables (agent/LLM-derived)

These values originate from agent-controlled data (potentially LLM-generated) and
are subject to strict encoding rules.

| Variable       | Source              |
|----------------|---------------------|
| `AGENT_NAME`   | Agent display name  |
| `TASK_SUMMARY` | Agent task summary  |
| `AGENT_STATUS` | Agent phase string  |
| `ERROR_MSG`    | Agent error message |

Security rules for untrusted variables:

1. Untrusted variables are **never** allowed in the URL host, path, or query
   parameters (prevents URL injection).
2. Untrusted variables are **never** allowed in headers (prevents header
   injection).
3. Untrusted variables are allowed **only in the body**, and only if explicitly
   listed in `action.allowedUntrustedVars`.
4. When substituted into the body, untrusted values are **JSON-escaped** (quotes,
   backslashes, and control characters are escaped) to prevent JSON injection.
5. The admin must consciously opt in each untrusted variable. This prevents an
   agent-controlled value from being substituted under the service account's
   authority.

## Error handling

### `onError` policy

| Value   | Behavior                                                       |
|---------|----------------------------------------------------------------|
| `log`   | (Default) Single attempt. Failure is logged; no retry.         |
| `retry` | Up to 3 attempts with exponential backoff (500 ms, 1 s, 2 s).  |

- **4xx responses are non-retryable** — they indicate a client error and are
  never retried, even with `onError: retry`.
- **5xx responses and network errors** are retryable.
- After all attempts are exhausted, the error is logged. Hook failures never
  propagate to the agent transition.

### Timeout

Each action has a per-attempt `timeoutSeconds` (maximum 30 seconds, default 10).
The timeout applies independently to each retry attempt.

## SSRF protection

The executor enforces multiple layers of SSRF (Server-Side Request Forgery)
protection:

- **IP blocking**: Connections to loopback (`127.0.0.0/8`, `::1`), link-local
  (`169.254.0.0/16`, `fe80::/10`), and unspecified addresses are blocked at the
  dialer level. The dialer resolves the hostname, selects the first non-blocked
  IP, and dials that specific IP — closing the DNS-rebinding TOCTOU window.
- **RFC1918 allowed**: Private addresses (`10/8`, `172.16/12`, `192.168/16`) are
  intentionally allowed, so hooks can reach internal service registries.
- **Redirect blocking**: All HTTP redirects are blocked to prevent SSRF via
  redirect chains.

## Audit behavior

Every hook execution attempt generates an audit event capturing:

- Hook ID, hook name, trigger, and agent ID
- Execution identity (SA email or record ID)
- Action type (`http` or `webhook`) and HTTP method
- The request **host only** (never the full URL, which may contain path-based
  tokens)
- Success/failure, HTTP status code, and failure class
- Latency (milliseconds) and attempt number

Security invariants for audit records:

- **Response bodies** are never recorded.
- **Authorization header values** (bearer tokens) are never recorded.
- **Full URLs** are never recorded — only the host portion.

## Reliability and HA

Hook execution is **non-blocking**: a hook never aborts, delays, or fails an
agent phase transition. Because hooks may be retried and may fire from multiple
Hub instances, **executors (the endpoints you call) should be idempotent**.

Cross-instance HA de-duplication guarantees **exactly-once** hook firing across
multiple Hub instances. The evaluator auto-selects a deduplication strategy based
on the configured database backend:

- **Postgres (production / HA)**: A **durable store-backed CAS (compare-and-set)
  deduper** is selected automatically. Each instance receives every agent status
  event via Postgres `NOTIFY`, but only the instance that wins the atomic CAS on
  the `lifecycle_hook_agent_phase` table fires the hook. The CAS uses `SELECT …
  FOR UPDATE` row locking to serialize concurrent attempts.
- **SQLite (single-instance / dev)**: An **in-memory deduper** is used. Since
  SQLite deployments are single-instance, there is no cross-instance contention.
  The in-memory map is seeded from the store on evaluator startup to survive
  restarts within the same process.

Deduper entries are pruned only when an agent is **deleted**, not on terminal
phases. Retaining the entry after `stopped`/`error` ensures a redelivered terminal
event (pub/sub redelivery, retries, or heartbeats while terminal) is recognized as
a non-transition and does not re-fire the hook. The overhead is at most one entry
per agent (bounded by the agents table).

## Example: register / deregister flow

A common pattern registers an agent with a service registry when it starts and
deregisters it when it stops.

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

You can add matching deregister hooks for the `suspended` and `error` triggers to
ensure agents are removed from the registry in all terminal and inactive states.

## Out of scope (v1)

The following are intentionally **not** part of the first release:

- In-container or blocking hooks (hooks always run Hub-side and never block a
  transition).
- `script` action types (only `http` and `webhook` are supported).
- Activity-change triggers (only the four authoritative phase transitions fire
  hooks).
- Project-scoped hooks (`scopeType` is `hub` in v1; `project` is reserved for a
  future release and is not usable yet — use the selector to target a subset of
  agents).
- Agent-label selectors (selectors match on `projectId` and `template` only).
