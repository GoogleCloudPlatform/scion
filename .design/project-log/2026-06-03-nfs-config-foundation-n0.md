# NFS Workspace Config Foundation (N0-1, N0-2, N0-3)

**Agent:** config-agent  
**Date:** 2026-06-03  
**Branch:** postgres/wave-b-integration  
**Commit:** d8f8c987

## Summary

Landed the Phase 0 config-only foundation for NFS-backed workspace storage.
Three tasks, all no-op at runtime (backend defaults to "local").

### N0-1: Workspace-storage config block
- Added `V1WorkspaceStorageConfig`, `V1NFSConfig`, `V1NFSShare` types to `pkg/config/settings_v1.go`
- Wired `WorkspaceStorage` into `V1ServerConfig`
- `ApplyNFSDefaults()` fills mount_options, UID/GID=1000, subpath_root="projects" when backend=nfs
- No NFS block materialized for local/empty backend
- Tests: YAML round-trip via LoadVersionedSettings, JSON round-trip, defaults, nil-safety

### N0-2: VolumeMount nfs type + validation
- Added `Server` field to `VolumeMount` (`pkg/api/types.go`)
- Extended `Validate()` with `case "nfs":` requiring Server+Source+Target
- Updated default error message to list all three valid types
- Flipped existing nfs fixtures from rejected→valid in both `types_test.go` and `templates_test.go`
- Added negative cases: nfs-missing-server, nfs-missing-source, genuinely-invalid type ("bogus")

### N0-3: Workspace-sharing-mode enum alignment
- Added typed `WorkspaceSharingMode` (string) with 3 canonical values (`pkg/store/models.go`)
- `ResolveWorkspaceSharingMode(label)` maps legacy label values, new values, empty/unknown→default
- Existing `LabelWorkspaceMode`, `WorkspaceModeShared`, `WorkspaceModePerAgent` constants unchanged (lossless)
- Unit tests in new `pkg/store/models_test.go` cover all cases

## Process Notes
- Pre-existing test failures in pkg/config (IsInsideProject, FindProjectRoot, etc.) are environment-dependent and unrelated to these changes
- All N0-specific tests green; `go build ./...` clean
