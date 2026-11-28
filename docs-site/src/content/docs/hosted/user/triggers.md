---
title: Event Triggers & Webhooks
description: Automate agent messaging, headless agent dispatch, and GitHub replies from signed webhooks and GitHub App events.
---

Scion **Event Triggers** automate project work in response to external push events. An external **Source** (a project webhook endpoint or the Hub's GitHub App) delivers a verified **Event** into the Hub's durable inbox; matching **Triggers** evaluate ordered filter rules and execute one or more **Actions** under recorded principal authority, logging every attempt as a **Fire**.

:::note[Experimental]
Event triggers are currently **alpha** and **off by default** on the Hub:

- **`hub.triggers`**: Enables webhook endpoints (`POST /api/v1/hooks/{id}`), event triggers, and received trigger events. While off, webhook ingress and all project `/webhook-endpoints`, `/triggers`, and `/trigger-events` routes return `404`.
- **`hub.github_triggers`** (also requires **`hub.triggers`**): Enables routing GitHub App webhook deliveries into the trigger system, trigger presets (`/api/v1/trigger-presets*`), GitHub readiness/settings inspection, and primary-project repository routing.

A Hub administrator enables these flags in the Hub Experiments settings. See the [Experiments Reference](/scion/reference/experiments/#server-experiments-with-notes) for details.
:::

## Overview

A trigger pipeline consists of four project-scoped concepts:

1. **Source**: Either a project-owned **Webhook endpoint** (`source_kind: "webhook"`, receiving `POST /api/v1/hooks/{id}`) or the project's **GitHub App** repository source (`source_kind: "github"`).
2. **Event**: An immutable durable inbox record captured at ingress with a canonical `type`, optional `subject`, `dedup_key`, extracted `attributes`/`lists`, allow-listed headers, and bounded JSON `payload`, processed asynchronously through the Hub's durable queue.
3. **Trigger**: A rule definition attached to a source that filters incoming events by `type` and optional ordered CEL match rules (`--where`), then executes one or more actions (`message`, `dispatch_agent`, or `github.comment` in the CLI; the Hub trigger API also supports `exec`).
4. **Fire**: An idempotent execution record (`trigger_fires`) per `(event_id, trigger_id, action_index)` that tracks in-flight states (`coalescing`, `deferred`, `firing`, `waiting`) through terminal outcomes (`fired`, `skipped`, `failed`, `refused`, or `expired`).

### Triggers vs. Scheduling

Both systems share the Hub's fire core and recorded-authority model, but they serve different automation needs:

| Capability | Event Triggers (`scion trigger`) | Scheduling & Events (`scion schedule`) |
| :--- | :--- | :--- |
| **What starts a fire** | Inbound signed webhook or GitHub App event | Time-based one-shot delay (`--in`/`--at`) or cron expression (`--cron`) |
| **Supported actions** | `message`, `dispatch-agent`, `github.comment` in the CLI (plus `exec` via the Hub API); single or ordered `actions[]` | `message`, `dispatch_agent`, `exec` (experimental) |
| **Event filtering & routing** | Event type matching, ordered CEL rules, subject concurrency & coalescing | Fixed target and schedule timing |
| **Canonical guide** | This page | [Scheduling & Events](/scion/hosted/user/scheduling/) |

Use **Scheduling** for time-based checks, self-callbacks, and recurring cron jobs; use **Event Triggers** when external systems (CI pipelines, deploy webhooks, Stripe, or GitHub PRs and issues) should drive agent work immediately.

---

## Webhook Endpoints

Webhook endpoints provide project-scoped HTTP ingress at `POST /api/v1/hooks/{id}`. Requests must have `Content-Type: application/json` and a body within the endpoint's size cap (default `64 KiB`, configurable up to `1 MiB` via `--max-body`; larger bodies return `413`).

### Authentication & Signature Modes

Choose an authentication mode with `--auth`:

- **`hmac` (default)**: Verifies an HMAC-SHA256 signature generated from a Hub-minted 32-byte base64url signing secret. Do not send an `Authorization` header to an `hmac` endpoint (an invalid credential is rejected with `401` before HMAC verification). Select the wire format with `--signature-mode`:
  - **`scion_v1` (default, replay-protected)**: Senders sign `"v1\n" + t + "\n" + X-Scion-Delivery + "\n" + X-Scion-Event + "\n" + X-Scion-Subject + "\n" + body` (empty string for omitted optional `X-Scion-*` headers) and send `X-Scion-Signature: t=<unix_seconds>,v1=<hex_hmac_sha256>`. Timestamps must fall within `--replay-window` seconds (range `30` to `900`, default `300`). Setting `X-Scion-Event: <name>` sets `event.type` to `webhook.<name>` (otherwise `webhook.received`).
  - **`github_v1` (GitHub repository webhooks; message actions only, not replay-protected)**: Verifies `X-Hub-Signature-256` over the raw body; maps `X-GitHub-Event` to `webhook.<event>` and `X-GitHub-Delivery` to deduplication. Because GitHub repository webhooks are not timestamped, triggers with `dispatch-agent` or API-authored `exec` actions cannot attach to `github_v1` endpoints.
  - **`stripe_v1` (Stripe webhooks; replay-protected)**: Verifies `Stripe-Signature` over `"<t>.<body>"` and defaults routing hints to `/type` (event type), `/id` (dedup key), and `/data/object/id` (subject).
- **`hub_token` (replay-protected)**: Authenticates senders with Hub bearer credentials authorized to send to the endpoint. Pass `--allow-project-agents` to let any agent in the same project post to the endpoint.

You can override event routing extraction from the verified JSON body with RFC 6901 JSON pointers (`--type-path`, `--subject-path`, `--dedup-path`) and tune per-endpoint ingress throttling with `--rate` (default `10` req/s) and `--burst` (default `60`).

### Managing Webhook Endpoints

| Command | Description |
| :--- | :--- |
| `scion trigger webhook create --name <name>` | Create an endpoint and print its signing secret **once**. Supports optional one-call default message trigger flags (`--agent <agent> --message <text>`, or `--to-self` inside an agent container, plus `--wake`, `--interrupt`, `--bind`, `--persistent`). |
| `scion trigger webhook list` | List project webhook endpoints (filter with `--status active\|disabled`, `--bound <agent>`). Agent callers see only endpoints they own. |
| `scion trigger webhook get <id\|name>` | Show endpoint configuration, URL, signature mode, routing hints, secret version, and received/rejected counters (never returns the signing secret). |
| `scion trigger webhook update <id\|name>` | Update description, `--status`, `--auth`, `--allow-project-agents`, `--signature-mode`, routing hints, `--rate`/`--burst`, `--max-body`, `--replay-window`, or agent binding (`--bind`/`--unbind`, `--persistent`/`--agent-lifetime`). |
| `scion trigger webhook disable <id\|name>` | Disable ingress so incoming deliveries return `404` while already-queued events finish processing. |
| `scion trigger webhook enable <id\|name>` | Re-enable a disabled endpoint. |
| `scion trigger webhook rotate-secret <id\|name>` | Mint and print a new signing secret **once**. The previous secret remains valid for a **24-hour grace window** (rotating again within 24 hours retires the older secret immediately). |
| `scion trigger webhook delete <id\|name>` | Delete the endpoint and automatically pause dependent triggers with `status_reason=source_deleted`. |

:::caution[Save the signing secret immediately]
`scion trigger webhook create` and `scion trigger webhook rotate-secret` display the HMAC signing secret **only once**. Subsequent `get` and `list` calls never return it. Anyone with read access to the project's triggers can inspect payloads received by its endpoints.
:::

### Example: Create an Endpoint and Send a Signed Request

```bash
# Create an endpoint and a default message trigger in one call
scion trigger webhook create \
  --name deploys \
  --agent lead \
  --message "Inspect incoming deploy webhook" \
  --wake

# Send a signed scion_v1 webhook to the returned URL
HOOK_URL="https://hub.example.com/api/v1/hooks/<endpoint-id>"
SECRET="<one-time-secret>"
TS="$(date +%s)"
DELIVERY="$(uuidgen)"
EVENT="deploy.finished"
SUBJECT="service/api"
BODY='{"env":"prod","service":"api","commit":"abc1234"}'
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

---

## Creating & Managing Event Triggers

### Trigger Lifecycle Commands

All commands accept `--json` for machine-readable output and resolve triggers by UUID, exact name, or unique ID prefix:

| Command | Description |
| :--- | :--- |
| `scion trigger create` | Create a trigger attached to `--webhook <id\|name>` or `--github`. |
| `scion trigger list` | List triggers (filter with `--source webhook\|github`, `--webhook <id\|name>`, `--status active\|paused`). Agent callers see only triggers they hold. |
| `scion trigger get <id\|name>` | Show full configuration, numbered match rules, subject policy, ordered actions, and fire counters. |
| `scion trigger update <id\|name>` | Patch trigger fields (users only; agent callers cannot PATCH triggers). Supports `--revision <n>` for optimistic concurrency. Action updates support `message` and `dispatch-agent` (`--comment`/`--reply-in-thread` are not registered on `update`). |
| `scion trigger pause <id\|name>` | Pause an active trigger. |
| `scion trigger resume <id\|name>` | Resume a paused trigger under the caller's live authority. |
| `scion trigger delete <id\|name>` | Permanently delete a trigger. |
| `scion trigger history <id\|name>` | List fire history newest first (`--limit`, `--cursor`, and `--verbose` for per-element rule evaluation traces). |
| `scion trigger render <id\|name> --event <id>` | Dry-run template rendering for every action template slot against a stored event without firing. |

### Matching Events & Ordered Rules (CEL)

- **Event types (`--type`)**: Repeatable event type filter. Defaults to `webhook.received` for `--webhook` sources; required when `--github` is set (for example `--type github.issue_comment.created`).
- **Ordered match rules (`--where` / `--where-file`)**: Repeat `--where [cel:]<expr>` (1 to 8 rules, up to 4096 bytes each; total encoded rule list <= 16 KiB) or pass `--where-file <path>` containing a JSON array of `{"dialect": "cel", "expr": "..."}` objects (up to 64 KiB). Triggers admit the pure, no-I/O `cel` dialect exclusively (other rule dialects are rejected with HTTP `400` `dialect_not_allowed`).
- **Short-circuit AND evaluation**: Rules are logically AND'd and evaluated strictly in order, stopping on the first `false` result. Put cheaper CEL checks first so simple guards filter events before heavier expressions run.
- **Available CEL variables**: Expressions are compiled at authoring time against:
  - `event.id`, `event.type`, `event.source.kind`, `event.source.id`, `event.subject`, `event.occurred_at`, `event.received_at`, `event.attributes`, `event.lists` (`map<string, list<string>>`, for example `event.lists.labels` used by GitHub presets), `event.payload`
  - `trigger.id`, `trigger.name`, `trigger.project_id`
- **Safety and regex guidelines**:
  - Guard optional fields with `has(...)` (for example `has(event.payload) && has(event.payload.env)`) so missing keys evaluate to `false` instead of failing as missing input.
  - `matches()` accepts literal RE2 patterns only (dynamic patterns fail compilation at create/update time). Run `matches()` against `event.attributes.<key>` or a single `event.payload.<field>`, not the entire raw payload.

### Actions & Ordered Action Lists (`actions[]`)

The `scion trigger` CLI exposes three action kinds (the Hub trigger API also supports `exec`, which, like `dispatch_agent`, requires a replay-protected source and cannot attach to `github_v1` webhook endpoints):

1. **`message` (default)**: Delivers an event envelope and message to `--agent <name|id>` with `--message <template>`. Supports `--wake` (resume if suspended), `--interrupt`, `--plain` (omit sender prefix; cannot combine with untrusted event placeholders), `--offline skip|queue` (default `skip`), and `--queue-ttl <duration>` (`60s` to `48h`, default `24h`, requires `--offline queue`).
2. **`dispatch-agent` (`dispatch_agent`)**: Spawns a headless agent from `--template <name>` (literal template name, no placeholders) with `--task <template>` or `--task-file <path>`. Supports `--agent-name`, `--on-name-conflict suffix|skip` (default `suffix`), `--branch`, repeatable `--label key=value`, `--reuse-message` (used when `--on-subject reuse` reuses a live agent), `--ack-comment` (GitHub sources only), `--github-token installation|user_preferred`, and `--agent-role`. Requires a replay-protected source (`scion_v1`, `stripe_v1`, `hub_token`, or GitHub App).
3. **`github.comment` (shorthand `--comment <template>`)**: Posts a comment on the triggering GitHub issue, pull request, or review comment thread (add `--reply-in-thread` to reply in a review thread). Requires `--github`.

#### Chaining Multiple Actions in Order (`actions[]`)

A single trigger can execute up to **8 ordered actions** (64 KiB total encoded limit) per matching event. Repeat `--action <kind>` on `scion trigger create` (or `scion trigger update` for `message` and `dispatch-agent` actions), grouping each action's flags immediately after its `--action` flag from left to right:

```bash
# 1) Acknowledge on GitHub, 2) dispatch a reviewer agent, 3) notify the project lead
scion trigger create \
  --name pr-opened-workflow \
  --github \
  --type github.pull_request.opened \
  --where 'has(event.payload.pull_request) && !event.payload.pull_request.draft' \
  --action github.comment \
    --comment 'Starting automated review for PR #${event.attributes.number}.' \
  --action dispatch-agent \
    --template reviewer \
    --agent-name 'pr-${event.attributes.number}' \
    --task 'Review PR #${event.attributes.number}: ${event.payload.pull_request.title}' \
  --action message \
    --agent lead \
    --message 'Dispatched reviewer for PR #${event.attributes.number}' \
    --wake
```

How ordered `actions[]` execute:

- Actions run sequentially in order (`ActionIndex` `#0`, `#1`, ...).
- All sibling actions for a matched event share a single per-`(trigger, event)` rate-limit admission decision.
- Each action records its own fire row in `scion trigger history` (shown in the `ACTION` column as `#0:github.comment`, `#1:dispatch_agent`, `#2:message`) and applies action-specific guards independently (for example, stale-event refusals apply to `dispatch_agent` and `github.comment` without blocking passive `message` siblings).
- When modifying actions on an existing multi-action trigger with `scion trigger update`, pass `--action` for every action in the replacement list. Note that `scion trigger update` does not register `--comment` or `--reply-in-thread`, and single-action `update` supports only `message` and `dispatch-agent` (rejecting `github.comment`); update triggers containing `github.comment` via the Hub API or recreate them.

### Concurrency, Coalescing, Templates & Authority

- **Subject concurrency & coalescing**: Derive a concurrency key with `--subject <cel-expr>` (string, <= 1024 bytes), control behavior when work for that subject is already active with `--on-subject new|skip|reuse` (default `new`), and coalesce burst events within a window using `--coalesce <duration>` (up to `10m`, `0s` disables).
- **Rate and concurrency caps**: Bound trigger throughput with `--max-per-minute` (`0` = unlimited, max `600`) and active dispatched agents with `--max-concurrent` (`0` = default `5`, max `50`).
- **Template preview (`render`) & untrusted variables**: Templates use `${path}` (required) and `${path?}` (optional) placeholders (quote with single quotes in bash so the shell does not expand `${...}`). Preview all rendered template slots against any stored event before activating or changing a trigger:

```bash
scion trigger render pr-opened-workflow --event <event-id-or-prefix>
```

The CLI highlights untrusted event variables used in each slot and escapes control characters. On `create` and `update`, the CLI automatically derives required untrusted variable paths from your templates, or you can specify an explicit allow-list with repeatable `--allow <path>` (supports trailing `.*`).
- **Recorded authority & automatic pauses**: Triggers execute under the recorded authority and frozen permission ceiling of the principal that created, updated, or resumed them. The Hub automatically pauses a trigger with a structured `status_reason` if its authority or dependencies disappear (`creator_deleted`, `target_deleted`, or `source_deleted`). Inspect `scion trigger history <name> --verbose` to see rule evaluation traces and refusal/skip reasons.

---

## Trigger Presets

Trigger presets compile validated event types, CEL match rules, and subject policies from structured `--field key=value` inputs. Presets require `hub.github_triggers` (and `hub.triggers`).

| Command | Description |
| :--- | :--- |
| `scion trigger preset list [--source github]` | List available server-defined trigger presets. |
| `scion trigger preset show <id>` | Inspect a preset's description, supported action kinds, and field schema (types, defaults, enums). |
| `scion trigger preset apply <id>` | Compile a preset on the Hub and create a trigger (or preview with `--dry-run`). |

Built-in GitHub presets:

| Preset ID | Matches |
| :--- | :--- |
| `github.command` | Slash command mention (for example `@<app-slug> /review`) on issues or pull requests |
| `github.pr_opened` | Pull request opened, reopened, or marked ready for review |
| `github.pr_updated` | Pull request updated with new commits pushed |
| `github.issue_opened` | Issue opened |
| `github.label_added` | Label added to an issue or pull request |
| `github.review_submitted` | Pull request review submitted |

Use `--dry-run` to inspect the compiled CEL rules before creating a trigger, and pass `--strict` to fail if your field values widen eligibility beyond trusted defaults (author associations outside `OWNER`, `MEMBER`, `COLLABORATOR`, or enabling `include_forks` / `allow_bots` with a `dispatch-agent` action):

```bash
# Preview compiled rules for a slash-command trigger
scion trigger preset apply github.command \
  --field command=review \
  --field on=pull_request \
  --dry-run

# Create a trigger from the preset with --strict safety enforcement
scion trigger preset apply github.command \
  --name pr-review-cmd \
  --field command=review \
  --field on=pull_request \
  --strict \
  --agent lead \
  --message "Handle /review command" \
  --wake
```

---

## Inspecting Received Events

Every verified webhook or GitHub App delivery is stored in the project's trigger event inbox and can be inspected from the CLI:

```bash
# List recent events (filter by --source, --webhook, --status pending|processed|failed, --since RFC3339)
scion trigger event list --source webhook --webhook deploys --limit 25

# Inspect event metadata, extracted attributes, lists, headers, and payload
scion trigger event get <event-id-or-prefix>

# Stream raw JSON payload (works for overflow payloads stored out of line)
scion trigger event get <event-id-or-prefix> --payload --out ./event.json
```

Pass `--force` alongside `--out` to overwrite an existing file. Agent callers see only events visible to triggers or endpoints they own.

---

## GitHub App Event Triggers

To trigger agents from GitHub pull requests, issues, reviews, and slash-command comments, configure a GitHub App with webhooks active and the required repository permissions (`contents`, `pull_requests`, `issues` read/write, and `metadata` read-only) plus event subscriptions (`pull_request`, `issues`, `issue_comment`, `pull_request_review`, `pull_request_review_comment`, and optionally `push`). See [GitHub App Setup](/scion/hosted/single-node/github-app/) and [Auth & Tenancy: Permissions and Events for GitHub Triggers](/scion/hosted/single-node/auth/#permissions-and-events-for-github-triggers) for setup details.

Manage and diagnose GitHub App trigger integration with `scion trigger github`:

| Command | Scope | Description |
| :--- | :--- | :--- |
| `scion trigger github status` | Project (+ Hub admin details) | Show GitHub App trigger readiness (`ready`, missing events/permissions), project installation association, repository routing status (`ok`, `none`, `stale_primary`), and unmatched-command settings. |
| `scion trigger github settings` | Project | View or update unmatched `/command` reply behavior (`--unmatched-command-reply on\|off`, `--unmatched-command-text <text>`; pass `""` to reset text to default). |
| `scion trigger github primary list` | Hub admin | List repository primary-project routing entries (optional `--installation <id>`). |
| `scion trigger github primary set <owner/repo> --project <id\|slug>` | Hub admin | Pin the primary project that receives GitHub trigger webhooks when multiple projects share a repository (optional `--installation <id>`). |
| `scion trigger github primary unset <owner/repo>` | Hub admin | Remove the primary-project routing entry for a repository (optional `--installation <id>`). |

```bash
# Verify App readiness and repository routing for the current project
scion trigger github status

# Enable replies when a @<app-slug> /command matches no active trigger
scion trigger github settings --unmatched-command-reply on

# (Hub admin) Pin one project as primary when multiple projects track acme/backend
scion trigger github primary set acme/backend --project backend-prod
```

## See also

- [Scheduling & Events](/scion/hosted/user/scheduling/): time-based one-shot and recurring cron automation.
- [GitHub App Setup](/scion/hosted/single-node/github-app/): registering and installing the Hub's GitHub App.
- [Auth & Tenancy: Permissions and Events for GitHub Triggers](/scion/hosted/single-node/auth/#permissions-and-events-for-github-triggers): GitHub App permissions, mention identity, and routing rules.
- [Experiments Reference](/scion/reference/experiments/#server-experiments-with-notes): enabling `hub.triggers` and `hub.github_triggers`.
