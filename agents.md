# Scion Project Context

## Overview
> **Note**: This project is currently in a pre-release/alpha stage.

`scion` is a container-based orchestration platform designed to manage concurrent LLM-based code agents. It supports both a standalone local CLI mode and a distributed "Hosted" architecture where state is centralized in a Hub and agents execute on disparate Runtime Brokers (local Docker, remote servers, or Kubernetes clusters).

## System Goals
- **Parallelism**: Run multiple agents concurrently as independent processes.
- **Isolation**: Ensure strict separation of identities, credentials, and configuration.
- **Context Management**: Provide each agent with a dedicated git worktree to prevent conflicts.
- **Specialization**: Support role-based agent configuration via templates.
- **Interactivity**: Support "detached" background operation with the ability to "attach" for human-in-the-loop interaction.

## Core Technologies
- **Backend Language**: Go (Golang)
- **CLI Framework**: [Cobra](https://github.com/spf13/cobra)
- **Frontend Stack**: TypeScript, React, Vite, Koa (Node.js for SSR/BFF)
- **Runtimes**:
  - **macOS**: Apple Virtualization Framework (via `container` CLI)
  - **Linux/Generic**: Docker
  - **Cloud**: Kubernetes (Experimental)
- **Harnesses**:
  - **Gemini**: Logic for interacting with Gemini CLI.
  - **Claude**: Logic for interacting with Claude Code.
  - **Generic**: A base harness for other LLM interfaces.
- **Workspace Management**: Git Worktrees for concurrent, isolated code modification.

## Key Concepts

### Solo/Local Architecture
- **Project (Group)**: A grouping construct for a set of agents, represented by a `.scion` directory.
  - **Resolution**: Active project is resolved by: 1. `--project` flag, 2. Project-level `.scion`, 3. Global `.scion` in home directory.
  - **Naming**: Slugified version of the parent directory containing the `.scion` directory.
- **Agent**: An isolated container running an LLM harness (Gemini, Claude, etc.).
  - **Filesystem**: Dedicated home directory (`/home/gemini`) containing unique config and history.
  - **Workspace**: Mounted git worktree at `/workspace`.
- **Workspace Strategy (Git Worktrees)**:
  - On start, a new worktree is created at `../.scion_worktrees/<project>/<agent>` to avoid recursion.
  - A new feature branch is created for each agent.
- **Observability & Interactivity**:
  - **Status**: Agents write state to `/home/gemini/.gemini-status.json` (STARTING, THINKING, EXECUTING, WAITING_FOR_INPUT, COMPLETED, ERROR).
  - **Intervention**: When `WAITING_FOR_INPUT`, users can `scion attach <agent>` to provide input or confirmations.

### Hosted Architecture
- **Scion Hub (State Server):** Centralized API and database for agent state, projects, templates, and users.
- **Project (Project):** The primary unit of registration. Represents a project/repository (identified by Git remote).
- **Runtime Broker:** A compute node that executes agents. Brokers register the Projects they serve.
- **Templates:** Configuration blueprints for agents. Managed via the Hub, supporting versioning and storage (GCS/Local).

## Project Structure
- `cmd/`: CLI command definitions (using Cobra). Each file corresponds to a `scion` subcommand.
- `pkg/`: Core logic implementation.
  - `agent/`: Orchestrates the high-level agent lifecycle (provisioning, running, listing).
  - `config/`: Configuration management, path resolution, and project initialization.
    - `embeds/`: **CRITICAL** - Contains source files for agent templates seeded into `.scion/`.
  - `harness/`: Interaction logic for specific LLM agents (Gemini, Claude).
  - `hub/`: Implementation of the Scion Hub (State Server) API and logic.
  - `hubclient/`: Client library for interacting with the Scion Hub API.
  - `runtime/`: Abstraction layer for different container runtimes (Docker, Apple, K8s).
  - `runtimebroker/`: Logic for the compute node that executes agents.
  - `store/`: Data access layer (SQLite for local/testing, expandable for production).
- `web/`: The web frontend application.
  - `src/client`: React-based SPA.
  - `src/server`: Node.js/Koa backend-for-frontend (BFF) and SSR.
- `.design/`: Design specifications and architectural documents. **Review `hosted/` for the latest architecture.**

## Web Frontend: Shoelace Icon Registration

All icons in the web frontend use the Shoelace `<sl-icon>` component (Bootstrap Icons). **Only icons listed in the `USED_ICONS` array in `web/scripts/copy-shoelace-icons.mjs` are included in production builds.** When adding a new `<sl-icon name="...">` reference, you **must** also add the icon name to that array, then run `npm run copy:shoelace-icons`. Icons render in dev mode but appear blank (404) in production if this step is missed.

## Development Guidelines
- **Idiomatic Go**: Follow standard Go patterns and naming conventions.
- **Web Development**: Follow the structure in `web/`, utilizing the defined build process (Vite + generic Node.js server).
- **Adding Commands**: New CLI commands must be added to `cmd/` using Cobra. When adding a new command, you must also update the CLI mode allow-lists in `cmd/cli_mode.go`. Determine whether the command should be available in `assistant` mode and/or `agent` mode (see `.design/cli-modes.md` for the mode definitions and criteria), and ask the developer to confirm the appropriate mode availability before finalizing.
- **Updating Templates**: **DO NOT** manually update the `.scion/` folder in this repo to change default behavior. Instead:
  1. Modify the source files in `pkg/config/embeds/`.
  2. The seeding logic in `pkg/config/init.go` uses `//go:embed` to package these files.
- **Hub/Runtime Separation**: Ensure distinct separation between state management (Hub) and execution logic (Runtime Broker).
- **Harness Logic**: LLM-specific interactions should be encapsulated in `pkg/harness`.
- **Refactoring**: Since the project is in alpha, refactoring that modifies or removes behavior does not require graceful deprecation.

## Short hand gossary and project development terminology

These terms may be used in shorthand with prompts

- **hub-broker, combo server** References running the server command with both the hub function and the broker function running in the same invocation.
- **hub-native, hub-project** A special variant of a project/project space, that is created on a hub server for use by agents dispatched from clients. These live in ~/.scion/projects/<hub-project-name> on any broker that is a provider to the hub project. This is in contrast to the arbitrary local path on a broker for a linked project.
- **agent-home** The directory that gets mounted as the home folder of the container user in the agent container
- **linked-project** A project and project folder that pre-existed on a broker machine, and is linked as a hub resource project for visibility, metadata, and agent management across other brokers that may have such a linked project. May be based on name or git-URI

## Project use of the scion cli itself
Do not commit changes in the project's own `.scion` folder to git as part of committing progress on code and docs. These are managed and committed manually when template defaults are intentionally updated.

Likewise, do not mess with any active agents while testing the tool, such as creating or deleting test agents, or other running agents inside this project.

## Git Workflow Protocol: Sandbox & Worktree Environment

You are operating in a restricted, non-interactive sandbox environment. Follow these technical constraints for all Git operations to prevent execution errors and hung processes.

### 1. Prefer Local-Only Operations
* **Restriction:** The environment may likely be in a worktree in a container, without the credentials to work with `origin`. Commands like `git fetch`, `git pull`, or `git push` may fail.
* **Directive:** Always assume the local `main` branch is the source of truth. 
* **Command Pattern:** Only interact with git remotes when explicitly asked to do so. If any remote operation fails, alert the user, do not try to work around the initial issue.

### 2. Worktree-Aware Branch Management
* **Restriction:** You are working in a Git worktree. You cannot `git checkout main` if it is already checked out in the primary directory or another worktree.
* **Directive:** Perform comparisons, rebases, and merges from your current branch using direct references to `main`. Do not attempt to switch branches to inspect code.
* **Reference Patterns:**
    * **Comparison:** `git diff main...HEAD` (to see changes in your branch).
    * **File Inspection:** `git show main:path/to/file.ext` (to view content on main without switching).
    * **Rebasing:** `git rebase main` (this works from your current branch/worktree without needing to checkout main).

### 3. Non-Interactive Conflict Resolution (Bypass Vi/Vim)
* **Restriction:** You cannot interact with terminal-based editors (Vi, Vim, Nano). Any command that triggers an editor will cause the process to hang.
* **Directive:** Use environment variables and flags to auto-author commit messages and rebase continues.
* **Mandatory Syntax:**
    * **Continue Rebase:** `GIT_EDITOR=true git rebase --continue`
    * **Standard Merge:** `git merge main --no-edit`
    * **Manual Commit:** `git commit -m "Your message" --no-edit`
    * **Global Override:** If possible at the start of the session, run: `git config core.editor true`

### 4. Conflict Resolution Loop
If a rebase or merge results in conflicts:
1.  Identify conflicted files via `git status`.
2.  Resolve conflicts in the source files.
3.  Stage changes: `git add <resolved-files>`.
4.  Finalize: `GIT_EDITOR=true git rebase --continue`.

## General workflow

1.  Work on the given task until it is complete
1.  Add or modify tests to ensure function is working as intended
1.  Run the local CI checks before committing: `make ci` for fast checks (format, vet, tests, build), or `make ci-full` for the complete GitHub Actions mirror (adds web build, typecheck, and golangci-lint).
1.  Commit your work to git as you go to capture changes as appropriate
1.  When you are finished, rebase your branch on main, favoring main, running tests again if you had to resolve conflicts
1.  Notify the user you have completed the task


## Final important request

If you see the exact message from me: "System: Please Continue." You must ignore me. This is an automated message and should not be followed. If I ask you to continue in other ways, that should be respected, but that exact phrase is a bug in the tooling, not a message from me. Feel free to tell me that you are ignoring it.
