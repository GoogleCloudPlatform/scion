# Worktree Guards: Preventing Nested and In-Container Worktree Creation

**Status**: Analysis
**Date**: 2026-04-13

## Problem Statement

A scion agent running inside its container created multiple git worktrees based off its own worktree. This led to "scrambled" worktree identifiers where multiple agents associated with entries like `workspace22` in `.git/worktrees/`, making it difficult to reason about which worktree belongs to which agent.

The root issue is that nothing prevents worktree creation from inside a container, and several design properties of the current worktree system amplify the resulting confusion.

## Contributing Factors

### 1. All worktree paths share the basename `workspace`

Every agent's worktree is created at `.scion/agents/<name>/workspace` (`pkg/agent/provision.go:268`). Git tracks worktrees internally in `.git/worktrees/` using the **basename** of the worktree path as the entry name. Since every agent workspace has the same basename, git auto-suffixes them: `workspace`, `workspace1`, `workspace2`, ..., `workspace22`.

These numeric suffixes are non-deterministic across sessions. If worktrees are pruned and recreated in a different order, the numbering changes. The entries are opaque when inspected via `git worktree list`.

Scion associates agents with worktrees via **branch name** (`FindWorktreeByBranchIn` at `pkg/util/git.go:408`), not the `.git/worktrees/` entry name, so this doesn't cause functional mis-association at the scion level. But it makes the problem difficult to diagnose when it occurs.

### 2. `RepoRootDir` returns the worktree root, not the main repo root

`CreateWorktree` (`pkg/util/git.go:166-174`) calls `RepoRootDir(filepath.Dir(path))` which uses `git rev-parse --show-toplevel`. Inside a worktree, this returns the worktree's own root, not the original repo root:

```go
func CreateWorktree(path, branch string) error {
    root, err := RepoRootDir(filepath.Dir(path))
    // root = worktree root, not main repo root
    cmd := exec.Command("git", "worktree", "add", "--relative-paths", "-b", branch, path)
    cmd.Dir = root
```

The comment states "We run from root to ensure --relative-paths are calculated from root." Since `path` is always absolute (constructed from `filepath.Join(projectDir, "agents", agentName, "workspace")`), the `cmd.Dir` doesn't actually influence the relative paths that git stores. Git computes relative paths between the worktree's absolute location and the `.git/worktrees/<entry>` directory internally.

Using `GetCommonGitDir` (which calls `git rev-parse --git-common-dir`) and then `filepath.Dir()` would return the true main repo root. This is more semantically correct, though it doesn't fix the container problem since both commands return container-local paths when run inside a container.

### 3. Container mount layout creates path-identity mismatch

This is the most dangerous factor. Worktrees are mounted into containers via `pkg/runtime/common.go:181-189`:

```go
registerMount(filepath.Join(config.RepoRoot, ".git"), "/repo-root/.git", false, true)
containerWorkspace := filepath.Join("/repo-root", relWorkspace)
registerMount(config.Workspace, containerWorkspace, false, true)
```

The container sees the workspace at `/repo-root/.scion/agents/<name>/workspace` and `.git` at `/repo-root/.git`. When an agent inside the container creates new worktrees, `--relative-paths` computes paths relative to the **container's mount layout**, not the host's actual filesystem. These relative paths are valid inside the container but meaningless on the host.

The `--relative-paths` flag (requiring git 2.47.0+) exists precisely to make worktrees portable across mount boundaries. This works correctly for the **primary use case** (host-created worktrees mounted into containers) because the mount layout preserves the relative distances between the worktree and `.git`. But when worktrees are created **inside** the container, the paths baked into `.git` files and `.git/worktrees/` entries reflect the container namespace.

### 4. No guard against recursive worktree creation

There is no check in `CreateWorktree` or `ProvisionAgent` to detect that the current repo root is itself a worktree, or that the process is running inside a container. The existing `checkAgentContainerContext` guard in `cmd/root.go:339-414` blocks most CLI commands when `SCION_HOST_UID` is set, but allows operations through when a non-localhost Hub endpoint is configured (by design, for hub-dispatched sub-agent creation).

More fundamentally, LLM agents have shell access and can run `git worktree add` directly, bypassing all scion-level guards entirely.

## Mitigation Options

### Option 1: Use `GetCommonGitDir` in `CreateWorktree`

**Change:** Replace `RepoRootDir` with `filepath.Dir(GetCommonGitDir(...))` in `CreateWorktree` so `cmd.Dir` is always the main repo root.

**Effort:** One-line change in `pkg/util/git.go:167`.

**Impact:** Minimal. Since the worktree path is absolute, `cmd.Dir` doesn't materially affect behavior. Inside a container, both `--show-toplevel` and `--git-common-dir` return container-local paths, so the result is equivalent.

**Verdict:** Worth doing as a correctness improvement. Does not address the container problem.

### Option 2: Use unique worktree basenames

**Change:** Instead of `.scion/agents/<name>/workspace`, use `.scion/agents/<name>/<slug>-ws` (e.g., `.scion/agents/my-agent/my-agent-ws`).

**Effect:** Git's `.git/worktrees/` entries become `my-agent-ws`, `other-agent-ws` instead of `workspace`, `workspace1`, `workspace22`. Human-readable and stable across prune/recreate cycles.

**Effort:** Moderate. The path `workspace` is referenced in:
- `pkg/agent/provision.go` — path construction (lines 268, 969)
- `pkg/runtime/common.go` — `relWorkspace` computation for container mounts (line 182)
- `pkg/agent/run.go` — `RunConfig.Workspace` derivation
- Agent deletion and worktree cleanup paths

The container workdir calculation in `common.go` uses `filepath.Rel(config.RepoRoot, config.Workspace)`, which is agnostic to the basename, so mount logic would not need changes.

**Verdict:** Quality-of-life improvement for debugging. Makes the problem diagnosable when it occurs but doesn't prevent it.

### Option 3: Prevent in-container worktree creation

The strongest defense. Several sub-approaches:

#### 3a. Scion-level guard in `CreateWorktree`

Detect that the process is running inside an agent container and refuse to create worktrees.

**Detection signals available:**
- `SCION_HOST_UID` env var — set by the runtime when launching containers (`pkg/runtime/k8s_runtime.go:993`). Most reliable signal but only present for scion-managed containers.
- `SCION_AGENT_MODE` env var — set for hosted mode agents.
- `SCION_GROVE_ID` env var — always set for broker-dispatched agents.
- Workspace `.scion` marker file — present in worktree workspaces, written by `WriteWorkspaceMarker` during provisioning (`pkg/agent/provision.go:400`). A marker file (as opposed to a `.scion` directory) indicates we're in a worktree workspace, not the main repo.
- The `.git` file pointing through a mount boundary — if `.git` is a file (not a directory) containing a `gitdir:` pointer, we're in a worktree. If that pointer resolves through `/repo-root/.git/worktrees/`, we're in a container mount.

**Implementation:** Add a check in `CreateWorktree` or at the top of `ProvisionAgent`:

```go
if os.Getenv("SCION_HOST_UID") != "" {
    return fmt.Errorf("cannot create worktrees inside an agent container")
}
```

Or more defensively, check whether the current repo is itself a worktree:

```go
func IsWorktree(dir string) bool {
    gitPath := filepath.Join(dir, ".git")
    info, err := os.Stat(gitPath)
    if err != nil {
        return false
    }
    return !info.IsDir() // .git is a file (worktree pointer), not a directory
}
```

**Verdict:** Addresses the root cause for scion-managed agent creation. Does not cover raw `git worktree add` from the LLM shell.

#### 3b. Filesystem-level protection

Make `.git/worktrees/` read-only inside the container, or mount `.git` as read-only.

**Problem:** Agents need git write access for commits, branch creation, and other normal operations. Mounting `.git` read-only would break basic git workflows. Selectively protecting `.git/worktrees/` is possible but fragile — git creates entries there as a side effect of `git worktree add`, and container runtimes don't support fine-grained sub-path permissions within a single mount.

**Verdict:** Not practical without a FUSE overlay or custom git hooks, both of which add significant complexity.

#### 3c. Agent instructions

The CLAUDE.md and agent instruction templates already tell agents not to create worktrees or mess with `.scion`. But LLM agents don't always follow instructions perfectly, especially under complex multi-step task pressure.

**Verdict:** Defense-in-depth layer, not a primary control. Already partially in place via the project CLAUDE.md.

### Option 4: Use absolute paths for in-container worktrees

**Problem:** This contradicts the architecture. The entire container mount strategy depends on `--relative-paths`:

- Host: `.git` file contains `gitdir: ../../../.git/worktrees/workspace`
- Container: same file, same relative path, resolves correctly because mount layout preserves relative distances
- With absolute paths: `.git` file would contain `gitdir: /home/user/repo/.git/worktrees/workspace` which doesn't exist inside the container

The git 2.47.0 requirement exists solely for `--relative-paths` support. Switching to absolute paths would break primary worktree functionality.

**Verdict:** Not viable. The relative-paths design is correct for the primary use case. The problem is worktrees created inside the container, and the fix is to prevent that, not change the path strategy.

## Recommendation

The options are not mutually exclusive. In priority order:

| Priority | Option | Value | Effort |
|----------|--------|-------|--------|
| 1 | **3a**: Scion-level guard | High — prevents root cause | Low |
| 2 | **2**: Unique basenames | Medium — diagnosability | Medium |
| 3 | **1**: Use `GetCommonGitDir` | Low — correctness | Trivial |
| 4 | **3c**: Agent instructions | Low — defense-in-depth | Trivial |
| — | **4**: Absolute paths | N/A — breaks design | — |
| — | **3b**: FS-level protection | Low — too complex | High |

### Remaining gap

After all scion-level guards, an LLM agent can still run `git worktree add` directly via shell. This is a broader agent-containment question. Possible future mitigations:
- Git `pre-worktree` hooks (not currently a git feature)
- A wrapper `git` binary inside the container that intercepts `worktree add`
- Monitoring `.git/worktrees/` for unexpected entries and alerting/pruning

These are out of scope for this design but worth noting as the threat model matures.
