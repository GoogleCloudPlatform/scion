# PR-A2: Settings/Config Conversion + M1/M2 Security Fix

**Date:** 2026-08-27
**Branch:** `scion/pf-p2a-settings`
**Author:** dev-pf-p2a-settings

## Summary

This PR delivers two things: security fixes M1 and M2 from the PR-A1 audit, and the first Layer 2 handler family conversion (settings/config endpoints) from inline role checks to permission-based checks via route metadata.

## Changes

### Part A: Security Fixes

**M1 — Deny hub-level resources in `enforceUATConstraints`** (`pkg/hub/authz.go`)

Added an `else if` branch after the existing project constraint checks. Resources with a non-empty type that are neither type `"project"` nor parented by a project are now denied with reason `"token not scoped for hub-level resources"`. This closes the gap where hub-level resource types (e.g., `"hub"`, `"user"`, `"group"`) silently fell through the project constraint check.

**M2 — Clear UATScope from hub permissions** (`pkg/hub/permissions/registry.go`, `web/src/components/shared/token-list.ts`)

Set `UATScope: ""` for all 7 hub permissions that had 3-part scope strings (`hub:settings:read`, etc.). These never matched at runtime because `enforceUATConstraints` constructs 2-part scopes. Removed the corresponding 7 hub scope entries from the frontend token scope picker to prevent misleading UI options.

### Part B: Settings/Config Handler Conversion

**Route metadata** (`pkg/hub/route_metadata.go`): Added `Permission`, `Resource`, and `Action` fields to 4 route entries:
- `/api/v1/admin/server-config/schema` → `hub.config.read`
- `/api/v1/admin/server-config/sections/` → `hub.config.update`
- `/api/v1/admin/server-config` → `hub.config.update`
- `/api/v1/admin/project-defaults` → `hub.project_defaults.update`

**Inline admin checks removed** from 4 handler sites:
- `admin_settings.go:handleAdminServerConfig` — removed user+role check
- `admin_settings.go:handleAdminServerConfigSectionReset` — removed role check (kept user for logging)
- `admin_settings_db.go:handleAdminServerConfigSchema` — removed user+role check
- `admin_project_defaults.go:handleAdminProjectDefaults` — removed user+role check

**Bypass census** (`pkg/hub/bypass_census_test.go`): Removed the 4 PR-A2 allowlist entries.

### Tests

Added `pkg/hub/routeguard_settings_test.go` with:
- `TestRouteGuardSettingsConversion`: 12 test cases (4 endpoints x 3 roles: super-admin allowed, hub-admin allowed, member denied)
- `TestM1_UATDeniedForHubLevelResources`: 5 test cases covering hub resources denied, user resources denied, project-child resources allowed, matching project allowed, mismatched project denied
- `TestM2_HubPermissionsNoUATScope`: Registry scan verifying all hub permissions have empty UATScope

## Files Changed

- `pkg/hub/authz.go` — M1 fix
- `pkg/hub/permissions/registry.go` — M2 fix (clear UATScope)
- `web/src/components/shared/token-list.ts` — M2 fix (remove hub scopes from UI)
- `pkg/hub/route_metadata.go` — permission fields on 4 routes
- `pkg/hub/admin_settings.go` — remove 2 inline admin checks
- `pkg/hub/admin_settings_db.go` — remove 1 inline admin check
- `pkg/hub/admin_project_defaults.go` — remove 1 inline admin check
- `pkg/hub/bypass_census_test.go` — remove 4 allowlist entries
- `pkg/hub/routeguard_settings_test.go` — new test file (17 test cases)

## Verification

All tests pass via `make ci`.
