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

Starts a new agent or resumes an existing one.

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

Stops a running agent.

**Usage:** `scion stop <agent-name>`

### `scion resume`

Resumes a stopped agent.

**Usage:** `scion resume <agent-name> [flags]`

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

## Hub Integration

### `scion auth`

Manages authentication with a Scion Hub, including User Access Tokens.

- `login`: Authenticate against the Hub (opens a browser).
- `tokens`: Manage Personal Access Tokens (PATs).
    - `list`: List all active tokens.
    - `create <name>`: Create a new token with optional scopes.
    - `revoke <token-id>`: Revoke a specific token.

### `scion hub`

Manages connection to and interaction with a Scion Hub.

- `scion hub auth`: Manage Hub authentication.
    - `login`: Authenticate with Hub server (supports `--provider github`).
    - `logout`: Clear stored credentials.
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

### `scion version`

Prints the Scion version information.

**Usage:** `scion version`


