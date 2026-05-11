# Content-Aware Fallback for Workspace Path Resolution

**Date:** 2026-05-11
**Branch:** fix/workspace-path-fallback
**PR:** #47

## Problem

The workspace path fallback from `~/.scion/projects/{slug}` to `~/.scion/groves/{slug}` only checked `os.IsNotExist()`. If the `projects/` directory existed but was empty (or only contained infrastructure directories like `shared-dirs/` and `.scion/`), the fallback never triggered and the real workspace content in `groves/` was shadowed.

## Solution

Replaced all `os.IsNotExist`-based fallback checks with a content-aware `hasWorkspaceContent()` function that reads directory entries and returns `true` only if the directory contains files beyond the known infrastructure directories (`shared-dirs`, `.scion`).

### Changes

1. **`pkg/hub/handlers.go`** — Rewrote `hubNativeGrovePath()` to use `hasWorkspaceContent()` for both the `projects/` and `groves/` paths. Added the `hasWorkspaceContent()` helper.

2. **`pkg/runtimebroker/start_context.go`** — Updated the fallback in `buildStartContext()` and added a package-local copy of `hasWorkspaceContent()`.

3. **`pkg/runtimebroker/handlers.go`** — Updated three fallback sites:
   - `handleStartAgent` grove slug resolution (line ~390)
   - `handleStartAgent` workspace directory resolution (line ~601)
   - `deleteGrove` path resolution (line ~2283)

4. **`pkg/hub/handlers_grove_test.go`** — Added `TestHubNativeGrovePath_EmptyProjectsFallsBackToGroves` which creates a `projects/` dir with only `shared-dirs/` and `.scion/`, a `groves/` dir with real content, and verifies the fallback returns the groves path.

### Design Decisions

- **Duplicated `hasWorkspaceContent` across packages** rather than extracting to a shared utility. The function is small (15 lines) and the two packages (`hub` and `runtimebroker`) have no shared internal utility package. Duplication is acceptable here.
- **Did NOT modify `server.go`** — the `findAgentInHubNativeGroves` and `discoverAuxiliaryRuntimes` functions already iterate both directories correctly.

## Verification

- `go build ./...` passes
- `go vet ./pkg/hub/ ./pkg/runtimebroker/` passes
- All existing `TestHubNative*` tests pass
- New empty-directory fallback test passes
- Pre-existing `TestMessageBrokerProxy_*` failures are unrelated (reproduce on base branch)
