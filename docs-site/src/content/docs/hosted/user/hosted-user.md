---
title: Connecting to a Hub
description: Connecting your CLI to a Scion Hub to dispatch agents, share projects, and collaborate.
---

**What you will learn**: How to connect your local CLI to a Scion Hub, dispatch agents remotely, use the Web Dashboard, and collaborate with others.

[Hosted mode](/scion/choosing-a-mode/) lets you — or a team — share state, infrastructure, and agent configurations by connecting to a networked Scion Hub. This guide is the **shared user journey** for both hosted tiers ([Single-node](/scion/hosted/single-node/overview/) and [HA](/scion/hosted/ha/overview/)): the connection and dispatch workflow is identical on either.

## Connecting to a Hub

Add the Hub to your hub list under a short nickname, log in, and make it your current Hub:

```bash
scion hub add team https://hub.example.com
scion hub auth login --hub team
scion use --hub team
```

`scion --hub https://hub.example.com hub auth login` also adds a Hub URL that is not in the list yet, with a nickname derived from its host; it does not make it the current Hub. Commands run outside a project directory now target the current Hub. To pick a project on it as well, run `scion use <project>`; then commands such as `scion list` and `scion start` work from any directory without `--project`. See [Working with Several Hubs](/scion/hosted/user/multiple-hubs/) for the hub list, `scion use`, and how the CLI chooses a Hub and project.

**Note:** In workstation mode, a background `scion server start` (the default) adds the local Hub as `local` for you. Authentication is not required there: the CLI uses a machine-specific developer token, and the Hub listens only on localhost.

### Authentication

```bash
scion hub auth login
```

This will open your browser to complete the OAuth flow. The hub URL comes from the root `--hub` flag (a URL or a hub nickname), then the `SCION_HUB_ENDPOINT` or `SCION_HUB_URL` environment variable, then, in a linked directory, the Hub it is linked to, then the current Hub — the same order as every other Hub command. (`--hub-url` still works as a deprecated alias of `--hub`.) A login never writes your settings and never changes the current Hub.

### Working with several Hubs

The CLI keeps a list of Hubs with nicknames, and you pick a current Hub and project per user (a `hub.endpoint` in settings is no longer read, apart from a one-time import into this list):

```bash
scion hub add prod https://scion.yourcompany.com
```

```bash
scion use my-project
```

`scion use <project>` (alias of `scion project use`) checks the known-project list first, then searches the registered Hubs you are logged in to (or only the Hub given with `--hub`, `SCION_HUB_ENDPOINT` or `SCION_HUB_URL`), and asks you to pick one when several have the project. `scion use --hub prod` switches the current Hub, and `scion use` on its own shows the effective Hub and project and where each came from. A nickname works anywhere `--hub` takes a URL, including `scion server` and `scion runtime-broker` commands, except inside agents, which need a URL. A background workstation `scion server start` (with the web server on) adds the local Hub as `local` (`--foreground` does not add it).

The current project applies only outside a scion project directory. Inside one, the current Hub is used only when the project has no link entry. Flags and `SCION_HUB_ENDPOINT` always take precedence, and while `SCION_HUB_URL` is set, link entries and the current Hub are not used; `scion hub link` records the project's Hub in the hub list, so a linked checkout keeps targeting its own Hub after you switch. The list and the current context are stored in `~/.scion/context.yaml`. See [`scion project use`](/scion/reference/cli/#scion-project) and [`scion hub`](/scion/reference/cli/#scion-hub). See [Working with Several Hubs](/scion/hosted/user/multiple-hubs/) for details. `--global` still targets the global project, for example for global settings.

To let a coding agent on your machine (for example Claude Code or Gemini CLI) drive the CLI under a scoped token, see [Using the scion CLI from a coding agent](/scion/hosted/user/coding-agent-cli/).

Commands that take a Hub project reference, such as `scion start -g <hub-project>`, look it up on the Hub the command targets. With no Hub configured, they fail with `No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.`

## Project Linking (Projects)

In a team environment, a **Project** represents a shared project. You link your local directory to a Project on the Hub to share context with your team.

```bash
# Link the current directory to the Hub
scion hub link
```

If this directory is already linked (its Hub project ID names a Hub project of the same name), Scion reports that it is already linked. Otherwise it looks for Hub projects with the same name (the repository name from the Git remote, or the directory name). If it finds any, it asks whether to link to one of them or register a new project; with `--yes` it links to the first match, and `--non-interactive` refuses when more than one matches. If none match, it registers a new project. On the local workstation Hub the link confirmation is skipped (a prompt to pick among matching projects, and the offer to import agents, still appear), and a repository links itself on its first command, so you rarely need `scion hub link` there. The project is linked on the Hub that applies in this directory, usually your current Hub, and the link is recorded in your hub list, so the directory keeps using that Hub after you switch Hubs with `scion use`. With no Hub configured at all, `scion hub link` fails with `No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.`

When a project still has agents from a release that ran them without a Hub, `scion hub link` offers to import them; when a repository links itself on the local workstation Hub, the import runs without asking. An agent whose container is running is adopted as it is. Other agents are imported as created, not started, so the import never restarts dormant agents or re-sends their task. See the [migration page](/scion/reference/local-mode-removal/#what-the-import-does-with-each-agent).

`scion hub unlink` only clears the link for this directory (it sets `hub.linked: false`, removes the Hub project ID, and removes the directory's link entry from your hub list). It does not remove the project or its agents from the Hub; existing agents keep running on their broker and stay reachable with `--project <hub-slug>`. Agent commands in the unlinked directory refuse and point you to `scion hub link`.

### Project Configuration

When linked, your `.scion/settings.yaml` will include the Project ID:

```yaml
hub:
  project_id: "uuid-of-the-project"
```

### Workspace Mode Change for Git Projects

Once a git project is linked to a team (remote) Hub, agents use **HTTPS clone-based provisioning** by default rather than local Git worktrees — even if the broker machine already has the repository on disk — unless the project was created with worktree-per-agent mode (requires git 2.48+ on the broker).

**Exception: the local workstation Hub.** A git project that linking creates on the local workstation Hub is created with workspace mode `worktree-per-agent`, so new agents get a git worktree of your checkout under `.scion/agents/<name>/workspace` (see [Linked git projects](/scion/local/workspace/#linked-git-projects)). A project that already existed on the Hub keeps its mode. This needs git 2.48 or later. If git is older, or the checkout's `origin` does not match the project's repository, agent creation fails with an error instead of falling back to a clone. Agents on a Kubernetes runtime do not use the checkout.

For clone-based provisioning, this means:
- A `GITHUB_TOKEN` with at least **Contents: Read** access is required. Set it as a secret or ensure it is in your local environment:
  ```bash
  scion hub secret set --project=my-project GITHUB_TOKEN ghp_xxxxxxxxxxxx
  ```
- SSH credentials are not used for workspace provisioning.
- The CLI will confirm the clone path when starting agents:
  ```
  Using hub, cloning repo https://github.com/org/repo.git
  ```
- To use git worktrees instead, use a project with the `worktree-per-agent` workspace sharing mode, which is set when the project is created on the Hub.

Clone-per-agent is one of Scion's three **workspace sharing modes** (Shared-plain, Worktree-per-agent, Clone-per-agent). For how each mode provisions the workspace and which apply to Hub-managed projects, see [Workspaces & Sharing Modes](/scion/local/workspaces-and-sharing/) and [About Workspaces](/scion/local/workspace/).

## Using Remote Infrastructure

With the Hub connected, you can dispatch agents to **Runtime Brokers** managed by your team, rather than running them on your local laptop.

### Selecting a Broker
The Hub automatically routes tasks to available brokers. You can tag agents to request specific capabilities (e.g., `gpu-capable`).

### Running agents on your own machine
To run agents on your own machine instead, start the local workstation server (`scion server start`) and point the CLI at it; see [Workstation Server Mode](/scion/workstation/workstation-server/).

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
scion hub secret set --project=my-project GITHUB_TOKEN ghp_xxxxxxxxxxxx
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
scion hub secret set --project=acme-backend GITHUB_TOKEN ghp_xxxxxxxxxxxx

# 3. Start an agent remotely on the project
scion start my-agent --project acme-backend "add user authentication"

# 4. Monitor the agent
scion list --project acme-backend
```

## Collaboration

- **Web Dashboard**: Use the Hub's web interface to view running agents, logs, and status.
- **Remote Attach**: You can attach to a remote agent's terminal session using `scion attach`, tunneling through the Hub. See [Attaching to a remote agent](#attaching-to-a-remote-agent).

## Attaching to a remote agent

`scion attach <agent>` opens the agent's tmux session in your terminal. The CLI opens a WebSocket to the Hub (`/api/v1/agents/<id>/pty`). The Hub relays it over its control channel to the Runtime Broker running the agent, and the broker runs the attach inside the container. `scion start <agent> --attach` and `scion resume <agent> --attach` start or resume the agent, wait until it is running, and then attach the same way.

**Who can attach.** Attaching is the `agent.attach` permission. By default, the agent's owner (the user who created it), users in the agent's ancestry chain, and Hub admins can attach. No built-in project role grants `agent.attach`, so project owners and admins cannot attach to other members' agents. With a [personal access token](/scion/hosted/user/personal-access-tokens/), the token also needs the `agent:attach` scope. Several people can be attached to the same agent at once. They all see the same screen and can all type.

**Detaching.** Press `Ctrl-b`, then `d`. The agent keeps running and you can attach again later. Closing the terminal window also leaves the agent running. On Docker brokers, `Ctrl-\` followed by `Ctrl-^` also ends the attach (see [Interactive Sessions with Tmux](/scion/local/tmux/#basic-operations)).

**A terminal is required.** Attach needs an interactive terminal on both stdin and stdout. From a script or a coding harness it fails at once with a non-zero exit. Use `scion look <agent>` to see the screen and `scion message <agent>` to send input instead.

**Preflight.** Before it first connects, and again before each automatic reconnect, `scion attach` asks the Hub whether it can reach the agent's terminal, through the Runtime Broker or, when the broker's runtime has no attach, through the agent's own session. If the Hub refuses (for example `503`), the command exits with the Hub's reason and does not retry. If the Hub cannot be reached before a reconnect, the reconnect is retried after the usual backoff, up to the same limit of 3 short attempts in a row. When there is no path at all, the message says the agent's runtime has no attach and the agent has no session that serves a terminal.

**Reconnect.** When the Hub closes the session with `4503` (a planned restart, or the Runtime Broker connection dropped), `4504` (a transient failure) or `1011` (an internal error), `scion attach` reconnects by itself, once per close, and the screen redraws. It stops after 3 reconnects in a row whose sessions each ended within a minute; press Ctrl-C during the wait to stop it. For `4503` it waits a random delay of up to 5 seconds first, so many clients closed at once do not all reconnect at the same moment. If that reconnect fails, or the network drops, the Hub restarts without a close message, or the agent's session ends, `scion attach` exits with a message that names the cause and the next command to run. (The web terminal also reconnects on its own.) The messages follow the [PTY close codes](/scion/reference/api/#pty-close-codes):

| Close code | What the CLI tells you | What to do |
| :--- | :--- | :--- |
| `1000` | Nothing; this is a normal detach. | |
| `4410` | The agent's terminal session has ended (the agent exited, or its container stopped or was removed). | Check with `scion list`, then `scion resume <agent> --attach`. |
| `4404` | The Runtime Broker cannot find the agent or its container. | Check with `scion list`. |
| `4503` | The Hub lost its connection to the Runtime Broker, or the session is not ready yet, and the automatic reconnect also failed. | Run `scion attach <agent>` again. |
| `4504` | A transient failure (for example the agent's session was lost), and the automatic reconnect also failed. | Run `scion attach <agent>` again. |
| `1006` | The connection to the Hub dropped without a close message. | Run `scion attach <agent>` again. |
| `1011` | The Hub or the Runtime Broker hit an internal error, and the automatic reconnect also failed. | Run `scion attach <agent>` again. |

Other codes get a generic message that includes the code and its reason, so you can look it up.

**Known limit.** On a Hub that runs several replicas behind a load balancer, attach may fail with `503` if the request reaches a replica that does not hold the broker's control channel. Retrying may reach the right replica.

**Attach through Conduit.** With the `hub.conduit` [experiment](/scion/reference/experiments/) on (alpha, off by default), the Hub can also attach through the agent's own Conduit session, which works from any Hub replica. It takes that path only when the agent's runtime does not support attach and the agent's session offers a terminal. When the agent's runtime supports attach, attach goes through the Runtime Broker's control channel as before, and the multi-replica limit above still applies. When the runtime has no attach and the session offers no terminal, attach is refused. `scion attach` and the web terminal ask the Hub which path to use, so you run them the same way either way. See [`GET /:id/pty`](/scion/reference/api/#agents-apiv1agents) for the path rules and refusal reasons.
