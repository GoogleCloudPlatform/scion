---
title: Skills — Authoring & Publishing
description: Author, version, and publish reusable agent skills through the Scion Skill Bank.
---

A **skill** is a reusable, harness-agnostic instruction snippet — a folder containing a `SKILL.md` file plus any supporting files — that is mounted into an agent's harness at provisioning time. Skills follow the open [Agent Skills](https://agentskills.io/home) convention.

Scion supports two ways to deliver skills to agents:

- **Template-mounted skills** — skill folders committed inside a template's `skills/` directory. These travel with the template and require no Hub. See [Templates & Roles](/scion/local/templates/#skills).
- **The Skill Bank** — a Hub-backed registry that lets you publish versioned skills once and reference them by URI from any template or agent, with semantic-version resolution, content-hash caching, and federation across external registries.

This page covers the Skill Bank: how to author, publish, version, and consume skills with the `scion skills` command group. For Hub-side registry administration and federation, see [Skill Registry & Federation](/scion/hosted/single-node/skill-registry/).

:::note
The `scion skills` commands (except `create`) talk to a Hub. Make sure a Hub endpoint is configured — see [Connecting to a Hub](/scion/hosted/user/hosted-user/).
:::

## Anatomy of a skill

A skill is a directory whose only required member is a `SKILL.md` file. `SKILL.md` has YAML frontmatter followed by Markdown instructions:

```text
my-skill/
├── SKILL.md          # required: frontmatter + instructions
├── reference.md      # optional: supporting docs
└── scripts/
    └── helper.sh     # optional: any supporting files
```

**`SKILL.md` example:**

```markdown
---
name: deploy-checklist
description: Pre-deployment checklist and rollback steps for production services.
---

# Deploy Checklist

1. Confirm the change has an approved PR.
2. Run the smoke-test suite.
3. ...
```

Frontmatter fields:

| Field | Required | Description |
| :--- | :--- | :--- |
| `name` | Yes | Skill name in **kebab-case** (lowercase alphanumeric with hyphens, 1–64 chars, no leading/trailing hyphen). |
| `description` | Yes | A short, human-readable summary shown in listings. |

### Scaffolding a new skill

`scion skills create` scaffolds a local skill directory with a starter `SKILL.md`. It is a purely local operation — nothing is published.

```bash
scion skills create deploy-checklist
# Created skill directory: deploy-checklist/
#   deploy-checklist/SKILL.md  (edit this file)
```

Edit the generated `SKILL.md`, add any supporting files, then [publish](#publishing-a-skill) when ready.

## Referencing skills from an agent

Skills are consumed by declaring them in the `skills:` list of a `scion-agent.yaml` (in a template or an agent config). Each entry references a skill by URI and is resolved at provision time:

```yaml
# scion-agent.yaml
schema_version: "1"
name: release-manager

skills:
  - uri: deploy-checklist              # bare name → latest, default search order
  - uri: skill://scion/global/notes@^1.2   # semver range
  - uri: gh://my-org/skills/linting        # federated GitHub source
    as: lint-rules                          # mount under a different name
  - uri: skill://scion/user/alice/scratch
    optional: true                          # do not fail provisioning if unresolved
```

| Field | Required | Description |
| :--- | :--- | :--- |
| `uri` | Yes | A [skill reference URI](#skill-reference-uris) or bare skill name. |
| `as` | No | Mount the skill under a different directory name. |
| `optional` | No | If `true`, provisioning continues even when the skill cannot be resolved. |

At provisioning time Scion resolves every required skill, downloads its files (using the [content-hash cache](#content-hash-caching)), and mounts them into the harness's skills directory (for example `.claude/skills/` or `.gemini/skills/`).

## Skill reference URIs

A skill reference is either a **bare name** or a full `skill://` URI. Federated sources use their own schemes (`gh://`, `gcp-skill://`).

**Grammar:**

```text
skill://<registry>/<scope>/<scopeId>/<name>@<version>
```

Most segments are optional. Common forms:

| Form | Meaning |
| :--- | :--- |
| `deploy-checklist` | Bare name — default registry (`scion`), scope search order, `latest`. |
| `skill://scion/global/deploy-checklist` | Explicit registry + `global` scope. |
| `skill://scion/project/<projectId>/deploy-checklist` | Project-scoped skill (scope + scope ID). |
| `skill://scion/user/<userId>/scratch@1.4.0` | User-scoped skill, exact version. |
| `skill://project/deploy-checklist` | Scope-alias form (registry defaults to `scion`). |
| `skill://registry.example.com/global/tool@^2.0` | External registry (federation). |
| `gh://<owner>/<repo>/<path>[@<ref>][?token=<SECRET>]` | Skill sourced from a GitHub repository. Supports private repos and per-URI credentials. |
| `gcp-skill://<alias>/<skillId>@<version>` | Skill sourced from GCP Vertex AI. |

### Version specifiers

The `@version` suffix accepts:

- `latest` (the default when omitted) — the highest published, non-deprecated stable version.
- An exact version, e.g. `1.4.0` (a leading `v` is stripped).
- A semver range, e.g. `^1.0`, `~1.2`, `>=1.0.0` — resolves to the highest matching version.
- A content hash, e.g. `sha256:abc123…` — resolves to the exact bytes with that hash.

### GitHub skill URIs (`gh://`)

The `gh://` scheme retrieves skill files directly from a GitHub repository. It works out-of-the-box for public repositories, and supports secure private repository resolution.

#### Private repository resolution

For private repositories, Scion resolves skills using the project's configured git credentials or custom secrets:

1. **Default credentials**: A plain `gh://` URI automatically uses the project's default `GITHUB_TOKEN` (such as a GitHub App installation token or a PAT configured at the project level).
2. **Named credentials (per-URI selection)**: Use the `?token=SECRET_NAME` query parameter on the URI to target a specific project secret:
   ```yaml
   - uri: gh://acme-corp/partner-skills/my-skill@v1.2.3?token=PARTNER_GITHUB_TOKEN
   ```
   Scion retrieves the secret from the `ProvisionCredentials` channel during provisioning. The secret value is processed completely in memory and is **never** forwarded to the agent's container environment or harness scripts.
3. **Local CLI fallback**: In local CLI mode, the resolver automatically falls back to your local `GITHUB_TOKEN` environment variable.

#### Input validation & auto-normalization

To make adding skills as seamless as possible, Scion automatically validates and transforms full GitHub browser URLs to canonical `gh://` shorthand. This is supported in the CLI, Web UI, and Hub API:

- **Browser tree URLs**: `https://github.com/owner/repo/tree/main/skills/my-skill` is normalized to `gh://owner/repo/my-skill@main`.
- **Browser blob URLs**: `https://github.com/owner/repo/blob/main/skills/my-skill/SKILL.md` is normalized to `gh://owner/repo/my-skill@main`.
- **Secret validation**: Any named secret via `?token=SECRET_NAME` is validated to ensure it matches standard environment variable naming (`[A-Z][A-Z0-9_]*`).

:::note
The old `scion://` scheme is no longer supported and is rejected during validation with a clear error pointing to `skill://`.
:::

### Scopes and resolution order

Skills live in one of four **scopes**:

| Scope | Description |
| :--- | :--- |
| `core` | Built-in platform skills. |
| `global` | Shared across all users of the Hub. |
| `project` | Scoped to a specific project (requires a scope ID). |
| `user` | Personal to a specific user (requires a scope ID). |

When a reference does not name a scope, the Hub searches in this order and returns the first match:

```text
user → project → global → core
```

More specific scopes therefore override broader ones — a user's own skill shadows a global skill of the same name.

### Destination-Name Collision Resolution

When multiple skills are resolved for an agent, they are mounted into the harness's skills directory. If multiple skills resolve to the same destination folder name (for example, if they share a name but come from different scopes, or use overlapping `as` aliases), Scion performs a precedence-based deduplication pass to resolve the collision rather than failing.

The precedence hierarchy for resolving destination name collisions is (highest priority wins):

```text
project > template > user > hub > platform
```

- **Project**: Skills explicitly defined at the project scope.
- **Template**: Skills mounted or supplied by the agent's template.
- **User**: Personal/user-scoped skills.
- **Hub**: Global skills published to the Hub's Skill Registry.
- **Platform**: Built-in system or workspace skills injected by Scion.

#### Collision Reporting

When a destination-name collision is detected and resolved:
1. Scion logs the collision and the resolution outcome in the Hub and agent dispatch logs.
2. The collision details and final resolved mapping are recorded in a `resolved-skills.json` file inside the agent's run/state directory.

Each `SkillReference` includes a `Scope` field, annotated at all injection sites, allowing full traceability of where each skill was sourced and how collisions were handled.

## Publishing a skill

`scion skills publish` uploads a local skill directory to the Hub as an immutable, versioned release. A `--version` (valid [SemVer 2.0.0](https://semver.org/)) is required, and the directory must contain a `SKILL.md`.

```bash
# Publish version 1.0.0 (creates the skill if it does not yet exist)
scion skills publish ./deploy-checklist --version 1.0.0

# Publish into a specific scope for a new skill (default is global)
scion skills publish ./deploy-checklist --version 1.0.0 --scope project

# Publish a new version of an existing skill by ID
scion skills publish ./deploy-checklist --version 1.1.0 --skill-id <skill-id>
```

**Flags:**

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--version` | *(required)* | SemVer version to publish (e.g. `1.0.0`). |
| `--scope` | `global` | Scope for a **newly created** skill: `core`, `global`, `project`, or `user`. |
| `--skill-id` | *(auto)* | Publish a new version for an existing skill ID. If omitted, Scion matches by directory name and creates the skill when no match exists. |

**Per-version limits:** at most **50 files**, **10 MB** per file, and **50 MB** total. `.git/`, `.DS_Store`, `__pycache__`, and files matching `.gitignore` patterns are excluded automatically.

On success the command reports the resolved version and its `sha256:` content hash:

```text
Published deploy-checklist v1.0.0 (hash: sha256:9f2b…)
```

## Versioning and immutability

Each publish creates a new `SkillVersion`. Versions are content-addressed by a `sha256:` hash computed over their files, so a given `name@version` always resolves to the same bytes. This makes resolution reproducible and enables the [content-hash cache](#content-hash-caching).

To retire a version without deleting it, deprecate it. Deprecated versions remain resolvable by exact reference but are skipped by `latest` and range resolution.

```bash
scion skills deprecate deploy-checklist \
  --version 1.0.0 \
  --message "Superseded by 2.x" \
  --replacement "skill://scion/global/deploy-checklist@^2.0"
```

| Flag | Required | Description |
| :--- | :--- | :--- |
| `--version` | Yes | The version to deprecate. |
| `--message` | Yes | Message shown to users of the deprecated version. |
| `--replacement` | No | Skill URI users should migrate to. |

## Content-hash caching

Runtime brokers cache resolved skill content on disk, keyed by its `sha256:` content hash, under:

```text
~/.scion/cache/skills/
```

Because the cache is content-addressed, identical content is stored once regardless of how many skills or versions reference it, and a cached entry is reused across agents and re-provisions without re-downloading. The cache is size-bounded (100 MB by default) with least-recently-used eviction.

## Managing skills

```bash
# List skills (optionally filter by scope, search text, or tags)
scion skills list
scion skills list --scope global --search deploy
scion skills list --tags ci,production          # comma-separated, AND semantics

# Show a skill's details and its versions
scion skills show deploy-checklist

# List all versions of a skill
scion skills versions deploy-checklist

# Resolve a URI to a concrete version, hash, and file manifest
scion skills resolve "skill://scion/global/deploy-checklist@^1.0"

# Soft-delete a skill (archived, retained for history)
scion skills delete deploy-checklist        # alias: rm
```

Most commands accept either a skill **name** or **ID**. Add the global `--format json` flag for machine-readable output.

:::tip
`scion skill` (singular) is an alias for `scion skills`.
:::

## Auto-Injected Skills (Multi-Scope)

While you can manually reference skills inside templates or agent configurations, Scion also supports **auto-injection**. This allows you to configure skills that are automatically injected into every provisioned agent, without needing to modify your templates or configurations.

Injected skills are managed at multiple scopes. During agent provisioning, Scion resolves these scopes in order from broadest to most specific. More specific scopes have higher precedence and can override broader ones (lowest to highest precedence):

```text
Hub (System + User-Defined) → User → Project → Template
```

1. **Hub Scope** (broadest): Configured by the Hub administrator for the entire platform. This includes pre-seeded **System** (built-in) skills under the `scion-platform://` URI scheme (such as status signaling and messaging operations) as well as admin-defined global skills.
2. **User Scope**: Configured by you. These skills are automatically injected into every agent you provision, regardless of the project.
3. **Project Scope**: Configured for a specific project. These skills are automatically injected into all agents running within that project.
4. **Template Scope** (highest): Defined directly inside the agent's template. A template-level skill reference always overrides any injected skill of the same name.

### Managing Project-Scoped Injected Skills

Use the `scion project skills` command group to manage skills that should be auto-injected for every agent in a project:

```bash
# List injected skills for the current project (or a specified project)
scion project skills list
scion project skills list my-project

# Add a skill to the project's injected list
scion project skills add skill://scion/global/deploy-checklist@^1.0
scion project skills add my-project skill://scion/global/deploy-checklist@^1.0 --as checklist --optional

# Discover and batch-add all skills from a GitHub directory path
scion project skills add --from-directory https://github.com/my-org/skills/tree/main/production-skills

# Remove a skill by its UUID or full URI
scion project skills remove <uuid>
scion project skills remove skill://scion/global/deploy-checklist@^1.0
```

### Managing User-Scoped Injected Skills

Use the `scion user skills` command group to manage skills that should be auto-injected for every agent you provision, across all of your projects:

```bash
# List your personal injected skills
scion user skills list

# Add a skill to your personal injected list
scion user skills add skill://scion/global/personal-notes@latest
scion user skills add skill://scion/global/personal-notes@latest --as notes --optional

# Discover and batch-add all skills from a GitHub directory path
scion user skills add --from-directory https://github.com/my-org/skills/tree/main/personal-skills

# Remove a skill by its UUID or full URI
scion user skills remove <uuid>
scion user skills remove skill://scion/global/personal-notes@latest
```

### Batch-Adding Injected Skills via the Web UI

In addition to managing injected skills via the CLI, the Scion Web Dashboard allows configuring **Injected Skills** for a project, user profile, or globally on the Hub. To simplify adding multiple skills at once, Scion provides a **directory-discovery** workflow:

1. Under the settings page (Project Settings, User Profile, or Hub Settings), open the **Injected Skills** panel and click **Add Skill**.
2. Paste a GitHub directory URL (e.g., `https://github.com/org/repo/tree/main/skills-directory`) into the **Skill URI** input.
3. If the input matches a directory structure on GitHub, a **Discover Skills from Directory** helper button appears.
4. Clicking it triggers an automated scan of the remote directory. A **checkbox interstitial dialog** will display all discovered skill subdirectories.
5. Select or deselect skills using the checkboxes, then click confirm. 
6. Scion adds all checked skills as individual URI references in a single atomic update.

:::note[Security & Privacy]
Before performing discovery or logging requests, Scion's API automatically strips any embedded userinfo or credentials from the URL, preventing sensitive tokens or usernames from appearing in system logs.
:::

## Platform skills

Beyond skills you publish, **platform skills** are injected automatically at provisioning — a set of skills embedded in the Scion binary and injected into every agent. They provide baseline capabilities without any Hub or template setup.

Platform skills honor an optional `inject_when` frontmatter condition in `SKILL.md`, which gates injection on the agent's environment:

| `inject_when` | Injected when |
| :--- | :--- |
| *(unset)* | Always. |
| `git_workspace` | The workspace is a git repository. |
| `hub_enabled` | The agent is connected to a Hub. |

A skill supplied by a template always takes precedence over a platform skill of the same name.

## See also

- [Skill Registry & Federation](/scion/hosted/single-node/skill-registry/) — Hub-side registry administration, external registries, and trust/pinning.
- [Templates & Roles](/scion/local/templates/#skills) — template-mounted skills.
- [Scion CLI Reference](/scion/reference/cli/#scion-skills) — the full `scion skills` command reference.
