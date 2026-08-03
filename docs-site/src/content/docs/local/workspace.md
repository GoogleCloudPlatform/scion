---
title: About Workspaces
---

Every Scion agent has a dedicated **Workspace**, mounted at `/workspace` inside the agent's container. This is where the agent reads code, makes changes, and runs commands.

Scion provides flexible options for how this workspace is backed on your host machine, ranging from isolated git worktrees to direct directory mounts.

## Workspace Resolution

When you start an agent, Scion determines its workspace based on the following precedence:

1.  **Explicit Workspace** (`--workspace` flag):
    If you provide a path via `--workspace`, Scion mounts that directory directly. This works in both Git and non-Git environments.

2.  **Git Worktree** (Git repositories):
    If you are in a Git repository and do not provide an explicit workspace, Scion uses [Git Worktrees](https://git-scm.com/docs/git-worktree) to give the agent its own isolated working directory and branch.

3.  **Project Root / CWD** (Non-Git environments):
    If you are not in a Git repository, Scion mounts the project root (or current directory for global agents) directly.

---

## 1. Explicit Workspaces (`--workspace`)

You can tell Scion exactly which directory to use as the workspace. This is useful for:
- Working on a specific subfolder.
- Using a shared directory across multiple agents.
- Working on a path outside the current repository without creating a worktree.

### Absolute vs. Relative Paths

The `--workspace` (or `-w`) flag accepts both **absolute host paths** and **project-relative paths**, distinguished automatically by `filepath.IsAbs()`:

#### Absolute Paths

If you specify an absolute host path, Scion mounts that exact directory directly.

```bash
# Mount a specific absolute host path
scion start my-agent "run analysis" --workspace /home/user/my-service
```

- **Behavior**: The specified directory is mounted directly to `/workspace`.
- **Isolation**: **None**. Changes made by the agent are immediately visible on the host and to any other agents sharing this directory.
- **Git**: No new worktree or branch is created, even if inside a repo.

#### Relative Paths (Contained Subdirectory Mounts)

If you specify a relative path, it is interpreted as a subdirectory scoped to your project's **logical root** (or current working directory). This allows you to confine an agent to a specific subdirectory, such as a package in a monorepo:

```bash
# Scope the agent to a project subdirectory
scion start my-agent "fix web bugs" --workspace packages/web
```

- **Behavior**: Scion resolves the subdirectory path against the project root, performs strict containment checks (preventing directory traversal and escaping via symlinks), and mounts only that subtree to `/workspace`.
- **Isolation**: The agent has **full containment** — it cannot see or access any files or folders outside of that specific subdirectory.
- **Support**: Supported for directory (non-git) projects — both linked and hub-managed.
- **Git-Clone Projects Limitation**: Relative `--workspace` paths are **not supported** for per-agent git (clone-based) projects and will be rejected with an error. For those projects, use an absolute path or omit the `--workspace` flag.

---

## 2. Git Worktrees (Automatic Isolation)

When working inside a Git repository without an explicit `--workspace`, Scion automatically manages **Git Worktrees**. This ensures that each agent has its own isolated checkout of the code, allowing them to work on different branches simultaneously without interfering with your main working directory.

### Prerequisites
- Git **2.47.0** or newer is required (for relative path support).

### Branch Resolution
Scion determines which branch to check out in the worktree:

1.  **Explicit Branch** (`--branch`, `-b`):
    ```bash
    scion start my-agent -b feature/login "add logging"
    ```
    - If the branch exists and has a worktree, Scion **reuses the existing worktree** (see below).
    - If the branch exists but has no worktree, Scion creates a new worktree for it.
    - If the branch doesn't exist, Scion creates it (based on current HEAD) and a worktree.

2.  **Agent Name Matching**:
    If you don't specify a branch, Scion checks if a branch named after the agent exists (e.g., `my-agent`).
    - **Match Found**: It behaves exactly as if you passed `-b my-agent`.
    - **No Match**: Scion creates a new branch named `my-agent` and a corresponding worktree.

### Reusing Existing Worktrees
If you request a branch that is already checked out in another worktree (e.g., by another agent or manually created), Scion detects this.
- Instead of failing or creating a conflict, Scion **mounts the existing worktree path**.
- A warning is displayed: `Warning: Relying on existing worktree for branch '...'`.
- This allows multiple agents to collaborate on the same branch/worktree if desired.

---

## 3. Non-Git Environments

In non-git projects (where no `.git` directory is found):
- Scion defaults to mounting the **project root** (the directory containing `.scion`).
- For global agents, it defaults to the **current working directory**.
- All agents share the same files. There is no isolation or branching.

---

## 4. Hub-Managed Workspaces

When a Scion Hub is enabled, workspace strategy changes depending on the project type. The Hub supports three types of remote workspaces:

### Hub-Managed Projects (no git repository)
Hub-Managed projects allow you to create project workspaces directly through the Hub API and Web Dashboard **without an external Git repository**.
- The Hub automatically initializes a seeded `.scion` structure.
- Workspace files are managed locally by the Hub and its distributed runtime brokers.
- You can directly download individual workspace files or generate ZIP archives of entire projects using the Hub API or Web Dashboard, making it easy to export your data.

### Git Projects (clone-based, Hub-managed)
Projects created from a remote git repository URL use **clone-based provisioning**: the agent's workspace is initialized from the repository at startup.

```bash
# Create a git project from a URL (Hub-managed)
scion hub project create https://github.com/org/repo.git
```

#### How Git Projects Work

1. The Hub stores the git remote URL and default branch as project metadata.
2. When an agent starts, the Runtime Broker injects `SCION_GIT_CLONE_URL`, `SCION_GIT_BRANCH`, and `SCION_GIT_DEPTH` as environment variables.
3. The `sciontool init` process inside the container uses a `git init` + `git fetch` strategy to provision the workspace into `/workspace`. This approach handles workspaces that may already contain `.scion` metadata or `.scion-volumes` directories, and properly clears stale artifacts before initialization.
4. A feature branch `scion/<agent-name>` is created and checked out automatically.

#### Project ID Format

Project IDs are always randomly generated UUIDs (v4). A git remote is associated metadata, not identity, so the same repository can back multiple projects, and re-creating a project from the same URL produces a new ID.

#### Agent Branch Strategy

Each agent gets its own branch named `scion/<agent-name>`. This prevents conflicts when multiple agents work on the same repository concurrently.

#### Shallow Clones

By default, git projects use a shallow clone with `depth=1` for fast startup. If an agent needs full history (e.g., for `git log` or `git blame`), it can fetch the rest:

```bash
git fetch --unshallow
```

#### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_GIT_CLONE_URL` | HTTPS URL of the repository to clone | *(required)* |
| `SCION_GIT_BRANCH` | Branch to clone | `main` |
| `SCION_GIT_DEPTH` | Clone depth | `1` |

Authentication is handled via the `GITHUB_TOKEN` environment variable, injected from the project's secrets or your local environment through the env-gather flow.

### Linked Projects (clone-based, even when the repo is local)

When you link an existing local git project to a Hub (`scion hub link`), the project becomes **Hub-managed**. Once linked, **all agents started via the Hub use clone-based provisioning**, even if the broker machine already has the repository checked out locally.

This is intentional: the Hub enforces a consistent, unambiguous workspace strategy for all git-based projects. Local worktrees are a local-mode feature only.

**What this means in practice:**

- **SSH credentials are not used** for workspace provisioning. Even if your machine has SSH keys configured for the repo, agents always clone via HTTPS using `GITHUB_TOKEN`.
- **A `GITHUB_TOKEN` is required.** Set it as a project or user secret on the Hub, or ensure it is present in your local environment (the env-gather flow will collect it):
  ```bash
  scion hub secret set --project my-project GITHUB_TOKEN=ghp_xxxxxxxxxxxx
  ```
- **The CLI will tell you** when this mode is in effect. When starting an agent via a Hub-linked git project, you will see:
  ```
  Using hub, cloning repo https://github.com/org/repo.git
    (Hub mode uses HTTPS clone with GITHUB_TOKEN; local worktrees are not used)
  ```
- **Merging agent work** is done via git push and pull request, not by merging worktrees back into your local checkout.

To return to local worktree-based mode, disable hub integration:

```bash
scion hub disable
# or run with --no-hub
scion start my-agent --no-hub "fix the bug"
```

---

## 5. Project Shared Directories

Project Shared Directories provide a persistent, mutable storage layer that can be shared between multiple agents within a single project. This is ideal for sharing build artifacts, shared caches, or state files without relying on version control or the Hub database.

### Default Scratchpad Auto-Provisioning

To ensure that newly created projects have an immediate space for agent file transfers, message attachments, and logs, the Hub can automatically provision a default shared directory named `scratchpad` upon project creation.

- **Auto-Provisioning Toggle**: This behavior is controlled by the `project_defaults.default_scratchpad` setting in the Hub's operational settings (default is **`ON` / `true`**).
- **Inbound Message Attachments**: In isolated workspace modes, Scion automatically routes agent message attachments through the default `scratchpad` shared volume to prevent silent delivery failures.

### Managing Shared Directories

You can manage shared directories using the `scion shared-dir` CLI commands:

```bash
# Create a new shared directory
scion shared-dir create <name>

# List shared directories in the current project
scion shared-dir list

# View details about a specific shared directory
scion shared-dir info <name>

# Remove a shared directory (permanently deletes contents)
scion shared-dir remove <name>
```

### Mounting Shared Directories

When an agent is created in a project that has shared directories, they are automatically mounted into the agent's container.

Each shared directory's mount location depends on its `in_workspace` setting:
- **`in_workspace: false` (default):** `/scion-volumes/<name>` — mounted at the standard volume path, outside the workspace.
- **`in_workspace: true`:** `/workspace/.scion-volumes/<name>` — mounted inside the workspace tree.

A shared directory is mounted at one of these locations, not both.

### Common Patterns

Shared directories are most useful when several agents in a project need to exchange data outside of version control. The patterns below show typical setups.

#### Shared Build Caches

Agents working on the same project can reuse a compilation cache instead of rebuilding from scratch, dramatically speeding up warm builds.

```bash
# Create a shared cache, mounted outside the workspace (default)
scion shared-dir create build-cache
```

Point the toolchain's cache at the mount. For a Go project, set `GOCACHE` to the shared path in each agent (for example, in the agent's shell profile or start command):

```bash
export GOCACHE=/scion-volumes/build-cache
```

The same approach works for other caches — for example a Bazel `--output_base=/scion-volumes/build-cache`, or a `node_modules/.cache` symlinked into the mount. All agents read from and write to the same cache, so the first agent to compile a target populates it for the rest.

#### Shared Artifacts (Producer/Consumer)

One agent produces build artifacts or data files that another consumes — a build/test or build/deploy split.

```bash
scion shared-dir create artifacts
```

The **producer** agent writes its output to the mount:

```bash
go build -o /scion-volumes/artifacts/myservice ./cmd/myservice
```

The **consumer** agent picks the artifact up from the same path for testing or deployment:

```bash
/scion-volumes/artifacts/myservice --run-tests
```

Because the mount is a plain directory, no Hub round-trip or git commit is needed to hand data between agents.

#### Shared Context / Knowledge Base

A directory of reference material — design docs, research findings, or generated context — that every agent can read, and selected agents can write to.

```bash
# Read/write for agents that curate the knowledge base
scion shared-dir create knowledge --in-workspace
```

With `--in-workspace`, the directory mounts at `/workspace/.scion-volumes/knowledge`, keeping reference material alongside the code an agent is editing. A research agent writes its findings:

```bash
echo "## API rate limits: 100 req/s per token" >> /workspace/.scion-volumes/knowledge/notes.md
```

A developer agent reads them while working. To make a knowledge base read-only for consumers so they cannot accidentally overwrite curated content, create it with `--read-only`:

```bash
scion shared-dir create knowledge --read-only
```

#### Coordination Files

Lightweight, file-based signalling between agents — status markers, lock files, or simple message passing — without a message broker.

```bash
scion shared-dir create coordination
```

For example, a build agent signals that a phase is complete by writing a marker file:

```bash
# Producer signals completion
touch /scion-volumes/coordination/build.done
```

A downstream agent waits for the marker before starting its work:

```bash
# Consumer waits for the signal
until [ -f /scion-volumes/coordination/build.done ]; do sleep 5; done
```

The same pattern supports simple lock files (create a file to claim a resource, remove it to release) or drop-box message passing (write a request file, poll for a response file). Keep coordination files small and treat them as ephemeral signals rather than durable state.

### Web Dashboard File Viewer

You can browse the contents of Shared Directories and view file previews directly from the Hub's Web Dashboard. In the project view, navigate to the Shared Directories tab to inspect files, view sizes, and review content previews without needing to attach to an agent.

### Storage Backends
- **Local Workstations:** Backed by directories on the host filesystem.
- **Kubernetes:** Backed by PersistentVolumeClaims (PVCs) with project-scoped lifecycle management, ensuring data persists across pod restarts and can be accessed by any agent in the project.

---

## The `cdw` Command

Scion provides a helper command, `cdw` (Change Directory to Worktree), to quickly navigate to an agent's workspace on your host.

```bash
scion cdw <agent-name>
```

- Spawns a new shell inside the agent's workspace directory.
- Works for both managed worktrees and manual mounts (if resolvable).

Will also take a branch/worktree name outside of scion agents, most useful for getting back to main.

```bash
scion cdw <agent-name>
```

## Cleanup

When you delete an agent:
```bash
scion delete <agent-name>
```
- **Worktrees**: The worktree directory is removed and git metadata is pruned.
- **Branches**: By default, the branch is deleted. Use `--preserve-branch` (or `-b`) to keep it.
- **Explicit Workspaces**: Directories mounted via `--workspace` are **NOT** deleted. Scion only cleans up resources it created.

:::caution[Committed is not Pushed]
Committing changes within your agent's workspace is **not** an automatic guarantee of safety.
- **`--preserve-branch` does not push:** The `--preserve-branch` (or `-b`) flag only keeps the agent's branch inside the *local* repository clone on the host/broker. It does **not** push the branch to a remote origin repository.
- **Avoid losing work:** If you are operating on an ephemeral or remote broker container (where the local clone is discarded when the agent is deleted), any unpushed commits on the agent's branch will be **permanently lost**. Always ensure your agents run `git push` (or you push manually from the workspace) to save your commits upstream before deleting the agent.
:::
