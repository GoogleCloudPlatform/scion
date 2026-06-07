# Phase 2 Plan: Worktree-Per-Agent Lifecycle

**Branch:** `scion/worktree-phase2` (off upstream `main`, which now contains Phase 1 via #350)
**Tracking:** #158. Builds directly on the merged Phase 1.
**Status:** scoped per the 2026-06-07 question resolutions (ptone).

## Resolved policy inputs (recorded in `worktree-per-agent.md` §11)
- **Q2 (GC):** GC only on teardown; not a priority yet → no GC work now.
- **Q3 (base teardown):** **Keep the base** after the last agent → no base removal / orphan sweep.
- **Q6 (default mode):** clone-per-agent stays default; worktree-per-agent is opt-in; **UI must make the options obvious.**

These collapse the original Phase 2 (base teardown + orphan sweep + GC) down to three concrete tasks.

## Tasks

### T1 — Record resolutions (DONE, this commit)
Q2/Q3/Q6 marked RESOLVED in §11; §8 (base teardown) and §12 (rollout) updated to reflect
"keep base / GC deferred / clone-per-agent default."

### T2 — Delete-path teardown for the hub-managed worktree layout  [developer]
The teardown primitives exist (`util.RemoveWorktree` / `PruneWorktreesIn` / `DeleteBranchIn`,
`pkg/agent/provision.go`) and the broker delete path calls `mgr.Delete(..., removeBranch)`.
**Verify and fix** that for a hub-managed worktree-per-agent agent, deletion:
- removes the agent's worktree at `<ProjectDir>/workspace/worktrees/<agentID>` and (when
  `removeBranch`) its branch;
- prunes the stale `.git/worktrees/<agentID>` registration in the shared base
  (`<ProjectDir>/workspace`) — same hazard fixed in the #350 failure-cleanup;
- **never** touches the shared base or sibling worktrees.
Add a regression test (two agents; delete one; assert its worktree+registration gone and the
base + sibling intact). Confirm the workspace/repoRoot resolution on the delete path matches
the new layout (worktree path vs base).

### T3 — NFS worktree-per-agent end-to-end validation  [developer]
Now feasible since #169 unified provisioning (broker-side + k8s init-container both call the
shared Tier-1 `provision.ProvisionShared`). Validate worktree-per-agent on the NFS backend
end-to-end (base clone once on the export, per-agent worktrees, dual-mount resolves); fix any
gaps. Scope to validation + targeted fixes, not new architecture.

### T4 — UI: surface workspace-mode options (Q6)  [developer, web]
Make the workspace-mode choice obvious at project (and/or agent) creation in the web UI:
`shared` / `clone-per-agent` (default) / `worktree-per-agent`, with brief helper text. Wire
to the existing `scion.dev/workspace-mode` label / `CreateProjectRequest.WorkspaceMode`.

## Out of scope (deferred per resolutions)
Base last-agent teardown, orphan-base sweep, GC-on-teardown (Q2/Q3); K8s node-local worktree
(Phase 3, Q4); migration path (Q5); shared-worktree multi-mount + refcount (#168 / Q7).

## Orchestration
- I (manager) own T1 and overall oversight; verify each task independently (diff + build +
  tests) before it lands, as in Phase 1.
- Delegate T2, T3, T4 to developer agents (sequential on the branch to avoid push races, or
  separate sub-branches if parallelized). T2 first (safety-critical, smallest).
- Scope is moderate → developer agents suffice; no sub-manager needed. Revisit if T3 (NFS
  e2e) uncovers larger work.
- PRs on the fork targeting `main`, merged upstream by a maintainer (same flow as Phase 1).
