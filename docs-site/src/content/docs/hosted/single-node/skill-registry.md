---
title: Skill Registry & Federation
description: Administer the Hub skill registry and federate skills from GitHub, GCP Vertex AI, and external registries.
---

The Hub is the authoritative **Skill Registry** for the Skill Bank: it stores published skills and their versions, resolves skill reference URIs, and can **federate** resolution to external sources. This page is for Hub operators and administrators. If you are authoring or publishing skills, start with [Skills — Authoring & Publishing](/scion/local/skills/).

## Registry storage model

The Hub persists two record types:

- **Skill** — the logical skill: `name`, `slug`, `description`, `tags`, `scope` (`core`, `global`, `project`, `user`) and optional `scope_id`, `visibility`, ownership, and `status` (`active` or `archived`). Skills are unique per `(slug, scope, scope_id)`.
- **SkillVersion** — an immutable release of a skill: `version` (semver), `status` (`draft`, `published`, `deprecated`, `archived`), a `sha256:` `content_hash`, the file manifest, publisher, download count, and — for deprecated versions — a deprecation message and optional replacement URI.

`scion skills delete` performs a **soft delete**: the skill's status becomes `archived` and its records are retained for audit and history.

The registry is exposed over the Hub REST API under `/api/v1/skills` (list/create, versions, upload/finalize, deprecate, download, and resolve), and administered through the [web UI](#web-ui).

## Federation overview

By default a skill reference resolves against the local Hub. Federation lets the Hub resolve references from **external sources** instead, keyed by the URI scheme or by a named external registry:

| Source | URI form | Notes |
| :--- | :--- | :--- |
| Another Scion Hub / external registry | `skill://<registry-name>/…` | Configured via `scion skills registries`. |
| GitHub repository | `gh://<owner>/<repo>/<path>@<ref>` | Also accepts `https://github.com/…` URLs. |
| GCP Vertex AI | `gcp-skill://<alias>/<skillId>@<version>` | Resolves the alias to a registry endpoint. |

### GitHub source (`gh://`)

Skills can be sourced directly from a GitHub repository path. The resolver uses the GitHub Contents API and caches its resolutions locally; provide a `GITHUB_TOKEN` (or `GH_TOKEN`) in the agent environment for authenticated access and higher rate limits. Requests are retried with exponential backoff, and individual files are capped at 10 MB.

### GCP Vertex AI source (`gcp-skill://`)

The `gcp-skill://<alias>/<skillId>` form resolves the alias to a registered `gcp`-type registry endpoint and fetches the skill using GCP Application Default Credentials (ADC) with the `cloud-platform` scope. Ensure the broker/agent environment has ADC available.

## Managing external registries

External registries are managed with the `scion skills registries` command group (all commands require a Hub connection):

```bash
# List configured registries
scion skills registries list

# Add an external registry (pinned trust by default)
scion skills registries add partner-hub \
  --endpoint https://hub.partner.example.com \
  --type hub \
  --description "Partner skill hub"

# Show, update, or remove a registry
scion skills registries show partner-hub
scion skills registries update partner-hub --trust trusted
scion skills registries update partner-hub --status disabled
scion skills registries remove partner-hub
```

**`add` flags:**

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--endpoint` | *(required)* | Registry endpoint URL (HTTPS). |
| `--trust` | `pinned` | Trust level: `trusted` or `pinned` (see [Trust model](#trust-model)). |
| `--type` | `hub` | Registry type: `hub` (another Scion Hub) or `gcp` (Vertex AI). |
| `--description` | — | Human-readable description. |
| `--auth-token` | — | Bearer token for private registries. Sent as `Authorization: Bearer …`. |
| `--resolve-path` | `/api/v1/skills/resolve` | Custom resolve endpoint path if the registry differs. |

`update` accepts the same flags plus `--status` (`active` or `disabled`). Only the flags you set are changed. A `disabled` registry is skipped during resolution.

## Trust model

Every external registry has a **trust level** that governs whether its content is accepted:

- **`trusted`** — content resolved from the registry is accepted as-is.
- **`pinned`** (default) — only content whose `sha256:` hash has been explicitly **pinned** for a given URI is accepted. If the resolved content's hash does not match the pin, resolution fails with a trust violation. This protects against a compromised or mutated upstream registry.

Pin a hash for a pinned-trust registry:

```bash
scion skills registries pin partner-hub \
  "skill://partner-hub/global/deploy-tool@1.0.0" \
  --hash sha256:9f2b…
```

| Argument / flag | Required | Description |
| :--- | :--- | :--- |
| `<name-or-id>` | Yes | Registry name or ID. |
| `<skill-uri>` | Yes | The skill URI to pin. |
| `--hash` | Yes | The `sha256:` content hash to trust for that URI. |

Pins can also be managed from the [registry admin UI](#web-ui). Obtain the hash to pin with `scion skills resolve <uri>` or `scion skills versions <name>`.

## Web UI

The web dashboard includes both user-facing and admin surfaces for skills:

- **Skills** (`/skills`) — browse and search skills by scope, view details and version history (`/skill-detail`), and create or publish skills (`/skill-create`) subject to your permissions.
- **Skill Registries admin** (`/admin/skill-registries`) — list, create, edit, and remove external registries, toggle their status, and set their trust level. The registry detail page manages the registry's **pinned hashes** (add and remove pins) for pinned-trust registries.

Registry administration is capability-gated; it is available to users with the appropriate admin permissions.

## See also

- [Skills — Authoring & Publishing](/scion/local/skills/) — authoring, publishing, versioning, and the `scion skills` commands.
- [Scion CLI Reference](/scion/reference/cli/#scion-skills) — full command reference including `scion skills registries`.
