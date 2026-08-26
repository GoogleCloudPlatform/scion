# P1: Cloud Run Sandbox Registration

**Date:** 2026-08-25
**Branch:** `scion/p1-registration`
**Agent:** `dev-p1-registration`

## Summary

Added `cloudrun-sandbox` as a new runtime type in the registration surface. The
runtime itself is a stub that errors on every lifecycle method (real implementation
is P3). Also fixed the pre-existing stale JSON schema enum and added
`SandboxLauncherAvailable()` with autodetect wiring.

## Changes

### `pkg/runtime/cloudrun_sandbox_runtime.go` (new)
- `CloudRunSandboxRuntime` struct implementing the full `Runtime` interface
- All lifecycle methods (`Run`, `Stop`, `Delete`, `List`, `Exec`, `Attach`,
  `GetLogs`, `Sync`, `GetWorkspacePath`) return "not yet implemented" errors
- Image methods (`ImageExists`, `PullImage`, `ImageID`, `RemoveImage`) return
  no-op success (omni-image is always present per design doc section 4.2)
- `Name()` returns `"cloudrun-sandbox"`, `ExecUser()` returns `"scion"`
- `SandboxLauncherAvailable()` function that probes for the sandbox binary at
  `/usr/local/gcp/bin/sandbox`

### `pkg/runtime/factory.go`
- Added `"cloudrun-sandbox"` to the profile-name allowlist
- Added `CLOUD_RUN_INSTANCE` + `SandboxLauncherAvailable()` autodetect probe
  before the existing `K_SERVICE` check in the auto-detect block
- Added corresponding `CLOUD_RUN_INSTANCE` detection in the `case "docker":`
  switch branch (defensive, matches existing `K_SERVICE` override pattern)
- Added `case "cloudrun-sandbox":` in the type switch

### `pkg/config/schemas/settings-v1.schema.json`
- Fixed stale runtime type enum: added `"cloudrun-instances"` and
  `"cloudrun-sandbox"` (was missing `cloudrun-instances` too)

### `pkg/config/settings_v1.go`
- Added `V1CloudRunSandboxConfig` struct with `SandboxBin` field
- Added `CloudRunSandbox` field to `V1RuntimeConfig`

### `pkg/runtimebroker/pty_handlers.go`
- Added `isCloudRunSandbox` check in `LocalPTYSession.Run()`,
  `StreamPTYHandler.Run()`, and `waitForTmuxSession()` — all return
  "not yet implemented" errors to prevent Docker CLI grammar from being
  used with a sandbox runtime

### `pkg/runtime/cloudrun_sandbox_runtime_test.go` (new)
- Tests for `Name()`, `ExecUser()`, all lifecycle methods, image no-ops
- Tests for `SandboxLauncherAvailable()` (absence case)
- Factory tests: direct profile name, settings-based resolution,
  `CLOUD_RUN_INSTANCE` precedence over docker

## Design Decisions

- Added `CLOUD_RUN_INSTANCE` detection in the `case "docker":` switch branch
  in addition to the auto-detect block. Without this, default settings
  resolving to "docker" would skip auto-detection entirely, making the
  `CLOUD_RUN_INSTANCE` probe unreachable on a real Cloud Run Instance with
  default configuration. This mirrors the existing `K_SERVICE` pattern.

## Verification

- `go test ./pkg/runtime/... ./pkg/config/... ./cmd/...` all pass
- JSON schema validates as valid JSON
- New tests cover all stub methods, factory wiring, and autodetect paths
