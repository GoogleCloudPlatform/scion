# Phase 1 Implementation Plan: Worktree-Per-Agent (Docker × node-local)

**Branch:** `scion/worktree-per-agent`
**Scope:** Design doc §12 Phase 1 only — Docker × node-local. NFS parity (Phase 2)
and K8s (Phase 3) are out of scope here.
**Tracking:** #158 (parent), #168 (shared-worktree refcount — Q7, deferred to a later phase).

---

## Post-Phase-0 baseline (rebased onto `scion/storage-provisioning-phase0`)

Phase 0 of epic #169 (PR #170) landed the universal-provisioning extraction. This branch
is rebased on it. The seam Phase 1 builds against is now:

- `WorkspaceBackend` interface = **Resolve + Realize + Name** only. `Provision` was
  **removed** (`pkg/runtime/workspace_backend.go`).
- `provisionShared(in ProvisionInput) error` — the standalone Tier-1 universal function
  (`pkg/runtime/workspace_provision.go`). It does clone + `ensureWorktree` + advisory
  lock + sentinel. **Still unwired** — no live caller yet.
- `localBackend` / `nfsBackend` = Resolve + Realize; provisioning content all moved into
  `provisionShared`.

Two things in the extracted code are NFS-shaped and are Phase-1's job to fix:

1. **`ensureWorktree` uses plain `git worktree add -b` — no `--relative-paths`**
   (`workspace_provision.go:283`). Mandatory for container path-identity (design §6);
   `util.CreateWorktree` already does this correctly and should be reused.
2. **No Q1 layout** — it clones into `Resolved.HostPath` and nests `worktrees/<agentID>`
   under it, leaving `HostPath` checked out on the default branch. Q1 requires the base to
   hold **no** branch (so a coordinator can own `main`).

Also cosmetic: every error string in `provisionShared` still says `"nfsBackend.Provision:"`
(extraction leftover) — fix to neutral `"provisionShared:"`.

Consequence: "broker dispatch branches on mode" (design §4.3, §5) remains the heaviest
task — it is the **first real wiring** of `provisionShared` + the backend abstraction into
the Docker node-local lifecycle. No non-test caller of `SelectWorkspaceBackend` or
`provisionShared` exists yet; NFS provisioning still runs via the separate k8s
init-container path (`k8s_runtime.go`), whose unification is #169 PR2, not this work.

What already exists and is reused as-is:
- `util.CheckGitVersion()` / `CompareGitVersion()` — git ≥ 2.47 gate (`pkg/util/git.go`).
- `util.CreateWorktree()` — already uses `--relative-paths` + reuse-branch fallback.
- Dual-mount recipe — `pkg/runtime/common.go:188-222` (`.git` at `/repo-root/.git` +
  worktree at `/repo-root/<rel>`), gated on `RepoRoot`+`Workspace` set and `GitClone==nil`.
- `SCION_HOST_UID` guard forcing `isGit=false` in-container (`provision.go:303-309`).
- Teardown: `RemoveWorktree` / `PruneWorktreesIn` / `DeleteBranchIn` (`provision.go:35-146`).
- Advisory lock + sentinel guard (`workspace_backend_nfs.go:141-268`).

---

## Target node-local layout (Phase 1)

**Layout decision (path-identity constraint, design §6).** The container dual-mount in
`common.go:188-222` only fires when `filepath.Rel(RepoRoot, Workspace)` does **not** start
with `..` — i.e. the worktree must live **inside** the repo root, exactly how the proven
local-repo case nests worktrees. The design doc's sibling `base/` + `worktrees/` layout
(§3) would make `rel = ../worktrees/<id>` and `common.go` rejects it. So Phase 1 mirrors
the local case rather than the §3 diagram:

```
~/.scion.projects/<slug>/        # localBackend project root == RepoRoot (the base checkout)
  .git/                          # shared object store + packed-refs; gc.auto=0
  <base working tree>            # cloned, then `git switch --detach` → owns NO branch
  worktrees/<agentID>/           # per-agent worktree nested inside repo root
    .git                         #   FILE: relative gitdir (via --relative-paths)
  .scion-provisioned             # sentinel: base clone complete
```

Base detached at default HEAD so `main` is free for an optional coordinator worktree (Q1).
`worktrees/` is added to `.git/info/exclude` so it never shows as untracked in the base.

> **Sentinel-location refinement (P1.4b).** `ProvisionShared` writes its sentinel at
> `filepath.Dir(HostPath)`. To give a per-project sentinel (not a node-shared one),
> `localBackend.Resolve` returns `HostPath = <ProjectDir>/workspace` for worktree mode, so
> the base checkout lives at `~/.scion.projects/<slug>/workspace` and the sentinel at
> `~/.scion.projects/<slug>/.scion-provisioned`. Worktrees nest at
> `<ProjectDir>/workspace/worktrees/<agentID>`. This matches `ProvisionShared`'s NFS-shaped
> contract (HostPath's parent is the per-project root).
Per-agent non-workspace state (prompt.md, scion-agent.json, home/) continues to live in
external split storage — unchanged from shared-workspace mode.

> Revisiting the §3 sibling layout (no base working tree) is deferred: it requires teaching
> `common.go` to mount a common parent at `/repo-root` and handle a `..`-relative worktree.
> Out of Phase 1 scope; noted for follow-up.

---

## Sub-tasks

Each is sized to commit+push within the agent 3-turn limit. Dependencies in brackets.

### P1.1 — Bring `provisionShared`'s worktree path to the Phase-1 layout
Edit `ensureWorktree` (and the base-clone step) in `pkg/runtime/workspace_provision.go`.
Responsibilities (design §4.1, §4.2, §4.2a, §6):
- After the base clone into `Resolved.HostPath`, `git -C <HostPath> switch --detach` so
  the base owns no branch; set `git config gc.auto 0`; add `worktrees/` to
  `.git/info/exclude`.
- Replace the plain `git worktree add -b` with **`--relative-paths`** (mandatory, §6).
  Prefer reusing `util.CreateWorktree` / `sanitizeBranchName` so there is one worktree-add
  implementation. Keep the reuse-branch fallback (attach existing branch instead of `-b`)
  for the coordinator/`main` case (§4.2a). Worktree stays nested:
  `<HostPath>/worktrees/<agentID>`.
- Write `.scion` workspace marker into the worktree (`config.WriteWorkspaceMarker`).
- Single-worktree-per-branch invariant: clear error if the branch is already checked out
  elsewhere (don't let raw git fail opaquely).
- Clean up the `"nfsBackend.Provision:"` error strings in `provisionShared` →
  `"provisionShared:"` (neutral, now that it is the shared Tier-1 fn).
- Update the worktree tests in `workspace_provision_test.go` to the new layout
  (detached base, `--relative-paths` `.git` pointer). **SharedPlain and ClonePerAgent
  tests stay untouched and green.**

### P1.2 — `localBackend.Resolve` for worktree mode  [needs P1.1]
(No `backend.Provision` anymore — provisioning is the standalone `provisionShared`, invoked
by the broker in P1.4.)
- `Resolve`: when `Mode == WorktreePerAgent`, return `HostPath` = the node-local project
  root (the base checkout / `RepoRoot`). The worktree path
  (`<HostPath>/worktrees/<agentID>`) is derived by `provisionShared`. Other modes
  unchanged (zero behavior change).
- Confirm `Realize` still emits the plain local bind mount; the **dual-mount** (`.git` +
  worktree) is contributed by `common.go` when the broker sets `RepoRoot`+`Workspace`
  (P1.4), not by `Realize`.
- Unit tests for the worktree-mode Resolve path.

### P1.3 — git-version gate + clone-per-agent fallback
- Decision helper (e.g. `worktreeEligible() (bool, reason string)`) wrapping
  `util.CheckGitVersion()`. On git < 2.47: log a warning and signal fallback to
  clone-per-agent (design §6, §9.1).
- Unit test the decision (inject version).

### P1.4 — Broker dispatch branches on mode  [needs P1.2, P1.3]
The core wiring. In `pkg/runtimebroker/start_context.go` (where `opts.GitClone` is set,
~437-454) and/or the dispatch handler:
- Resolve sharing mode for the dispatch (from threaded mode — see P1.5).
- If `worktree-per-agent` **and** git ≥ 2.47:
  - `resolved := SelectWorkspaceBackend(cfg, mode).Resolve(...)`, then
    `provisionShared(ProvisionInput{Resolved: resolved, Mode, GitClone, AgentID,
    AgentName, Locker, ...})` on the host (base clone + worktree).
  - Set `opts.Workspace = <HostPath>/worktrees/<agentID>` and `RepoRoot = <HostPath>`
    (the detached base) so `common.go` takes the **dual-mount** path; **do not** set
    `opts.GitClone` (suppress in-container clone).
- Else: existing clone-per-agent path (set `GitClone`, clone in container).
- Keep the `SCION_HOST_UID` guard intact.

### P1.5 — Hub: permit worktree-per-agent on git hub-managed projects  [supports P1.4]
- Allow/stamp `scion.dev/workspace-mode = worktree-per-agent` for git-backed
  hub-managed projects (`pkg/hub/handlers.go`); update the "reserved for Phase 1+"
  doc comment on `pkg/store/models.go`.
- Thread the resolved mode into the dispatch request (`pkg/runtimebroker/types.go`
  `RunRequest`) so the broker (P1.4) can branch without re-deriving from labels.

### P1.6 — Verification + suite green  [needs P1.4, P1.5]
- Docker × node-local end-to-end: create 2 agents on one git project → confirm a single
  base clone, two `worktrees/<id>` with distinct branches, dual-mount resolves inside
  the container, `git status` clean in each. Optional coordinator with `--branch main`.
- Full `go build ./...` + `go test ./...` green.
- Open PR against `main` (do **not** merge).

---

## Sequencing & orchestration

```
P1.1 ─┬─> P1.2 ─┐
      │         ├─> P1.4 ─┐
      └  P1.3 ──┘         ├─> P1.6 (verify + PR)
         P1.5 ────────────┘
```

- One developer agent per sub-task, in dependency order; P1.3 and P1.5 can run parallel
  to the P1.1→P1.2 chain.
- Each agent commits **and pushes** after its sub-task (3-turn-limit workaround).
- Manager reports to coordinator at each milestone (P1.1, P1.2/P1.3, P1.4/P1.5, P1.6+PR).

## Resolved through design dialogue (2026-06-07, thread 155)
- **Provisioning abstraction** — extracted to standalone Tier-1 `provisionShared` in
  epic #169 PR1 (Phase 0), now merged-to-branch and rebased under this work. P1.1 builds on
  it; no NFS layout reconciliation needed in this phase (NFS validation is #169 / NFS
  Phase 2).
- **Mode threading (P1.5)** — add an explicit mode field to `RunRequest` rather than
  re-deriving from labels in the broker.
- **Layout** — Phase 1 uses the nested-worktree layout (base checkout = repo root, detached;
  worktrees nested inside) to satisfy `common.go`'s path-identity constraint; the §3 sibling
  layout is a deferred follow-up. See "Target node-local layout" above.

## Remaining open point
- **Coordinator UX** — Phase 1 only needs the reuse-branch path working for `--branch main`;
  the Hub's single-owner-per-branch enforcement (§4.2a) can be a thin check now, hardened
  later. (Proposed; confirm if it needs more in Phase 1.)
