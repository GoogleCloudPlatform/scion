# Project Log: N1-4 + N1-6 — NFS Workspace Provisioning & Cleanup

**Date:** 2026-06-03
**Agent:** provisioning-agent
**Branch:** `nfs/n1-4-6-provisioning` (from `postgres/wave-b-integration`)
**Tasks:** N1-4 (workspace provisioning + advisory lock), N1-6 (project-deletion cleanup)

## What was done

### N1-4A: Per-project advisory lock API extension
- Extended `AdvisoryLocker` interface with `TryAdvisoryLockObject(ctx, classID, objID)` — uses Postgres's two-integer form `pg_try_advisory_lock(int4, int4)` for per-project locks
- `LockWorkspaceProvision` (0x5C101001) as classID, `StableProjectHash()` (FNV-32a) for deterministic objID from project UUID
- SQLite implementation: no-op (always acquired) — single-writer already serializes
- Implemented in `pkg/store/concurrency.go` (interface + hash helper) and `pkg/store/entadapter/locking.go` (Postgres/SQLite impl)

### N1-4B: nfsBackend.Provision implementation
- Full first-access provisioning flow in `pkg/runtime/workspace_backend_nfs.go`:
  1. Acquire per-project advisory lock (retry loop, 30 attempts × 1s)
  2. Check sentinel `.scion-provisioned` → short-circuit if present
  3. mkdir -p workspace + shared-dirs
  4. Git clone if project is git-backed
  5. chown to stable NFS UID/GID (one-time, non-fatal if unprivileged)
  6. Write sentinel atomically (temp + rename)
  7. For WorktreePerAgent: create per-agent git worktree
  8. Release lock
- ClonePerAgent mode asserted out (defense in depth)
- `ProvisionInput` extended with `Locker`, `NFSUID/NFSGID`, `AgentName`

### N1-6: Project-deletion cleanup
- `CleanupNFSProject` helper in `pkg/runtime/workspace_cleanup.go`
- Removes `<MountRoot>/<shareID>/projects/<projectID>/` subtree
- Safety: `ValidateNotExportRoot` + path traversal protection + idempotent
- Wired into broker's `deleteProject` handler via `NFSConfig` on `ServerConfig`
- Hub passes `project_id` query param; broker reads it for NFS path computation
- Extended `RuntimeBrokerClient.CleanupProject` signature to accept `projectID`

## Test coverage
- **42 new test cases** across 4 test files
- `pkg/store/concurrency_test.go`: StableProjectHash determinism, range, key uniqueness
- `pkg/store/entadapter/locking_test.go`: TryAdvisoryLockObject SQLite no-op, independence
- `pkg/runtime/workspace_provision_test.go`: Full provisioning lifecycle — git clone, sentinel short-circuit, worktree creation, two-agent worktree independence, lock retry/mutual exclusion, ClonePerAgent rejection, degraded mode (no locker), missing-field validation, branch name sanitization, sentinel atomicity
- `pkg/runtime/workspace_cleanup_test.go`: Subtree removal, idempotency, share-root refusal, path traversal refusal, project isolation, nil config, no shares, default SubPathRoot

## Findings & observations
1. The two-int advisory lock form fit naturally — no awkwardness with the existing API. The Postgres `pg_try_advisory_lock(int4, int4)` namespace is separate from the single-int form, so no collision risk.
2. Git clone in tests required `--initial-branch=main` to work across git versions.
3. `chown` in provisioning is non-fatal — tests run unprivileged and the operator may have pre-set ownership. This is by design (§9.1).
4. The `RuntimeBrokerClient.CleanupProject` interface change touched 5 implementations (HTTP transport, control channel, hybrid, authenticated, HTTP dispatcher) — all mechanical pass-throughs. The project_id query param is backward-compatible (empty = no NFS cleanup).

## Commits
```
59f6ee78 feat(store): per-project advisory lock (two-int form) for NFS provisioning guard (N1-4A)
1b1ecb0b feat(runtime): NFS workspace provisioning with advisory-lock race guard (N1-4B)
b68116ad feat(runtime): NFS project-deletion cleanup mirroring K8s cleanupSharedDirPVCs (N1-6)
7a59f4e2 fix(hub): add TryAdvisoryLockObject to lockerStore mock (test fix for N1-4A)
```
