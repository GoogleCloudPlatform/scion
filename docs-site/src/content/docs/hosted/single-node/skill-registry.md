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
| GitHub repository | `gh://<owner>/<repo>/<path>[@<ref>][?token=<SECRET>]` | Sourced directly from GitHub. Supports private repos and browser URLs. |
| GCP Vertex AI | `gcp-skill://<alias>/<skillId>@<version>` | Resolves the alias to a registry endpoint. |

### GitHub source (`gh://`)

Skills can be sourced directly from a GitHub repository path. The resolver uses the GitHub Contents API and caches its resolutions locally. Requests are retried with exponential backoff, and individual files are capped at 10 MB.

#### Authentication & reliability

The GitHub resolver includes several optimizations and fallbacks:

* **Authentication Fallback**: The resolver first searches for a `GITHUB_TOKEN` or `GH_TOKEN` in the environment. If neither is set, it falls back to the project-scoped `GITHUB_TOKEN` provisioned credential on the Hub.
* **Unauthenticated Warning**: If no token can be resolved, the resolver proceeds unauthenticated but emits a warning. Unauthenticated requests are heavily rate-limited by GitHub (60 requests/hour), so providing a token is highly recommended.
* **Full SHA Short-Circuit**: If the requested `@<ref>` is a full 40-character git SHA, the resolver bypasses all GitHub API branch-existence checks and constructs the raw download URL directly, saving rate-limit quota.
* **Cache Diagnostics**: Cache initialization errors are recorded with detailed logging for immediate operator visibility.

#### Resolution caching and performance

To avoid hitting GitHub API rate limits during concurrent agent launches, the Runtime Broker implements a **broker-level singleton resolution cache**:
- **Broker-Level Singleton:** The cache is a long-lived, shared singleton service running within the Runtime Broker daemon, rather than a per-request ephemeral cache.
- **Extended TTLs:** General resolution metadata remains cached for **30 minutes**.
- **Git SHA Resolution (24h TTL):** Once a reference (like a branch or tag name) has been fully resolved to a specific Git commit SHA, the SHA resolution is cached for **24 hours**, bypassing external API queries entirely on subsequent requests.

#### Private repository resolution

For private GitHub repositories, Scion resolves skills securely using your project's configured git credentials or custom secrets:

- **Default credentials**: A bare `gh://` URI automatically uses the project's default `GITHUB_TOKEN` (such as a GitHub App installation token or a Personal Access Token configured at the project level).
- **Named credentials (per-URI selection)**: You can select a specific project secret using the `?token=SECRET_NAME` query parameter on the URI:
  ```text
  gh://acme-corp/partner-skills/my-skill@v1.2.3?token=PARTNER_GITHUB_TOKEN
  ```
  The secret value is fetched during provisioning from the `ProvisionCredentials` channel. It is processed in memory and **never** forwarded to the agent's container environment or harness scripts.

#### Input validation & auto-normalization

To prevent typos, 404s, and duplicate entries, Scion automatically validates and transforms browser URLs to canonical `gh://` shorthand when added via the CLI, Web UI, or Hub API:

- **Browser tree URLs**: `https://github.com/org/repo/tree/main/skills/my-skill` is normalized to `gh://org/repo/my-skill@main`.
- **Browser blob URLs**: `https://github.com/org/repo/blob/main/skills/my-skill/SKILL.md` is normalized to `gh://org/repo/my-skill@main`.
- **Secret validation**: The secret name specified via `?token=SECRET_NAME` must match standard environment variable naming rules (`[A-Z][A-Z0-9_]*`).

:::note
The old `scion://` scheme is no longer supported and is rejected during validation with a clear error pointing to `skill://`.
:::

:::note
For private repository skills, the resolver preserves credentials end-to-end to prevent unauthenticated 404 errors during multi-step downloading. See [Secrets & Environment — Convention-based project secrets](/scion/hosted/user/secrets/#naming-convention) for configuring credentials for external private repositories.
:::

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
