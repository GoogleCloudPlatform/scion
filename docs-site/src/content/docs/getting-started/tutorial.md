---
title: Tutorial
---

This tutorial takes you from a fresh install to your first running agent. Scion runs agents
through a **hub**. On your own machine that hub is the workstation server: one process with the
Hub, a Runtime Broker and the web dashboard, listening on `127.0.0.1`. You start it once, and every
`scion` command in any repository uses it.

Before you start, [install Scion](/scion/getting-started/install/) and a container runtime
(Docker, Podman, or Apple Container).

## 1. Start the local hub

```bash
scion server start
```

The server starts in the background and prints its URL. It also saves the endpoint in your global
settings, so later commands find the hub:

```
Configured hub endpoint: http://127.0.0.1:8080 (run 'scion hub status' to verify)
```

On the first run your browser opens the [Onboarding Wizard](/scion/getting-started/onboarding/) at
`http://127.0.0.1:8080/onboarding`. It sets up your machine: runtime, image registry and harness
images. Its last step, **First workspace**, is optional. You can skip it, because the next step
links your repository from the command line.

If the browser does not open (over SSH, or with `SCION_NO_BROWSER` set), open the URL yourself.

:::caution[macOS with Apple container]
On macOS, Scion picks the Apple `container` runtime first when it is installed (then Podman, then
Docker). With Apple container, agents can reach the hub only through a `sudo` DNS rule that must be re-created after each reboot (or installed as a LaunchDaemon). The wizard's
runtime step shows the command; run it yourself. See
[Apple Container DNS](/scion/local/apple-container/), including how to automate it after reboots. Choosing Podman or Docker in the wizard
avoids this step.
:::

:::note[No image registry yet?]
Without an `image_registry`, the server starts the hub and the web UI but not the Runtime Broker,
and prints how to set the registry. Set it in the wizard (or with
`scion config set --global image_registry <your-registry>`), then run `scion server restart`.
:::

## 2. Initialize your repository

Go to a git repository you want agents to work on and initialize it:

```bash
cd ~/src/my-repo
scion init
```

`scion init` creates the project's `.scion` directory and links the project to the local hub,
without a prompt:

```
Linking project 'my-repo' to the local hub at http://127.0.0.1:8080
```

If the hub is not running, `init` still succeeds and prints a note; the project then links on its
first `scion` command.

## 3. Give agents model access

Agents need credentials for their model provider. On the workstation server, the harness login
files in your home directory (for example after you sign in to Claude Code or Gemini CLI on this
machine) are given to agents automatically; see
[Agent Credentials](/scion/local/agent-credentials/#host-credential-files-on-a-workstation).
Alternatively, export an API key in the shell you run `scion` from:

```bash
# For Claude
export ANTHROPIC_API_KEY="your-api-key"

# For Gemini
export GEMINI_API_KEY="your-api-key"
```

If your repository is **private** on GitHub, agents also need a `GITHUB_TOKEN` with at least
read access to its contents, because each agent clones the repository (next step). Export it the
same way, or store it on the hub:

```bash
export GITHUB_TOKEN="ghp_..."
# or
scion hub secret set --project=my-repo GITHUB_TOKEN ghp_...
```

## 4. Start an agent

In the repository, give the agent a name and a task:

```bash
scion start my-first-agent "Write a python script that prints Hello World"
```

The agent runs in the background in its own container. In a git repository with a remote, it
clones the repository inside its container and works on the branch `scion/my-first-agent`, so
your working directory stays clean. It starts from what is pushed to the remote, not from your
local changes. See [Linked git projects](/scion/local/workspace/#linked-git-projects) for
how agents get their workspace.

## 5. List agents

```bash
scion list
```

The table shows each agent in the project with its template, harness config, Runtime Broker, phase and last activity. The same
agents appear in the [Web Dashboard](/scion/workstation/dashboard/) at `http://127.0.0.1:8080`.

## 6. Check on an agent

See the agent's recent terminal output, or its logs:

```bash
scion look my-first-agent
scion logs my-first-agent
```

If the agent needs your input, attach to its terminal session:

```bash
scion attach my-first-agent
```

To detach and leave the agent running, press `Ctrl-b`, then `d` (see
[Interactive Sessions with Tmux](/scion/local/tmux/)).

You can also start an agent and attach to it in one go:

```bash
scion start --attach my-other-agent
```

## 7. Clean up

When the agent has finished, review its changes. Its work is on the branch
`scion/my-first-agent` in its own clone: ask the agent to push the branch, then merge it or open a
pull request. When you no longer need the agent, delete it:

```bash
scion delete my-first-agent
```

This stops the agent's container and removes its workspace, so push anything you want to keep
first.

## Working from outside the repository

Inside a project directory, commands target that project. To work with the project from anywhere
else, select it once as your current project:

```bash
scion use my-repo
scion list
```

Commands run outside a project directory now target `my-repo` on the local hub. Run `scion use` to
see what is selected, and `scion use --clear` to forget it. See
[`scion use`](/scion/reference/cli/#scion-use).

## Keeping the hub running

The server keeps running in the background until you stop it:

- `scion server status` shows whether it is running; `scion server stop` stops it.
- To start it at login, generate a service file with `scion server install`; see
  [Workstation Server Mode](/scion/workstation/workstation-server/).

When the hub is not running, commands say so:

```
Local scion server is not running. Start it with 'scion server start'.
```

Use `scion --help` and `scion <command> --help` to learn more about each command.

## Next steps

- [Working with Agents](/scion/local/agent-lifecycle/): the full agent lifecycle.
- [Choosing a Mode](/scion/choosing-a-mode/): when to move from the workstation to a shared hub.
- [Setting Up Telegram](/scion/getting-started/telegram/): message your agents from your phone.
- Upgrading from a release that ran agents without a hub? See the
  [migration page](/scion/reference/local-mode-removal/).
