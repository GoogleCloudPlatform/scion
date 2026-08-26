# P5 Ephemeral: Tier 0 Honesty

**Date:** 2026-08-26
**Agent:** dev-p5-ephemeral
**Branch:** scion/p5-ephemeral

## Summary

Made the Cloud Run Instance (single-node hosted, Tier 0) deployment tier
honest about ephemeral storage by adding three things: an explicit write
permission in `workspaceWriteBlocked()`, a deployment warnings banner via
the health API, and verification that the existing ephemeral path warning
remains untouched.

## Changes

### 1. Explicit ephemeral-Instance write permission

**File:** `pkg/hub/project_workspace_handlers.go`

- Added `isCloudRunInstance()` helper that checks `CLOUD_RUN_INSTANCE` env var.
  This env var is set by the platform on Cloud Run Instances but NOT on
  Cloud Run Services (which use `K_SERVICE`).
- Updated `workspaceWriteBlocked()` to explicitly recognize Cloud Run Instances
  and permit writes with a detailed comment explaining the design decision.
  Previously, writes worked by accident because `K_SERVICE` is not set on
  Instances, meaning `isCloudRunEnv()` returned false. The explicit check
  prevents breakage if someone later makes `isCloudRunEnv()` aware of Instances.

### 2. Ephemeral storage banner (health API)

**File:** `pkg/hub/handlers_health.go`

- Added `DeploymentWarnings []string` field to `HealthResponse` struct
  (omitted from JSON when empty).
- Populated `DeploymentWarnings` in `GetHealthInfo()` when running on a
  Cloud Run Instance, with a message listing what is lost on redeploy:
  workspaces, agent homes, databases, and project trees.

### 3. Frontend deployment warnings banner

**File:** `web/src/components/pages/diagnostics.ts`

- Added `deploymentWarnings` to the TypeScript `HealthResponse` interface.
- Added `renderDeploymentWarnings()` method that displays a warning banner
  (amber/warning style, consistent with existing degradation panel) when
  the health response includes deployment warnings.
- Banner renders above the status banner on the diagnostics page.

### 4. Tests

**File:** `pkg/hub/workspace_storage_test.go`

- `TestIsCloudRunInstance_Set` / `TestIsCloudRunInstance_Unset` — unit tests
  for the helper function.
- `TestWorkspaceWriteBlocked_CloudRunInstance` — verifies writes are permitted
  when `CLOUD_RUN_INSTANCE` is set (even without `K_SERVICE`).
- `TestWorkspaceWriteBlocked_CloudRunInstanceEvenWithLocalBackend` — verifies
  the CRI check takes precedence over backend config.
- `TestHealthCheck_DeploymentWarnings_CloudRunInstance` — verifies the warning
  is populated in the health response on CRI.
- `TestHealthCheck_DeploymentWarnings_NotOnCloudRunInstance` — verifies the
  warning is absent when not on CRI.

### 5. Verification

- `warnEphemeralProjectPath` in `pkg/hub/workspace_storage.go` is untouched
  (no diff). It will continue to fire correctly on Cloud Run Instance
  deployments.
- `go build ./...` passes.
- `go vet ./pkg/hub/...` passes.
- All relevant tests pass.

## Files modified

- `pkg/hub/project_workspace_handlers.go` — `isCloudRunInstance()` helper,
  `workspaceWriteBlocked()` update
- `pkg/hub/handlers_health.go` — `DeploymentWarnings` field, population logic
- `pkg/hub/workspace_storage_test.go` — 6 new tests
- `web/src/components/pages/diagnostics.ts` — deployment warnings banner
