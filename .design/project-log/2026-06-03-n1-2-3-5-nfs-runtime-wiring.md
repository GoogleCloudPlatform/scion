# N1-2, N1-3, N1-5 — Docker-runtime NFS wiring

**Date:** 2026-06-03  
**Agent:** runtime-agent-2  
**Branch:** `nfs/n1-2-3-5-runtime`  
**Base:** `postgres/wave-b-integration` @ dfed3bc7

## Tasks completed

### N1-2: Broker NFS mount reconciliation (37a51b97)
- Created `pkg/runtimebroker/nfs_mount.go` with `NFSMountReconciler`
- `MountChecker` interface isolates mount syscalls for testability
- Reconciliation logic: not mounted→mkdir+mount; correct→no-op; wrong source→remount
- Per-share health tracking with `IsHealthy()`/`HealthCheckString()` for integration with broker health endpoint
- `EnsureShareMounted(shareID)` for pre-dispatch verification
- Multi-share support: shares are a set keyed by share-id
- 15 unit tests covering all reconciliation scenarios (idempotency, failures, multi-share)

### N1-3: Redirect path resolution to NFS (c919fab8)
- Created `pkg/runtime/nfs_path_guard.go` with:
  - `ValidateNotExportRoot` — isolation guard rejecting export-root binds (design §9.4)
  - `NFSSharedDirsToVolumeMounts` — NFS-aware shared dir volume mount builder
- Updated `nfsBackend.Realize` from stub to full Docker bind-mount descriptor
- Isolation guard enforced in both `Realize` and `NFSSharedDirsToVolumeMounts`
- Local backend behavior byte-identical — verified via tests
- 14 unit tests covering isolation guard, NFS shared dirs, end-to-end resolve→realize

### N1-5: Stable UID/GID branch for NFS (745f967c)
- Added `WorkspaceBackendName`, `NFSUID`, `NFSGID` to `RunConfig` (minimal threading)
- `buildCommonRunArgs`: local→os.Getuid(), nfs→NFS.UID/GID (default 1000:1000)
- Exports `SCION_WORKSPACE_BACKEND` env var for sciontool init chown skip
- Podman rootless + NFS rejected with clear error message
- 8 unit tests covering UID branching, default values, backend env, rootless rejection

## Design decisions
- **No behavior change for backend=local** — all three tasks verified this via explicit tests
- **Isolation guard as a shared function** — used by both Realize and shared-dirs helpers
- **MountChecker interface** — avoids importing exec/syscall in test code
- **WorkspaceBackendName as string** — keeps RunConfig changes minimal; avoids importing config types into runtime

## Verification
- `go build ./...` — clean
- `go vet ./pkg/runtime/... ./pkg/runtimebroker/...` — clean
- All new + existing unit tests green (37 new tests total)
