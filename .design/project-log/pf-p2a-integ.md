# PR-A5: Integrations and Hooks Handler Conversion

**Date:** 2026-08-27
**Agent:** dev-pf-p2a-integ
**Branch:** scion/pf-p2a-integ

## Summary

Converted 6 inline admin bypass sites across 4 handler files from
`user.Role() != "admin"` checks to permission-based checks via route
metadata and inline `Decide` calls. This is part of the D4 permission
framework migration (Phase 2).

## Changes

### Route Metadata (`route_metadata.go`)

Added Permission/Resource/Action fields to 5 route entries:

| Pattern | Permission | Notes |
|---------|-----------|-------|
| `/api/v1/admin/integrations` | `hub.integrations.read` | GET only at guard |
| `/api/v1/admin/integrations/teams/manifest` | `hub.teams_manifest.read` | GET only |
| `/api/v1/admin/integrations/` | `hub.integrations.read` | GET at guard; PUT/POST/DELETE inline |
| `/api/v1/admin/lifecycle-hooks` | `hub.lifecycle_hooks.read` | GET at guard; POST inline |
| `/api/v1/admin/lifecycle-hooks/` | `hub.lifecycle_hooks.read` | GET at guard; PUT/DELETE inline |

### Handler Conversions (6 bypass sites)

1. **`handlers_integrations.go:handleAdminIntegrations`** -- Removed inline
   admin check. Route guard handles read permission for GET.

2. **`handlers_integrations.go:handleAdminIntegrationByName`** -- Removed
   inline admin check. Added inline `Decide` for `hub.integrations.update`
   on PUT/POST/DELETE methods.

3. **`handlers_lifecycle_hooks.go:handleAdminLifecycleHooks`** -- Removed
   inline admin check. Added inline `Decide` for
   `hub.lifecycle_hooks.update` on POST method.

4. **`handlers_lifecycle_hooks.go:handleAdminLifecycleHookByID`** -- Removed
   inline admin check. Added inline `Decide` for
   `hub.lifecycle_hooks.update` on PUT/DELETE methods.

5. **`handlers_teams_manifest.go:handleTeamsManifestDownload`** -- Removed
   inline admin check. Route guard handles read permission for GET.

6. **`passthrough_gate.go:authorizePassthroughIdentity`** -- Replaced inline
   `userIdent.Role() != "admin"` with `Decide` call for
   `hub.integrations.update`. This preserves the compound check (admin OR
   broker owner) while routing through the authorization pipeline.

### Bypass Census (`bypass_census_test.go`)

Removed 6 PR-A5 entries from the allowlist.

### Tests

- Added `TestIntegrationsHooksPermissionConversion` (27 sub-tests) covering:
  - Route guard checks for all 5 route patterns (super-admin, hub-admin, member)
  - Inline Decide checks for dual-method routes (read-only user denied writes)
  - Unauthenticated requests denied
- Updated existing auth tests in `handlers_integrations_test.go`,
  `handlers_lifecycle_hooks_test.go`, and `handlers_teams_manifest_test.go`
  to go through the route guard.
- Updated `route_classification_test.go` to provide authzService for
  scoped admin UAT rejection test.
- Added `!no_sqlite` build tags to test files that now depend on `testServer`.

### Test Files with Build Tag Changes

- `handlers_integrations_test.go` -- added `!no_sqlite`
- `handlers_integrations_activate_secrets_test.go` -- added `!no_sqlite`
- `update_completion_test.go` -- added `!no_sqlite`
- `handlers_teams_manifest_test.go` -- added `!no_sqlite`

## Verification

`make ci` passes (fmt-check, lint, compat-literals, check-authz-guards,
test-fast, build).
