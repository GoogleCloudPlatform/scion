# Hub-Managed Groves: Filesystem-Based Workspaces on the Hub

**Created:** 2026-02-23
**Status:** Partially-implemented
**Related:** `git-groves.md`, `sync-design.md`, `hosted-architecture.md`

---

## 1. Overview

Today, groves are created in two ways:

1. **Local-first**: Run `scion grove init` inside a local git checkout, optionally register with the Hub.
2. **Git-URL-first**: Create a grove on the Hub from a remote git URL (per `git-groves.md`). Agents clone the repo at startup.

This document proposes a third mode: **Hub-managed groves** — groves whose workspace is a plain directory on the Hub server's filesystem, with no backing git repository. These groves are created entirely through the web interface (or Hub API), enabling users to spin up agent workspaces without any local machine, CLI, or git hosting involvement.

### Motivation

- **Zero-infrastructure onboarding**: A user with only a browser and a Hub account can create a grove and start agents.
- **Scratch/ephemeral workspaces**: Useful for experimentation, prototyping, or one-off tasks that don't warrant a git repo.
- **Hub-as-IDE foundation**: Lays groundwork for a fully web-based workflow where code lives on the Hub.

### Goals

1. Allow grove creation entirely via the web UI or Hub API — no CLI or git URL required.
2. Workspace directories are managed on the Hub server's local filesystem under `~/.scion/groves/`.
3. Agents provisioned against these groves receive a functional workspace without git clone.
4. The resulting grove is a first-class Hub grove — visible in the web UI, supports agents, templates, and all existing grove operations.

### Non-Goals

- Replacing git-based groves for repositories that already have a remote.
- Providing a full web-based code editor (though this could build toward one).
- Multi-Hub replication or cross-Hub grove sharing.
- Workspace persistence guarantees beyond the Hub server's own storage.

---

## 2. Filesystem Layout

Hub-managed groves are stored under the global Scion directory on the Hub server:

```
~/.scion/groves/
  ├── my-project/
  │   └── .scion/
  │       ├── settings.yaml
  │       ├── templates/
  │       └── agents/
  ├── experiment-alpha/
  │   └── .scion/
  │       └── ...
  └── scratch-2026-02/
      └── .scion/
          └── ...
```

Each grove directory is equivalent to what `scion grove init` produces in a local project — a `.scion/` subdirectory with settings, templates, and agent metadata. The parent directory (`my-project/`) acts as the workspace root.

### 2.1 Directory Naming

The grove directory name is derived from the grove slug. Since slugs are already URL-safe and unique per Hub, they map directly to filesystem directories.

---

## 3. Creation Flow

### 3.1 Conceptual Steps

Creating a hub-managed grove is equivalent to:

```bash
mkdir -p ~/.scion/groves/<slug>
cd ~/.scion/groves/<slug>
scion grove init            # seeds .scion/ directory structure
scion hub enable --hub <this-hub-url>   # links grove to this Hub
```

But executed server-side by the Hub process itself, not by a CLI invocation.

### 3.2 API-Level Flow

1. **User submits** grove creation request via web UI or API with no `gitRemote` field.
2. **Hub server**:
   a. Creates the grove record in the database (existing `createGrove` handler).
   b. Creates the filesystem directory: `~/.scion/groves/<slug>/`.
   c. Runs the equivalent of `config.InitProject()` to seed `.scion/` structure.
   d. Writes grove settings linking to this Hub instance (hub endpoint, grove ID).
   e. Records the filesystem path on the grove record (or a label).
3. **Response** returns the grove object, same as any other grove creation.

### 3.3 Creation Approach

**Decision: Hub calls `InitProject()` directly (library call).**

The Hub server imports `pkg/config` and calls `InitProject(targetDir, harnesses)` directly, then writes the hub settings into the seeded `.scion/settings.yaml`. This works well with creating the config from a web form.

**Rationale:**
- No subprocess overhead.
- No dependency on `scion` binary being available in PATH on the Hub server.
- Type-safe; errors are Go errors.
- The Hub server already imports `pkg/config` for other purposes, and `InitProject()` is a pure filesystem operation with no interactive prompts.

**Considerations:**
- Couples the Hub server code to `pkg/config` initialization logic more tightly.
- Any init-time side effects (e.g., git detection) need to be accounted for in a non-git context.

**Rejected alternatives:**
- **Hub shells out to `scion` CLI** — Subprocess error handling is less ergonomic, and interactive prompts would need to be bypassed. Not worth the indirection.
- **Dedicated minimal handler** — Risk of parallel code paths diverging from local behavior. Could be a later evolution if hub-managed groves develop distinct requirements.

---

## 4. Agent Workspace Provisioning

When an agent is created against a hub-managed grove, the workspace must be made available to the agent container. Hub-managed groves should be treated the same as any local non-git grove — matching existing solo-mode behavior.

### 4.1 Colocated Broker: Direct Bind-Mount

When the Runtime Broker is colocated with the Hub (same machine), the grove directory is bind-mounted into the agent container as `/workspace`, the same way local solo-mode groves work. This is the default provisioning strategy.

```
Container /workspace  →  ~/.scion/groves/<slug>/
```

Multiple agents in the same grove share the same volume mount, matching solo-mode behavior.

### 4.2 Remote Brokers: Workspace Sync via GCS

For remote Runtime Brokers, the Hub uploads the workspace contents to GCS (using the existing sync-design pattern), and the broker downloads them at agent startup — identical to the flow described in `sync-design.md`.

On the remote broker, the workspace is created at the same conventional path (`~/.scion/groves/<slug>/`) as on the Hub server, so the location is consistent across all brokers.

**Trade-offs:**
- Requires GCS bucket configuration.
- Adds latency at agent startup.
- Workspace changes require explicit sync operations.
- Reuses existing infrastructure — no new transfer mechanisms introduced.

---

## 5. Data Model Changes

### 5.1 Grove Record

**Decision: Infer from absence of GitRemote (no schema change).**

If `GitRemote == ""`, the grove is hub-managed. The workspace path is derived conventionally from `~/.scion/groves/<slug>`. No new fields needed.

Hub-managed groves should be treated as much as possible like any local non-git grove on any broker, except at a conventional path location (`~/.scion/groves/<slug>`) instead of an arbitrary pre-existing path that gets stored when a broker is linked.

Labels can be added for metadata if needed without a migration.

### 5.2 Grove ID for Hub-Managed Groves

Git-anchored groves use a deterministic hash of the normalized git URL. Hub-managed groves have no URL to hash, so they should use a generated UUID — which is already the fallback in `GenerateGroveID()` when no git remote is found.

### 5.3 populateAgentConfig Changes

The existing `populateAgentConfig()` in `pkg/hub/handlers.go:4397` populates `GitClone` config when `grove.GitRemote != ""`. For hub-managed groves, it should instead set the `Workspace` field to the grove's filesystem path (for colocated brokers) or set `WorkspaceStoragePath` (for remote brokers after sync upload).

---

## 6. Resolved Questions

### Q1: Should hub-managed groves auto-initialize a git repo?

**Decision: No, not for now.** Hub-managed groves should mirror the behavior of other non-git workspaces. Auto-initializing git is a potential future improvement, possibly as an optional argument at creation time.

### Q2: How should the Hub server discover its own URL for `hub enable`?

**Decision: Read from `~/.scion/settings.yaml`.** The Hub server's own endpoint URL can be retrieved from the global settings file, which is already configured when the Hub is set up.

### Q3: What happens when a hub-managed grove is deleted?

**Decision: Full cleanup.** The grove record is deleted from the database, and the filesystem directory at `~/.scion/groves/<slug>/` is removed. When `deleteAgents=true` is passed, agent deletions are dispatched to their runtime brokers before the grove record is deleted (so containers are stopped and agent files are cleaned up). Database cascade handles removal of agent, template, and provider records.

### Q4: Should hub-managed groves be promotable to git-remote groves?

**Decision: Deferred.** This is a strong candidate for a future improvement — converting a hub-managed grove to a git-anchored grove by initializing git, adding a remote, pushing, and updating the grove record. Not in initial scope.

### Q5: Can multiple Hubs serve the same hub-managed grove?

**Decision: No.** The grove lives on one Hub's filesystem. This is a single-Hub feature by nature. Remote brokers can serve agents for it via sync, but the source of truth is one Hub's disk.

### Q6: Storage limits and quotas

**Decision: Deferred** to a future improvement. Not needed for initial implementation, but becomes important for multi-tenant deployments.

### Q7: Workspace seeding / initial content

**Decision: Start empty.** Hub-managed groves are created with an empty workspace. Content can be added via the existing CLI sync mechanism. File upload and starter templates are deferred to a later phase.

---

## 7. Web UI Considerations

### 7.1 Grove Creation Form

The grove creation form should include a **distinct mode selector** for choosing the grove type:

- **Git Repository** — Existing flow: user provides a git URL.
- **Hub-managed Workspace** — New flow: no git URL, creates a hub-managed grove.

The Hub-managed Workspace form should allow the user to specify:
- Grove name (required).
- Slug (auto-derived, optionally overridden).
- Visibility (private/team/public).

### 7.2 Grove Type Indicator

Grove type should be visually distinguishable via a **small glyph on the grove card component**:
- **Git-based groves**: branching icon with mouseover helptext "Git based grove".
- **Hub-managed groves**: folder icon with mouseover helptext "Folder based grove".

The grove detail view should omit the git remote / clone URL section for hub-managed groves.

### 7.3 File Browser

A web-based file browser for hub-managed grove workspaces is a natural follow-on feature but is **out of scope** for this design. Noted as a motivating future use case.

---

## 8. Security Considerations

### 8.1 Filesystem Access

The Hub process creates and manages directories under `~/.scion/groves/`. The Hub is the sole arbitrator of access control for these directories and must avoid acting as a confused deputy — ensuring that API-level authorization checks are enforced before any filesystem operation, so that one user's request cannot access another user's grove directory.

Key considerations:
- The Hub process user must have write access to `~/.scion/groves/`.
- Groves from different users share the same filesystem namespace — slug uniqueness prevents collisions, but the Hub must enforce user-level authorization at the API layer.
- In multi-tenant deployments, consider per-user subdirectories: `~/.scion/groves/<user-id>/<slug>/`.

### 8.2 Path Traversal

The grove slug is used as a directory name. The slug derivation (`api.Slugify()`) must guarantee no path traversal characters (`..`, `/`, etc.) can appear in the slug. The existing `Slugify` implementation should be audited for this.

### 8.3 Agent Container Isolation

When bind-mounting grove directories into agent containers, standard container isolation applies. Agents should not be able to escape their mount to access other groves' directories.

---

## 9. Implementation Phases

### Phase 1: Minimal Hub-Managed Grove
completed

- Hub API accepts grove creation with no `gitRemote`.
- Hub creates `~/.scion/groves/<slug>/` directory with `InitProject()`.
- Writes hub settings into `.scion/settings.yaml` (hub endpoint read from global settings).
- Colocated broker bind-mounts the grove directory for agents (shared mount, matching solo-mode).
- Web UI grove creation form with distinct mode selector for git vs hub-managed workspace.
- Grove card glyph distinguishing git-based and folder-based groves.

### Phase 2: Remote Broker Support
completed

- Hub uploads workspace to GCS for remote broker provisioning.
- Remote broker creates workspace at `~/.scion/groves/<slug>/`.
- Reuses `sync-design.md` infrastructure.
- Workspace sync back from agents to Hub filesystem.

### Phase 3: Workspace Content Seeding
completed

- Web UI allows uploading initial files into a hub-managed grove.
- Hub API endpoint for uploading files to a grove's workspace.
- Optional starter templates for common project types.

### Phase 4: Grove Promotion
pending

- Convert a hub-managed grove to a git-anchored grove.
- Initialize git, add remote, push existing content.
- Update grove record with `GitRemote`.

### Future Improvements

- **Optional git init at creation**: Allow an optional `--git` argument when creating hub-managed groves to auto-initialize a git repository.
- ~~**Filesystem purge/prune**: Hub admin/maintenance tooling to clean up filesystem directories for soft-deleted groves.~~ (Implemented: grove deletion now removes filesystem directories.)
- **Storage quotas**: Per-user and per-grove disk usage limits for multi-tenant deployments.
- **Web file browser**: Browse and edit grove workspace files through the web UI.

---

## 10. Relationship to Existing Designs

| Design | Relationship |
|--------|-------------|
| `git-groves.md` | Complementary — git groves use clone, hub-managed groves use local filesystem. Same grove API, different workspace strategy. |
| `sync-design.md` | Hub-managed groves use workspace sync for remote broker support (Phase 2). |
| `hosted-architecture.md` | Hub-managed groves fit the grove-centric model. The Hub is both state server and workspace host. |
| `secrets.md` | Hub-managed groves use the same secret management. No git tokens needed, but API keys and other secrets still apply. |
