---
title: Workspaces & Sharing Modes
description: The three workspace sharing modes — Shared-plain, Worktree-per-agent, and Clone-per-agent — that decide how a project's agents share (or isolate) their working directory.
---

Every Scion **agent** runs against a **workspace** — the working directory mounted into its container at `/workspace`, where it reads code, makes changes, and runs commands. When a project runs several agents at once, a key question follows: do they share one directory, or does each get its own?

Scion answers this with a project-level setting called the **workspace sharing mode**. There is **one universal set of three modes**, intended for both local and Hub-managed projects. This page explains each mode and when to use it. The definitions follow the canonical [`GLOSSARY.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/GLOSSARY.md).

:::note[Three modes, not two]
Earlier documentation framed workspaces as "two strategies" (worktrees vs. a git-init clone). That framing is superseded. The current model is **three sharing modes** — **Shared-plain**, **Worktree-per-agent**, and **Clone-per-agent** — described below.
:::

## The three sharing modes

### Shared-plain

One workspace directory is mounted into **every agent, with no per-agent isolation**. All agents in the project see and modify the same files at the same time.

This is the model used for **plain (non-git) projects**, where there is no git history to branch from. It suits data directories, document sets, and other non-source content that a group of agents collaborate on directly.

- **Isolation:** none — agents share one directory.
- **Requires git:** no.
- **Best for:** plain projects; tasks where agents are meant to work on a common, shared set of files.

### Worktree-per-agent

Each agent gets its own [git worktree](https://git-scm.com/docs/git-worktree) over a **shared checkout**, isolating working trees while sharing one clone's history.

Every agent operates on the same repository history but has an independent working directory (typically created under `../.scion_worktrees/<project>/<agent>` on a dedicated branch) mounted as `/workspace`. Agents cannot step on each other's uncommitted changes, and their work is merged back to the main branch manually (for example, `git merge <agent-branch>`).

- **Isolation:** per-agent working tree; shared history.
- **Requires git:** yes.
- **Availability:** supported in **local mode** today; not yet available on Hub-managed projects.
- **Best for:** local git projects where multiple agents work in parallel on the same repository.

### Clone-per-agent

Each agent gets its **own full git clone** of the repository — the strongest isolation of the three.

When a Hub manages a git-based project, agents are provisioned with an independent clone via a robust `git init` + `git fetch` strategy rather than a shared worktree. The broker injects `SCION_GIT_CLONE_URL`, `SCION_GIT_BRANCH`, and a `GITHUB_TOKEN`; `sciontool init` then initializes the workspace, fetches the repo over HTTPS, and checks out a `scion/<agent-name>` branch. This strategy is consistent across all broker machines, whether or not the repo already exists locally, and cleanly handles workspaces that already contain `.scion` metadata.

- **Isolation:** full — each agent has its own clone.
- **Requires git:** yes (and a `GITHUB_TOKEN`; host SSH credentials are not used).
- **Best for:** Hub-managed git projects, and any case where agents need completely independent checkouts across machines.

## Choosing a sharing mode

| | Shared-plain | Worktree-per-agent | Clone-per-agent |
|---|---|---|---|
| **Isolation** | None (shared dir) | Per-agent working tree | Full per-agent clone |
| **Git required** | No | Yes | Yes |
| **Shares history** | n/a | Yes (one clone) | No (independent clones) |
| **Typical setting** | Plain projects | Local git projects | Hub-managed git projects |

A useful rule of thumb:

- **No git, collaborate on shared files** → **Shared-plain**.
- **Local git repo, parallel agents, one shared history** → **Worktree-per-agent**.
- **Hub-managed git project, or agents that need fully independent checkouts** → **Clone-per-agent**.

Note that the same git project used locally with worktrees may switch to clone-based provisioning once it is managed by a Hub, because Worktree-per-agent is not yet supported for Hub-managed projects.

## Runtime environment variables

Agents can discover their workspace provisioning at startup through two environment variables emitted by the broker into every container.

### `SCION_WORKSPACE_MODE`

The canonical workspace sharing mode for the project. Always present; defaults to `shared-plain` when no mode label is set.

| Value | Description |
|---|---|
| `shared-plain` | One workspace directory shared by all agents (no per-agent isolation). |
| `clone-per-agent` | Each agent has its own full git clone. |
| `worktree-per-agent` | Each agent has its own git worktree over a shared checkout. |

**Example:** An agent in a Hub-managed git project reads `SCION_WORKSPACE_MODE=clone-per-agent` to know it has a private checkout and can safely commit without affecting other agents.

### `SCION_WORKSPACE_GIT`

Present (value `"true"`) when the workspace is a git repository. Absent when the workspace is not git-backed.

`SCION_WORKSPACE_GIT` is separate from `SCION_WORKSPACE_MODE` because `shared-plain` can be either git-backed or a plain directory — the mode alone cannot disambiguate. Absent means false; there is no `"false"` string value.

**Compatibility note:** Older broker versions emitted only `SCION_SHARED_WORKSPACE=true` for shared-plain git workspaces. The `sciontool init` compat shim prefers the new vars and falls back to `SCION_SHARED_WORKSPACE` when they are absent. `SCION_SHARED_WORKSPACE` is deprecated and tracked for removal in [ptone/scion#575](https://github.com/ptone/scion/issues/575).

---

## Workspace Orientation & Invariants (Mandatory Boilerplate)

All Scion agents should run a **workspace orientation check** immediately upon startup. By reading `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT`, an agent can adapt its behavior to match its level of isolation.

### 1. Per-Mode Behavior Guidelines

Depending on the effective workspace sharing mode, agents must adhere to the following rules:

* **When `SCION_WORKSPACE_MODE` is `shared-plain`**:
  * **Concurrency Hazard**: You are sharing a single filesystem directory with other agents in real-time. Edits you make are immediately visible to others, and others may modify files while you are working.
  * **Non-Assumption**: Do *not* assume a file remains unchanged between the time you read it and the time you write it.
  * **Coordination**: Before making broad, structural, or disruptive filesystem changes (such as renaming directories or refactoring shared libraries), use `scion message` to coordinate with other agents or the project coordinator.
* **When `SCION_WORKSPACE_MODE` is `worktree-per-agent`**:
  * **Private Working Tree**: Your working tree `/workspace` is private and isolated. You can edit files freely without stepping on other agents' uncommitted changes.
  * **Shared Git Repo**: The underlying git repository history, local branches, and refs are shared. Treat local branch names as a shared namespace to avoid collisions.
* **When `SCION_WORKSPACE_MODE` is `clone-per-agent`**:
  * **Full Isolation**: Your clone is entirely your own. Nothing you do to the working tree, local refs, or local branches will affect any other agent's workspace.

### 2. The Shared-Directories Invariant

Scion allows mounting persistent **Shared Directories** (such as a shared cache or scratchpad) into agent containers.

:::danger[Shared Directories are Always Shared]
**Shared directories are shared across all agents in every workspace mode — including the highly isolated `worktree-per-agent` and `clone-per-agent` modes.** They bypass working tree isolation. Treat shared directory paths as concurrent-access, zero-isolation storage.
:::

---

## Git Operations & Safety for Agents

When operating within a git-backed workspace (`SCION_WORKSPACE_GIT=true`), agents must follow strict safety procedures to avoid data loss and build breaks.

### 1. Working Tree Reset Safety (`-fd` vs `-fdx`)

If you need to clean or reset a working tree to get a clean slate, **always default to `git clean -fd`, never `git clean -fdx`**.

* **`git clean -fd` (Safe)**: Removes untracked files but **respects `.gitignore`**. Crucial agent-local state files, `.scion/` directory metadata, and local cached credentials survive the clean.
* **`git clean -fdx` (Dangerous)**: Deliberately ignores `.gitignore` and **deletes everything untracked**. This will wipe your `.scion/` metadata, agent state, and configurations, causing immediate container and agent dysfunction.

### 2. Rebase-After-Deletion Guidance

When a pull request or merge deletes a file, performing a `git rebase` introduces a hidden logical hazard:

* **Standard Conflicts**: If someone concurrently edits the deleted file, git will raise a `modify/delete` conflict and halt the rebase, which is easily detected.
* **Dangling References**: If a concurrent change in a *different* file adds a new reference or call to the deleted file, git sees these as disjoint changes. The rebase will complete with **no conflicts** and report a successful merge, leaving a dangling reference that breaks the build or runtime execution.

**Required Procedure**: After performing any rebase or pull that deletes a file, you must run a repository-wide `grep` search for the deleted file's name/imports to verify that no new dangling references were introduced in other files.

---

## Related workspace concepts

A few adjacent terms are worth distinguishing from the sharing mode itself:

- **Shared directory** — a persistent, mutable volume shared by the agents within one project, separate from each agent's `/workspace`.
- **Agent home** — the directory mounted as the container user's home folder, holding that agent's unique config and history.

Both are independent of which sharing mode a project uses.

## See also

- [About Workspaces](/scion/local/workspace/) — the operational guide to worktrees, mounts, and host-side backing.
- [Core Concepts](/scion/concepts/) — how workspaces fit alongside agents, projects, and the Hub.
- [Glossary](/scion/glossary/) — canonical definitions for every term used here.
