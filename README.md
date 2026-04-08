# Scion

Run multiple agents in parallel — each in its own container, with its own workspace, collaborating on your code or project files simultaneously.

_sci·on /ˈsīən/ — a young shoot or twig, cut for grafting or rooting._

Scion is an experimental multi-agent orchestration testbed designed to manage "deep agents" running in containers.


Scion orchestrates "deep agents" (Claude Code, Gemini CLI, Codex, and others) as isolated, concurrent processes. Each agent gets its own container, git worktree, and credentials — so they can work on different parts of your project without stepping on each other. Agents run locally, on remote VMs, or across Kubernetes clusters.

Rather than prescribing rigid orchestration patterns, Scion takes a "less is more" approach: agents dynamically learn a CLI tool, letting the models themselves decide how to coordinate among agents. This makes it a rapid prototype testbed for experimenting with multi-agent patterns through natural language prompting. Read more in [Philosophy](https://googlecloudplatform.github.io/scion/philosophy/).


## See It in Action

[Relics of Athenaeum](https://github.com/ptone/scion-athenaeum) is an "agent game" that demonstrates multi-agent orchestration defined entirely in markdown. A group of agents collaborate to solve computational puzzles, coordinating through group and direct messaging — all running in containers on off-the-shelf harnesses.

<a href="https://github.com/ptone/scion-athenaeum"><img width="425" height="238" alt="Relics of Athenaeum" src="https://github.com/user-attachments/assets/cbee74a3-f3aa-4739-b423-0a83d5dd4c13" /></a>&nbsp;<a href="https://www.youtube.com/watch?v=w16bsh6lFL8"><img width="300" height="200" alt="Visualization of agent coordination" src="https://github.com/user-attachments/assets/a615da24-33d8-4882-abe1-95adea4ed79a" /></a>

the visualization above replays the actual telemetry collected from messages and file access in the shared workspace while the agents solved the challenges of the game. While this is a "game", the same the same proccess of team definition works for software engineering, data research, and platform engineering workflows.

## Quick Start

Sadly - as an open source project we are not yet able to provide pre-built binaries or containers. You will need to [build images](https://googlecloudplatform.github.io/scion/getting-started/install/#build-container-images) first.

### Install

See the full [Installation Guide](https://googlecloudplatform.github.io/scion/getting-started/install/), or install from source, requires golang:

```bash
go install github.com/GoogleCloudPlatform/scion/cmd/scion@latest
```

If you've cloned the repo and want the **web UI** included, use `make all` (not `make build`):

```bash
make all    # builds web frontend + Go binary with embedded assets
```

> **Note:** `make build` compiles only the Go binary without web assets. The web UI will return 404s if you use `make build` alone. Use `make all` for a fully functional server with the browser-based frontend.

### Build Container Images

Fork the repo and use the **Build Scion Images** GitHub Actions workflow, or build locally.

**GitHub Actions:** Go to Actions > "Build Scion Images" > Run workflow. Fill in:
- **Container registry**: `ghcr.io/<your-github-username>` (e.g., `ghcr.io/myuser`)
- **Build target**: `all` (required for first-time builds — `common` assumes `core-base` already exists in your registry)
- **Image tag**: `latest`
- **Target platform(s)**: Match your machine architecture (see note below)

> **Important — Apple Silicon users:** GitHub Actions runners are x86_64. Building `linux/arm64` images on these runners uses QEMU emulation and can take **over an hour**. For much faster builds, build locally instead:
>
> ```bash
> # Log in to GHCR
> gh auth token | docker login ghcr.io -u <your-username> --password-stdin
>
> # Build and push (minutes on native arm64 vs. 1hr+ on GitHub Actions)
> image-build/scripts/build-images.sh \
>   --registry ghcr.io/<your-username> \
>   --target all \
>   --tag latest \
>   --platform linux/arm64 \
>   --push
> ```

### Initialize your machine and a Grove (project)

Navigate to your project and create a Scion grove (the `.scion` directory that holds agent config) - use the registry where you built images:

```bash
scion init --machine
scion config set --global image_registry ghcr.io/<your-username>
cd my-project
scion init
```

> **Tip:** Add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees.

Scion auto-detects your OS and configures the default runtime (Docker on Linux/Windows, Container on macOS). Override this in `.scion/settings.json`.

**NOTE** Currently this project is early and experimental. Most of the concepts are settled in, but many features may not be fully implemented, anything might break or change and the future is not set. Local use is relatively stable, Hub based workflows now highly usable, Kubernetes runtime support still has rough edges.

### Start Agents

```bash
# Start and immediately attach to the session
scion start debug "Help me debug this error" --attach
```

### Manage Agents

| Command | Description |
|---------|-------------|
| `scion list` (`ps`) | List active agents |
| `scion attach <name>` | Attach to a running agent's tmux session |
| `scion message <name> "..."` (`msg`) | Send a message to a running agent |
| `scion logs <name>` | View agent logs |
| `scion stop <name>` | Stop an agent |
| `scion resume <name>` | Resume a stopped agent |
| `scion delete <name>` | Remove agent, container, and worktree |

## Key Features

- **Harness Agnostic** — Works with Gemini CLI, Claude Code, OpenCode, and Codex. Adaptable to anything that runs in a container.
- **True Isolation** — Each agent runs in its own container with separated credentials, config, and a dedicated `git worktree`, preventing merge conflicts.
- **Parallel Execution** — Run multiple agents concurrently as fully independent processes, locally or remotely.
- **Attach / Detach** — Agents run in `tmux` sessions for background operation. Attach for human-in-the-loop interaction, enqueue messages while detached, and tunnel into remote agents securely.
- **Specialization via Templates** — Define agent roles ("Security Auditor", "QA Tester") with custom system prompts and skill sets. See [Templates](https://googlecloudplatform.github.io/scion/advanced-local/templates/).
- **Multi-Runtime** — Manage execution across Docker, Podman, Apple containers, and Kubernetes via named profiles.
- **Observability** — Normalized OTEL telemetry across harnesses for logging and metrics across agent swarms.

## Core Concepts

| Concept | Description |
|---------|-------------|
| **Agent** | A containerized process running a deep agent harness (Claude Code, Gemini CLI, etc.) |
| **Grove** | A project namespace and collection of agents, commonly 1:1 with a git repo |
| **Template** | An agent blueprint — system prompt plus a collection of skills |
| **Runtime** | A container runtime: Docker, Podman, Apple Container, or Kubernetes |
| **Hub** | Optional central control plane for multi-machine orchestration |
| **Runtime Broker** | A machine (laptop or VM) offering its runtimes to a Hub |

Not all concepts apply in every scenario — local mode is simpler. See [Concepts](https://googlecloudplatform.github.io/scion/concepts/) for the full picture.

## Workstation Server with Tailscale

The [workstation server](https://googlecloudplatform.github.io/scion/advanced-local/workstation-server/) runs Hub, Runtime Broker, and the web frontend as a single daemon. Combined with [Tailscale](https://tailscale.com/), you can orchestrate agents from any device on your tailnet.

```bash
# Start the server (binds to localhost by default)
scion server start

# Expose via Tailscale (HTTPS + your tailnet hostname, no config changes needed)
tailscale serve 8080
```

Access the web UI at `https://<your-tailscale-hostname>:8080/` from any device on your tailnet.

This approach keeps scion bound to loopback with its defaults — Tailscale handles network exposure and TLS termination. No need to use `--host 0.0.0.0`.

Other useful server commands:

| Command | Description |
|---------|-------------|
| `scion server status` | Check component health and daemon PID |
| `scion server stop` | Stop the daemon |
| `scion server restart` | Restart (picks up new binary after `make all`) |
| `scion server install` | Generate a launchd (macOS) or systemd (Linux) service file |

## Documentation

Visit our **[Documentation Site](https://googlecloudplatform.github.io/scion/)** for comprehensive guides and reference.

- **[Overview](https://googlecloudplatform.github.io/scion/overview/)**: Introduction to Scion.
- **[Installation](https://googlecloudplatform.github.io/scion/getting-started/install/)**: How to get Scion up and running.
- **[Concepts](https://googlecloudplatform.github.io/scion/concepts/)**: Understanding Agents, Groves, Harnesses, and Runtimes.
- **[CLI Reference](https://googlecloudplatform.github.io/scion/reference/cli/)**: Comprehensive guide to all Scion commands.
- **Guides**:
    - [Using Templates](https://googlecloudplatform.github.io/scion/advanced-local/templates/)
    - [Using Tmux](https://googlecloudplatform.github.io/scion/advanced-local/tmux/)
    - [Kubernetes Runtime](https://googlecloudplatform.github.io/scion/hub-admin/kubernetes/)

## Project Status

This project is early and experimental. Core concepts are settled, but expect rough edges:

- **Local mode** — relatively stable
- **Hub-based workflows** — ~80% verified
- **Kubernetes runtime** — early, with known rough edges

## Disclaimers

This is not an officially supported Google product. This project is not eligible for the [Google Open Source Software Vulnerability Rewards Program](https://bughunters.google.com/open-source-security).

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).