---
title: Filter Queries
description: Unified key:value filter query dialect for Scion web list views.
---

Scion list views share a Gmail/GitHub-style `key:value` filter query dialect.

## Syntax

| Feature | Example | Meaning |
| --- | --- | --- |
| **Pure-text search** | `api-server` | When no recognised `key:` term is present, the trimmed input is passed verbatim to the view's text search (no quote or `-` processing). |
| **Key filter** | `phase:running` | Match rows whose field equals the value (keys and enum values are case-insensitive). |
| **Implicit AND** | `phase:running label:team=ui` | Space-separated terms must all match. |
| **OR within a key** | `phase:error,stopped` | Comma-separated values match any of them. |
| **Negation** | `-phase:stopped`, `-label:tmp` | Leading `-` negates the term (`-k:a,b` means neither `a` nor `b`). |
| **Quoting** | `label:"team=web ui"`, `-"draft notes"` | Use double quotes for values or text containing spaces, commas, or a leading `-`. Only `\"` and `\\` are valid escapes. |
| **Free text + keys** | `phase:running api -test` | Non-key terms join into one positive phrase; `-word` excludes text. |
| **Unknown keys** | `foo:bar` | Treated as plain text with a non-blocking warning in the UI. |

### Reserved syntax and limits

- Bare `OR`, `AND`, parentheses `( )`, and comparison prefixes (`>`, `<`, `>=`, `<=`) are reserved and return a syntax error in structured mode.
- Queries are bounded to 512 bytes, 32 terms, 20 values per key, 200 characters of search text, and 20 distinct reference values across all reference keys (`project`, `broker`, `owner`, `agent`, `conversation`, `by`).
- On syntax or value errors, the web UI keeps the last valid filter applied while displaying the error inline; the Hub API returns `400 invalid_filter_query` with `details.offset` and `details.length` measured in **UTF-8 bytes** into the raw `q` query string.

## Views

### `agents` (Agents list)

Text search matches agent **name** or **slug** (case-insensitive substring). On `GET /api/v1/agents` in sorted mode (`sort=created|updated`), `stats`, the `fit` probe, and `complete: true` rows ignore `phase:` terms in `q`, so a `q=phase:running&fit=N` caller can receive non-running rows when `complete` is `true` and must filter `phase` locally.

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | `created`, `provisioning`, `cloning`, `starting`, `running`, `suspended`, `stopping`, `stopped`, `error`, `halted` | server | `phase:running,error` | Agent lifecycle phase |
| `activity` | enum | `initializing`, `working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `idle`, `completed`, `limits_exceeded`, `stalled`, `offline`, `crashed`, `harness_error` | server | `activity:working,idle` | Agent runtime activity |
| `label` | label | `key` or `key=value` (case-sensitive) | server | `label:team=ui`, `-label:tmp` | Label key or key=value pair |
| `project` | ref | project slug or UUID | server | `project:web-app` | Project slug or ID |
| `harness` | string | harness configuration name (case-insensitive) | server | `harness:claude` | Harness configuration name |
| `broker` | ref | runtime broker name or ID | server | `broker:local-docker` | Runtime broker name or ID |
| `template` | string | template name (case-insensitive) | server | `template:developer` | Agent template name |
| `owner` | ref | user ID, email, or `me` | server | `owner:me` | Owner ID, email, or 'me' |
| `is` | flag_set | `mine`, `shared` (mutually exclusive) | server | `is:mine`, `is:shared` | Scope filter (mine or shared) |
| `suspendPolicy` | enum | `auto`, `never` | server | `suspendPolicy:never` | Automatic suspension policy |
| `mode` | enum | `hub`, `project`, `tree`, `direct`, `owner`, `none`, `can_message`, `cannot_message` | client | `mode:owner`, `mode:can_message` | Messaging mode or reachability (client-side) |

### `project_agents` (Project agents list)

Text search matches agent **name** or **slug** (case-insensitive substring). Scoped to the current project (`project:` and `is:` are not exposed). On `GET /api/v1/projects/:id/agents` in sorted mode, `stats`, the `fit` probe, and `complete: true` rows likewise ignore `phase:` terms in `q`.

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | `created`, `provisioning`, `cloning`, `starting`, `running`, `suspended`, `stopping`, `stopped`, `error`, `halted` | server | `phase:running,error` | Agent lifecycle phase |
| `activity` | enum | `initializing`, `working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `idle`, `completed`, `limits_exceeded`, `stalled`, `offline`, `crashed`, `harness_error` | server | `activity:working` | Agent runtime activity |
| `label` | label | `key` or `key=value` (case-sensitive) | server | `label:team=ui`, `-label:tmp` | Label key or key=value pair |
| `harness` | string | harness configuration name (case-insensitive) | server | `harness:claude` | Harness configuration name |
| `broker` | ref | runtime broker name or ID | server | `broker:local-docker` | Runtime broker name or ID |
| `template` | string | template name (case-insensitive) | server | `template:developer` | Agent template name |
| `owner` | ref | user ID, email, or `me` | server | `owner:me` | Owner ID, email, or 'me' |
| `suspendPolicy` | enum | `auto`, `never` | server | `suspendPolicy:never` | Automatic suspension policy |
| `mode` | enum | `hub`, `project`, `tree`, `direct`, `owner`, `none` | client | `mode:owner` | Messaging mode (client-side) |

### `artifacts` (Artifacts list)

Text search matches artifact **title** or **key** (case-insensitive substring).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `is` | flag_set | `review`, `mine`, `shared`, `draft` (`mine` and `shared` are mutually exclusive; `draft` matches artifacts with an unsubmitted saved draft review by the caller) | server | `is:review`, `is:mine`, `is:draft`, `-is:shared` | Artifact state or access |
| `project` | ref | Project slug or UUID | server | `project:web-app`, `-project:archive` | Home project slug or ID |

### `artifacts-project` (Project artifacts list)

Text search matches artifact **title** or **key** (case-insensitive substring).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `is` | flag_set | `shared` | server | `is:shared`, `-is:shared` | Artifacts shared with this project |

### `decisions` (Project Decisions page)

Text search matches decision **question**, **answer**, **conversation label**, or **agent name** (case-insensitive substring). Pure-text `q` requires at least 2 characters, and all `q` values follow the dialect limits above (max 512 bytes, 200 characters of search text; over-limit queries return `400 invalid_filter_query`). Covers both the **Waiting on you** (Pending) and **Made** tabs; Made-only keys (`by`, `provenance`) are ignored on the Pending tab with a non-blocking warning. Date range bounds (`From` / `To`) remain date inputs alongside the filter box on the Made tab. Inline `agents`, `answerers`, and `conversations` facets cap at 500 entries per list response; when any facet exceeds that cap, the response sets the corresponding flag on `facetsTruncated`, and the filter box queries remote value suggestions (`GET /api/v1/projects/{id}/decisions/suggestions`) only for truncated keys so values past the cap remain discoverable.

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `agent` | ref | Asking agent name or ID (case-insensitive exact name match over readable scope) | server | `agent:deploy-frontend`, `agent:a,b`, `-agent:cleanup-bot` | Asking agent name or ID |
| `conversation` | ref | Conversation label (quote when it contains spaces) or chat key | server | `conversation:#ops`, `conversation:"DM with deploy"`, `-conversation:#general` | Conversation label or chat key |
| `by` | ref | `me`, email, user ID, or display name (quote when it contains spaces); Made tab only | server | `by:me`, `by:ada@example.com`, `by:"Ada Lovelace"`, `-by:me` | Answerer (Made tab only) |
| `provenance` | enum | `direct`, `interpreted`, `confirmed`, `overridden`, `dismissed`; Made tab only | server | `provenance:direct,confirmed`, `-provenance:dismissed` | How it was answered (Made tab only) |

### `profile-decisions` (Profile Decisions page)

Text search matches decision **question**, **answer**, **conversation label**, or **agent name** (case-insensitive substring), with the same `q` limits as `decisions` (min 2 characters for pure text, max 512 bytes, max 200 characters of search text; `400 invalid_filter_query` when exceeded). Exposes the same keys as `decisions` plus `project:`, which resolves strictly within the caller's readable member projects (up to the 200-project scope). Made-only keys (`by`, `provenance`) are ignored on the Pending tab with a non-blocking warning, and `From` / `To` remain date inputs alongside the filter box on the Made tab. As on the project page, inline `agents`, `answerers`, and `conversations` facets cap at 500 entries with `facetsTruncated` signalling truncation; remote suggestions (`GET /api/v1/users/me/decisions/suggestions`) are queried only for truncated `agent`, `conversation`, and `by` keys (`project:` stays local-only).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `agent` | ref | Asking agent name or ID (case-insensitive exact name match over readable scope) | server | `agent:deploy-frontend`, `agent:a,b`, `-agent:cleanup-bot` | Asking agent name or ID |
| `conversation` | ref | Conversation label (quote when it contains spaces) or chat key | server | `conversation:#ops`, `conversation:"DM with deploy"`, `-conversation:#general` | Conversation label or chat key |
| `by` | ref | `me`, email, user ID, or display name (quote when it contains spaces); Made tab only | server | `by:me`, `by:ada@example.com`, `by:"Ada Lovelace"`, `-by:me` | Answerer (Made tab only) |
| `provenance` | enum | `direct`, `interpreted`, `confirmed`, `overridden`, `dismissed`; Made tab only | server | `provenance:direct,confirmed`, `-provenance:dismissed` | How it was answered (Made tab only) |
| `project` | ref | Project slug or ID within the caller's readable member projects | server | `project:web-app`, `project:alpha,beta`, `-project:archive` | Project slug or ID |

### `files` (File browser)

Text search matches file **path** as a case-insensitive regular expression, falling back to an ordered-character fuzzy match. For workspace and shared-directory sources, only the text part is sent to the server (`?q=`); key terms post-filter the merged results on the client. Template and harness-config sources filter entirely on the client. Dotfiles visibility remains controlled by the **show .dot files** toggle.

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `ext` | string | File extension without leading dot (case-insensitive) | client | `ext:ts,tsx`, `-ext:map` | File extension without leading dot (e.g. ts, go) |

### `graph` (Agent graph)

Text search matches agent **name** or **slug** (case-insensitive substring). Non-matching ancestors of matched agents are rendered dimmed as lineage context; all other agents are hidden.

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | `created`, `provisioning`, `cloning`, `starting`, `running`, `suspended`, `stopping`, `stopped`, `error`, `halted` | client | `phase:running` | Agent lifecycle phase |
| `activity` | enum | `initializing`, `working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `idle`, `completed`, `limits_exceeded`, `stalled`, `offline`, `crashed`, `harness_error` | client | `activity:working,blocked` | Current agent activity |
| `label` | label | `key` or `key=value` (case-sensitive) | client | `label:team=ui`, `-label:tmp` | Label key or key=value pair |
| `project` | ref | project slug, name, or ID | client | `project:demo` | Project slug, name, or ID |
| `template` | string | template name | client | `template:developer` | Agent template name |
| `harness` | string | harness config name | client | `harness:antigravity` | Harness config name |
| `mode` | enum | `hub`, `project`, `tree`, `direct`, `owner`, `none`, `can_message`, `cannot_message` | client | `mode:direct`, `mode:can_message` | Messaging mode or messageability |
