---
title: Scion CLI Reference
---

The Scion CLI is the primary interface for managing agents, groves, and server components.

## Global Flags

These flags are available on all commands:

- `-g, --grove <string>`: Grove identifier: path, slug (with Hub), or git URL (with Hub).
- `--global`: Use the global grove (equivalent to `--grove global`).
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
    - `-b, --broadcast`: Send the message to all running agents in the current grove.
    - `-a, --all`: Send the message to all running agents across all groves.
    - `--notify`: Get notified when the target agent(s) respond or reach a terminal state after receiving the message.

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

### `scion grove`

Manages the Scion workspace (Grove).

- `scion grove init`: Initialize a new grove. By default, creates a `.scion` directory in the current directory or the root of the current git repository.
    - Flags:
        - `--global`: Initialize the global grove in the home directory.
        - `--machine`: Perform full machine-level setup (seeds harness-configs, templates, settings).
        - `--image-registry <string>`: Configure the container image registry path (e.g., `ghcr.io/myorg`).
    - **Note:** If you are in a git repository, add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees: `echo ".scion/agents" >> .gitignore`
    - **Hub Integration:** If a Hub endpoint is configured, `init` will prompt to register the new grove with the Hub.
- `scion grove list` (alias `ls`): List all groves known to Scion on this machine, including their type, agent count, status, and workspace path.
- `scion grove prune`: Detect and remove grove configurations whose workspace directories no longer exist. This stops any running containers associated with orphaned groves before cleaning up.
- `scion grove reconnect <new-workspace-path>`: Reconnect a moved workspace to its externalized grove configuration. This fixes groves that show as "orphaned" after being relocated.

### `scion clean`

Removes the scion grove configuration from the current project or global location.

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

### `scion cd-grove`

Open a new shell in the active grove's workspace directory.

**Usage:** `scion cd-grove`

### `scion cdw`

Change directory to the workspace of an agent.

**Usage:** `scion cdw <agent-name>`

### `scion shared-dir`

Manages shared directories for agents within a grove.

- `list`: List shared directories in the current grove.
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
- `sync [--all]`: Sync grove-level templates with the Hub. Use `--all` to sync all templates at once.
- `status`: Show the sync status of templates relative to the Hub.

## Hub Integration

### `scion hub`

Manages connection to and interaction with a Scion Hub.

- `scion hub status`: Show the current Hub connection status.
- `scion hub auth login`: Authenticate against the Hub (opens a browser).
- `scion hub notifications`: Retrieve a list of recent system notifications and agent alerts.
- `scion hub link`: Link the current local grove to the Hub.
- `scion hub unlink`: Unlink the current grove from the Hub locally.
- `scion hub groves`: List all groves registered on the Hub.
- `scion hub brokers`: List all runtime brokers registered on the Hub.
- `scion hub secret`: Manage write-only secrets on the Hub.
    - `set <key> <value>`: Set a secret.
    - `get [key]`: Get secret metadata.
    - `clear <key>`: Remove a secret.
- `scion hub env`: Manage environment variables on the Hub.
    - `set <key>=<value>`: Set a variable.
    - `get [key]`: Get variable values.
    - `clear <key>`: Remove a variable.
- `scion hub grove create <git-url>`: Create a grove from a remote git repository.
    - Flags: `--slug`, `--name`, `--branch`, `--visibility`, `--json`

## Infrastructure

### `scion broker`

Manages the local host as a Runtime Broker.

- `scion broker status`: Show status of the local broker server.
- `scion broker start`: Start the broker server as a background daemon.
- `scion broker stop`: Stop the broker daemon.
- `scion broker register`: Register this host as a Runtime Broker with the Hub.
- `scion broker deregister`: Remove this broker's registration from the Hub.
- `scion broker provide`: Add this broker as a provider for a grove.
- `scion broker withdraw`: Remove this broker as a provider from a grove.

### `scion server`

Manages Scion server components (Hub and Broker).

- `scion server start`: Start one or more server components.
    - Flags: `--enable-hub`, `--enable-runtime-broker`, `--port`, `--db`, `--dev-auth`.

## Image Management

### `scion images`

Commands for building and managing Scion container images.

#### `scion images build`

Builds Scion container images locally using `docker buildx`. Must be run from within a clone of the Scion source repository (or use `--source`).

**Usage:** `scion images build --registry <registry> [flags]`

- **Flags:**
    - `--registry <path>`: **(Required)** Target registry path (e.g., `ghcr.io/myorg`).
    - `--target <target>`: Build target — `common` (scion-base + harnesses), `all` (full rebuild including core-base), `core-base`, or `harnesses`. (default: `common`)
    - `--push`: Push images to the registry after building.
    - `--platform <plat>`: Target platform(s). Use `all` for `linux/amd64,linux/arm64`, or specify directly.
    - `--tag <tag>`: Image tag. (default: `latest`)
    - `--source <path>`: Path to a Scion source repository clone. (default: current directory)

**Example:**
```bash
# From within a clone of the scion repo
scion images build --registry ghcr.io/myorg --push

# With all options
scion images build \
  --registry ghcr.io/myorg \
  --target all \
  --platform all \
  --tag v1.0 \
  --push
```

After building, configure Scion to use the registry:
```bash
scion config set image_registry ghcr.io/myorg
```

## Miscellaneous

### `scion version`

Prints the Scion version information.

**Usage:** `scion version`


