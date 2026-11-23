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
- On syntax or value errors, the web UI keeps the last valid filter applied while displaying the error inline; the Hub API returns `400 invalid_filter_query` with `details.offset` and `details.length`.

## Views

### `agents` (Agents list)

Text search matches agent **name** or **slug** (case-insensitive substring).

| Key | Kind | Values | Execution | Example | Description |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | `created`, `provisioning`, `cloning`, `starting`, `running`, `suspended`, `stopping`, `stopped`, `error`, `halted` | server | `phase:running,error` | Agent lifecycle phase |
| `label` | label | `key` or `key=value` (case-sensitive) | server | `label:team=ui`, `-label:tmp` | Label key or key=value pair |
