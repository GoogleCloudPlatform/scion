# N2-1..N2-5: GKE NFS Workspace Realization (Wave 2, Model B)

**Date:** 2026-06-03  
**Agent:** k8s-agent  
**Branch:** `nfs/n2-gke` (from `postgres/wave-b-integration`)

## Summary

Implemented all five N2 tasks for GKE NFS workspace support in
`pkg/runtime/k8s_runtime.go`. These changes realize the NFS workspace
backend on Kubernetes, converting the design's §5 (Model B) into
working pod spec transformations.

## Commits

| SHA | Task | Description |
|-----|------|-------------|
| `75032d8a` | N2-1 | NFS-backed workspace volume — replace EmptyDir with PVC+subPath |
| `2da9afbc` | N2-2 | Init-container workspace provisioning with sentinel idempotency |
| `caf874d5` | N2-3 | Skip workspace kubectl cp when backend=nfs |
| `36737080` | N2-4 | Stable FSGroup/UID (NFS GID default 1000) |
| `45b95293` | N2-5 | Generalize shared-dir PVC helpers for NFS subPath |

## Design Decisions

1. **PVC+subPath isolation (N2-1):** Each NFS pod mounts the shared PVC with
   `subPath: projects/<pid>/workspace`, ensuring pods only see their project's
   subtree — never the export root. Falls back to EmptyDir if NFSPVClaimName
   is empty.

2. **Init-container provisioning (N2-2):** Uses a `workspace-provision` init
   container that checks `.scion-provisioned` sentinel before cloning. The
   advisory lock is NOT used in-pod — init containers serialize per-pod
   naturally, and the sentinel provides cross-pod idempotency. Full advisory
   lock integration deferred to NM2 live cluster gate.

3. **Workspace sync skip (N2-3):** NFS workspace bytes are pre-populated by
   the init container, so kubectl cp is skipped. Home-dir/secret sync and the
   startup gate are RETAINED for both backends.

4. **FSGroup branching (N2-4):** NFS pods use stable GID (config or default
   1000) instead of host GID. This avoids permission issues across nodes.

5. **Shared-dir subPath (N2-5):** NFS shared dirs mount from the same PVC
   with `subPath: projects/<pid>/shared-dirs/<name>`, eliminating per-dir PVCs.
   Refactored into generalized `ensureProjectRWXClaim`/`cleanupProjectRWXClaims`
   helpers.

## New RunConfig Fields

- `NFSPVClaimName` — PVC name for the NFS workspace volume
- `NFSSubPath` — project-scoped subPath within the PVC  
- `NFSStorageClass` — StorageClass for NFS PVCs
- `GitCloneForInit` — git clone config for init-container provisioning

## Tests

All changes include unit tests in `pkg/runtime/k8s_nfs_test.go`:
- `go build ./...` clean
- `go vet` clean  
- All existing k8s_runtime tests pass (no regressions)
- 20+ new test cases covering NFS and local backend paths

## Zero Behavior Change Guarantee

Every NFS branch is gated on `config.WorkspaceBackendName == "nfs"`.
When backend is local (default/empty), all five tasks produce exactly
the same pod spec as before.
