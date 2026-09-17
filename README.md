# Scion

An open foundation for agent collaboration.

_sci·on /ˈsīən/ — a young shoot or twig, cut for grafting or rooting._

Scion is an open-source orchestration platform for teams of AI agents and the people working with them. Bring your choice of agent harnesses and models, give agents specialized roles, and let them delegate tasks, share findings, and work in parallel — on your laptop or across cloud infrastructure.

As agents take on longer, more complex projects, the challenge becomes how they work together: who owns a task, what context they share, when they ask for help, and how you follow their progress. Scion provides the running environments, communication, identity, and visibility to explore those questions in practice.

Run harnesses such as Claude Code, Gemini CLI, Codex, and OpenCode as independent processes, with per-agent configuration and credentials. Choose shared workspaces or separate Git worktrees and clones to suit the work. Start with a local CLI or the Workstation web UI, then move to a hosted Hub when your team needs shared access and remote execution.

Scion's philosophy is **less is more**. Agents learn the `scion` CLI through skills and help text; you describe responsibilities and collaboration patterns in instructions. Teams can grow, delegate, review, and regroup as a task unfolds. Use Scion as a practical testbed for emerging patterns in agent collaboration, with the freedom to choose your models, tools, and infrastructure. Read more in [Philosophy](https://googlecloudplatform.github.io/scion/philosophy/).


## In Action

Coding harnesses give agents access to files, tools, and a working environment. Scion puts those capabilities to work across software development, research, testing, and creative projects. Internal experiments at Google have included software porting, market research, and product testing, with cloud operations and scientific research under exploration. The projects below show how collaboration patterns can be developed through real tasks and human feedback.

### Scion Films

<img height="200" alt="scion-films-poster" src="https://github.com/user-attachments/assets/51be1c80-4bb3-48ad-90e9-356aeffcb678" />


In [Scion Films](https://films.scion-ai.dev/), agent crews explored filmmaking, where success depends on human judgment as well as technical execution. Viewer feedback and agent retrospectives shaped the team's skills, tools, and collaboration across a series of pilots. An agent documentary crew then used those same practices to tell the story of the experiment.

### Relics of Athenaeum

[Relics of Athenaeum](https://github.com/ptone/scion-athenaeum) is an "agent game" that demonstrates multi-agent orchestration defined entirely in markdown. A group of agents collaborate to solve computational puzzles, coordinating through group and direct messaging — all running in containers on off-the-shelf harnesses.

<a href="https://github.com/ptone/scion-athenaeum"><img height="200" alt="Relics of Athenaeum" src="https://github.com/user-attachments/assets/cbee74a3-f3aa-4739-b423-0a83d5dd4c13" /></a>&nbsp;<a href="https://www.youtube.com/watch?v=w16bsh6lFL8"><img height="200" alt="Visualization of agent coordination" src="https://github.com/user-attachments/assets/a615da24-33d8-4882-abe1-95adea4ed79a" /></a>

The visualization above replays telemetry from messages and shared-workspace file access as the agents solved the game's challenges. The same approach — define responsibilities, exchange findings, inspect what happened, and refine the instructions — can be applied to software engineering, research, and operations.

## Scion architecture companions

Scion is designed to be one part of your agent system. Compose templates and reusable skills with the task tracking, memory, data, and network tools that fit your project. For software work, that might mean GitHub Issues, Linear, [Farmtable](https://github.com/scion-frontiers/farmtable), or [Beads](https://github.com/gastownhall/beads). Shared directories give agents a place to exchange artifacts and notes; specialized memory systems can extend that foundation.

The Skill Bank supports publishing and versioning skills, with resolution from GitHub and external registries. Harness configuration bundles make agent setup extensible, while the A2A bridge connects Scion to other agent systems. These boundaries let the surrounding ecosystem evolve alongside Scion.

## Quick Start

### Install with Homebrew (recommended)

The easiest way to get Scion is the community [homebrew-scion](https://github.com/homebrew-scion/homebrew-scion) tap:

```bash
brew tap homebrew-scion/scion
brew install homebrew-scion/scion/scion
```

This installs the `scion` CLI — pre-configured to use `ghcr.io/homebrew-scion` as the default image registry — along with the `scion-plugin-telegram` broker plugin. To upgrade later:

```bash
brew update && brew upgrade homebrew-scion/scion/scion
```

Then start the Workstation server:

```bash
scion server start
```

Your browser opens to the onboarding wizard at `http://127.0.0.1:8080/onboarding`, which walks you through runtime detection (Docker, Podman, or Apple Container), identity configuration, container image setup, and creating your first workspace.

After onboarding, start your first agent:

```bash
scion start my-agent "Your task here"
```

See the [homebrew-scion tap](https://github.com/homebrew-scion/homebrew-scion) for the full list of pre-built multi-arch container images and distribution details.

### Install from Source

Follow the [Installation Guide](https://googlecloudplatform.github.io/scion/getting-started/install/) to build from a clone with `make all`. This builds the web frontend and embeds it in the binary; use the Go version specified in `go.mod` and the guide's Node.js prerequisites.

### Initialize your machine and a project

> **Tip:** If you used `scion server start` above, the onboarding wizard handles machine initialization automatically — you can skip this section.

Navigate to your project and create a Scion project (the `.scion` directory that holds agent config):

```bash
scion init --machine
cd my-project
scion init
```

> **Tip:** Add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees.

Scion auto-detects your OS and configures the default runtime (Docker on Linux/Windows, Container on macOS). Override this in `.scion/settings.yaml`.

**NOTE** This project is evolving rapidly. Local mode, Hub-based workflows, and Kubernetes runtime are all supported and in active use. Expect continued iteration — APIs and configuration may change between releases.

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

- **Your choice of agents** — Mix supported harnesses and models across a team. Extend agent setup through [harness configuration bundles](harnesses/README.md), with provisioning and authentication defined outside Scion's core.
- **Teams that adapt** — Agents can delegate through the CLI, message collaborators, and receive notifications when work finishes or needs attention. Define the collaboration pattern in templates, skills, and instructions.
- **Workspaces with deliberate sharing** — Run agents in containers with their own configuration and credentials. Choose shared directories, Git worktrees, or separate clones according to the project's coordination needs.
- **People in the conversation** — Follow work in native web chat or connect Telegram, Discord, Slack, Google Chat, and Microsoft Teams. Attach to supported interactive harness sessions when you want to work directly with an agent.
- **Reusable expertise** — Package instructions and tools in [templates](https://googlecloudplatform.github.io/scion/local/templates/) and versioned skills. Publish, discover, and resolve skills through the Skill Bank and connected registries.
- **Local to hosted execution** — Use Docker, Podman, Apple Container, Kubernetes, or Cloud Run. Choose local, Workstation, single-node hosted, or HA hosted operation, with a managed-agent API path for supported providers.
- **Visibility and control** — Inspect logs, session metrics, and system health. Hosted deployments add roles, access boundaries, scoped secrets, and federated identity to control how people and agents work together.
- **Open interfaces** — Integrate through the CLI, APIs, lifecycle hooks, MCP configuration, and the A2A bridge. Build on Apache-2.0-licensed code and contribute new patterns back to the project.

## Core Concepts

| Concept | Description |
|---------|-------------|
| **Agent** | A worker with its own identity, typically running an agent harness in a container; managed agents use a provider API |
| **Project** | A project namespace and collection of agents, commonly 1:1 with a git repo |
| **Template** | An agent blueprint — system prompt plus a collection of skills |
| **Runtime** | The execution technology: Docker, Podman, Apple Container, Kubernetes, or Cloud Run |
| **Hub** | The control plane for Workstation and hosted operation, managing shared state, identity, and dispatch |
| **Runtime Broker** | A service that provisions and runs containerized agents on behalf of a Hub |

Not all concepts apply in every scenario — local mode is simpler. See [Concepts](https://googlecloudplatform.github.io/scion/concepts/) for the full picture.

## Documentation

Visit our **[Documentation Site](https://googlecloudplatform.github.io/scion/)** for comprehensive guides and reference.

- **[Overview](https://googlecloudplatform.github.io/scion/overview/)**: Introduction to Scion.
- **[Installation](https://googlecloudplatform.github.io/scion/getting-started/install/)**: How to get Scion up and running.
- **[Concepts](https://googlecloudplatform.github.io/scion/concepts/)**: Understanding Agents, Projects, Harnesses, and Runtimes.
- **[CLI Reference](https://googlecloudplatform.github.io/scion/reference/cli/)**: Comprehensive guide to all Scion commands.
- **Guides**:
    - [Using Templates](https://googlecloudplatform.github.io/scion/local/templates/)
    - [Using Tmux](https://googlecloudplatform.github.io/scion/local/tmux/)
    - [Kubernetes Runtime](https://googlecloudplatform.github.io/scion/hosted/ha/kubernetes/)

## Project Status

Scion is an actively developed, pre-release project. It is a place to explore emerging agent collaboration patterns and improve the infrastructure they need through practical use.

- **Local** — run agents directly from the CLI.
- **Workstation** — use a local Hub, Runtime Broker, and web UI for one person.
- **Single-node hosted** — share a networked Hub running on a VM or Cloud Run.
- **HA hosted** — operate a replicated Hub with Postgres and shared storage.

Capabilities and operational requirements vary by mode. APIs and configuration continue to evolve; see [Choosing a Mode](https://googlecloudplatform.github.io/scion/choosing-a-mode/), the [release notes](https://googlecloudplatform.github.io/scion/release-notes/), and the [public roadmap](https://github.com/orgs/scion-frontiers/projects/5/views/2).

## Disclaimers

This is not an officially supported Google product. This project is not eligible for the [Google Open Source Software Vulnerability Rewards Program](https://bughunters.google.com/open-source-security).

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).
