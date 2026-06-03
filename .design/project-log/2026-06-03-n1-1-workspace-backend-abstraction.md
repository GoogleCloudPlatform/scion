# N1-1: Workspace Storage Backend Abstraction

**Date:** 2026-06-03
**Agent:** runtime-agent-1
**Branch:** `nfs/n1-1-backend-abstraction`
**Commit:** `eca1b882`
**Status:** Complete

## What was done

Introduced the `WorkspaceBackend` interface in `pkg/runtime/` with three methods mapping to the NFS design's three questions:

1. **Resolve** — deterministic path computation from project/agent IDs + sharing mode. No DB, no I/O.
2. **Provision** — stub for N1-4 (NFS clone+advisory-lock); no-op for local.
3. **Realize** — stub for N1-3 (mount wiring); local returns today's bind-mount descriptor.

### Files added (4)

- `pkg/runtime/workspace_backend.go` — interface + input/output structs + `SelectWorkspaceBackend` helper
- `pkg/runtime/workspace_backend_local.go` — `localBackend` wrapping today's behavior
- `pkg/runtime/workspace_backend_nfs.go` — `nfsBackend` with complete Resolve, stub Provision/Realize
- `pkg/runtime/workspace_backend_test.go` — 22 table-driven tests

### Key design decisions

- **Package placement:** `pkg/runtime/` chosen because it can import both `config` and `store` without cycles (neither imports `runtime`), and `runtimebroker` already imports `runtime`.
- **SelectWorkspaceBackend** routes `nfsBackend` only when `Backend=nfs` AND mode ∈ {SharedPlain, WorktreePerAgent}. ClonePerAgent always gets `localBackend` — the deliberate node-local escape hatch per design §3.1.
- **localBackend.Resolve** returns `ProjectDir` as-is — faithful to today's broker path resolution, zero behavior change.
- **nfsBackend.Resolve** uses first configured share, lays out `<SubPathRoot>/<projectID>/workspace` and `<SubPathRoot>/<projectID>/shared-dirs/<name>`.

### Test results

- `go build ./...` — clean
- `go vet ./pkg/runtime/` — clean
- All 22 new tests pass; existing runtime tests unaffected

## Process notes

- Shared workspace was concurrently switched to another branch by a parallel agent. Resolved by creating a git worktree at `/tmp/nfs-n1-1` on the correct branch, avoiding conflicts.
- Phase-0 config structs (`V1WorkspaceStorageConfig`, `V1NFSConfig`, `V1NFSShare`, `WorkspaceSharingMode`) were already present on the integration branch — used directly.
