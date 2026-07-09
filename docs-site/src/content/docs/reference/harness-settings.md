---
title: Harness-Specific Settings
---

This document describes how to configure individual LLM tools and harnesses inside a Scion agent.

## Purpose
While Scion manages the orchestration and execution of containers, the tools running *inside* those containers (like the Gemini CLI or Claude Code) often have their own configuration systems.

## Locations
Each agent has a dedicated "Home" directory that is mounted into the container. Harness-specific settings are typically found in a hidden subdirectory:
- **Gemini**: `/home/gemini/.gemini/settings.json`
- **Claude**: `/home/claude/.claude.json` (or similar)
- **Opencode**: `/home/opencode/opencode.json`

## Seeding from Harness-Configs & Templates
When an agent is created, Scion composes its home directory by layering files from multiple sources:
1.  **Harness-Config**: Base settings for the specific LLM tool (from `~/.scion/harness-configs/<name>/home/`).
2.  **Template**: Role-specific prompts and configuration (from `.scion/templates/<name>/home/`).
3.  **Common Files**: Shared dotfiles like `.tmux.conf` and `.zshrc`.

This multi-layered approach allows you to define a "base" Gemini configuration once, and then overlay different "roles" (like Code Reviewer or Security Auditor) on top of it.

## Managing Harness-Configs

A **harness-config** is a named, versioned bundle that defines a harness: its `config.yaml`
(harness type, container image, capabilities, auth methods, MCP mapping) plus supporting files
(the `home/` directory, `provision.py`, `dialect.yaml`, `capture_auth.py`, and the shared
`scion_harness.py` library). Bundles live in `~/.scion/harness-configs/<name>/` (global) or
`.scion/harness-configs/<name>/` (project-level); project-level configs override global ones with
the same name.

Since all harnesses are now provisioned via container-script (see
[Supported Agent Harnesses](/scion/supported-harnesses/)), harness-configs are the unit you
install, refresh, publish, and delete. Manage them with the `scion harness-config` command group
(alias `hc`) and the Hub's web UI.

### The `name` field

A harness-config's identifier defaults to its **directory name** (e.g. `claude`,
`gemini-experimental`). A `config.yaml` may set an explicit `name:` field to override this; path
separators (`.`, `..`, `/`, `\`) are rejected. On the Hub, each config also has a URL-safe
`slug`, and lookups match either the name or the slug.

### Installing from a source

Add a new harness-config from a URL, local path, or archive:

```bash
# From a GitHub repo (full URL or shorthand), a local directory, an rclone URI, or an archive
scion harness-config install github.com/myorg/scion-harnesses/tree/main/hermes
scion harness-config install file:///path/to/my-harness
```

- `--name <name>`: override the derived config name.
- `--force`: overwrite an existing local directory.

When a Hub is available, `install` registers the config **on the Hub** (project scope by default,
or global with `--global`); otherwise it installs it locally.

### Source-URL tracking and "Refresh from Source"

When a config is installed from a remote source, the Hub records the **`sourceUrl`** it came
from. You can later re-import (refresh) the config from that source:

```bash
# Re-import a single config from its stored source URL
scion harness-config update <name>

# Override / update the stored source URL
scion harness-config update <name> --url github.com/myorg/scion-harnesses/tree/main/hermes

# Re-import every config that has a stored source URL
scion harness-config update --all
```

`--url` and `--all` are mutually exclusive. A config with no stored `sourceUrl` is skipped by
`--all`; for a single config, pass `--url` to supply one. This CLI command is the equivalent of
the **"Refresh from Source"** button on the harness-config detail page in the web UI. Both require
a Hub connection.

### Image status

The Hub tracks the availability of each harness-config's container image in an `imageStatus`
field with one of four values:

| `imageStatus` | Meaning |
| :--- | :--- |
| `unknown` | Not yet checked (or a bare local-only image name that was not found locally). |
| `valid` | The image was found — either in the local daemon or in the remote registry. |
| `invalid` | The remote registry returned "not found" (HTTP 404) for the image. |
| `error` | The image check failed (registry unreachable, unauthorized, etc.). |

The check probes the local container daemon first, then the remote registry. The Hub re-checks
lazily when the status is stale (older than ~5 minutes) and after image pulls; you can also force
a re-check with the **Re-check** button on the detail page.

:::note[Image status vs. the Local/Remote badge]
The web UI additionally shows a **Local/Remote** badge that is derived from the *shape* of the
image reference (a bare name like `scion-claude:latest` is treated as local; a fully-qualified
`registry/path:tag` is treated as remote). This badge is separate from the `imageStatus` values
above.
:::

### Publishing and pulling (Hub)

Share a locally-authored config with your Hub, or pull one down:

```bash
scion harness-config sync <name>          # upload local dir to the Hub (alias: push)
scion harness-config sync <name> --name <hub-name>   # publish under a different Hub name
scion harness-config pull <name>          # download from the Hub to the global dir
scion harness-config pull <name> --to <path>
```

`sync`/`push` upload only changed files (compared by content hash); `pull` verifies each file's
hash before writing.

### Listing, inspecting, and resetting

```bash
scion harness-config list                 # local configs (add --hub to merge in Hub configs)
scion harness-config show <name>          # details (local path/image, or Hub ID/status/source URL)
scion harness-config reset <name>         # restore the global dir to embedded defaults
scion harness-config upgrade [name]       # add missing support files / metadata
```

`reset` overwrites a config with the binary's embedded defaults. `upgrade` is non-destructive: it
adds newly-required support files and merges missing metadata without clobbering your values (use
`--dry-run` to preview, `--activate-script` to switch a config to container-script provisioning,
`--force` to override). With no name, `upgrade` processes every config in the global directory.

### Deleting

```bash
scion harness-config delete <name>
```

The CLI `delete` removes the config **from the Hub** (matched by name or slug); it does not delete
your local on-disk files. In the web UI, the delete dialog offers an **"Also delete stored files"**
checkbox to remove the Hub-stored files as well. To remove a local directory, delete it from the
filesystem or reinstall with `--force`.

## Key Concepts
- **Tools**: Allowlists of local or remote functions the LLM is permitted to call.
- **Profiles**: Harness-level profiles (distinct from Scion profiles) that control model parameters.
- **Credentials**: How API keys are injected and stored within the harness-specific configuration. Auth type selection uses universal Scion types (`api-key`, `auth-file`, `vertex-ai`) set via `auth_selectedType` in the settings profile. Scion translates these to harness-native values during provisioning (e.g., `api-key` becomes `gemini-api-key` in Gemini's `settings.json`). See [Agent Credentials](/scion/local/agent-credentials/) for the full credential pipeline.
