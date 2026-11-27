---
title: Triggers & Webhooks
description: Connect external webhooks and events to Scion agents with signed endpoints, ordered match rules, templating, offline queuing, and failure notifications.
---

Triggers let external systems—CI pipelines, issue trackers, payment providers, monitoring tools, or other Scion agents—deliver events to a project and run automated actions under your recorded authority.

:::caution[Experimental]
Triggers and webhook endpoints are behind the `hub.triggers` [experiment](/scion/reference/experiments/), which is off by default. While it is off, all trigger, webhook, event, and ingress routes answer `404`, the project **Triggers** page is hidden, and trigger drainers do not claim queued events. GitHub App repository sources additionally require `hub.github_triggers`.
:::

:::note[Workstation mode]
Webhook endpoints are reachable wherever your Hub is reachable. On a hosted Hub, external senders can post directly to the Hub's public URL. On a loopback workstation Hub, only senders on the same machine can reach the endpoint.
:::

## Concepts

A trigger pipeline moves through five resources (defined in the [Glossary](/scion/glossary/#triggers)):

1. **Source** — an origin that emits events into a project's inbox. For general webhooks, a source is a project-owned **webhook endpoint** (`source_kind: "webhook"`). Projects can also attach triggers to a GitHub App repository source (`source_kind: "github"`, gated on `hub.github_triggers`).
2. **Event** — an immutable inbox record (`trigger_events`) captured when a source delivers a verified request. Events are retained for 7 days and processed asynchronously through the Hub's durable queue (`trigger.event`).
3. **Trigger** — a project-scoped rule attached to a source. It filters events by `types` and an optional ordered list of **rule elements** (`where`), optionally derives a concurrency **subject**, and executes one or more ordered **actions** under the authoring principal's recorded authority.
4. **Action** — the operation performed when a trigger matches: `message` (deliver an event envelope to an agent), `dispatch_agent` (create and start an agent from a template), `exec` (run a command over Conduit), or `github.comment` (for GitHub App sources). A single trigger can run up to **8 ordered actions** per matched event.
5. **Fire** — one execution attempt of one action of a trigger for a specific event (`trigger_fires`, unique per `(event_id, trigger_id, action_index)`). A fire moves from `firing` (or `waiting` when queued offline) to a terminal status: `fired`, `skipped`, `failed`, `refused`, or `expired`.

## Webhook Endpoints

Webhook endpoints are project-owned (`POST /api/v1/hooks/{id}`) and managed with `scion trigger webhook`.

### Creating an endpoint

```bash
scion trigger webhook create --name deploys --description "CI deploy callbacks"
```

For HMAC endpoints (the default), the Hub generates a 32-byte base64url signing secret and prints it **once** in the creation output, along with the endpoint URL and a `curl` + `openssl` sender example. Save the secret immediately; subsequent `get` and `list` calls never return it.

### Authentication modes (`--auth`)

| Mode | Flag | How senders authenticate |
| :--- | :--- | :--- |
| **`hmac`** (default) | `--auth hmac` | Signed HTTP request verified with the endpoint's HMAC secret according to `--signature-mode`. Do **not** send an `Authorization` header to an HMAC endpoint—an invalid credential is rejected with `401` before the HMAC signature is checked. |
| **`hub_token`** | `--auth hub_token` | Standard Hub bearer credential (user session bearer, a [User Access Token](/scion/hosted/user/personal-access-tokens/) with the `webhook_endpoint:send` scope, or an authorized agent token). No HMAC header is required. |

For `hub_token` endpoints:

- Project members, admins, and owners hold `webhook_endpoint.send` by default.
- Agents may send only if they own the endpoint or are its bound agent—or, if `--allow-project-agents` is enabled on the endpoint (off by default), any agent in the same project whose token holds `project:agent:notify`.
- Unsigned `X-Scion-Event`, `X-Scion-Subject`, and `X-Scion-Delivery` headers are honoured because the caller is authenticated. `attributes.signature_mode` is `"hub_token"`, `attributes.sender` records `<principal kind>:<id>`, and `external_payload.verified` is `"hub-token"`.

### Signature modes (`--signature-mode`)

When `--auth hmac` is selected, choose the signature format that matches your sender:

| Mode | Header & canonical string | Replay protection | Routing fields |
| :--- | :--- | :--- | :--- |
| **`scion_v1`** (default) | `X-Scion-Signature: t=<unix>,v1=<hex>` over `"v1\n" + t + "\n" + X-Scion-Delivery + "\n" + X-Scion-Event + "\n" + X-Scion-Subject + "\n" + body` | **Yes** (`±replay_window_seconds`, default `300`, range `30`–`900`) | Signed `X-Scion-Event` (`webhook.<name>`), `X-Scion-Subject`, `X-Scion-Delivery`; optional routing hints fill only empty fields |
| **`stripe_v1`** | `Stripe-Signature: t=<unix>,v1=<hex>` over `"<t>.<body>"` | **Yes** (`±replay_window_seconds`) | Routing hints, defaulting to `/type`, `/data/object/id`, `/id` |
| **`github_v1`** | `X-Hub-Signature-256: sha256=<hex>` over raw body only | **No** (GitHub signs only the body) | Unsigned `X-GitHub-Event` (`webhook.<event>`) and `X-GitHub-Delivery`; optional `--subject-path` (e.g. `/repository/full_name`) |

#### `scion_v1` sender example

```bash
HOOK_URL="https://hub.example.com/api/v1/hooks/<endpoint-id>"
SECRET="<one-time-secret>"
TS="$(date +%s)"
DELIVERY="$(uuidgen)"
EVENT="deploy.finished"
SUBJECT="service/api"
BODY='{"status":"ok","commit":"abc1234"}'

SIG="$(printf 'v1\n%s\n%s\n%s\n%s\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY" \
  | openssl dgst -sha256 -hmac "$SECRET" -r | awk '{print $1}')"

curl -fsS -X POST "$HOOK_URL" \
  -H "Content-Type: application/json" \
  -H "X-Scion-Signature: t=${TS},v1=${SIG}" \
  -H "X-Scion-Delivery: ${DELIVERY}" \
  -H "X-Scion-Event: ${EVENT}" \
  -H "X-Scion-Subject: ${SUBJECT}" \
  --data-raw "$BODY"
```

Requests must use `Content-Type: application/json` and a valid JSON body. `X-Scion-Event: <name>` sets `event.type` to `webhook.<name>` (omitting it sets `webhook.received`).

### Routing hints

Any webhook endpoint can configure RFC 6901 JSON pointers (up to 128 bytes, max 32 segments) to extract routing fields from the verified JSON body:

- `--type-path` (`routingHints.typePath`) — sets `event.type` to `webhook.<value>` when the value matches `^[a-z0-9_]+(\.[a-z0-9_]+)*$` (no case conversion; non-conforming values fall back to `webhook.received` and record a content-free `attributes.routing_hint_error`).
- `--subject-path` (`routingHints.subjectPath`) — sets `event.subject`.
- `--dedup-path` (`routingHints.dedupPath`) — sets the delivery deduplication key.

Values must be JSON strings or numbers. On `scion_v1` endpoints, signed `X-Scion-*` headers always take precedence over routing hints. On overflow bodies (> 64 KiB), routing hints are extracted from the first 64 KiB only. Note that duplicate JSON keys resolve first-wins.

### Updating, disabling, and rotating secrets

Project owners and admins (and agents on endpoints they own) can update endpoints, temporarily disable ingress, or rotate secrets:

```bash
# Disable ingress (incoming POSTs return 404; already-queued events still process)
scion trigger webhook disable deploys
scion trigger webhook enable deploys

# Update configuration
scion trigger webhook update deploys --max-body 512KiB --replay-window 120

# Rotate the HMAC signing secret (24-hour grace window for the previous version)
scion trigger webhook rotate-secret deploys
```

`rotate-secret` prints the new secret once and keeps the previous version valid for **24 hours** so senders can switch over without downtime. Rotating a second time within 24 hours immediately retires the oldest version.

:::tip[Rotate the secret when changing signature modes]
If you change an endpoint's `--signature-mode` (for example from `scion_v1` or `stripe_v1` to `github_v1`), rotate the signing secret at the same time so signatures captured under the previous canonical format cannot be reused under the new mode.
:::

## Agent-Bound Endpoints and Lifetime

A webhook endpoint is always project-owned, but it can be **bound** to a specific agent with a **lifetime** (`agent` or `persistent`):

- **`--to-self`** (inside an agent container): atomically creates a webhook endpoint bound to the calling agent with lifetime `agent` and a default `message` trigger targeting itself (`Webhook event received.` by default when `--message` is omitted):

  ```bash
  scion trigger webhook create --name ci-callbacks --to-self
  ```

- **`--bind <agent>`** (user callers): binds an endpoint to an existing agent in the project. Add `--persistent` to keep the endpoint alive after the agent is deleted.

| Lifetime | Flag | What happens when the bound agent is deleted (including soft delete) |
| :--- | :--- | :--- |
| **`agent`** (default for `--to-self` / `--bind`) | *(default)* | The endpoint, its secret rows, and any triggers on it authored by the deleted agent are **deleted**. Triggers on it authored by other principals are paused (`status_reason: source_deleted`). |
| **`persistent`** | `--persistent` | The endpoint is **unbound** (`bound_agent_id` cleared); its URL, secret, and owner stay intact. Triggers targeting the deleted agent pause with `status_reason: target_deleted` and can be re-targeted via `scion trigger update`. |

Restoring a soft-deleted agent does not restore a deleted `agent`-lifetime endpoint or automatically rebind a `persistent` endpoint. A bound agent can read its endpoint and that endpoint's events (`scion trigger event list`/`get`), and can disable, enable, rotate, or delete endpoints it owns. Each agent may have at most **5** bound endpoints (counting toward the project's 20-endpoint cap).

## Match Rules

A trigger filters incoming events in two stages:

1. **Event type prefilter (`--type`)** — repeatable glob/literal patterns against `event.type` (defaults to `webhook.received` for `--webhook` triggers; required for `--github` triggers).
2. **Ordered match list (`--where` / `--where-file`)** — an optional ordered list of 1–8 **rule elements** (`match.rules`), each up to 4,096 bytes (16 KiB encoded list total).

### Ordered mixed-dialect `where` list

Each `--where` flag accepts `[<dialect>:]<expr>` (default dialect `cel`). A `<dialect>:` prefix is recognized only for registered dialect names (`cel`, `system_one`); any other prefix is treated as part of a CEL expression. You can also pass `--where-file <path>` containing a JSON array of `{"dialect": "...", "expr": "..."}` objects.

Rules are **logically AND'd strictly in author order** with short-circuit evaluation:

- **Put cheap deterministic CEL guards first** so inexpensive checks filter out non-matching events before heavier predicates run.
- A rule element that evaluates to `false` stops evaluation immediately as a **silent non-match** (no fire row is recorded).
- A rule element that evaluates to `Uncertain`, `Failed`, or runs out of the shared per-event evaluation budget (1 second across all candidate triggers and elements of an event) stops evaluation and records a `skipped` fire with a distinguishable reason (`rule uncertain`, `rule evaluation failed`, or `rule budget exhausted`). `Uncertain` and `Failed` never match.
- Every recorded fire includes `matched_through` (`n/N`) and `decided_dialect` in `scion trigger history` (pass `--verbose` for the content-free per-element trace).
- **Admitted dialects:** `cel` is admitted for the `trigger` consumer. Model-backed dialects such as `system_one` are refused at authoring (`dialect_not_allowed`) unless explicitly granted for the trigger consumer and allow-listed for the project.

### CEL environment

CEL expressions are compiled at authoring time against:

| Variable | Type / Notes |
| :--- | :--- |
| `event.id`, `event.type`, `event.subject` | strings |
| `event.source.kind`, `event.source.id` | strings |
| `event.occurred_at`, `event.received_at` | timestamps |
| `event.attributes` | `map<string, string>` (webhook keys: `endpoint_name`, `event`, `delivery`, `subject`, `signature_mode`, `content_type`, `payload_bytes`, `remote_ip`, and `sender` for `hub_token`) |
| `event.payload` | dynamic JSON map/value (**inline bodies ≤ 64 KiB only**; `has(event.payload)` is `false` for overflow bodies) |
| `trigger.id`, `trigger.name`, `trigger.project_id` | strings |

Guard optional fields with `has(...)` (for example `has(event.payload) && has(event.payload.env) && event.payload.env == "prod"`) so absent keys evaluate cleanly instead of skipping as `missing_input`. `matches()` accepts literal RE2 patterns only; run it on `event.attributes.<k>` or a single `event.payload.<field>`, not the entire payload.

### Concurrency subjects and subject claims

Every event carries a subject (from `X-Scion-Subject`, `--subject-path`, or derived via a scalar CEL `--subject` expression up to 1,024 bytes, producing a non-empty printable string up to 256 bytes). `--on-subject` controls how concurrent work on the same subject is handled:

- **`new`** (default) — fire every matching event independently.
- **`skip`** — for `message` triggers, skip while another fire for `(trigger, action_index, subject)` is currently `firing`. For `dispatch_agent` triggers, acquire a durable **subject claim** (`trigger_subject_claims`) tied to the live dispatched agent; subsequent events for the same subject are recorded as `skipped` (`subject busy`) until that agent is deleted or reaches a terminal phase (`stopped`, `error`, or `halted`). For `exec` triggers, hold the claim until the command run finishes.
- **`reuse`** — (GitHub/Stage 3 workflows) reuse an existing live agent holding the subject claim instead of dispatching a new one.

:::note[Subject claim edge cases]
- **"Terminal" includes `stopped`:** once a bound agent enters `stopped`, `error`, or `halted`, its claim is released and a new event may dispatch a replacement. Manually restarting the older `stopped` agent afterward can result in two live agents for the same subject.
- **Crash after commit:** if the Hub crashes right after committing a dispatched agent, the subject remains busy until that agent becomes terminal or is deleted. Unbound abandoned claims (where agent creation never committed) expire after 5 minutes based on replica clocks (clock skew > 5 minutes between replicas can cause early takeover of an unbound claim).
- **`subject_rule` fallback:** if evaluating `--subject` fails at dispatch time, the Hub falls back to the raw `event.subject`, which can bypass deduplication for events that would have normalized to the same derived key.
- **Signed senders and subject collisions:** because `X-Scion-Subject` and payload fields come from the sender, any authenticated sender holding the endpoint secret can supply a colliding subject to suppress dispatch for another item on the same trigger. Derive subjects from fields the sender cannot spoof across unrelated items where possible.
:::

## Actions

A trigger runs **1 to 8 ordered actions** (`actions[]`, encoded list ≤ 64 KiB). The Hub evaluates the trigger's match rules and derives its subject **once** per event, then executes each action in list order with its own fire row `(event_id, trigger_id, action_index)`, its own authority check under the trigger's recorded authority, and its own subject claim `(trigger_id, action_index, subject)`.

- **Failure isolation:** one action's `failed`, `refused`, or `skipped` outcome never stops its sibling actions from running.
- **Rate limits & counters:** the per-trigger `--max-per-minute` limit counts **matched events**, not individual actions. On synchronous paths, `fire_count` and `error_count` aggregate per matched event; on asynchronous paths (detached `exec` runs, offline-queue retries/skips, coalesce supersedes, and reaper passes), counters are recorded **per action row**.

In the CLI, repeat `--action <type>` and group each action's flags immediately after it:

```bash
scion trigger create --name deploy-pipeline --webhook deploys --type webhook.deploy.failed \
  --action dispatch-agent --template investigator --task 'Investigate ${event.payload.service}' \
  --action message --agent lead --message 'Deploy failed for ${event.payload.service}' --offline queue
```

### `message` action and the offline queue

Delivers a structured event envelope (`type: "event"`, `event.type: "webhook.received"` or `webhook.<name>`) to a live target agent:

```bash
scion trigger create --name ci-alerts --webhook deploys \
  --action message --agent lead \
  --message 'Deploy ${event.payload.status} for ${event.payload.service}' \
  --wake --offline queue --queue-ttl 24h
```

- **Message templating:** `--message` supports the `${<path>}` / `${<path>?}` placeholder grammar described below. When untrusted placeholders are expanded, the delivered envelope includes `event.templated_vars` listing the **untrusted paths only** (never values). Passing `--plain` (no envelope framing) together with untrusted placeholders is refused (`400 plain_with_untrusted`).
- **Offline queuing (`--offline queue`):** by default (`--offline skip`), a fire targeting a non-running agent (after any `--wake` attempt) is `skipped`. With `--offline queue`, if the target is in `suspended`, `stopped`, `provisioning`, `starting`, `created`, or `stopping` phase, the fire enters `waiting` and is retried with exponential backoff (30 s → 5 min cap) up to `--queue-ttl` (default `24h`, range `60s`–`48h`). When the target transitions into `running`, the Hub nudges its waiting fires for prompt delivery.
  - Every retry re-runs full recorded-authority, standing, and live authorization checks. If the trigger is paused, deleted, or edited (`authorization_revision` changed), the waiting fire finishes `skipped` (`trigger changed`).
  - If the target enters `error` or `halted`, or is deleted, the waiting fire finishes `skipped`.
  - If `--queue-ttl` elapses before delivery, the fire finishes `expired` (`offline queue expired`), which counts as a trigger error and triggers failure notifications.
  - If the Hub shuts down while a wake attempt is in progress, the fire is reaped as `failed` (equivalent to Stage 1; it is never silently re-delivered twice).
  - Queue caps: **100** waiting fires per target agent and **1,000** per project (`skipped` with `offline queue full` above either cap).

### `dispatch_agent` action

Creates and starts a new agent from a template whenever an event matches:

```bash
scion trigger create --name triage-pr --webhook ci-hooks --type webhook.build.failed \
  --action dispatch-agent \
  --template triage \
  --agent-name 'triage-${event.payload.run_id}' \
  --branch 'fix/${event.payload.branch}' \
  --task 'Investigate failed build ${event.payload.run_id}: ${event.payload.summary?}' \
  --label 'service=${event.payload.service}' \
  --max-concurrent 5
```

- **Identity & role:** dispatched agents are owned by the trigger's recorded author, with credentials enabled and role chosen from the project/Hub default ladder (capped by project max and the author's delegation ceiling), or set explicitly via `--agent-role`.
- **Agent name & collisions:** `--agent-name` is optional; when omitted, the Hub generates `<trigger-name>-<6 base32>`. If a live agent already holds the slug, `--on-name-conflict suffix` (default) appends `-<4 hex>` (up to 3 retries), while `skip` records `skipped` (`agent name in use`). Soft-deleted names are reusable.
- **GitHub token precedence (`--github-token`):** chooses `installation` (suppress user/project GitHub PATs and inject only the project's GitHub App installation token) or `user_preferred` (standard precedence). Defaults to `installation` for `github` sources and `user_preferred` for webhook sources, resolved and stored at authoring time.
  - When `installation` is active and no GitHub App installation is available for the project, the agent receives no GitHub token and its task trailer notes that GitHub access is unavailable. That note keys on whether a mint project resolves (not on whether a later token mint succeeds, such as a revoked installation)—the agent still never receives a PAT.
  - Note: `GH_{OWNER}` custom secrets or Copilot sign-in material stored outside standard `GITHUB_TOKEN`/`GH_TOKEN` keys are not automatically classified as GitHub PATs; keep user-scoped GitHub credentials out of projects where untrusted event-driven agents run.
- **Replay protection required:** `dispatch_agent` triggers may only attach to replay-protected sources (`scion_v1`, `stripe_v1`, `hub_token`, or `github` App). Attaching to `github_v1` is refused at authoring (`400 source_not_replay_protected`), switching an endpoint's mode while such triggers are active is refused (`409`), and events ingested under a non-replay-protected mode are refused at fire time.
- **Ephemeral flag (`--ephemeral`):** currently refused by the Hub (`400 ephemeral_unsupported`) until ephemeral trigger-agent reaping lands.

### Event context injected into dispatched agents

Every agent started by a `dispatch_agent` trigger receives:

1. **Labels (hub-applied, reserved):** `trigger=<trigger-id>`, `trigger_event=<event-id>`, `trigger_source=<source-kind>` (3 of the 16-label max; authoring refuses these three keys as custom `--label` inputs). Find sibling agents with `scion list --label trigger=<trigger-id>`.
2. **Environment variables:** `SCION_TRIGGER_ID`, `SCION_TRIGGER_EVENT_ID`, and `SCION_TRIGGER_EVENT_FILE` (the resolved path to `~/.scion/trigger-event.json`).
3. **Home file (`~/.scion/trigger-event.json`):** written by the Runtime Broker before the harness starts, containing the same `EventBody` JSON as a message envelope (`event_id`, `event_type`, `source`, `trigger`, `fire_id`, `subject`, `attributes`, `external_payload` bounded to 16 KiB with `truncated`/`bytes`). Works for every agent role including `none`. If the Runtime Broker does not advertise home-file support, the fire fails rather than starting the agent without its event file. A `ResetHome` reprovision drops this file.
4. **Event read access:** the created agent can fetch its own event (including overflow payloads) with `scion trigger event get "$SCION_TRIGGER_EVENT_ID" --payload` when its role allows API calls.

### Template grammar, trust, and `scion trigger render`

Templates are plain UTF-8 text with `${<path>}` (required—missing value refuses the fire) and `${<path>?}` (optional—missing value renders empty). Escape a literal `${` as `$${`. Expanded values are never re-scanned, and there are no expressions or filters inside placeholders. Max 64 placeholders per slot.

| Path prefix | Trust class | Notes |
| :--- | :--- | :--- |
| `event.id`, `event.source.kind`, `event.source.id`, `event.source.name`, `event.received_at` | **Trusted** | Hub-assigned |
| `trigger.id`, `trigger.name`, `project.id`, `project.name` | **Trusted** | Authoring / Hub metadata |
| `result.agent_id`, `result.agent_name`, `result.agent_url`, `result.fire_id` | **Trusted (post-fire only)** | Only valid in post-fire slots such as `--ack-comment` |
| `event.type`, `event.subject`, `event.occurred_at` | **Untrusted** | Sender-controlled |
| `event.attributes.<key>` | **Untrusted** | String attribute |
| `event.payload.<path>` | **Untrusted** | JSON scalar (string, number, boolean) for inline bodies ≤ 64 KiB; `null`/objects/arrays refuse the fire. Use the canonical bracket form `event.payload.items[0]` (not `items.[0]`) so `--allow` exact matches work consistently. |

| Slot | Max bytes | Untrusted value rules | Framing |
| :--- | :--- | :--- | :--- |
| `task` | 16,384 (incl. trailer) | Literal UTF-8, no NUL | Appends a Hub-generated `Trigger event context` trailer listing quoted untrusted values |
| `message` | 16,384 | Literal UTF-8, no NUL | Envelope `event.templated_vars` lists untrusted paths |
| `name` | 63 | `^[a-z0-9-]{1,32}$`; whole name must equal its own slug | None |
| `branch` | 100 | `^[A-Za-z0-9._/-]{1,64}$`, no `..`, no leading `-` or `.`; valid git ref | None |
| `label` | 63 | Printable ASCII (`0x20`–`0x7E`); valid label map | None |

`template`, environment variables, secrets, and workspace settings can never be templated. Exceeding a slot's byte or charset bound **refuses** the fire (values are never truncated).

- **Opt-in list (`--allow` / `untrustedVars`):** the Hub requires every untrusted placeholder to be covered by `untrustedVars` (exact path or trailing `.*`). In the CLI, omitting `--allow` automatically derives the exact list from your templates and prints which event values will be inserted as data; pass `--allow` to override it explicitly.
- **Dry-run preview:** test templates against any stored event without firing or creating resources:

  ```bash
  scion trigger render triage-pr --event <event-id>
  ```

### `exec` action

Runs a command (`argv`, executed directly via `execve` with no shell) inside a running target agent's container over Conduit—no message and no LLM turn. Requires `hub.triggers`, `hub.exec_schedules`, and `hub.conduit`, plus live `agent.attach` on the target at authoring, resume, and fire time, and a replay-protected source. Currently authored via the Hub API (`action.type: "exec"` with `agentId`/`agentName`, `command`, `timeoutSeconds`, and optional `contextAttributes`).

- **No templating in `argv`:** `${` is refused in `command` at authoring (`400 templating_not_allowed_in_argv`).
- **Context via `SCION_TRIGGER_CONTEXT`:** the Hub passes fixed JSON (`event_id`, `trigger_id`, `fire_id`, plus opt-in `subject` and allow-listed `attributes` via `contextAttributes`, ≤ 16 KiB total) in the `SCION_TRIGGER_CONTEXT` environment variable. Requires a `sciontool` advertising `exec_context.v1`.
- **Argv redaction:** `command` is revealed in API/CLI responses only to callers holding live `agent.attach` on the target; other readers see `commandRedacted: true`, `argc`, and `commandSha256`. Command output is never stored.

## Large Payloads (> 64 KiB, up to 1 MiB)

By default, webhook endpoints accept JSON bodies up to **64 KiB**, stored inline in `trigger_events.payload`. You can raise an endpoint's `--max-body` up to **1 MiB**:

- Bodies **≤ 64 KiB** are stored inline and exposed in CEL as `event.payload` and in templates as `${event.payload.*}`.
- Bodies **> 64 KiB** (up to `max_body_bytes`, max 1 MiB) are stored out of line in Hub-private object storage (`trigger-events/<project-id>/<event-id>.json`, never via public or signed URLs) before the ingress database transaction commits.
  - `has(event.payload)` is **`false`** in CEL; route on headers, attributes (`event.attributes.payload_bytes`, `payload_ref`), and routing hints (extracted from the first 64 KiB).
  - Template placeholders under `event.payload.*` are treated as out of line (`${event.payload.x?}` renders empty; required `${event.payload.x}` refuses the fire).
  - Envelopes and `~/.scion/trigger-event.json` include a 16 KiB preview in `external_payload` with `truncated: true` and `bytes: <N>`.
  - Fetch the full body through the authorized Hub endpoint (`GET /api/v1/projects/{p}/trigger-events/{id}?payload=1`) or CLI:

    ```bash
    scion trigger event get <event-id> --payload --out ./event.json
    ```

  - Each project has a **256 MiB** budget of live overflow objects over the 7-day retention window (above that, ingress returns `429 queue full`). Objects are purged with their event rows after 7 days (and an hourly orphan sweep cleans up unreferenced objects older than 5 minutes).
  - Ensure any reverse proxy in front of your Hub permits request bodies up to your configured `--max-body`.

<!-- P7a: verify -->
## Notify-on-Failure

Triggers can notify a human and/or an in-project agent when a fire fails, is refused, expires in the offline queue, or (optionally) is skipped:

- **User notice sink (default on):** sends a content-free Hub user notification (`trigger_fire_failed`) to the trigger's creator—or, when the trigger was authored by an agent, to that agent's delegating user—with the trigger name, outcome, generic reason, and a link to the fire history row. Never contains event payloads or `exec` argv.
- **Agent message sink (optional):** delivers a synthetic `trigger.fire_failed` message to an in-project agent under the trigger's recorded authority (checked at authoring and at every notification; never uses `wake`, never recurses). Recorded as a `trigger_fires` row with `action_type: notify`.
- **Cooldown:** notifications are rate-limited per `(trigger, outcome, reason)` with a **1-hour** default cooldown so a flapping target does not flood recipients.
- **Project live-agent cap:** breaching the 25-agent per-project trigger cap also routes through notify-on-failure (once per hour per trigger).

CLI flags (`scion trigger create` / `scion trigger update`):

- `--notify-user` / `--notify-user=false` — toggle creator user notices (default `true`).
- `--notify-agent <name|id>` — optional in-project agent to notify.
- `--notify-on failed,refused,expired[,skipped]` — outcomes that trigger a notification (default `failed,refused,expired`).

Editing `notify` settings bumps `authorization_revision`.
<!-- /P7a: verify -->

## Web UI

When `hub.triggers` is enabled, each project displays a **Triggers** button in its header linking to `/projects/{id}/triggers` with four tabs:

- **Endpoints** — create endpoints (shows the one-time signing secret and sender example in a result dialog; secrets are never kept in component state), edit settings/bindings, disable/enable, rotate secrets, and delete (listing rules that will pause).
- **Rules** — create and edit triggers with an ordered `where` rule list editor (add/remove/reorder rows, admitted-dialect selector, per-row compile errors), ordered action editors, limits, <!-- P7a: verify -->notify settings,<!-- /P7a: verify --> and a **Preview** button calling `POST .../render` against recent events with untrusted values highlighted.
- **Events** — browse received events filtered by endpoint, status, or time; inspect attributes, headers, `external_payload` (with download for overflow bodies), and resulting fires.
- **History** — project-wide fire history (`GET .../trigger-fires`) filtered by trigger, status, or outcome, linking to the event, created agent, or `waiting` deadline (`wait_until`).

Hub administrators can also inspect durable queue health (`durableQueues`) on the Admin Scheduler page.

## Security Notes

- **Anyone with `trigger.read` can read event bodies** sent to any webhook endpoint in the project. Do not send unencrypted secrets or sensitive personal data in webhook payloads unless every project reader is authorized to see them.
- **All event content is untrusted data, not instructions.** Treat `external_payload`, templated values in `task` (flagged by the **last** `Trigger event context` trailer), and `templated_vars` in message envelopes as attacker-influenced input.
- **Sender-set mutability:** An endpoint's sender set may be widened by authorized endpoint owners/admins (including the owning agent) after triggers are attached (for example, switching to `hub_token` admits every project member) without author re-consent. Changes are audited on the endpoint, and authors are not notified. Triggers—including those with System One elements—then evaluate events from the new senders under the author's recorded authority, with all event content treated as untrusted and only the element's declared, size-bounded inputs sent to an allow-listed instance. Only the replay-protection rule for agent-creating and `exec` triggers is enforced across mode changes.
- **`exec` security:**
  - `SCION_TRIGGER_CONTEXT` is untrusted, sender-influenced JSON. Always parse it with a JSON parser (such as `jq` or Python `json`) and **never** expand `$SCION_TRIGGER_CONTEXT` unquoted in a shell command.
  - Never put secrets in `exec` `argv`: `commandSha256` and `argc` are visible to trigger readers without `agent.attach`, so low-entropy secrets in `argv` could be guessed offline. Use agent secrets or environment variables instead.
  - **Authenticated vs. unauthenticated attributes:** on `scion_v1`, `event`, `delivery`, `subject`, and the JSON body are covered by the HMAC signature; `endpoint_name` and `signature_mode` are Hub-derived; **`content_type` and `remote_ip` are NOT covered by the signature**. On `github_v1`, `X-GitHub-Event` (`event`) and `X-GitHub-Delivery` (`delivery`) are unsigned headers—only the body is signed.
- **Pre-authentication memory on large-body endpoints:** raising `--max-body` above the 64 KiB default increases the maximum request body buffered before signature verification (up to 1 MiB per in-flight request, bounded by per-IP and per-endpoint rate limits). Leave `--max-body` at 64 KiB unless your sender requires larger payloads.
- **Overflow payload stream & timing:** overflow payloads are served only through the authorized Hub endpoint (`application/json`, `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, never via signed URLs). The 5-minute orphan-sweep grace assumes Hub and object-storage clocks stay within ~4 minutes of each other. The 256 MiB per-project budget window counts rows over the 7-day retention window and can transiently overshoot by up to ~1 hour of ingress while aged rows await the hourly purge.
- **Agent-mode secrets:** running `scion trigger webhook create` or `rotate-secret` inside an agent prints the one-time secret into the agent's transcript; agent-owned endpoints are isolated so that secret only feeds that agent's own endpoint.

## Limits

| Limit | Default | Maximum / Range | On breach |
| :--- | :--- | :--- | :--- |
| Webhook endpoints per project | — | 20 | `409` |
| Bound endpoints per agent | — | 5 | `409` |
| Triggers per project | — | 200 | `409` |
| Active triggers per source | — | 50 | `409` |
| Match rules (`where`) per trigger | — | 8 rules (4,096 B each; 16 KiB total) | `400` |
| Actions per trigger | 1 | 8 actions (64 KiB encoded total) | `400` |
| Per-endpoint ingress rate / burst | 10 req/s / 60 | ≤ Hub defaults (can lower, not raise) | `429 Retry-After` |
| Per-IP ingress rate / burst | 20 req/s / 100 | Hub constant | `429` |
| Per-endpoint body size (`--max-body`) | 64 KiB | 1 MiB (> 64 KiB stored out of line) | `413` |
| Overflow storage per project (7 d) | — | 256 MiB | `429 queue full` |
| Replay window (`--replay-window`) | 300 s | 30 s – 900 s | `401` |
| Secret rotation grace window | 24 h | Fixed | `401` after grace |
| Per-trigger fire rate (`--max-per-minute`) | Unlimited (`0`) | 600 / min (counts matched events) | `skipped` (`rate limit`) |
| Per-trigger dispatch concurrency (`--max-concurrent`) | 5 | 50 live agents | `skipped` (`concurrency limit`) |
| Per-project live trigger-created agents | 25 | Hub constant | `skipped` (`project trigger-agent cap`) |
| Offline queue per target / per project | — | 100 / 1,000 waiting fires | `skipped` (`offline queue full`) |
| Offline queue TTL (`--queue-ttl`) | 24 h | 60 s – 48 h | `expired` |
| Event and fire history retention | 7 days | Hub constant | Purged hourly |

## CLI Reference

See [`scion trigger`](/scion/reference/cli/#scion-trigger) in the CLI Reference for the complete list of subcommands and flags.
