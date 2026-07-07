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

## Related workspace concepts

A few adjacent terms are worth distinguishing from the sharing mode itself:

- **Shared directory** — a persistent, mutable volume shared by the agents within one project, separate from each agent's `/workspace`.
- **Agent home** — the directory mounted as the container user's home folder, holding that agent's unique config and history.

Both are independent of which sharing mode a project uses.

## See also

- [About Workspaces](/scion/local/workspace/) — the operational guide to worktrees, mounts, and host-side backing.
- [Core Concepts](/scion/concepts/) — how workspaces fit alongside agents, projects, and the Hub.
- [Glossary](/scion/glossary/) — canonical definitions for every term used here.
