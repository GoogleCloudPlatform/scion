# Scion

_sci·on /ˈsīən/ noun 1. a young shoot or twig of a plant, especially one cut for grafting or rooting._

Scion is a container-based orchestration tool designed to manage concurrent LLM-based code agents across your local machine and remote clusters. It enables developers to run specialized sub-agents with isolated identities, credentials, and workspaces, allowing for parallel execution of tasks such as coding, auditing, and testing.

**NOTE** Currently this project is early and experimental. Most of the concepts are starting to settle in, but anything might break or change and the future is not set.

## Key Features

- **Parallelism**: Run multiple agents concurrently as independent processes either locally or remote.
- **Isolation**: Each agent runs in its own container with strict separation of credentials, configuration, and environment.
- **Context Management**: Scion uses `git worktree` to provide each agent with a dedicated workspace, preventing merge conflicts and ensuring clean separation of concerns.
- **Profiles**: Manage multiple execution environments (e.g., Local, Docker, Kubernetes) via named profiles.
- **Specialization**: Agents can be customized via [Templates](docs-site/src/content/docs/guides/templates.md) (e.g., "Security Auditor", "QA Tester") to perform specific roles.
- **Interactivity**: Agents run in `tmux` sessions by default, allowing for "detached" background operation, enqueuing messages to running agents, and "attaching" for human-in-the-loop interaction.
- **Multi-Runtime**: Supports Docker, Apple Virtualization Framework, and (Experimental) Kubernetes.
- **Harness Agnostic**: Works with Gemini CLI, Claude Code, OpenCode, and Codex. Easily adaptable to any harness which can run in a container.

## Documentation

Visit our **[Documentation Site](docs-site/)** for comprehensive guides and reference.

- **[Overview](docs-site/src/content/docs/overview.md)**: Introduction to Scion.
- **[Installation](docs-site/src/content/docs/install.md)**: How to get Scion up and running.
- **[Concepts](docs-site/src/content/docs/concepts.md)**: Understanding Agents, Groves, Harnesses, and Runtimes.
- **[CLI Reference](docs-site/src/content/docs/reference/cli.md)**: Comprehensive guide to all Scion commands.
- **Guides**:
    - [Using Templates](docs-site/src/content/docs/guides/templates.md)
    - [Using Tmux](docs-site/src/content/docs/guides/tmux.md)
    - [Kubernetes Runtime](docs-site/src/content/docs/guides/kubernetes.md)

## Installation

See the **[Installation Guide](docs-site/src/content/docs/install.md)** for detailed instructions.

Quick start from source:
```bash
go install github.com/ptone/scion-agent/cmd/scion@latest
```

## Quick Start

### 1. Initialize a Grove

Navigate to your project root and initialize a new Scion grove. This creates the `.scion` directory and seeds default templates.

```bash
cd my-project
scion init
```

Note: If you are in a git repository, it is recommended to add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees:
```bash
echo ".scion/agents" >> .gitignore
```

Note: Scion automatically detects your operating system and configures the default runtime (Docker for Linux/Windows, Container for macOS). You can change this in `.scion/settings.json`.

### 2. Start Agents

You can launch an agent immediately using `start` (or its alias `run`). By default, this runs in the background using the `gemini` template.

```bash
# Start a gemini agent named "coder"
scion start coder "Refactor the authentication middleware in pkg/auth"

# Start a Claude-based agent
scion run auditor "Audit the user input validation" --type claude

# Start and immediately attach to the session
scion start debug "Help me debug this error" --attach
```

### 3. Manage Agents

- **List active agents**: `scion list` (alias `ps`)
- **Attach to an agent**: `scion attach <agent-name>`
- **Send a message**: `scion message <agent-name> "New task..."` (alias `msg`)
- **View logs**: `scion logs <agent-name>`
- **Stop an agent**: `scion stop <agent-name>`
- **Resume an agent**: `scion resume <agent-name>`
- **Delete an agent**: `scion delete <agent-name>` (removes container, directory, and worktree)

## Configuration

Scion settings are managed in `settings.json` files, following a precedence order: **Grove** (`.scion/settings.json`) > **Global** (`~/.scion/settings.json`) > **Defaults**.

Profiles allow you to switch runtimes and configurations easily (e.g. `scion --profile remote start ...`).

Templates serve as blueprints and can be managed via the `templates` subcommand. See the [Templates Guide](docs-site/src/content/docs/guides/templates.md) for more details.

### Database

The project uses SQLite for storage. Due to the high memory requirements of the pure-Go SQLite driver (`modernc.org/sqlite`) during analysis, you can optionally exclude it during development.

- By default, SQLite support is included.
- If you encounter OOM issues during `go vet` or `go test`, you can skip SQLite-related code:
  ```bash
  go test -tags no_sqlite ./...
  go vet -tags no_sqlite ./...
  ```
- Standard builds produce a functional binary with SQLite support:
  ```bash
  go build ./cmd/scion
  ```
## Disclaimers

This is not an officially supported Google product. This project is not eligible for the [Google Open Source Software Vulnerability Rewards Program](https://bughunters.google.com/open-source-security)

## License

This project is licensed under the Apache License, Version 2.0. See the [LICENSE](LICENSE) file for details.