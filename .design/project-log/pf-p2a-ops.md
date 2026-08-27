# PR-A4: Operations Handler Conversion

**Date:** 2026-08-27
**Agent:** dev-pf-p2a-ops
**Branch:** scion/pf-p2a-ops
**Base:** main (post PR-A1)

## Summary

Converted 10 operations handler bypass sites from inline `user.Role() != "admin"` checks to permission-based route guard enforcement via route metadata. This is part of the D4 incremental conversion series (PR-A4).

## Changes

### Route Metadata (`pkg/hub/route_metadata.go`)
Added Permission/Resource/Action to 13 RouteHubAdmin entries:

| Pattern | Permission |
|---------|-----------|
| `/api/v1/admin/maintenance` | `hub.admin_mode.update` |
| `/api/v1/admin/maintenance/operations` | `hub.maintenance.execute` |
| `/api/v1/admin/maintenance/operations/` | `hub.maintenance.execute` |
| `/api/v1/admin/maintenance/migrations/` | `hub.maintenance.execute` |
| `/api/v1/admin/maintenance/check-updates` | `hub.maintenance.execute` |
| `/api/v1/admin/maintenance/restart` | `hub.maintenance.execute` |
| `/api/v1/admin/scheduler` | `hub.scheduler.read` |
| `/api/v1/admin/agents/reset-auth-all` | `hub.auth_reset.execute` |
| `/api/v1/admin/diagnostics/logs` | `hub.diagnostics.read` |
| `/api/v1/admin/diagnostics/logs/stream` | `hub.diagnostics.read` |
| `/api/v1/admin/health/summary` | `hub.health.read` |
| `/api/v1/metrics/` | `hub.metrics.read` |
| `/api/v1/admin/metrics-dashboard` | `hub.metrics.read` |

### Handler Conversions (10 bypass sites across 5 files)
- **admin_maintenance.go** (4 sites): `handleAdminMaintenanceOps`, `handleAdminMaintenanceMigrations`, `handleCheckForUpdates`, `handleAdminRestart`
- **admin_mode.go** (2 sites): `handleAdminMaintenance`, `handleAdminScheduler`
- **admin_reset_auth.go** (1 site): `handleAdminResetAuthAll`
- **handlers_diagnostics.go** (2 sites): `handleDiagnosticsLogs`, `handleDiagnosticsLogsStream`
- **handlers_health_summary.go** (1 site): `handleHealthSummary`

### Preserved
- AdminModeMiddleware bypass at `admin_mode.go:113` (infrastructure, not a handler)
- User identity extraction where still needed for logging/execution context

### Bypass Census
Removed 10 PR-A4 handler entries from the allowlist. Kept the AdminModeMiddleware `IsUnscopedLocalPlatformAdmin(user)` entry.

### Tests
Added `routeguard_ops_permission_test.go` verifying:
- Super-admin allowed for all converted endpoints
- Hub-admin allowed for hub-admin-accessible endpoints (`hub.scheduler.read`, `hub.health.read`, `hub.metrics.read`)
- Hub-admin denied for super-admin-only endpoints (`hub.maintenance.execute`, `hub.auth_reset.execute`, `hub.diagnostics.read`, `hub.admin_mode.update`)
- Regular member denied for all converted endpoints
- Route metadata field completeness

## Super-Admin-Only Behavior
Several permissions are intentionally NOT in the hub-admin role:
- `hub.maintenance.execute`, `hub.auth_reset.execute`, `hub.diagnostics.read`, `hub.admin_mode.update`

These are enforced correctly: super-admins pass via step-1 bypass in `checkAccessForUser`; hub-admins (who lack these permissions) are denied at step 3.
