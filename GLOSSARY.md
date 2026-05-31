# Scion Glossary

Scion is a container-based orchestration platform for running multiple LLM "deep agents" concurrently, each isolated in its own container, workspace, and credentials. This document fixes the preferred term for each domain concept so that code, docs, UI, and prompts share one vocabulary.

> Two naming rules run throughout: the concept formerly called *grove* is now **project**, and bare **"broker"** is never used on its own — it is ambiguous across three distinct concepts (**Runtime Broker**, **Message Broker**, and the **Event Bus**), so it must always be qualified (see the disambiguation rule under [Hub & Hosted](#hub--hosted)). The codebase does not yet fully match either rule; see [Exceptions & Future Cleanup](#exceptions--future-cleanup) for the known gaps.

## Orchestration

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
A harness-agnostic folder resource defining a generic agent — its system prompt, agent instructions, skills, services, and more — containing nothing specific to any one harness. A default harness-config may optionally be named, but is not required.
_Avoid_: role, blueprint, profile, config
_See also_: Harness-config (its harness-specific counterpart), Skill

**Harness**:
The external, vendor-supplied agent software that Scion drives, such as Claude Code, Gemini CLI, Codex, or OpenCode. Provided outside Scion; Scion only configures and runs it.
_Avoid_: model, backend, driver, tool
_See also_: Harness-config

**Harness-config**:
A named, reusable, harness-specific resource that configures a particular harness — which harness, plus its image, auth, secrets, model settings, and skills. The harness-specific counterpart to the (harness-agnostic) **Template**, and extensible the same way via a container-script provisioner rather than compiled-in logic.
_Avoid_: harness, harness adapter, integration, plugin
_See also_: Template, Harness, Container-script provisioner

**Container-script provisioner**:
The script-based provisioning model (`provisioner.type: container-script`) by which a harness-config extends agent setup with a container-side `provision.py`, making harness provisioning extensible — as opposed to a compiled-in (built-in) provisioner.
_Avoid_: built-in provisioner, plugin provisioner, install script, provision hook

**Skill**:
A reusable, harness-agnostic instruction snippet contributed by a template and mounted into the harness's skills directory at provisioning. Follows the open [Agent Skills](https://agentskills.io/home) convention.
_Avoid_: prompt snippet, macro, plugin
_See also_: Template, Plugin

**Plugin**:
An out-of-process extension built on `hashicorp/go-plugin` (gRPC) that supplies a Message Broker implementation without modifying Scion core. Harness implementations are *not* offered as plugins; additional plugin types may be added in future.
_Avoid_: extension, addon, module, skill
_See also_: Broker plugin, Message Broker

**sciontool**:
The helper utility injected into every agent container for status reporting, metadata access, and task management.
_Avoid_: agent tool, scion-tool

## Runtime & Workspace

**Runtime**:
The container technology that executes an agent's container: Docker, Podman, Apple Container, or Kubernetes.
_Avoid_: backend, engine, executor, environment
_See also_: Runtime Broker, Profile

**Workspace**:
The working directory mounted into a single agent's container at `/workspace`. How it is provisioned across a project's agents is set by the project's **workspace sharing mode**.
_Avoid_: project, repo, mount

**Workspace sharing mode**:
How a project's workspace is provisioned across its agents — one universal set of modes intended for both local and Hub-managed projects: **Shared-plain**, **Worktree-per-agent**, and **Clone-per-agent**.
_Avoid_: workspace mode, isolation mode

**Shared-plain**:
A workspace sharing mode where one workspace directory is mounted into every agent with no per-agent isolation — the model used for plain (non-git) projects.
_Avoid_: shared mount, plain workspace

**Worktree-per-agent**:
A workspace sharing mode where each agent gets its own git worktree over a shared checkout, isolating working trees while sharing one clone's history. Supported in local mode today; not yet on Hub-managed projects.
_Avoid_: worktree mode, shared checkout

**Clone-per-agent**:
A workspace sharing mode where each agent gets its own full git clone of the repository.
_Avoid_: clone mode, per-agent clone

**Shared directory**:
A persistent, mutable volume shared by the agents within one project.
_Avoid_: shared mount, shared volume, common dir

**Agent home**:
The directory mounted as the container user's home folder, holding that agent's unique config and history.
_Avoid_: home mount, config dir

## Hub & Hosted

**Hub**:
The control plane of Scion — it owns identity, auth, project registration, and state, exposes the APIs and notifications that agents and users interact with, and dispatches commands to runtime brokers. Present in both workstation and hosted mode, not only hosted.
_Avoid_: server, master, coordinator

**Runtime Broker**:
A compute node (laptop, VM, or cluster) that registers with a Hub to offer execution capacity and runs the agents dispatched to it. Always write in full; "broker" alone is forbidden because it collides with Message Broker.
_Avoid_: broker, node, runner, worker
_See also_: Runtime, Profile, Message Broker (distinct concept, same word)

**Profile**:
A named bundle of runtime broker settings selected as a unit — a runtime plus its execution settings (env, volumes, resources), default harness-config and template, image registry, secrets, and harness overrides. A runtime-broker-scoped concept; long form **Runtime Broker Profile**.
_Avoid_: environment, runtime config, preset, runtime profile
_See also_: Runtime Broker, Runtime

**Message Broker**:
The pluggable system that brokers messages between Scion actors (agents and users) and messaging surfaces — built-in brokers such as the web UI Messages view, and broker plugins to external systems like Telegram and Google Chat (Discord and Slack planned). Backs the `scion message` command. Always write in full; "broker" alone is forbidden because it collides with Runtime Broker.
_Avoid_: broker, message bus, queue, pub/sub
_See also_: Broker plugin, Built-in broker, Plugin, Event Bus (distinct), Runtime Broker (distinct, same word)

**Broker plugin**:
A Message Broker implementation for a specific external messaging system (e.g. Telegram, Google Chat), loaded through the broker plugin interface (`PluginTypeBroker`).
_Avoid_: connector, bridge, adapter
_See also_: Message Broker, Built-in broker, Plugin

**Built-in broker**:
A Message Broker implementation shipped with Scion rather than loaded as a plugin — for example the broker that surfaces messages in the web UI's Messages view.
_Avoid_: native broker, internal broker, default broker
_See also_: Message Broker, Broker plugin

> **Disambiguation rule:** Never use bare "broker" in prose, comments, docs, or new identifiers — always qualify it as **Runtime Broker** or **Message Broker**. Note that `pkg/broker` (NATS-style pub/sub) is *not* the Message Broker; it underpins the **Event Bus**. Existing bare usages in code are documented exceptions; see [Exceptions & Future Cleanup](#exceptions--future-cleanup).

**Event Bus**:
The NATS-style pub/sub system (`pkg/broker`) that brokers and dispatches real-time change events to live web views via server-sent events, supporting the move to a more stateless Hub. Distinct from the Message Broker; currently a latent capability.
_Avoid_: message broker, broker, change feed, live sync, event stream
_See also_: Message Broker (distinct concept)

**Hub-managed project**:
A project whose workspace is created and managed by Scion in the hub-controlled part of the broker filesystem (`~/.scion/projects/<slug>/`), shared across the project's agents — as opposed to a Linked project that points at a pre-existing path. May be plain (no git) or git-backed; git-backed hub-managed projects may share a remote with other projects. The workspace itself is the **Hub-managed workspace**.
_Avoid_: hub-native, hub-native project, hub workspace, hub-project, hosted project, cloud project

**Linked project**:
A project whose workspace is a pre-existing path on a broker machine, linked to a Hub for cross-broker visibility — as opposed to a Hub-managed project. May be plain or git-backed.
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

**Secret**:
A credential made available to an agent at runtime (e.g. API keys, tokens). A harness-config's `secrets` field *declares* which secrets an agent needs; the **Secret Backend** — a pluggable store (local SQLite for development, GCP Secret Manager in production, selected via `SCION_SERVER_SECRETS_BACKEND`) — *stores and resolves* them, scoped by user, project, runtime broker, or hub, and injects them into the container. Also holds the Hub's signing keys.
_Avoid_: credential, vault, secret store, env secret
_See also_: Harness-config, Profile

## Users & Access

**Group**:
A named collection of Hub users (and nested groups) used by the Hub permissions system to assign access. This is the primary meaning of "group" in Scion.
_Avoid_: team, org, role
_See also_: Message Group (different concept — message recipients, not users)

## Messaging

**Message Group**:
A set of recipients addressed by a single send, correlated by a shared `group_id`, as opposed to a direct message to one recipient or a broadcast to all agents in a project.
_Avoid_: group, set, group chat, room, thread
_See also_: Group (different concept — hub users, not recipients)

**Notification**:
An event delivered when an agent reaches a tracked trigger activity (e.g. `completed`, `waiting_for_input`, `limits_exceeded`). Recipients register a **Subscription** — scoped to a single agent or to a whole project, naming which trigger activities fire it and whether an agent or a user receives it. Backs `scion notifications` and the `--notify` flag on `scion message`.
_Avoid_: alert, event (for the notification), watch (for the subscription)
_See also_: Activity (notifications fire on activity values)

## Identity & State

**Project ID**:
A project's unique identifier — always a randomly generated UUID. A git remote is associated metadata, not identity, so multiple projects may share the same remote by design.
_Avoid_: grove ID, project key, repo ID, slug

**Ancestry chain**:
The tracked `root → parent → child` relationship between agents that governs transitive access control.
_Avoid_: lineage, hierarchy, agent tree, family

**Phase**:
The infrastructure lifecycle stage of an agent container, from `created` through `running` to `stopped` or `error`.
_Avoid_: status, stage, lifecycle state
_See also_: Activity (what the agent is *doing*, vs. its container stage)

**Activity**:
What a running agent is currently doing, such as `thinking`, `executing`, `waiting_for_input`, or `blocked`. Distinct from phase.
_Avoid_: status, state, mode
_See also_: Phase (the container stage), Blocked (a specific activity value)

**Blocked**:
The activity an agent assigns to itself when intentionally waiting for an expected event, so it is not mistaken for stalled.
_Avoid_: stalled, stuck, idle, waiting
_See also_: Activity (Blocked is one of its values)

## Modes

The three run modes at a glance — distinguish them by whether a server runs and who it serves:

| Mode | Server | Tenancy | State & isolation | Canonical use |
|------|--------|---------|-------------------|----------------|
| **Local mode** | None | Single user | Local machine; isolation via git worktrees | Agents launched directly via the `scion` CLI, no server |
| **Workstation mode** | Combo server (Hub + Runtime Broker + Web) on loopback | Single-tenant | That machine | The hosted experience locally, on your own machine |
| **Hosted mode** | Multi-user server deployment | Multi-user | Hub-coordinated across brokers | Coordinating state across users, projects, and runtime brokers |

**Local mode**:
Running Scion with no server at all — agents launched directly via the `scion` CLI, with state on the local machine and isolation via git worktrees.
_Avoid_: solo mode, standalone mode, single-user mode, workstation mode

**Workstation mode**:
Running a single-tenant Scion server (Hub + Runtime Broker + Web combined) on your own machine, giving the hosted experience locally over loopback. A local server, not the no-server CLI workflow.
_Avoid_: local mode, local server, dev mode, single-user mode

**Hosted mode**:
The umbrella term for running against a Hub that coordinates state across users, projects, and runtime brokers; a multi-user server deployment is the canonical example.
_Avoid_: hub mode, cloud mode, distributed mode, production mode

## Operations

**Attach**:
Connecting an interactive terminal to a running agent's tmux session for human-in-the-loop interaction; the agent keeps running once detached.
_Avoid_: connect, join, ssh in

**Dispatch**:
The Hub handing an agent lifecycle command to the appropriate runtime broker for execution.
_Avoid_: schedule, route, assign, delegate

**Schedule**:
A time-based trigger that fires an action — sending a message or dispatching (starting) an agent from a template — either once (a one-shot *scheduled event*, via `--at <time>` or `--in <duration>`) or repeatedly (a recurring *schedule* on a 5-field cron expression). Backs `scion schedule` and the `--at`/`--in` flags on `scion message`.
_Avoid_: cron job (recurring only), scheduled message (too narrow), reminder, timer
_See also_: Dispatch

## Potential Future Additions

Terms that recur in the codebase and may warrant canonical entries, but are **not yet defined** here. Listed so they aren't lost; promote to full entries (verified against the code) as the glossary matures.

- **Task** — the unit tracked by sciontool's task management; referenced operationally but not yet given a canonical definition.
- **Capability** — a declarative harness feature flag (e.g. `max_turns`, `session_resume`, `api_key`, `stdio`) describing what a harness supports.
- **Resource Spec** — per-agent CPU/memory/disk request and limit (Kubernetes-style), applied per agent or via a Profile.
- **Label** — `scion.*` key/value metadata attached to agents and containers for identification and filtering.
- **Access Policy / scope** — the Hub authorization model: allow/deny rules at `hub`, `project`, or `resource` scope (the mechanism that grants a Group its access).
- **Image registry** — the container image registry agents pull from (a Profile field).
- **Session / tmux session** — the per-agent tmux session an Attach connects to.
- **Service** — a background/sidecar process declared by a Template alongside the agent (currently referenced in the Template entry but undefined).
- **Content Hash / Manifest** — SHA-based identity for template and harness-config content, used for cache validation and transfer between Hub and runtime brokers.
- **Transfer** — the signed-URL mechanism that moves templates and harness-configs between the Hub and runtime brokers.
- **Service Account** — a GCP identity an agent can assume for Google Cloud auth, registered with the Hub.
- **Webhook / GitHub App** — external event triggers (e.g. GitHub webhooks) that can dispatch agents.
- **OAuth provider** — a configurable identity provider (Google, GitHub) for CLI and web authentication.

## Exceptions & Future Cleanup

Known places where the codebase does not yet match this glossary. These are **documented exceptions, not precedents** — always use the canonical term in new work.

### grove → project

"grove" was renamed to **project** but remains throughout the code as intentional backward-compat: JSON wire fields (`groveId`, `grove`), container labels (`scion.grove*`), env vars (`SCION_GROVE*`), NATS topics (`scion.grove.<id>.*`), SQL schema history, deprecated `--grove` CLI flags, and `/api/v1/groves/*` endpoints.

- **Safe now:** internal variable/function names, comments, log messages, and design docs may be renamed `grove` → `project` freely.
- **Future cleanup (needs coordination):** wire-format JSON tags, container labels, env vars, NATS topic prefix, and SQL schema should converge to `project` only at a breaking-change/version boundary.

### Bare "broker" is ambiguous (three-way)

"broker" is unqualified across three distinct concepts:

- `scion broker` CLI command → **Runtime Broker**
- `PluginTypeBroker` / broker plugins → **Message Broker** (the canonical messaging-integration system)
- `pkg/broker` package (NATS-style pub/sub) → the **Event Bus** (see next item), *not* the Message Broker

- **Safe now:** qualify "broker" in all new prose and identifiers; reserve **Message Broker** for the messaging-integration system only.
- **Future cleanup (needs coordination):** rename `pkg/broker` to the **Event Bus** (e.g. `pkg/eventbus`), and consider qualifying the `scion broker` command, so bare "broker" no longer spans three meanings.

### Event Bus: rename `pkg/broker`

The NATS-style pub/sub in `pkg/broker` is the **Event Bus** — a latent capability for brokering and dispatching real-time change events to live web views (via server-sent events) as the Hub moves to a more stateless serving model. It is *not* the Message Broker and should not be called one.

- **Future cleanup:** rename `pkg/broker` and its identifiers to **Event Bus** (e.g. `pkg/eventbus`) so the code reflects the term and is not confused with the Message Broker.

### Reconcile `hub-native` / "Hub Workspace" → Hub-managed

The same concept — a project whose workspace is created and managed in the hub-controlled broker FS — is called **`hub-native`** in code (`ProjectType = 'hub-native'`) and **"Hub Workspace"** in the web UI (new-project dialog `value="hub"`, `git-remote-display.ts`). Canonical term is now **Hub-managed project** (workspace: **Hub-managed workspace**).

- **Future cleanup:** converge both to "hub-managed" — web `ProjectType`/labels (`web/src/shared/types.ts`, `project-create.ts`, `git-remote-display.ts`), Go identifiers/comments (`findAgentInHubNativeProjects`, `pkg/runtimebroker/handlers.go`, storage paths in `pkg/storage/storage.go`), and docs — keeping wire/string-value backward-compat where the type string crosses the API.

### Remove harness plugins

Harness configurations are no longer offered via plugin — only **Message Broker** plugins remain (and the plugin system may gain other types in future, but not harness). The harness plugin type still exists in code: `PluginTypeHarness` (`pkg/plugin/config.go`), its discovery/settings wiring (`pkg/plugin/discovery.go`, `settings.go`), `harness_plugin.go`, and the `plugin` branch of `Implementation` in `pkg/harness/resolve.go`.

- **Future cleanup:** remove all harness-plugin code (`PluginTypeHarness` and the `plugin` harness implementation path), leaving the plugin system broker-only (plus room for future plugin types).

### Migrate built-in provisioning → container-script

The canonical harness provisioning model is the **Container-script provisioner**, which makes harness-configs extensible like templates. Some harness types still use the compiled-in **built-in** provisioner (`Implementation: builtin`, e.g. `pkg/harness/claude/embeds/config.yaml: type: builtin`); OpenCode has already moved to `container-script`.

- **Future cleanup:** migrate the remaining built-in harness types to the container-script provisioning model so all harness provisioning is script-based and extensible, then retire the built-in provisioner path.

### Reconcile project identity, type, and workspace axes

The project model overloads several independent concepts and still carries a stale identity story:

- **Identity is already random UUIDs**, but stale language implies a deterministic UUID v5 from the git remote (`pkg/config/settings.go:950-951`; `HashProjectID` in `pkg/util/git.go` is explicitly *no longer used for project IDs*). `Project.GitRemote` is non-unique — "multiple projects may share the same remote." Multiple projects per repo is **by design** (notably for git-backed Hub-managed shared workspaces).
- **`ProjectType` is muddled:** the constants define only `linked` and `hub-native`, but `Project.ProjectType`'s comment lists "git", "linked", or "hub-native" — folding a git-backing flag into the type.
- **Orthogonal axes are conflated:** (a) workspace ownership = **Linked** vs **Hub-managed**; (b) git-backing = has `GitRemote` or not; (c) workspace sharing = shared vs per-agent (`scion.dev/workspace-mode`). `IsSharedWorkspace()` returns true only for git + shared, yet Hub-managed plain projects *also* share one managed workspace — so "shared" is applied inconsistently.
- **Git-backing is inferred at runtime, not declared:** code branches on the presence of a `.git` dir (`os.Stat(.../.git)` at `pkg/agent/provision.go:69`, `util.IsGitRepoDir` at `:288`, `util.IsGitRepo` via `pkg/config/init.go`). A plain workspace can silently "transform" into a git one when a repo is initialized inside it, producing mixed-mode agents in a single workspace.
- **Workspace sharing isn't universal:** git worktrees are supported in local mode but not on Hub-managed projects, so the sharing options differ by ownership.

- **Future cleanup / simplification:**
  - Model three explicit, orthogonal attributes — **workspace ownership** (linked | hub-managed), **git-backing** (explicit boolean metadata), and **workspace sharing mode** — and drop the stray "git" `ProjectType` value plus all remaining deterministic-git-URL identity language.
  - Make **git-backing explicit project metadata**; stop inferring it from a `.git` directory at runtime, so a workspace cannot change mode underfoot.
  - Make **workspace sharing a single universal set of modes** for both local and Hub-managed projects — **Shared-plain**, **Worktree-per-agent**, **Clone-per-agent** — replacing the binary `scion.dev/workspace-mode` (shared | per-agent), and make "shared workspace" uniform across Hub-managed plain and git-shared projects. The mode is a **project-level setting chosen at creation, changeable post-creation, and overridable per agent**.

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

### Skills should be template-only

A **Skill** should be contributed only by a **Template**, but provisioning currently also copies skills from the harness-config base layer: `pkg/agent/provision.go:596-605` copies `<harness-config>/skills`, then `:607-618` overlays each template's `skills`. `templates.md` likewise documents skills coming from both templates and harness-configs.

- **Future cleanup:** make templates the sole source of skills — remove the harness-config skill-copy step (and update the docs) so skills are template-only and harness-agnostic, consistent with the [Agent Skills](https://agentskills.io/home) convention.
