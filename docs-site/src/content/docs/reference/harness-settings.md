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

- `--name <name>`: override the derived config name. Without it, the name is the `name` field in
  the source's `config.yaml`, then its `harness` field, then the source directory name.
- `--force`: overwrite an existing config with the same name in the target scope (a local directory,
  or a Hub config in the same scope). Without it, `install` refuses to replace an existing config.

When a Hub is available, `install` registers the config **on the Hub** (project scope by default,
or global with `--global`); otherwise it installs it locally. The output shows the Hub scope the
config was created or updated in. A project-scoped config and a global config with the same name
are separate configs, so check the printed scope to see which one you changed.

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
the **"Refresh from Source"** button on the harness-config detail page, as well as the **"Refresh All from Source"** button on the harness-configs list page in the Web UI (which triggers parallel reimport of all tracked configs with per-row progress indicators). Both require a Hub connection.

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

`sync`/`push` upload only changed files (compared by content hash) and mirror local deletions: a
file removed from the local directory is removed from the Hub, and the removed paths are printed.
Backup and temp files (`*.bak.<timestamp>`, `.*.tmp-*`) are never uploaded, so any left on the Hub
are removed the same way. A directory with no files left to sync is refused. `pull` verifies each
file's hash before writing. Like `install`, `sync`/`push` target the current project's Hub scope by
default, or the global scope with `--global` (which needs hub admin rights). `--global` also reads
the config from the global directory (`~/.scion/harness-configs`), so to publish a config globally
it must live there. They create the config in that scope or update an existing one of the same name,
and print the scope they used.

### Listing, inspecting, and resetting

```bash
scion harness-config list                 # local configs (add --hub to merge in Hub configs)
scion harness-config show <name>          # details (local path/image, or Hub ID/status/source URL)
scion harness-config reset <name>         # restore the global dir to embedded defaults
scion harness-config upgrade [name]       # add missing support files / metadata, refresh provisioner scripts
```

`reset` overwrites a config with the binary's embedded defaults. `upgrade` does not clobber your
config values: it adds newly-required support files and merges missing metadata into
`config.yaml` (use `--dry-run` to preview, `--activate-script` to switch a config to
container-script provisioning, `--force` to override). It does refresh provisioner scripts, as
described below. With no name, `upgrade` processes every config in the global directory.

The provisioner scripts `provision.py`, `scion_harness.py`, and `capture_auth.py` belong to the
harness bundle, not to you:

- **Non-force seeding** (`scion init --machine`, hosted-mode `scion server` start, and Hub
  system-init) writes only to the bundled config directories, such as `harness-configs/claude`.
  It replaces these scripts on every run, the same way it treats `config.yaml`.
- **`upgrade`** replaces them with the bundled copy when they differ, but only in a bundled
  config: one whose directory name matches its harness. It backs up each script as
  `<file>.bak.<timestamp>` first and reports it as a `refresh_file` action.

Either way, provisioner fixes reach nodes that already have the config, and your other files
are kept. Non-force seeding and non-force `upgrade` never touch two kinds of script:

- Scripts in a **custom-named** config, such as one installed with
  `harness-config install --name my-claude`.
- Scripts that are **symlinks**. These are treated as user-managed and skipped (`upgrade`
  reports a `skip_file` action).

The force paths still replace the whole config, and they write through symlinks. These are
`harness-config reset`, `upgrade --force`, and workstation-mode `scion server` start.

To customize a provisioner, publish it as your own harness-config under a different name. Do
not edit the bundled scripts in place.

### Deleting

```bash
scion harness-config delete <name>
```

The CLI `delete` removes the config **from the Hub** (matched by name or slug); it does not delete
your local on-disk files. In the web UI, the delete dialog offers an **"Also delete stored files"**
checkbox to remove the Hub-stored files as well. To remove a local directory, delete it from the
filesystem or reinstall with `--force`.

## Configuration (`config.yaml`)

The `config.yaml` file at the root of a harness-config bundle defines its runtime parameters. Beyond basic fields like `image`, `harness` type, and `model_aliases`, it includes capabilities and launch configurations.

`image` and `image_pull_policy` (the latter is Kubernetes-only; values `Always`, `IfNotPresent`,
`Never`) can also be set — and, since ptone/scion#2156, overridden by an operator — via Hub
settings `harness_configs.<name>.image` / `.image_pull_policy`, without editing the bundle. An
explicit `image` or `kubernetes.imagePullPolicy` in a template or agent config still outranks the
Hub setting, which in turn outranks this file's own default. See [Settings
Precedence](/scion/reference/settings-precedence/#container-image-and-kubernetes-image-pull-policy--a-separate-chain-from-b1)
for the full chain.

### Thinking Level Map (`thinking`)

Scion carries the thinking level as a harness-agnostic integer from 0 to 100 (`--thinking-level` on
`scion start`, Hub agent defaults, templates). `--thinking-level` also accepts the shorthands
`low` (25), `medium` (50), `high` (75) and `max` (100), case-insensitive; they are stored as those
integers. Inside the container it arrives as
`SCION_THINKING_LEVEL`. A harness that honours it declares a `thinking:` block in its
`config.yaml`, next to `model_aliases`. The block maps level ranges to the harness's native tier
strings:

```yaml
thinking:
  # Ordered by ascending `max`. A level L (0-100, after clamping) maps to the
  # `value` of the first entry whose `max` >= L. `max` is an inclusive upper
  # bound. The last entry must have max: 100.
  levels:
    - {max: 25,  value: low}
    - {max: 50,  value: medium}
    - {max: 75,  value: high}
    - {max: 100, value: xhigh}
  # Optional. Used when SCION_THINKING_LEVEL is unset, blank or not an integer.
  # Omit it to emit nothing, so the harness CLI's own default applies.
  default: medium
```

Rules (checked when the harness-config is loaded; a violation fails the load):

- `levels` is required and must have at least one entry.
- Each entry has exactly `max` (an integer from 0 to 100) and `value` (a non-empty string).
- `max` must be strictly ascending, and the last `max` must be `100`.
- `default`, if present, is a non-empty string. It does not have to be one of the `value`s.
- No other keys are allowed.

The block travels to the container in the provision manifest. The bundle's `provision.py`
resolves it with `scion_harness.resolve_thinking(ctx)`, which owns the only parse rule: the level
is stripped, signs are accepted (`-5`, `+7`), and the result is clamped to 0-100. A value that is
not an integer (`abc`, `1.5`) logs a warning and is treated as unset. Writing the resolved value
to the harness's native setting is up to each `provision.py`: for example, codex writes
`model_reasoning_effort` in `~/.codex/config.toml`, antigravity passes `agy --effort`, and claude
sets the `CLAUDE_CODE_EFFORT_LEVEL` environment variable.

A harness with no `thinking:` block ignores the thinking level. Currently only `codex`,
`antigravity` and `claude` declare one; see [Supported Harnesses](/scion/supported-harnesses/) for
their tables. If you maintain a customized `config.yaml` for one of these harnesses, copy the
`thinking:` block from the bundled file, or run `scion harness-config upgrade <name>`, which
merges missing top-level keys such as `thinking:` without overwriting your values. Without the
block, the provisioner writes no thinking setting, so the CLI's own default applies. codex logs
a warning on every start; antigravity and claude log one when a thinking level was requested.

### Command Execution (`command`)

The `command` block defines how the agent's primary LLM tool is invoked.

*   `base`: A list of strings forming the base command to run.
    :::caution[Interactive Terminal Required]
    The tool specified in `command.base` **must** be an interactive application that keeps running and accepts input via the terminal. The Scion runtime manages terminal sessions via a PTY and requires the process to stay alive. Tools that execute a single prompt and exit immediately will cause the agent to terminate prematurely.
    :::
*   `task_flag`: CLI flag used to pass the task (e.g., `--prompt`).
*   `resume_flag`: CLI flag to resume a previous session (e.g., `--session`).
*   `system_prompt_flag`: CLI flag for the system prompt.

### Environment Templates (`env_template`)

The `env_template` block defines a map of environment variables to inject into the container, with support for Go template variable substitution.

:::warning[Host-Side Evaluation]
Templates are evaluated **on the host (broker)** *before* the container starts. They have access to host paths, not container paths.
:::

| Variable | Description |
| :--- | :--- |
| `{{ .AgentName }}` | The name (slug) of the agent being started. |
| `{{ .AgentHome }}` | The absolute path to the agent's home directory on the **host machine**. |

Use `env_template` sparingly to pass host-side paths to the container (e.g., passing a socket path that is bind-mounted). Do **not** use it to reference paths inside the container, as they will evaluate to the host path instead. Use the standard `env` map for static or container-side environment variables.

## Key Concepts
- **Tools**: Allowlists of local or remote functions the LLM is permitted to call.
- **Profiles**: Harness-level profiles (distinct from Scion profiles) that control model parameters.
- **Credentials**: How API keys are injected and stored within the harness-specific configuration. Auth type selection uses universal Scion types (`api-key`, `oauth-token`, `auth-file`, `vertex-ai`) set via `auth_selectedType` in the settings profile. Scion translates these to harness-native values during provisioning (e.g., `api-key` becomes `gemini-api-key` in Gemini's `settings.json`). See [Agent Credentials](/scion/local/agent-credentials/) for the full credential pipeline.
