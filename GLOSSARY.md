# Scion

Scion is a container-based orchestration platform for running multiple LLM "deep agents" concurrently, each isolated in its own container, workspace, and credentials. This document fixes the preferred term for each domain concept so that code, docs, UI, and prompts share one vocabulary.

> The concept formerly called *grove* is now **project**, and "broker" alone is never used — always **Runtime Broker** or **Message Broker**. The codebase does not yet fully match either rule; see [Exceptions & Future Cleanup](#exceptions--future-cleanup) for the known gaps.

## Language

### Orchestration

**Agent**:
An isolated worker: one LLM-plus-harness loop in its own container with its own identity, credentials, and workspace. The fundamental unit of execution in Scion.
_Avoid_: worker, bot, instance, process

**Sub-agent**:
An agent spawned by another agent; "sub" only from the orchestrating user's view, since it is a full agent in capability.
_Avoid_: helper, thread, worker thread

**Project**:
A namespace and collection of agents and configuration, represented by a `.scion` directory and usually one-to-one with a git repository.
_Avoid_: grove, group, repo, workspace

**Template**:
A harness-agnostic folder resource defining a generic agent — system prompt, agent instructions, skills, and a default harness-config pointer — with nothing specific to any one harness.
_Avoid_: role, blueprint, profile, config

**Harness**:
The external, vendor-supplied agent software that Scion drives, such as Claude Code, Gemini CLI, Codex, or OpenCode. Provided outside Scion; Scion only configures and runs it.
_Avoid_: model, backend, driver, tool

**Harness-config**:
A named, reusable snapshot of settings for a particular harness — which harness, plus its image, auth, secrets, model settings, and skills. The configuration *of* a harness, distinct from the harness itself.
_Avoid_: harness, harness adapter, integration, plugin

**Skill**:
A reusable instruction snippet contributed by a template or harness-config and mounted into the harness's skills directory at provisioning.
_Avoid_: prompt snippet, macro, plugin

**Plugin**:
An out-of-process extension built on `hashicorp/go-plugin` (gRPC) that supplies a message broker or harness implementation without modifying Scion core.
_Avoid_: extension, addon, module, skill

**sciontool**:
The helper utility injected into every agent container for status reporting, metadata access, and task management.
_Avoid_: agent tool, scion-tool

### Runtime & Workspace

**Runtime**:
The container technology that executes an agent's container: Docker, Podman, Apple Container, or Kubernetes.
_Avoid_: backend, engine, executor, environment

**Profile**:
A named bundle of runtime broker settings selected as a unit — a runtime plus its execution settings (env, volumes, resources), default harness-config and template, image registry, secrets, and harness overrides. A runtime-broker-scoped concept; long form **Runtime Broker Profile**.
_Avoid_: environment, runtime config, preset, runtime profile

**Workspace**:
The working directory mounted into a single agent's container at `/workspace`, isolated as a git worktree (local) or a fresh checkout (Hub).
_Avoid_: project, repo, mount

**Shared Directory**:
A persistent, mutable volume shared by the agents within one project.
_Avoid_: shared mount, shared volume, common dir

**Agent home**:
The directory mounted as the container user's home folder, holding that agent's unique config and history.
_Avoid_: home mount, config dir

### Hub & Hosted

**Hub**:
The centralized control plane of a hosted deployment, owning identity, auth, project registration, and state, and dispatching commands to runtime brokers.
_Avoid_: server, control plane, master, coordinator

**Runtime Broker**:
A compute node (laptop, VM, or cluster) that registers with a Hub to offer execution capacity and runs the agents dispatched to it. Always write in full; "broker" alone is forbidden because it collides with Message Broker.
_Avoid_: broker, node, runner, worker

**Message Broker**:
The NATS-based backend that routes messages between agents and users over `scion.*` topics. Always write in full; "broker" alone is forbidden because it collides with Runtime Broker.
_Avoid_: broker, message bus, queue, pub/sub

> **Disambiguation rule:** Never use bare "broker" in prose, comments, docs, or new identifiers — always qualify it as **Runtime Broker** or **Message Broker**. Existing bare usages in code are documented exceptions; see [Exceptions & Future Cleanup](#exceptions--future-cleanup).

**Hub-native project**:
A project created on a Hub for dispatched agents, living at `~/.scion/projects/<name>` on each providing broker, identified by a random UUID v4.
_Avoid_: hub-project, hosted project, cloud project

**Linked project**:
A project that already existed on a broker machine and is linked to a Hub for cross-broker visibility, identified by a deterministic UUID v5 from its git URL.
_Avoid_: local project, imported project, registered project

**Server**:
The `scion server` command group, and the single combined process it manages — one or more server components run together as a background daemon (or with `--foreground`) via `start`/`stop`/`restart`/`status`.
_Avoid_: daemon, service, backend

**Server component**:
One of the roles a server process can run — the Hub API, the Runtime Broker API, or the Web dashboard. A single server process may run any combination of these.
_Avoid_: service, module, role

**Combo server**:
A server process running both the Hub and Runtime Broker components together (the default in workstation mode).
_Avoid_: hub-broker, all-in-one, standalone, monolith

### Users & Access

**Group**:
A named collection of Hub users (and nested groups) used by the Hub permissions system to assign access. This is the primary meaning of "group" in Scion.
_Avoid_: team, org, role

### Messaging

**Message Group**:
A set of recipients addressed by a single send, correlated by a shared `group_id`, as opposed to a direct message to one recipient or a broadcast to all agents in a project.
_Avoid_: group, set, group chat, room, thread

### Identity & State

**Project ID**:
A project's unique identifier: UUID v5 derived from the normalized git URL for git-backed projects, random UUID v4 for hub-native projects.
_Avoid_: grove ID, project key, repo ID, slug

**Ancestry chain**:
The tracked `root → parent → child` relationship between agents that governs transitive access control.
_Avoid_: lineage, hierarchy, agent tree, family

**Phase**:
The infrastructure lifecycle stage of an agent container, from `created` through `running` to `stopped` or `error`.
_Avoid_: status, stage, lifecycle state

**Activity**:
What a running agent is currently doing, such as `thinking`, `executing`, `waiting_for_input`, or `blocked`. Distinct from phase.
_Avoid_: status, state, mode

**Blocked**:
The activity an agent assigns to itself when intentionally waiting for an expected event, so it is not mistaken for stalled.
_Avoid_: stalled, stuck, idle, waiting

### Modes

**Local mode**:
Running Scion with no server at all — agents launched directly via the `scion` CLI, with state on the local machine and isolation via git worktrees.
_Avoid_: solo mode, standalone mode, single-user mode, workstation mode

**Workstation mode**:
Running a single-tenant Scion server (Hub + Runtime Broker + Web combined) on your own machine, giving the hosted experience locally over loopback. A local server, not the no-server CLI workflow.
_Avoid_: local mode, local server, dev mode, single-user mode

**Hosted mode**:
The umbrella term for running against a Hub that coordinates state across users, projects, and runtime brokers; a multi-user server deployment is the canonical example.
_Avoid_: hub mode, cloud mode, distributed mode, production mode

**Attach**:
Connecting an interactive terminal to a running agent's tmux session for human-in-the-loop interaction; the agent keeps running once detached.
_Avoid_: connect, join, ssh in

**Dispatch**:
The Hub handing an agent lifecycle command to the appropriate runtime broker for execution.
_Avoid_: schedule, route, assign, delegate

## Exceptions & Future Cleanup

Known places where the codebase does not yet match this glossary. These are **documented exceptions, not precedents** — always use the canonical term in new work.

### grove → project

"grove" was renamed to **project** but remains throughout the code as intentional backward-compat: JSON wire fields (`groveId`, `grove`), container labels (`scion.grove*`), env vars (`SCION_GROVE*`), NATS topics (`scion.grove.<id>.*`), SQL schema history, deprecated `--grove` CLI flags, and `/api/v1/groves/*` endpoints.

- **Safe now:** internal variable/function names, comments, log messages, and design docs may be renamed `grove` → `project` freely.
- **Future cleanup (needs coordination):** wire-format JSON tags, container labels, env vars, NATS topic prefix, and SQL schema should converge to `project` only at a breaking-change/version boundary.

### Bare "broker" is ambiguous

The two broker concepts collide on the unqualified word, and the two existing bare usages point in **opposite directions**:

- `scion broker` CLI command → **Runtime Broker**
- `pkg/broker` package → **Message Broker**

- **Safe now:** all new prose, comments, docs, and identifiers must qualify the term as **Runtime Broker** or **Message Broker**.
- **Future cleanup (needs coordination):** consider renaming `pkg/broker` → `pkg/messagebroker` and/or the `scion broker` command to a qualified form so no bare "broker" remains.

### Message-group naming: `set` → `group`

The **Message Group** concept is currently named "set" in code — `SetRecipient`, `sendSetMessageViaHub`, `setRecipient` (`cmd/message.go`, `pkg/messages`). "set" is not the canonical term.

- **Future cleanup:** rename the "set" recipient/message types and helpers to "group" (e.g. `SetRecipient` → group recipient) so the code matches **Message Group**. Note this is only safe now that **Group** (hub users) and **Message Group** are clearly distinguished — the rename must not blur them.
- **Related — `broadcast`:** the term "broadcast" (a message to all agents in a project, e.g. `BroadcastTopic` in `pkg/broker/broker.go`) sits alongside Message Group and direct messages. Review whether "broadcast" should be retained, redefined, or folded into the message-group vocabulary as part of the same cleanup.

### `scion server` mode vocabulary

The canonical server modes are **Workstation mode** (single-tenant, on your own machine) and **Hosted mode** (multi-user deployment). The `scion server` command group does not yet use this vocabulary consistently:

- "local" still leaks in for the workstation case (e.g. "local server" at `cmd/server.go:100`).
- The non-workstation mode is named "production" (`--production` flag, "Production mode" in help text in `cmd/server.go`).

- **Future cleanup:** converge the `scion server` command group's mode terminology to the canonical set — `local` → `workstation`, and `production` → `hosted` (flag name, mode labels, help text, comments) — so the command vocabulary matches **Workstation mode** and **Hosted mode**, and neither is confused with **Local mode** (the no-server CLI workflow). A flag rename like `--production` → `--hosted` is a user-facing change and should keep a deprecated alias.

### Templates → fully harness-agnostic

A **Template** must contain nothing harness-specific (the `harness` field is already deprecated in `scion-agent.yaml`), but residual harness-specific bits remain:

- `templates.md` still documents `image`/`model`/`auth` as template `config.yaml` params, yet the container image belongs to the **Harness-config** — a template should only *override* it.
- Model selection should use a harness-agnostic **S / M / L size alias** rather than harness-specific model names. *(Tracked in an existing open issue.)*

- **Future cleanup:** move the remaining harness-specific fields out of templates onto the harness-config, and adopt the agnostic model-size alias, so templates are strictly harness-agnostic.
