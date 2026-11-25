---
title: Filter Queries
description: Unified key:value filter query dialect for Scion web list views.
---

Scion list views share a Gmail/GitHub-style `key:value` filter query dialect (gated behind the `filters.query_dialect` experiment).

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
- Queries are bounded to 512 bytes, 32 terms, 20 values per key, and 200 characters of search text.
- On syntax or value errors, the web UI keeps the last valid filter applied while displaying the error inline; the Hub API returns `400 invalid_filter_query` with `details.offset` and `details.length` measured in **UTF-8 bytes** into the raw `q` query string.

## Views

### `agents` (Agents list)

Text search matches agent **name** or **slug** (case-insensitive substring).

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
| `mode` | enum | `hub`, `project`, `tree`, `direct`, `owner`, `none`, `can_message`, `cannot_message` | client | `mode:owner`, `mode:can_message` | Messaging mode or reachability (client-side) |

### `project_agents` (Project agents list)

Text search matches agent **name** or **slug** (case-insensitive substring). Scoped to the current project (`project:` and `is:` are not exposed).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | `created`, `provisioning`, `cloning`, `starting`, `running`, `suspended`, `stopping`, `stopped`, `error`, `halted` | server | `phase:running,error` | Agent lifecycle phase |
| `activity` | enum | `initializing`, `working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `idle`, `completed`, `limits_exceeded`, `stalled`, `offline`, `crashed`, `harness_error` | server | `activity:working` | Agent runtime activity |
| `label` | label | `key` or `key=value` (case-sensitive) | server | `label:team=ui`, `-label:tmp` | Label key or key=value pair |
| `harness` | string | harness configuration name (case-insensitive) | server | `harness:claude` | Harness configuration name |
| `broker` | ref | runtime broker name or ID | server | `broker:local-docker` | Runtime broker name or ID |
| `template` | string | template name (case-insensitive) | server | `template:developer` | Agent template name |
| `owner` | ref | user ID, email, or `me` | server | `owner:me` | Owner ID, email, or 'me' |
| `mode` | enum | `hub`, `project`, `tree`, `direct`, `owner`, `none` | client | `mode:owner` | Messaging mode (client-side) |

### `artifacts` (Artifacts list)

Text search matches artifact **title** or **key** (case-insensitive substring).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `is` | flag_set | `review`, `mine`, `shared` (`mine` and `shared` are mutually exclusive) | server | `is:review`, `is:mine`, `-is:shared` | Artifact state or access |
| `project` | ref | Project slug or UUID | server | `project:web-app`, `-project:archive` | Home project slug or ID |

### `artifacts-project` (Project artifacts list)

Text search matches artifact **title** or **key** (case-insensitive substring).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `is` | flag_set | `shared` | server | `is:shared`, `-is:shared` | Artifacts shared with this project |

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
