---
title: Scion CLI Reference
---

The Scion CLI is the primary interface for managing agents, projects, and server components.

## Global Flags

These flags are available on all commands:

- `-g, --project <string>`: Project identifier: path, slug (with Hub), or git URL (with Hub).
- `--global`: Use the global project (equivalent to `--project global`).
- `-p, --profile <name>`: Configuration profile to use.
- `--format <string>`: Output format (`json` or `plain`).
- `--hub <url>`: Hub API endpoint URL (overrides `SCION_HUB_ENDPOINT`).
- `--no-hub`: Disable Hub integration for this invocation (local-only mode).
- `-y, --yes`: Skip confirmation prompts.
- `--non-interactive`: Full non-interactive mode (implies `--yes`, errors on ambiguous prompts).
- `--debug`: Enable verbose debug output.

## Agent Lifecycle

### `scion start` (or `run`)

Starts a new agent or resumes an existing one. Starting a **suspended** agent
implicitly resumes its harness session (continuing the prior conversation);
starting a **stopped** or **error** agent runs a fresh session. See
[`scion suspend`](#scion-suspend) and [`scion resume`](#scion-resume).

**Usage:** `scion start <agent-name> [task] [flags]`

- **Arguments:**
    - `<agent-name>`: Unique name for the agent instance.
    - `[task]`: (Optional) The initial instruction/task for the agent.
- **Flags:**
    - `-b, --branch <string>`: Target branch for the agent workspace.
    - `-t, --type <string>`: Template to use (default "gemini").
    - `-i, --image <string>`: Override container image.
    - `-a, --attach`: Attach to the agent immediately after starting.
    - `--no-auth`: Disable authentication propagation.
    - `-d, --detached`: Run in detached mode (default true).
    - `--config <path>`: Path to inline agent config file (YAML/JSON) for Just-In-Time (JIT) overrides, or `-` for stdin.
    - `--harness-config <string>`: Named harness configuration to use.
    - `--harness-auth <string>`: Override auth method for the harness (e.g., `api-key`, `vertex-ai`, `auth-file`).
    - `--broker <string>`: Preferred runtime broker ID or name for execution.
    - `--notify`: Get notified via the browser or system when the spawned agent reaches a terminal state.

### `scion stop`

Stops a running agent. This is a graceful shutdown (`SIGTERM`); the agent's
phase becomes `stopped` and the next `start` runs a fresh session.

**Usage:** `scion stop <agent-name>`

### `scion suspend`

Suspends a running agent, preserving its harness session for a later resume.
Unlike `stop`, suspending sets the agent's phase to `suspended`, and the next
`start` (or `resume`) **continues** the prior conversation instead of starting
fresh.

Only running agents can be suspended, and the agent's harness must support
session resume (Claude Code and Gemini CLI do; the generic harness does not —
use `stop` instead). See [Agent Lifecycle](/scion/local/agent-lifecycle/).

**Usage:** `scion suspend <agent-name> [flags]`

- **Flags:**
    - `-a, --all`: Suspend all running agents in the current project. Agents
      whose harness does not support resume are skipped.

### `scion resume`

Resumes an existing agent. For a **suspended** agent, the harness session is
continued (Claude Code receives `--continue`, Gemini CLI `--resume`, etc.). For
a **stopped** agent, there is no session to continue, so a fresh session is
started.

A plain `scion resume <agent-name>` (no task) simply **continues** the prior
session — the agent's original creation task is *not* re-injected. If you pass an
explicit prompt, it is sent as a **new message** on top of the continued
session.

**Usage:** `scion resume <agent-name> [task] [flags]`

- **Flags:**
    - `-a, --attach`: Attach to the agent immediately.

### `scion attach`

Connects to the interactive session of a running agent.

**Usage:** `scion attach <agent-name>`

- **Key Bindings:**
    - `Ctrl+P, Ctrl+Q`: Detach from the session without stopping the agent.

### `scion message` (or `msg`)

Sends a message to a running agent's harness by enqueuing it into its input stream (requires Tmux).

**Usage:** `scion message [agent] <message> [flags]`

- **Arguments:**
    - `[agent]`: The name of the agent (optional if `--broadcast` is used).
    - `<message>`: The text to send to the agent.
- **Flags:**
    - `-i, --interrupt`: Interrupt the harness before sending the message.
    - `-b, --broadcast`: Send the message to all running agents in the current project.
    - `-a, --all`: Send the message to all running agents across all projects.
    - `--notify`: Get notified when the target agent(s) respond or reach a terminal state after receiving the message.

### `scion messages` (aliases: `msgs`, `inbox`)

Manages bidirectional communication and persistent messages sent by agents to humans.

**Usage:** `scion messages [command] [flags]`

- **Commands:**
    - `list` (default): View unread messages.
    - `read <message-id>`: Mark a specific message as read.
    - `read-all`: Mark all messages as read.
- **Flags:**
    - `--agent <string>`: Filter messages by a specific agent.
    - `--all`: Show all messages, including those already marked as read.

### `scion logs`

Displays the logs of an agent.

**Usage:** `scion logs <agent-name> [flags]`

- **Flags:**
    - `-f, --follow`: Stream logs.

### `scion list` (or `ps`)

Lists all agents and their status.

**Usage:** `scion list [flags]`

- **Flags:**
    - `-a, --all`: Show all agents (including stopped ones).
    - `-r, --running`: Filter for active (running) agents.

### `scion delete` (or `rm`)

Deletes an agent, removing its container, home directory, and worktree.

**Usage:** `scion delete <agent-name> [flags]`

- **Flags:**
    - `-b, --preserve-branch`: Preserve the git branch associated with the worktree (default: deleted).
    - `--stopped`: Delete all agents with stopped containers.

### `scion sync`

Synchronizes the agent workspace between the host and the container.

**Usage:** `scion sync [to|from] <agent-name> [flags]`

- **Flags:**
    - `--dry-run`: Preview changes without syncing.
    - `--exclude <glob>`: Exclude files matching the pattern.

### `scion reset-auth`

Injects a fresh Hub token into a **running** agent's container and signals it to reload, without
restarting the agent. Use this to recover an agent whose token expired and cannot self-refresh
(e.g. after a Hub signing-key rotation). Requires a Hub connection. The same action is available
as a **Reset Auth** button in the web UI.

**Usage:** `scion reset-auth <agent-name>`

## Configuration & Workspace

### `scion project`

Manages the Scion workspace (Project).

- `scion project init`: Initialize a new project. By default, creates a `.scion` directory in the current directory or the root of the current git repository.
    - Flags:
        - `--global`: Initialize the global project in the home directory.
        - `--machine`: Perform full machine-level setup (seeds harness-configs, templates, settings).
        - `--image-registry <string>`: Configure the container image registry path (e.g., `ghcr.io/myorg`).
    - **Note:** If you are in a git repository, add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees: `echo ".scion/agents" >> .gitignore`
    - **Hub Integration:** If a Hub endpoint is configured, `init` will prompt to register the new project with the Hub.
- `scion project list` (alias `ls`): List all projects known to Scion on this machine, including their type, agent count, status, and workspace path.
- `scion project prune`: Detect and remove project configurations whose workspace directories no longer exist. This stops any running containers associated with orphaned projects before cleaning up.
- `scion project reconnect <new-workspace-path>`: Reconnect a moved workspace to its externalized project configuration. This fixes projects that show as "orphaned" after being relocated.

### `scion clean`

Removes the scion project configuration from the current project or global location.

**Usage:** `scion clean [flags]`

- **Flags:**
    - `--skip-hub-check`: Skip Hub connectivity check before removing.

### `scion config`

View and modify configuration settings.

- `list`: List all effective settings.
- `get <key>`: Get a specific configuration value.
- `set <key> <value>`: Set a configuration value.
- `validate`: Validate settings files against the schema.
- `migrate`: Migrate configuration to the latest versioned format.
- `dir`: Print the path to the active configuration directory.

### `scion cd-config`

Open a new shell in the active Scion configuration directory.

**Usage:** `scion cd-config`

### `scion cd-project`

Open a new shell in the active project's workspace directory.

**Usage:** `scion cd-project`

### `scion cdw`

Change directory to the workspace of an agent.

**Usage:** `scion cdw <agent-name>`

### `scion shared-dir`

Manages shared directories for agents within a project.

- `list`: List shared directories in the current project.
- `create <name>`: Create a new shared directory.
- `info <name>`: View details about a specific shared directory.
- `remove <name>`: Remove a shared directory (permanently deletes contents).

## Template Management

### `scion templates`

Manages agent templates.

- `list`: List available templates.
- `show <name>`: Show configuration of a template.
- `create <name> [--harness <type>]`: Create a new template.
- `clone <src> <dest>`: Clone a template.
- `delete <name>` (alias `rm`): Delete a template.
- `import <source>`: Import agent definitions (from Claude/Gemini) as templates.
- `update-default`: Update the global default template with the latest from the binary.
    - Flags:
        - `--force`: Overwrite the existing default template if it already exists.
- `sync [--all]`: Sync project-level templates with the Hub. Use `--all` to sync all templates at once.
- `status`: Show the sync status of templates relative to the Hub.

## Skill Bank

### `scion skills`

Manages skills in the Hub skill bank — reusable, versioned instruction snippets referenced by URI (`scion skill`, singular, is an alias). See [Skills — Authoring & Publishing](/scion/local/skills/) for the full guide. All subcommands except `create` require a Hub connection.

- `list`: List available skills.
    - Flags: `--scope <core|global|project|user>`, `--search <text>`, `--tags <a,b>` (comma-separated, AND semantics).
- `show <name-or-id>`: Show a skill's details and versions.
- `create <name>`: Scaffold a new local skill directory with a starter `SKILL.md` (local-only; does not publish).
- `publish <path>`: Publish a local skill directory to the Hub. Limits: 50 files, 10 MB/file, 50 MB total.
    - Flags: `--version <semver>` (required), `--scope <core|global|project|user>` (default `global` for new skills), `--skill-id <id>`.
- `versions <name-or-id>`: List all versions of a skill.
- `resolve <uri>`: Resolve a skill URI to a concrete version, content hash, and file manifest.
- `deprecate <name-or-id>`: Mark a published version as deprecated.
    - Flags: `--version <version>` (required), `--message <text>` (required), `--replacement <uri>`.
- `delete <name-or-id>` (alias `rm`): Soft-delete a skill (archived, retained for history).

### `scion skills registries`

Manages external skill registries for [federation](/scion/hosted/single-node/skill-registry/). Admin operations.

- `list`: List configured registries.
- `add <name>`: Register an external skill registry.
    - Flags: `--endpoint <url>` (required, HTTPS), `--trust <trusted|pinned>` (default `pinned`), `--type <hub|gcp>` (default `hub`), `--description <text>`, `--auth-token <token>`, `--resolve-path <path>`.
- `show <name-or-id>`: Show registry details.
- `update <name-or-id>`: Update a registry. Flags: `--endpoint`, `--trust`, `--status <active|disabled>`, `--description`, `--auth-token`, `--resolve-path` (only changed flags are applied).
- `remove <name-or-id>`: Remove a registry.
- `pin <name-or-id> <skill-uri>`: Pin a content hash for a pinned-trust registry. Flag: `--hash <sha256:...>` (required).

## Harness Configuration

### `scion harness-config` (alias `hc`)

Manages harness-config bundles — the named, versioned definitions of each harness (config,
image, capabilities, auth, and supporting files). See
[Harness-Specific Settings](/scion/reference/harness-settings/#managing-harness-configs) for the
full lifecycle.

- `list`: List local harness-configs. Flags: `--hub` (also include Hub-registered configs).
- `show <name>`: Show config details (local path/image, or Hub ID, image status, and source URL).
- `install <source>`: Install a config from a GitHub URL, local path, rclone URI, or archive. Flags: `--name` (override derived name), `--force` (overwrite existing), `--global` (register at global scope on the Hub).
- `update [name]`: Re-import (refresh) a config from its stored source URL. Flags: `--url <url>` (override/set the stored source URL for one config), `--all` (re-import every config that has a stored source URL). `--url` and `--all` are mutually exclusive; requires a Hub connection.
- `sync <name>` (alias `push`): Upload a local config to the Hub (changed files only). Flags: `--name` (publish under a different Hub name).
- `pull <name>`: Download a config from the Hub. Flags: `--to <path>` (destination; defaults to the global dir).
- `reset <name>`: Restore a config to the binary's embedded defaults.
- `upgrade [name]`: Add missing support files and metadata without clobbering user values. Flags: `--dry-run`, `--activate-script`, `--force`. With no name, upgrades all configs in the global directory.
- `delete <name>`: Delete a config from the Hub (does not remove local files). The web UI additionally offers an "Also delete stored files" option.

## Hub Integration

### `scion hub`

Manages connection to and interaction with a Scion Hub. Authentication lives under `scion hub auth` (there is no top-level `scion auth` command).

- `scion hub auth`: Manage Hub authentication.
    - `login`: Authenticate with Hub server (opens a browser; supports `--no-browser` for device flow and `--provider github`).
    - `logout`: Clear stored credentials.
- `scion hub token`: Manage user access tokens (scoped, revocable bearer tokens for CI/CD and automation).
    - `create`: Create a new token. Flags: `--project`, `--name`, `--scopes`, `--expires`.
    - `list`: List your access tokens.
    - `revoke <token-id>`: Revoke a token (remains visible in listings as revoked).
    - `delete <token-id>`: Permanently delete a token.
- `scion hub status`: Show the current Hub connection status.
- `scion hub notifications`: Retrieve a list of recent system notifications and agent alerts.
- `scion hub link`: Link the current local project to the Hub.
- `scion hub unlink`: Unlink the current project from the Hub locally.
- `scion hub projects`: List all projects registered on the Hub.
- `scion hub brokers`: List all runtime brokers registered on the Hub.
- `scion hub secret`: Manage write-only secrets on the Hub.
    - `set <key> <value>`: Set a secret.
    - `get [key]`: Get secret metadata.
    - `clear <key>`: Remove a secret.
- `scion hub env`: Manage environment variables on the Hub.
    - `set <key>=<value>`: Set a variable.
    - `get [key]`: Get variable values.
    - `clear <key>`: Remove a variable.
- `scion hub project create <git-url>`: Create a project from a remote git repository.
    - Flags: `--slug`, `--name`, `--branch`, `--visibility`, `--json`

## Infrastructure

### `scion broker`

Manages the local host as a Runtime Broker.

- `scion broker status`: Show status of the local broker server.
- `scion broker start`: Start the broker server as a background daemon.
- `scion broker stop`: Stop the broker daemon.
- `scion broker register`: Register this host as a Runtime Broker with the Hub.
- `scion broker deregister`: Remove this broker's registration from the Hub.
- `scion broker provide`: Add this broker as a provider for a project.
- `scion broker withdraw`: Remove this broker as a provider from a project.

### `scion server`

Manages Scion server components (Hub and Broker).

- `scion server start`: Start one or more server components.
    - Flags: `--enable-hub`, `--enable-runtime-broker`, `--port`, `--db`, `--dev-auth`.

## Miscellaneous

### `scion doctor`

Runs host-side diagnostics: checks Git, tmux, the active container runtime, and runtime-specific
health (Docker/Podman daemon, or Kubernetes cluster/namespace/RBAC/CSI access). Supports
`--format json`.

**Usage:** `scion doctor [flags]`

:::note[In-container diagnostics]
A separate **`sciontool doctor`** command runs *inside* an agent container and diagnoses the
agent's own health — environment variables, Hub token (presence/format/expiry), Hub reachability,
token refresh, the GCP metadata server, and the GitHub App token. See
[Harness Authentication](/scion/local/agent-credentials/#diagnostics).
:::

### `scion version`

Prints the Scion version information.

**Usage:** `scion version`


