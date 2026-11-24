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
| `mode` | enum | `project`, `branch`, `lineage`, `none`, `can_message`, `cannot_message` | client | `mode:lineage`, `mode:can_message` | Messaging mode or reachability (client-side) |

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
| `mode` | enum | `project`, `branch`, `lineage`, `none` | client | `mode:lineage` | Messaging mode (client-side) |

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
