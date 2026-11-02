---
title: Scion Concepts
---

This document defines the core concepts and terminology used in Scion.

## Core Concepts

### Agent
An **Agent** is an isolated process running an LLM + Harness loop (aka Agent) against a task. It acts as an independent worker with its own identity, credentials, and workspace. An agent is the fundamental unit of execution in Scion.

### Project
A **Project** is a namespace and collection of agents and configuration where agents live. It corresponds to a `.scion` directory on the filesystem, usually one-to-one with a git repository. It can exist at the project level (generally located at the root of a git repository), or globally in the user's home folder.

A Project is **not** the same thing as a **Group**. A Group is a named collection of Hub users used by the permissions system to assign access (see the [Glossary](/scion/glossary/)); it governs *who* can act, not *where* agents live.

Every project has a unique **Project ID** — a randomly generated UUID. A git remote is associated metadata, not identity, so multiple projects may share the same remote by design.

### Hub
The **Hub** is the central control plane of a hosted Scion architecture. It acts as the "brain" of the system, coordinating state across multiple users, projects, and runtime brokers.
- **Identity & Auth**: Manages user identities (via OAuth) and issues tokens for brokers and agents.
- **State Persistence**: Stores the definitive state of agents, projects, and templates in a central database.
- **Orchestration**: Dispatches agent lifecycle commands to the appropriate Runtime Brokers.
- **Collaboration**: Provides a shared view of the system via the Web Dashboard and Hub API.

### Profile
A **Profile** defines a complete execution environment by binding a specific **Runtime** to a set of behavior flags and **Harness** configuration overrides.
- Profiles allow you to switch between different environments (e.g., "Local Docker", "Production Kubernetes") without modifying agent templates.
- They are defined in the global or project `settings.yaml`.

### Harness-Configuration
A **Harness-config** adapts a specific underlying LLM tool or agent software (like Gemini CLI, Claude Code, or OpenAI Codex) into the Scion ecosystem.
- It handles the specifics of provisioning, configuration, and execution for that particular tool inside an OCI container.
- Examples: `GeminiCLI`, `ClaudeCode`, `Codex`, `OpenCode`.
- The harness ensures that the generic Scion commands (`start`, `stop`, `attach`, `resume`) work consistently regardless of the underlying agent software.

### Template
A **Template** is a blueprint for creating an agent. It defines the base configuration, system prompt, and tools that an agent will use.
- Templates are stored in `.scion/templates/` and can be project-level or global (`~/.scion/templates/`).
- Users can manage templates using the `scion templates` command suite (`create`, `clone`, `list`, `show`, `update-default`).
- Scion comes with default templates for supported harnesses (e.g., `gemini`, `antigravity`, `opencode`, `codex`), but users can create custom templates for specialized roles (e.g., "Security Auditor", "React Specialist").


### Runtime
The **Runtime** is the infrastructure layer responsible for executing the agent containers.
- Scion abstracts the container execution, allowing it to support different backends.
- **Docker**: The standard runtime for Linux and macOS.
- **Podman**: A daemonless, rootless alternative to Docker for Linux and macOS.
- **Apple Container**: Uses the native Virtualization Framework on macOS for improved performance.
- **Kubernetes**: Allows running agents as Pods in a Kubernetes cluster, enabling remote execution and scaling at production scale.
- **Substrate**: Runs each agent as an [Agent Substrate](https://github.com/agent-substrate/substrate) actor on GKE, driven through the Substrate `ateapi` control plane and `atenet` router by an in-cluster Runtime Broker. Selected explicitly with a `type: substrate` runtime entry; see [`deploy/substrate/`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/substrate) for cluster and Runtime Broker manifests and operations docs.

### Runtime Broker
A **Runtime Broker** is a *service* that manages the lifecycle of containerized agents on behalf of the **Hub** — it is not itself a compute node. It provisions workspaces, hydrates templates, streams logs, and delegates container operations to a pluggable **Runtime**.

Brokers vary along two dimensions:
- **Host access** — a **Node-Bound Broker** runs on the same machine as the containers it manages (required for runtimes that need direct host access, like Docker or Apple Container); a **Proxy Broker** is stateless and delegates to an API-mediated service such as Cloud Run or Kubernetes.
- **Process model** — a broker may run as its own standalone process, or be an **Embedded Broker** running inside the Hub process. The default platform backend is the **Hosted Broker** (an embedded proxy broker).

Note that a **Managed Agent** bypasses the Runtime Broker layer entirely, with its lifecycle driven directly by a cloud provider API.

For more details, see the [Runtime Broker Guide](/scion/hosted/ha/runtime-broker/).

### Agent State Model

Agent state uses a **layered model** with three dimensions:

- **Phase** — The lifecycle stage of the agent container:
  `created` → `provisioning` → `cloning` → `starting` → `running` → `stopping` → `stopped`
  with three off-the-happy-path destinations: `suspended` (paused for later resume), `error` (the agent crashed), and `halted` (the platform ended a run that should still be running).

- **Activity** — What the agent is doing within the `running` phase:
  `initializing` (shown as "starting"), `working`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `stalled`, `offline`, `crashed`
  (the `crashed` value exists in the enum, but a real crash now surfaces as the `error` *phase* — see below — rather than as an activity)

- **Detail** — Freeform context about the current activity (tool name, message, task summary).

This separation allows the UI and API consumers to distinguish between infrastructure lifecycle events (provisioning, stopping) and the agent's cognitive state (thinking, waiting for input). The `blocked` activity is set by agents themselves when they are intentionally waiting for an expected event (such as a child agent completing), which prevents the system from falsely marking them as stalled.

#### Sticky activities

The activities `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, and `crashed` are "sticky": routine harness events (model calls, tool runs, the end of a turn, sub-agent activity) do not replace them, except as listed below for `waiting_for_input`. A sticky activity is cleared by new work (a new prompt, or a new harness session), by an explicit status update from the agent (`sciontool status`), or by a lifecycle phase change such as stopping. Beyond that, each one has its own rules:

- **`limits_exceeded` and `crashed`** are terminal: they record how the agent ended. A later notification, or a later question from the agent (a Claude Code `AskUserQuestion` or `ExitPlanMode`), does not replace them.
- **`blocked` and `completed`** are kept through tool runs, which may follow `task_completed` as wrap-up. The agent asking the user (its own question, or a harness notification that asks the user) moves them to `waiting_for_input`. A sub-agent's question does not.
- **`waiting_for_input`** set by the agent itself (`sciontool status ask_user`, or the agent's own `AskUserQuestion` or `ExitPlanMode`) is cleared when the agent starts its next tool, because the user has answered. A wait set by a harness prompt (for example a permission dialog) is also cleared when the prompted tool ends or the turn ends. While a sub-agent's question is open, the wait stays until that sub-agent's question is answered or the sub-agent ends, even if the parent agent answers its own question first.

#### Suspended phase

`suspended` is distinct from `stopped`. Both tear down the container, but `suspended` records the **intent to resume**: when the agent is started again, its harness conversation is continued (Claude Code receives `--continue`, Gemini CLI receives `--resume`, and so on) rather than starting fresh. This is true session continuation, not a restart from a blank slate. Suspension is only available for harnesses that support session resume — see [Agent Lifecycle: Suspend & Resume](/scion/local/agent-lifecycle/).

#### Error phase (crashes and setup failures)

The most common cause of `error` is a **crash**: the agent process or container exited non-zero (for example, an out-of-memory kill or a `SIGKILL`). Scion distinguishes this from a clean shutdown:

- A clean exit (exit code 0, including the graceful `SIGTERM` that a normal `stop` triggers) → `stopped`.
- Hitting a configured limit on turns, model calls, or duration → `stopped` with the terminal activity `limits_exceeded`.
- A genuine crash → `error` (activity cleared), with the detail carried in the agent's message, such as `Agent crashed with exit code 137`.

A crash can be set from two places: `sciontool` reports it from the recovered exit code (authoritative), and the Hub also derives `error` from a non-zero container exit in the broker heartbeat (for cases where the container died before `sciontool` could report).

The `error` phase is not limited to runtime crashes — it also covers **setup failures** that happen before an agent ever reaches `running`, such as a failed git clone or a provisioning error. In all cases the phase is restartable.

The `error` phase is **restartable**: running `scion start` clears the error and launches a fresh session. See [Crash Recovery](/scion/local/agent-lifecycle/#crash-recovery-the-error-phase).

#### Halted phase (the platform ended the run)

`halted` means the platform, not you and not the agent, ended a run that was meant to keep running. The agent's **exit reason** says why:

| Exit reason | What happened |
|---|---|
| `preempted` | The Kubernetes scheduler evicted the pod to make room for a higher-priority one. |
| `evicted` | The pod was removed by another disruption, such as node-pressure eviction or a node drain. |
| `oom_killed` | The container was killed for exceeding its memory limit. |
| `container_missing` | The agent runtime kept reporting a complete inventory without the agent's container. |

A stopped or suspended agent can also carry the exit reason `signaled`. The agent reports it when it stopped after a termination signal (for example `docker stop`, or a host or node shutdown). It is not a halt reason: on its own it never halts an agent, it only fills an empty exit reason, and it is cleared when the agent next starts.

Only the agent runtime's own observations (its reports of the container, and the Hub's check of its inventory) can halt an agent; a status report an agent sends about itself never does, and never sets one of these reasons. An agent is halted only when all of these hold: it was meant to be running (it was started and not stopped since), no start, restart or stop of it is in progress, and its agent runtime is Docker, Podman, Apple container, or Kubernetes. Otherwise the same run ends `stopped` or `error`, as before. While the agent is running, a reason from the table reported by the agent runtime halts it whatever the exit code. If the agent has already reported that it stopped, a later reason from the table reported by the agent runtime with a clean exit (code 0) upgrades it to `halted` only when the agent's report said it stopped after a termination signal.

`halted` is terminal, like `stopped`: the agent has no container and does not count toward the agent quota. It stays `halted` until you act on it. Status reports from the old run do not move it.

To continue, run `scion start <agent>`. It starts the agent the way a start from `stopped` does. Whether the workspace survives depends on where it lives: a workspace on the agent's own pod storage (a Kubernetes EmptyDir) is re-cloned, and other workspaces are kept. `scion stop` on a halted agent records it as `stopped`.

`scion list` shows the reason next to the phase, for example `halted (preempted)` or `halted (out of memory)`. The agent's creator is notified once per halt (unless they chose their own notification triggers by subscribing), and so is anyone subscribed to the `HALTED` notification trigger (see [Messaging & Notifications](/scion/hosted/user/messaging/#halted-agent-notifications)). Lifecycle hooks can use the `halted` trigger (see [Lifecycle Hooks](/scion/hosted/ha/lifecycle-hooks/#triggers)).

#### Stalled, offline, and auto-suspend

The `stalled` activity is set by the platform when an agent's heartbeat is still arriving (the process is alive) but no activity events have been seen for a while (default: 5 minutes). It flags an agent that appears hung. Agents that have declared themselves `blocked` are excluded from stalled detection. If `server.hub.auto_suspend_stalled` is on (off by default), the Hub also **auto-suspends** a stalled agent to reclaim its container — see [Auto-Suspend of Stalled Agents](/scion/local/agent-lifecycle/#auto-suspend-of-stalled-agents).

The `offline` activity status occurs when an agent heartbeat has not been heard from for some time. Currently, this may be due to an agent being unable to refresh its auth token, which disconnects it from sending its heartbeat and other updates. These agents can be stopped and restarted to be provisioned with a new auth token. They should be able to refresh this token as long as they can maintain a connection to the Hub.

## Detailed Architecture

### A full approach to sub-agents

 Because an agent through its template can contain home folder content, env var definitions, and custom mounts that collectively exposes all configuration available to the harness (e.g., gemini-cli) scion-agents are not limited by the constraints of a harness' built-in sub-agent feature. While they are acting as sub-agents from the point-of-view of the Scion tool user-as-orchestrator, they are full agents in their capabilities.

### Agent Ancestry & Identity Scoping

To support multi-agent workflows, Scion implements **Agent Ancestry Chains**. When an agent spawns a child agent, the system tracks this relationship (`root` → `parent` → `child`). This ancestry chain is critical for **transitive access control**: any principal (user or agent) that exists in an agent's creation chain automatically gains access to manage that descendant agent. 

Furthermore, agent identities are **strictly scoped by their project** (e.g., `project--agent`). This naming convention prevents name collisions across different workspaces and ensures agents can only interact with peers and progeny within their designated boundary. Progeny agents also receive granular secret access controls to prevent privilege escalation.

### Workspace Sharing Modes

A **Workspace** is the working directory mounted into a single agent's container, at `/workspace`; in worktree-per-agent mode the agent instead works in its worktree under `/repo-root` (for a repository linked to the local workstation Hub, `/repo-root/.scion/agents/<name>/workspace`); scripts should use `$PWD` or `git rev-parse --show-toplevel`. How it is provisioned across a project's agents is set by the project's **workspace sharing mode**, chosen when the project is created. There are four modes:

- **Shared-plain** — One workspace directory is mounted into every agent with no per-agent isolation. This is the model used for plain (non-git) projects.
- **Worktree-per-agent** — Each agent gets its own [git worktree](https://git-scm.com/docs/git-worktree) over a shared checkout, isolating working trees while sharing one clone's history. Supported on repositories linked to the local workstation Hub (registered with this mode automatically; the worktrees are of your own checkout) and on Hub-managed git projects created with `worktree-per-agent` (see [Linked git projects](/scion/local/workspace/#linked-git-projects)); requires git 2.48 or later on the broker; not supported on Kubernetes.
- **Clone-per-agent** — Each agent gets its own full git clone of the repository.
- **Empty-per-agent** — For Hub-managed projects without git: each agent gets its own private directory that starts empty. Use [shared directories](/scion/local/workspaces-and-sharing/#2-the-shared-directories-invariant) to share or keep files.

**Worktree-per-agent (a repository linked to the local workstation Hub):**
- A new worktree of your checkout is created at `.scion/agents/<agent>/workspace` with a branch named after the agent. Your repository is mounted in the container at `/repo-root`, and the agent works in `/repo-root/.scion/agents/<agent>/workspace` (not `/workspace`).
- Agents operate on the same repository history as your checkout but have independent working directories.
- Work is merged back manually (e.g., `git merge <agent-branch>`). See [Linked git projects](/scion/local/workspace/#linked-git-projects) for the checks and cautions.

**Worktree-per-agent (Hub-managed git projects created with that mode):**
- The Runtime Broker keeps one shared base clone of the repository on its host and creates a worktree per agent in that clone's `worktrees` directory, on a dedicated branch.
- Work reaches your own checkout by pushing the agent's branch and merging it there (or through a pull request); the base clone is not your checkout.

**Clone-per-agent (Hub-managed git projects):**
When a Hub manages a git-based project and worktree-per-agent mode is not selected, agents are provisioned with an independent clone via a robust `git init` + `git fetch` strategy rather than a shared worktree.
- The broker injects `SCION_GIT_CLONE_URL`, `SCION_GIT_BRANCH`, and a `GITHUB_TOKEN` into the container.
- `sciontool init` inside the container initializes the workspace, fetches the repo over HTTPS, then checks out a `scion/<agent-name>` branch.
- This approach handles workspaces that already contain `.scion` metadata or `.scion-volumes` directories, clearing stale artifacts before initialization.
- SSH credentials on the host are not used; a `GITHUB_TOKEN` is required.
- This strategy is consistent across all broker machines, whether or not the repo exists locally.

This means a git project used locally with worktrees may switch to clone-based provisioning once it is managed by a Hub, unless the Hub project is created with workspace mode `worktree-per-agent`. When linking a git checkout to the local workstation Hub creates the project, it is created in that mode.

**Worktree-per-agent (Hub-managed git projects):** the Runtime Broker provisions a shared base clone and gives each agent its own worktree over it, the same mount shape as a linked checkout on the workstation Hub, gated on the Runtime Broker host's git version being 2.48 or later; the Runtime Broker falls back to clone-per-agent if worktree provisioning is not possible. On the local workstation Hub, agents of a linked checkout use that checkout instead, and creation fails rather than falling back (see [Workspace Mode Change for Git Projects](/scion/hosted/user/hosted-user/#workspace-mode-change-for-git-projects)). See the [About Workspaces](/scion/local/workspace/) guide for details.

### Resource Isolation
Scion enforces strict isolation between agents to prevent interference and cross-contamination of credentials or data.
- **Filesystem**: Each agent has a dedicated home directory (host path mounted to container) containing its unique history and configuration.
- **Shadow Mounts (tmpfs)**: Scion uses `tmpfs` shadow mounts to definitively prevent agents from accessing `.scion` configuration data or other agents' workspaces within the same project.
- **Environment**: Environment variables are explicitly projected into the container.
- **Credentials**: Sensitive credentials (like `gcloud` auth) are mounted read-only or injected via environment variables, ensuring they are available only to the specific agent.
- **Externalized Project Data**: Non-git project data and agent home directories are externalized to ensure they cannot be traversed by agents in the workspace.
- **Shared-Workspace Per-Agent Isolation**: In hub-hosted git projects where multiple agents share a single workspace mount, each agent's per-agent state (its task prompt and resolved configuration) is held outside that shared mount, so sibling agents in the same project cannot read each other's state through the workspace view.

### Platform and Workspace Skills
Scion automatically tailors an agent's operational context by injecting supplemental platform skills based on the workspace environment.
- **`git-sandbox`**: Injected only for git-associated workspaces, providing instructions on sandbox/worktree environments, local-only operations, and non-interactive conflict resolution.
- **Platform Skills**: Core instructions (such as status signaling, messaging, and command operations) are dynamically injected to guide the agent in interacting with the system.
These platform skills ensure agents understand their specific execution environment without requiring manual configuration in every template.

### Plugin System

Scion supports a plugin architecture built on `hashicorp/go-plugin` for extending system capabilities without modifying Scion core. Plugins are out-of-process extensions that communicate over gRPC.

Today the plugin interface supplies **Message Broker** implementations — custom message delivery backends (e.g. Telegram, Google Chat) that surface agent notifications and structured messaging. Additional plugin types may be added in the future.

:::note[Harnesses are not plugins]
**Harness** implementations (Claude Code, Gemini CLI, Codex, OpenCode, …) are *not* offered as plugins. A harness is external, vendor-supplied agent software that Scion drives; new harnesses are integrated through a **harness-config** and its container-script provisioner, not through the `go-plugin` interface. See the [Glossary](/scion/glossary/) for the distinction.
:::
