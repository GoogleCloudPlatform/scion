---
title: Templates & Roles
description: Define reusable, harness-agnostic agent roles with Scion templates, compose them with skills, and manage them locally or through a Hub.
---

A **template** defines an agent's *role* — its purpose, instructions, and persona — as a small directory of files you can version, share, and reuse. Templates are **harness-agnostic**: the same `code-reviewer` role can run on Claude, Gemini, or any other supported harness. The harness-specific mechanics (container image, concrete model, auth) live separately in a **harness-config**, so a template stays portable across backends.

Templates work hand-in-hand with **skills** — reusable instruction snippets a template can bundle or pull from the [Skill Bank](/scion/local/skills/). A role is the "who"; skills are the "how-to" capabilities you graft onto it.

:::note
Looking for a real-world set of roles? The [scion-frontiers/agent-team](https://github.com/scion-frontiers/agent-team) repository is a ready-to-use library of templates (`coordinator`, `developer`, `architect`, `code-reviewer`, `doc-writer`, …) and shared skills. It is referenced throughout this page as a practical example.
:::

## What a template is

A template is a directory. Its **name is the directory name** — there is no `name` field to keep in sync. A typical template looks like this:

```text
code-reviewer/
├── scion-agent.yaml     # role configuration (the only file Scion parses)
├── agents.md            # operational instructions for the agent
├── system-prompt.md     # the core persona / role definition
├── home/                # optional: portable dotfiles copied into the agent's home
│   └── .config/
│       └── my-tool.conf
└── skills/              # optional: template-mounted skills (see Skills, below)
    └── review-checklist/
        └── SKILL.md
```

Only `scion-agent.yaml` is required for Scion to recognize the directory as a template. Everything else is referenced *from* that file.

### `scion-agent.yaml`

The config file drives the role. Fields that point at content — `agent_instructions`, `system_prompt` — accept **either a filename** (resolved relative to the template directory) **or inline text**. The convention is to keep prose in separate `.md` files:

```yaml
schema_version: "1"
description: "Senior code reviewer — correctness, readability, security, performance"

agent_instructions: agents.md      # file reference (or inline text)
system_prompt: system-prompt.md

default_harness_config: claude     # harness-config to use when none is given

env:
  REVIEW_STRICTNESS: high

model: large                       # portable size alias — see Model size aliases

skills:
  - uri: "gh://scion-frontiers/agent-team/pr-code-review"

resources:
  requests:
    cpu: "500m"
    memory: "512Mi"
```

Common fields:

| Field | Purpose |
| :--- | :--- |
| `schema_version` | Schema marker; use `"1"`. |
| `description` | Human-readable summary of the role. |
| `agent_instructions` | Path to (or inline) operational instructions, mounted as the harness's agent instructions file. |
| `system_prompt` | Path to (or inline) the system prompt / persona. |
| `default_harness_config` | The harness-config to use when the user does not pass one. |
| `model` | Model **size alias** (`small`, `medium`, `large`, `extra-large`) — see [Model size aliases](#model-size-aliases). |
| `skills` | Skill references to resolve at provision time — see [Skills](#skills). |
| `env` | Environment variables for the agent. |
| `resources` | CPU/memory requests and limits. |
| `mcp_servers` | MCP server definitions, translated per harness. |
| `secrets` | Secrets the agent requires. |
| `volumes`, `services`, `max_turns`, `max_duration`, `telemetry`, `kubernetes` | Further runtime knobs. |

:::tip
Top-level keys accept **either hyphens or underscores** — `default-harness-config` and `default_harness_config` are equivalent. Underscored form is canonical.
:::

:::caution
Keep harness-specific settings **out** of the template. `image`, `auth_selectedType`, and concrete `model` names (e.g. `opus`, `gemini-pro`) are still accepted for backward compatibility but emit a deprecation warning — they belong in a [harness-config](#harness-configs) so the template stays portable. The legacy `harness` field is rejected outright; use `default_harness_config` or the `--harness-config` flag instead.
:::

## Creating an agent from a template

You reference a template by name (or by path/URI) with the `--type` / `-t` flag:

```bash
# Provision and launch an agent from the code-reviewer template
scion start my-review my task... --type code-reviewer

# Explicitly choose the harness-config as well
scion start my-review --type code-reviewer --harness-config gemini

# Provision without starting (writes the agent dir + prompt.md for later)
scion create my-review my task... --type code-reviewer
```

`--type` also accepts an **absolute path** or a **remote URI**, so you can run a template straight from a repository without importing it first:

```bash
# Run a role directly from GitHub (deep path into the agent-team repo)
scion start coord --type gh://scion-frontiers/agent-team/templates/coordinator

# Run from a locally cloned checkout
scion start coord --type ./templates/coordinator
```

### Which harness-config is used

When you do not pass `--harness-config`, Scion resolves it in this order:

1. **CLI flag** — `--harness-config` (alias `--harness`).
2. **Template default** — `default_harness_config` in the template's `scion-agent.yaml`.
3. **System default** — `default_harness_config` in the global `settings.yaml`.

## Template composition & inheritance

Every non-`default` template automatically **inherits the `default` template as a base layer**. When an agent is provisioned, Scion builds a chain — `default` first, then your template — and layers them:

- **Config** is merged field-by-field (`MergeScionConfig`): your template's values override the base, maps like `env` and `mcp_servers` are unioned, and `skills` are **appended** (so base skills are kept and yours are added).
- **Content references** (`agents.md`, `system-prompt.md`) are resolved by walking the chain from most-specific to least-specific, so a file inherited from the base is still found even if your template doesn't redefine it.
- **`home/` files** from the base are copied first, then overlaid by your template's `home/`.

This is why a minimal template only needs a `scion-agent.yaml` plus the files it wants to override — common dotfiles and defaults come from `default` for free.

The final composition layers on top of the chosen harness-config:

1. **Harness-config base layer** — runtime mechanics (image, model aliases, auth, base tool files).
2. **Template overlay** — the role (config, prompts, instructions, skills).
3. **Runtime overrides** — CLI flags and per-agent tweaks.

## Skills

Templates and skills are complementary: a template gives an agent its role, and skills give it reusable, harness-agnostic capabilities that are mounted into the harness's skills directory at provision time. There are **two ways** a template delivers skills.

### 1. Template-mounted skills

Commit skill folders directly inside the template's `skills/` directory. They travel with the template and need no Hub:

```text
web-builder/
├── scion-agent.yaml
├── agents.md
└── skills/
    ├── gcs-static-site/
    │   └── SKILL.md
    └── gcs-auth-proxy/
        ├── SKILL.md
        └── scripts/
            └── main.go
```

When the template chain is provisioned, Scion collects the skills from each template and mounts them into the harness-specific location:

| Harness | Skills directory |
| :--- | :--- |
| Claude | `.claude/skills/` |
| Gemini | `.gemini/skills/` |

When multiple templates are chained, skills from later templates overlay earlier ones.

### 2. Skill Bank references

Instead of bundling files, declare skills by **URI** in the `skills:` list. These are resolved at provision time from the [Skill Bank](/scion/local/skills/) (a Hub-backed registry) or a federated source such as GitHub:

```yaml
# templates/coordinator/scion-agent.yaml
schema_version: "1"
description: "Project coordinator — delegates implementation, drives progress"
agent_instructions: agents.md
system_prompt: system-prompt.md

skills:
  - uri: "gh://scion-frontiers/agent-team/agent-recovery"
  - uri: deploy-checklist                       # bare name → latest, default search order
  - uri: "skill://scion/global/notes@^1.2"      # semver range
  - uri: "gh://my-org/skills/linting"           # federated GitHub source
    as: lint-rules                              # mount under a different name
  - uri: "skill://scion/user/alice/scratch"
    optional: true                              # don't fail provisioning if unresolved
```

Each entry supports `uri` (required), `as` (mount under a different name), and `optional` (continue provisioning if it can't be resolved). For the full URI grammar, scopes, versioning, and the `scion skills` publishing workflow, see [Skills — Authoring & Publishing](/scion/local/skills/) and [Skill Registry & Federation](/scion/hosted/single-node/skill-registry/).

:::tip
A skill supplied by a template always takes precedence over a **platform skill** (one embedded in the Scion binary and injected automatically) of the same name.
:::

### The `team-creation` skill

Scion ships a built-in **`team-creation`** skill for generating coordinated multi-agent template sets. It scaffolds orchestrator-worker patterns with best-practice guidance for agent-to-agent communication and template structure — a fast way to bootstrap a whole team of roles rather than hand-writing each template.

## Built-in vs custom templates

- **`default`** — the built-in template shipped inside the Scion binary. It seeds common `home/` dotfiles and the baseline `agents.md` status-signaling instructions, and it is the base layer every other template inherits. It is **protected** — it cannot be deleted. Use `scion templates update-default` to refresh it from the binary (`--force` to overwrite an existing copy).
- **Custom templates** — anything you create, clone, or import. They live in one of two scopes.

### Template locations & resolution order

| Scope | Location |
| :--- | :--- |
| **Project** | `.scion/templates/<name>/` (in the current project) |
| **Global** | `~/.scion/templates/<name>/` (your machine) |

When you reference a template by bare name, Scion searches in this order and uses the first match:

1. **Remote URI** (GitHub URL / deep path, archive, or rclone) — fetched and cached.
2. **Absolute path** to a template directory.
3. **Project** templates (`.scion/templates/`).
4. **Global** templates (`~/.scion/templates/`).

When connected to a Hub, templates can also resolve from the Hub (project scope, then global scope). If a name matches in several locations, the CLI prompts you to choose; the `--local`, `--hub`, and `--global` flags narrow the search.

## The `scion templates` CLI

The command group is `scion templates` (singular `scion template` is an accepted alias).

### Local management

```bash
# List available templates (local, and Hub when connected), grouped by scope
scion templates list

# Show a template's resolved configuration
scion templates show code-reviewer

# Create a new template (seeded from the default template)
scion templates create my-new-role

# Create in global scope instead of the project
scion --global templates create my-new-role

# Clone an existing template (local or Hub source) to a new local one
scion templates clone code-reviewer my-custom-reviewer

# Delete a template (alias: rm)
scion templates delete my-old-role

# Refresh the built-in default template from the binary
scion templates update-default --force
```

### Importing existing agents

`scion templates import` converts agent definitions into Scion templates and writes them into your project (or global) templates directory. It understands Claude Code sub-agents (`.claude/agents/*.md`), Gemini CLI agents (`.gemini/agents/*.md`), and existing Scion templates (copied directly, no conversion):

```bash
# Import a single Claude sub-agent
scion templates import .claude/agents/code-reviewer.md

# Auto-discover and import everything under a project root
scion templates import --all .

# Import Scion templates from another repo (deep GitHub path)
scion templates import --all https://github.com/scion-frontiers/agent-team/tree/main/templates

# Preview without writing
scion templates import --dry-run .claude/agents/code-reviewer.md
```

Useful flags: `--all` (import every discovered agent), `--harness`/`-H` (force `claude`/`gemini` detection), `--name` (rename a single import), `--force` (overwrite), `--dry-run` (preview).

### Hub commands

When a Hub is enabled, these commands move templates between the local filesystem and the Hub:

```bash
# Upload a local template to the Hub (creates or updates; only changed files upload)
scion templates sync code-reviewer          # alias: scion templates push

# Sync every local project template at once
scion templates sync --all

# Sync to global scope, or under a different Hub name
scion --global templates sync code-reviewer
scion templates sync code-reviewer --name team-reviewer

# Download a Hub template to the local filesystem
scion templates pull code-reviewer --to .scion/templates/code-reviewer

# Compare local vs Hub (synced / out of date / local-only / hub-only)
scion templates status
```

`sync` is content-aware: it hashes files and uploads only what changed, and templates carry a content hash for traceability (visible in `scion templates list`, `scion templates show`, and the Web UI). Beyond the CLI, a connected Hub can import a whole repository of templates server-side via the **Load Templates** action in the Web UI, and imported templates can be browsed and edited directly in the dashboard.

For the condensed command list, see the [CLI reference](/scion/reference/cli/#template-management).

## Harness-configs

A **harness-config** is the counterpart to a template: it holds the runtime *mechanics* a template deliberately omits, so roles stay harness-agnostic. Harness-configs live in `~/.scion/harness-configs/` (global) or `.scion/harness-configs/` (project) and contain:

- `config.yaml` — runtime parameters (container image, model aliases, auth type).
- `home/` — base files copied into the agent's home directory (e.g. `.claude.json`, `.gemini/settings.json`).

### Model size aliases

Templates should express model choice with abstract **size aliases** — `small`, `medium`, `large`, `extra-large` (`xl`) — rather than provider model names. Each harness-config maps those aliases to real models:

```yaml
# ~/.scion/harness-configs/claude/config.yaml
harness: claude
image: scion-claude:latest
model_aliases:
  small: haiku
  medium: sonnet
  large: opus
```

```yaml
# ~/.scion/harness-configs/gemini/config.yaml
harness: gemini
image: scion-gemini:latest
model_aliases:
  small: gemini-flash-lite
  medium: gemini-flash
  large: gemini-pro
```

A template that sets `model: large` then resolves to `opus` under Claude and `gemini-pro` under Gemini — the same role, portable across harnesses. Concrete model names still pass through unchanged (for backward compatibility) but tie the template to one harness.

### Customizing and creating variants

Edit the files in a harness-config directory to change defaults (for example, remap `large` to a newer model, or add a persistent CLI flag in `home/.gemini/settings.json`). Copy the directory to make a variant:

```bash
cp -r ~/.scion/harness-configs/gemini ~/.scion/harness-configs/gemini-experimental
scion start test-agent --type default --harness-config gemini-experimental
```

If you break a stock harness-config, restore the factory defaults:

```bash
scion harness-config reset gemini
```

## Sharing templates

A template is just a directory, so sharing is straightforward:

- **Version it in a repo.** Commit templates under `.scion/templates/` (or a dedicated repo like [scion-frontiers/agent-team](https://github.com/scion-frontiers/agent-team)) and reference them by `gh://` URI or import them with `scion templates import`.
- **Publish to a Hub.** `scion templates sync` uploads a template so teammates on the same Hub can create agents from it by name.
- **Bundle capabilities as skills.** Pull shared behavior out into skills — either template-mounted or published to the [Skill Bank](/scion/local/skills/) — so multiple roles can reuse them.

## See also

- [Skills — Authoring & Publishing](/scion/local/skills/) — author, version, and publish reusable skills.
- [Skill Registry & Federation](/scion/hosted/single-node/skill-registry/) — Hub-side registry administration and external sources.
- [Scion CLI Reference](/scion/reference/cli/#template-management) — the full `scion templates` command list.
- [scion-frontiers/agent-team](https://github.com/scion-frontiers/agent-team) — a real-world library of roles and skills.
