---
title: Connecting to a Hub
description: Connecting your CLI to a Scion Hub to dispatch agents, share projects, and collaborate.
---

**What you will learn**: How to connect your local CLI to a Scion Hub, dispatch agents remotely, use the Web Dashboard, and collaborate with others.

[Hosted mode](/scion/choosing-a-mode/) lets you — or a team — share state, infrastructure, and agent configurations by connecting to a networked Scion Hub. This guide is the **shared user journey** for both hosted tiers ([Single-node](/scion/hosted/single-node/overview/) and [HA](/scion/hosted/ha/overview/)): the connection and dispatch workflow is identical on either.

## Connecting to a Hub

To connect your local CLI to a team Hub, you configure the `hub` section in your `settings.yaml`.

### Configuration

Edit `~/.scion/settings.yaml` (or use `scion config set`):

```yaml
hub:
  enabled: true
  endpoint: "https://scion.yourcompany.com"
  local_only: false
```

**Note:** In workstation mode, this should be `http://localhost:8080`.

### Authentication

**Note:** Authentication is not required in workstation mode, it uses a machine specific developer token, and is only listening on localhost.

Once the endpoint is configured, authenticate your CLI:

```bash
scion hub auth login
```

This will open your browser to complete the OAuth flow.

## Project Linking (Projects)

In a team environment, a **Project** represents a shared project. You link your local directory to a Project on the Hub to share context with your team.

```bash
# Link the current directory to the Hub
scion hub link
```

If the project is already registered (matched by Git remote), Scion will link it automatically. If not, it will prompt you to register a new Project.

### Project Configuration

When linked, your `.scion/settings.yaml` will include the Project ID:

```yaml
hub:
  project_id: "uuid-of-the-project"
```

### Workspace Mode Change for Git Projects

Once a git project is linked to a Hub, **all agents started via the Hub use HTTPS clone-based provisioning** rather than local Git worktrees — even if the broker machine already has the repository on disk.

This means:
- A `GITHUB_TOKEN` with at least **Contents: Read** access is required. Set it as a secret or ensure it is in your local environment:
  ```bash
  scion hub secret set --project my-project GITHUB_TOKEN=ghp_xxxxxxxxxxxx
  ```
- SSH credentials are not used for workspace provisioning when Hub mode is active.
- The CLI will confirm the clone path when starting agents:
  ```
  Using hub, cloning repo https://github.com/org/repo.git
  ```
- To use local worktrees instead, run with `--no-hub` or disable hub integration temporarily.

Clone-per-agent is one of Scion's three **workspace sharing modes** (Shared-plain, Worktree-per-agent, Clone-per-agent). For how each mode provisions the workspace and which apply to Hub-managed projects, see [Workspaces & Sharing Modes](/scion/local/workspaces-and-sharing/) and [About Workspaces](/scion/local/workspace/).

## Using Remote Infrastructure

With the Hub connected, you can dispatch agents to **Runtime Brokers** managed by your team, rather than running them on your local laptop.

### Selecting a Broker
The Hub automatically routes tasks to available brokers. You can tag agents to request specific capabilities (e.g., `gpu-capable`).

### Local Fallback
If you want to temporarily run agents locally even while connected to the Hub, add `--no-hub` to a single command, or set `hub.local_only: true` in your settings to keep the Hub configured but operate locally.

## Shared Secrets & Environment

Teams should manage configuration and secrets centrally on the Hub instead of sharing `.env` files or hardcoding credentials.

```bash
# Set an environment variable for the project
scion hub env set --project API_URL=https://api.staging.example.com

# Set a secret for the project
scion hub secret set --project OPENAI_API_KEY=sk-...
```

Secrets are encrypted and never returned via the API; they are securely injected into agents at runtime by the Runtime Broker.

These can also be managed via the web UI at either the user scope (under the profile) or at the Project scope (under Project settings page)

See the [Secret & Environment Management guide](/scion/hosted/user/secrets/) for details on scoping and projection modes.

## Remote & Hub-Managed Projects

Instead of linking a local directory, you can create projects directly on the Hub. This decouples agent execution from your local machine, allowing for remote-only development.

### Hub-Managed Projects
Hub-Managed projects allow you to create project workspaces without any external Git repository. The Hub manages the workspace files directly, and you can download or ZIP the workspace via the Web Dashboard.

```bash
# Target a Hub-Managed project remotely by its slug:
scion start my-agent --project my-hub-managed-slug "do some work"
```

### Git Projects
You can also create a project directly from a git repository URL. The agent's container will clone the repository at startup.

#### Creating a Project from a Git URL

```bash
scion hub project create https://github.com/org/my-project.git \
  --name "My Project" \
  --slug my-project \
  --branch develop
```

#### Setting Up Authentication

For private repositories, set a `GITHUB_TOKEN` secret on the project. The token needs at minimum **Contents: Read** permission.

```bash
scion hub secret set --project my-project GITHUB_TOKEN=ghp_xxxxxxxxxxxx
```

#### Starting Agents Remotely

Once the project is created, you can start agents targeting the remote project directly using the `--project` flag with the slug or git URL:

```bash
scion start my-agent --project my-project "implement feature X"
```

The agent's container will clone the repository at startup, create a `scion/<agent-name>` branch, and begin working.

### End-to-End Example

```bash
# 1. Create the project from a git URL
scion hub project create https://github.com/acme/backend.git --name "Acme Backend"

# 2. Set the GitHub token for private repo access
scion hub secret set --project acme-backend GITHUB_TOKEN=ghp_xxxxxxxxxxxx

# 3. Start an agent remotely on the project
scion start my-agent --project acme-backend "add user authentication"

# 4. Monitor the agent
scion list --project acme-backend
```

## Collaboration

- **Web Dashboard**: Use the Hub's web interface to view running agents, logs, and status.
- **Remote Attach**: You can attach to a remote agent's terminal session using `scion attach`, tunneling through the Hub.
