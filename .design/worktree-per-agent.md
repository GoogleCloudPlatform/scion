# Design: Worktree-Per-Agent Mode for Hub-Managed Workspaces

**Branch:** `scion/worktree-per-agent`
**Date:** 2026-06-06
**Author:** worktree-designer agent
**Status:** Design proposal — initial draft for review
**Vocabulary:** follows `GLOSSARY.md` (Runtime Broker, Project, workspace sharing modes)
**Reviewers:** @ptone
**Tracking issue:** https://github.com/ptone/scion/issues/158

**Inputs (verified against source):**
`pkg/store/models.go`, `pkg/runtime/workspace_backend.go`,
`pkg/runtime/workspace_backend_local.go`, `pkg/runtime/workspace_backend_nfs.go`,
`pkg/agent/provision.go`, `pkg/runtime/common.go`, `pkg/api/types.go`,
`.design/nfs-workspace.md`, `.design/worktree-guards.md`,
`.design/hub-shared-workspace-isolation.md`, `.design/git-workspace-hybrid.md`.

---

## 1. Problem statement

In hub-managed mode, every agent created against a git-backed project performs its
**own full clone** of the project's remote. The Hub dispatches a `GitCloneConfig`
(`pkg/api/types.go`) and `sciontool` clones the repository into the agent's workspace
**inside the container** at startup (the `gitClone != nil` branch in
`pkg/agent/provision.go:371-396`). Concretely:

1. Hub computes the workspace mode for the project and builds a dispatch carrying
   `GitClone` (URL + branch + depth).
2. The Runtime Broker prepares an empty per-agent workspace dir and mounts it.
3. `sciontool` clones into `/workspace` when the container starts.

This is simple and isolates agents perfectly, but it scales badly:

- **Startup latency** is dominated by a network clone, paid **per agent**.
- **Disk amplification** — N agents on a node hold ≈ N full copies of the same
  history and working tree.
- **No cheap coordination** — two agents working the same repo can't see each
  other's branches/objects without round-tripping to the remote.

The canonical mode that fixes this already exists in the type system —
`SharingModeWorktreePerAgent` (`pkg/store/models.go:208-212`) — but its doc comment
states it is *"not yet on Hub-managed projects — reserved for Phase 1+"*, and backend
routing only wires it to **NFS**:

```go
// pkg/runtime/workspace_backend.go:200-210
func SelectWorkspaceBackend(cfg *config.V1WorkspaceStorageConfig, mode store.WorkspaceSharingMode) WorkspaceBackend {
	if cfg != nil && cfg.Backend == "nfs" {
		switch mode {
		case store.SharingModeSharedPlain, store.SharingModeWorktreePerAgent:
			return NewNFSBackend(cfg.NFS)
		}
	}
	return NewLocalBackend()
}
```

The node-local `localBackend.Provision` is a **no-op**
(`pkg/runtime/workspace_backend_local.go:65-70`), so there is no worktree-per-agent
path for the common single-node, node-local hub-managed deployment.

**Goal:** the Hub/Runtime Broker maintains **one shared base clone per node** for a
project, and each agent gets its own **git worktree** (own branch, own working tree)
over that shared `.git` object store — instead of a full clone. First agent pays the
clone; every subsequent agent pays only a cheap `git worktree add`.

### 1.1 The mechanism already exists for *local* git projects

This is not a from-scratch effort. For **local** (non-hub) git projects, Scion already:

- Creates host-side `--relative-paths` worktrees per agent at
  `.scion/agents/<name>/workspace` via `util.CreateWorktree`
  (`pkg/agent/provision.go:449-485`, Case 2 at lines 412-434).
- Dual-mounts the shared `.git` and the per-agent worktree into the container so the
  relative gitdir pointer resolves (`pkg/runtime/common.go:181-189`):

  ```go
  registerMount(filepath.Join(config.RepoRoot, ".git"), "/repo-root/.git", false, true)
  containerWorkspace := filepath.Join("/repo-root", relWorkspace)
  registerMount(config.Workspace, containerWorkspace, false, true)
  ```

- Tears worktrees + branches down on delete with pruning of stale records
  (`util.RemoveWorktree`, `PruneWorktreesIn`, `DeleteBranchIn` —
  `pkg/agent/provision.go:35-146`).

So worktree-per-agent **already runs in production** for the local-repo case. The work
is to bring this model to **hub-managed** projects (which today take the
clone-in-container path) and to formalize it as the `worktree-per-agent` sharing mode
on node-local storage, reusing the NFS backend's `ensureWorktree` logic where it fits.

### 1.2 Non-goals

- **Auto-migrating** existing full-clone agents to worktrees. New mode applies to new
  projects/agents; live conversion is out of scope.
- **A distributed lock manager.** Reuse the existing advisory-lock + sentinel guard.
- **Cross-node base sharing on node-local storage.** A node-local base is per-node by
  definition; cross-node sharing is the NFS backend's job (§7).
- **Replacing clone-per-agent.** It remains the right default when agents need fully
  independent histories or must survive base-repo corruption.

---

## 2. Background: the three sharing modes

`pkg/store/models.go:197-231` defines the canonical modes and the label mapping
(`scion.dev/workspace-mode`, `ResolveWorkspaceSharingMode`):

| Mode | Constant | Storage today | Isolation |
|------|----------|---------------|-----------|
| Shared plain | `SharingModeSharedPlain` (`shared`) | one dir, all agents mount it | none |
| Clone per agent | `SharingModeClonePerAgent` (`per-agent`) | full clone per agent (node-local) | full |
| **Worktree per agent** | `SharingModeWorktreePerAgent` | **NFS only today** | per-branch working tree, shared object store |

This design makes the third row a first-class option for **hub-managed projects on
node-local storage**, and aligns it with the NFS implementation so a single code path
serves both backends.

---

## 3. Target layout

Per node, per project, the broker maintains a single **base repo** and a `worktrees/`
subtree, mirroring the NFS backend's layout (`workspace_backend_nfs.go:331-332`):

```
<project-root>/                         # e.g. ~/.scion.projects/<slug>/
  base/                                 # the one shared clone (.git lives here)
    .git/                               #   shared object store + packed-refs
    <checked-out files of base branch>
  worktrees/
    <agentID>/                          # per-agent worktree (own branch + working tree)
      .git                              #   FILE: gitdir pointer (relative) → base/.git/worktrees/<id>
      <agent's working tree>
  .scion-provisioned                    # sentinel: base clone complete
```

Per-agent **non-workspace** state (prompt.md, scion-agent.json, home/) stays in the
external split-storage location, exactly as shared-workspace mode does today
(`.design/hub-shared-workspace-isolation.md`, `provision.go:120-143`), so siblings
never see each other's prompts through a shared mount.

> Naming note: `.design/worktree-guards.md` calls out that every agent worktree using
> the basename `workspace` causes git to auto-suffix entries (`workspace`, `workspace1`,
> …). Using `worktrees/<agentID>` as the worktree path gives each a **unique basename**
> (the agent UUID), eliminating that ambiguity. Scion associates worktrees by branch
> (`FindWorktreeByBranch`) regardless, but unique basenames make `git worktree list`
> legible.

---

## 4. Provisioning flow

### 4.1 First agent (base clone)

When the first agent for a project lands on a node in worktree-per-agent mode:

1. **Acquire the per-project advisory lock** (`store.AdvisoryLocker` /
   `LockWorkspaceProvision`, keyed by a stable project hash — same guard the NFS
   backend uses, `workspace_backend_nfs.go:227-268`). On Postgres this is
   `pg_try_advisory_lock`; on SQLite/single-node it serializes naturally.
2. **Check the `.scion-provisioned` sentinel.** If present, skip to §4.2.
3. **Clone the remote once** into `base/` using the dispatched `GitCloneConfig`
   (URL/branch/depth). This is the *only* network clone for the project on this node.
4. **Write the sentinel** atomically, release the lock.

This is the `localBackend` analogue of `nfsBackend.Provision`
(`workspace_backend_nfs.go:141-225`); the difference is the root path (node-local
project dir vs NFS export) — the guard logic is identical and should be **shared**.

### 4.2 Every agent (worktree add)

Under the same per-project lock (worktree add/remove mutates shared `.git` metadata —
`workspace_backend_nfs.go:318-376`, design §9.2):

1. Compute `worktreePath = <project-root>/worktrees/<agentID>`.
2. If it already exists, no-op (idempotent restart).
3. Derive branch: `branch = sanitizeBranchName(agentName)` /
   `api.Slugify(agentName)`. If the branch already exists, attach to it instead of
   `-b` (the reuse fallback at `workspace_backend_nfs.go:363-371`).
4. `git -C base worktree add --relative-paths -b <branch> <worktreePath>`.
   `--relative-paths` (git ≥ 2.47) is **mandatory** so the gitdir pointer survives the
   container mount remap (§6).
5. Write the `.scion` workspace marker into the worktree
   (`config.WriteWorkspaceMarker`, as `provision.go:475-484` does) so the in-container
   CLI can discover project context — worktrees don't carry `.scion`.

### 4.2a The coordinator / `main` agent (Q1 resolved)

The base is cloned **non-bare** then **detached** at the default-branch HEAD
(`git -C base switch --detach`), so the base's own working tree never holds a branch and
stays clean — a pure object-store + refs directory. `main` (the default branch) is
therefore free to be attached by a *linked* worktree like any other branch.

A user may create one **coordinator agent** with an explicit `--branch main`. It is not a
special code path: it is simply the agent whose worktree owns the `main` branch (attached
via the reuse path, not `-b`). Because every agent branch lives in the shared object
store, this coordinator can `git merge <agent-branch>` **locally** with no remote round
trip — the in-hub analogue of "a non-agent merges to main" in local-use mode.

**Single-worktree-per-branch invariant.** Git forbids the same branch checked out in two
worktrees, so at most one worktree may *own* a given branch (e.g. `main`). The Hub
enforces this and returns a clear error rather than letting a raw `git worktree add`
fail. See Q7 for the explicitly-requested **shared-mount** exception, which lets >1 agent
attach to the *same* worktree directory without violating this invariant.

### 4.3 Reconciling with the clone-in-container dispatch path

Today hub dispatch sets `GitClone` and the clone happens **inside** the container at
startup. Worktree-per-agent inverts this: the worktree is created **on the host/broker**
(where the base `.git` lives) *before* the container starts, then mounted in. So:

- When mode is `worktree-per-agent`, the broker provisions the worktree on the host and
  takes the **dual-mount** path (`common.go:181-189`) instead of passing `GitClone`
  through to sciontool.
- `GitCloneConfig` is still used by `Provision` (§4.1) to perform the *base* clone, but
  it is consumed broker-side, not container-side.
- The `SCION_HOST_UID` guard that forces `isGit = false` inside containers
  (`provision.go:303-309`) stays — agents must **never** create worktrees from inside
  the container (see §6, `.design/worktree-guards.md`).

---

## 5. Backend selection changes

`SelectWorkspaceBackend` (`workspace_backend.go:200-210`) currently sends
`worktree-per-agent` to local **only** by falling through (which is a no-op backend).
Two changes:

1. **Implement `localBackend.Provision`** to perform the §4 base-clone + worktree-add
   when `in.Mode == SharingModeWorktreePerAgent`, factoring the shared guard/worktree
   logic out of `nfsBackend` into a helper both backends call (e.g.
   `ensureBaseAndWorktree(root, in)`).
2. **Route node-local worktree-per-agent to `localBackend`** (already the fall-through),
   and keep NFS worktree-per-agent on `nfsBackend`. The mode is now valid on **both**
   backends; the backend only decides *where the root lives*, not *whether worktrees are
   supported*.

`localBackend.Realize` already emits a bind mount (`workspace_backend_local.go:74-85`);
the runtime layer adds the **second** mount (shared `base/.git`) per `common.go:181-189`
when the resolved workspace is a worktree.

---

## 6. Isolation & the container path-identity constraint

This is the sharpest constraint, documented in `.design/worktree-guards.md` §3.

A worktree's `.git` is a **file** containing `gitdir: <path>` pointing at
`base/.git/worktrees/<id>`. For that pointer to resolve inside the container, the base
`.git` and the worktree must keep the **same relative distance** across the mount
boundary. The proven recipe (`common.go:181-189`):

- Mount shared git dir at a fixed container path (`/repo-root/.git`).
- Mount the worktree at `/repo-root/<relWorkspace>` preserving the host relative path.
- Worktrees created with `--relative-paths` then resolve identically on host and in
  container.

Hard rules that fall out:

- **git ≥ 2.47** on the broker host (for `--relative-paths`). Gate provisioning on a
  version check; fall back to clone-per-agent with a logged warning if absent.
- **No in-container worktree creation.** Relative paths computed against the container
  namespace are meaningless on the host (`.design/worktree-guards.md` §3). The existing
  `SCION_HOST_UID` guard enforces this; keep it.

Other shared-state isolation concerns:

- **Object store & packed-refs are shared.** Concurrent ref updates are safe (git locks
  refs), but a `git gc` in one worktree repacks objects for all. Recommendation:
  **disable auto-gc** in the base (`git config gc.auto 0`) and run GC only during a
  controlled "last agent" teardown or maintenance window.
- **`.git/config` is shared.** Per-agent git identity/credentials must live in the
  agent's `$HOME/.gitconfig` (already the pattern for shared-workspace mode,
  `provision.go:881-901`), never written into the shared base.
- **Per-agent non-workspace state** (prompt.md, scion-agent.json) uses external split
  storage (§3) so it isn't visible through the shared tree.

---

## 7. How the Hub manages the shared base repo

- **Mode selection.** Hub stamps the project label
  `scion.dev/workspace-mode = worktree-per-agent` at project-create time (parallel to
  the existing `shared` stamping in `pkg/hub/handlers.go`). `ResolveWorkspaceSharingMode`
  already maps the wire value (`models.go:225-226`).
- **Dispatch.** Hub keeps sending `GitCloneConfig` (URL/branch/depth); the broker
  decides — based on resolved mode — whether to consume it as a base clone (worktree
  mode) or pass it through for in-container clone (clone-per-agent mode).
- **Base lifecycle.** The base is **per node**. The Hub does not track base repos
  directly; the broker owns them via the sentinel + advisory lock. The Hub's role is
  mode selection and ensuring agents for a worktree-mode project are dispatched
  consistently.
- **Multi-node.** Two nodes each keep their own base clone (acceptable — clone cost is
  amortized per node, not per agent). True cross-node base sharing requires the **NFS
  backend**, where the base + all worktrees live on a single export and
  `nfsBackend.ensureWorktree` already implements §4.2. The mode is identical; only the
  root path differs — which is exactly why §5 factors the logic into a shared helper.

Backend × runtime matrix:

| | Docker / VM | Kubernetes |
|---|---|---|
| **node-local** | base clone on host, dual bind-mount (§6) | base on node, worktrees per pod via hostPath/emptyDir — **needs validation** |
| **NFS** | base + worktrees on export, NFS mount | base + worktrees on RWX PVC + subPath (existing NFS design §9) |

The K8s × node-local cell is the least-proven and may be restricted to NFS in v1.

---

## 8. Lifecycle & cleanup

Agent deletion already does the right thing for worktrees
(`pkg/agent/provision.go:35-146`):

1. `util.RemoveWorktree(agentWorkspace, removeBranch)` — removes the worktree and
   optionally its branch.
2. `util.PruneWorktreesIn(repoRoot)` — clears stale `.git/worktrees/<id>` records.
3. `util.DeleteBranchIn(repoRoot, branchName)` fallback by slugified name.
4. External per-agent state dir removed (with podman-unshare fallback).

New work for the base repo:

- **Last-agent teardown.** When the final worktree for a project is removed on a node,
  optionally GC and/or remove `base/` (policy: keep for fast re-provision vs reclaim
  disk). Guard the decision under the same advisory lock.
- **Orphan base detection.** A maintenance sweep that prunes worktrees and, if none
  remain, applies the teardown policy — analogous to existing project-prune flows.

---

## 9. Limitations

1. **git ≥ 2.47 required** on the broker host (`--relative-paths`). Older hosts fall
   back to clone-per-agent.
2. **No nested / in-container worktrees.** Enforced by the `SCION_HOST_UID` guard;
   agents that try to `git worktree add` inside the container get path-identity
   corruption (`.design/worktree-guards.md`).
3. **Shared object store is a shared fate.** Corruption or an ill-timed `gc` in the base
   affects all agents on that node. Clone-per-agent remains the choice when independence
   matters more than speed.
4. **Node-local base is per node.** Cross-node sharing needs NFS; the disk win is
   per-node, not global.
5. **Working-tree-only isolation.** Agents share history and refs; a force-push or a
   shared-ref rewrite is visible to siblings. Branch-per-agent contains *normal*
   workflows, not adversarial ones.
6. **K8s node-local path unproven** — may be NFS-only in v1 (§7).

---

## 10. Reuse map (what already exists)

| Need | Existing primitive | Location |
|------|--------------------|----------|
| Worktree add (host, relative) | `util.CreateWorktree` | `pkg/util/git.go`, `provision.go:470` |
| Worktree add (NFS, with reuse) | `nfsBackend.ensureWorktree` | `workspace_backend_nfs.go:318-376` |
| Branch name from agent | `api.Slugify` / `sanitizeBranchName` | `workspace_backend_nfs.go:378-393` |
| First-access guard | sentinel + `store.AdvisoryLocker` | `workspace_backend_nfs.go:141-268` |
| Dual mount (`.git` + worktree) | runtime mount registration | `common.go:181-189` |
| Worktree teardown + prune | `RemoveWorktree`/`PruneWorktreesIn`/`DeleteBranchIn` | `provision.go:35-146` |
| In-container worktree guard | `SCION_HOST_UID` → `isGit=false` | `provision.go:303-309` |
| Per-agent state isolation | external split storage | `.design/hub-shared-workspace-isolation.md` |
| Backend abstraction | `WorkspaceBackend` Resolve/Provision/Realize | `workspace_backend.go:33-64` |

The net new code is small and concentrated: implement `localBackend.Provision` for the
worktree case, factor `ensureBaseAndWorktree` out of the NFS backend so both backends
share it, branch the broker dispatch on mode (host worktree vs in-container clone), add
the git-version gate, and define base-repo teardown.

---

## 11. Open questions

1. **Q1 — Base branch policy. [RESOLVED 2026-06-06]** Base is cloned **non-bare** then
   **detached** at default-branch HEAD; it never checks out a working branch and stays
   clean. The `main` branch is owned by an optional **coordinator agent** created with an
   explicit `--branch main`, which holds the `main` worktree like any other agent and can
   merge sibling branches locally from the shared object store. The Hub enforces a
   single owner per branch (see §4.2a). Bare-base variant deferred as a later refinement.
2. **Q2 — GC policy.** Disable auto-gc entirely and GC only on teardown, or allow
   scheduled GC under the project lock?
3. **Q3 — Base teardown.** Keep `base/` after the last agent (fast re-provision) or
   reclaim disk immediately? Configurable via settings?
4. **Q4 — K8s node-local.** Support node-local worktrees on K8s in v1, or restrict
   worktree-per-agent to NFS on K8s?
5. **Q5 — Migration.** Any opt-in path to convert a running clone-per-agent project to
   worktree-per-agent, or strictly new-projects-only?
6. **Q6 — Default mode.** Should worktree-per-agent become the default for new
   git-backed hub-managed projects once proven, with clone-per-agent as opt-out?
7. **Q7 — Explicit shared worktree (multi-agent mount).** Requirement (maintainer,
   2026-06-06): support **>1 agent mounting the same branch/worktree** when explicitly
   requested. Since git forbids the same branch in two *separate* worktrees, this means N
   agents **bind-mount the same worktree directory** into their containers (shared
   working tree + shared branch), rather than each creating its own. Open sub-questions:
   how it is requested (e.g. `--branch <b> --shared` or attaching to an existing agent's
   worktree); concurrency/write-conflict expectations within the shared tree (this is
   shared-plain semantics scoped to one branch — cf. `.design/hub-shared-workspace-isolation.md`);
   how per-agent home/prompt state stays isolated while the workspace is shared; and
   refcounted teardown so the worktree is removed only when the last mounting agent exits.
   To be taken up as its own question after Q2–Q6.

---

## 12. Phased rollout (proposed)

- **Phase 0 (this doc).** Design + issue #158.
- **Phase 1.** Factor `ensureBaseAndWorktree`; implement `localBackend.Provision` for
  worktree mode; broker dispatch branches on mode; git-version gate. Docker × node-local
  only.
- **Phase 2.** NFS parity validated end-to-end (largely existing); base teardown +
  orphan sweep.
- **Phase 3.** K8s story (NFS-first); decide default-mode question (Q6).
