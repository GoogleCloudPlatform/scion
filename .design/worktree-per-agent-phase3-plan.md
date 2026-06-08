# Phase 3 Plan: Worktree-Per-Agent on Kubernetes (NFS-only)

**Branch:** `scion/worktree-phase3-k8s` (off upstream `main` b40cd057 — has Phase 1+2+#168).
**Tracking:** #158. Final planned phase.
**Q4 RESOLVED (ptone, 2026-06-08): NFS-only on K8s in v1.**

## Scope (small — guardrail + validation + docs)
worktree-per-agent on K8s is supported **only** with the NFS backend. Node-local worktrees
on K8s are **not** supported in v1 and must be rejected cleanly (or fall back to
clone-per-agent) rather than producing a broken host-bind mount (the broker's host-side
`ProvisionShared` + dual-mount is a Docker/VM mechanism; a K8s pod can't bind-mount a host
worktree).

### T1 — Guardrail: reject node-local worktree-per-agent on K8s
- Where: broker dispatch (`pkg/runtimebroker/start_context.go` `tryProvisionWorktree` /
  `resolveWorktreeProvision`) and/or the K8s runtime path. Investigate the exact flow first.
- Behavior: when the runtime is **Kubernetes** AND mode is **worktree-per-agent** AND the
  backend is **not NFS** (i.e. node-local) → do NOT take the host-side worktree-provision
  path. Fall back to clone-per-agent with a clear `slog.Warn` (recommended — graceful), or
  return a clear error. Decide per how the existing eligibility/fallback is structured
  (mirror `WorktreeModeEligible` git-version fallback).
- K8s + **NFS** + worktree-per-agent must continue to use the existing NFS init-container
  provisioning path (#169) unchanged.

### T2 — Validate K8s × NFS worktree-per-agent
- Confirm (with a test where feasible) that on K8s + NFS, worktree-per-agent provisions the
  base once on the RWX export, each agent gets its worktree, and the pod mount (PVC +
  subPath) resolves to the worktree. Phase 2 T3 validated `nfsBackend.Resolve` +
  `ProvisionShared` for NFS; extend to the K8s pod-spec/mount layer
  (`pkg/runtime/k8s_runtime.go` / `k8s_nfs_test.go`) — assert the worktree subPath mount is
  produced and the node-local-on-K8s guardrail rejects.

### T3 — Docs
- Already updated in `worktree-per-agent.md` (Q4 RESOLVED, §7 matrix, limitations). Ensure
  any user-facing note (UI helper text / README) reflects "K8s worktree-per-agent requires
  NFS" if such copy exists.

## Out of scope
Node-local worktrees on K8s (the unproven hostPath/emptyDir cell); Q5 migration path
(still open, low priority — not in this phase unless ptone asks).

## Orchestration
One developer agent: investigate the K8s worktree flow, add the guardrail (T1), add/extend
the validation test (T2), confirm docs (T3). Manager verifies (build + tests + the
config-free-leaf invariant) before landing. PR on the fork → upstream compare URL for ptone.
