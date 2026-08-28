# Project Log: PR-B3 — Quota API Endpoints

**Date:** 2026-08-28
**Agent:** dev-pf-p2b-api
**Branch:** scion/pf-p2b-api
**PR:** PR-B3 (Permissions Phase 2B — Quota Management API)

## Summary

Implemented the quota management API layer — 13 endpoints across 3 resource types (limit definitions, entitlement bindings, usage queries) plus a self-service `/usage/me` endpoint for regular users. This builds on PR-B1 (schema/store) and PR-B2 (QuotaService enforcement).

## Changes

### Permissions (`pkg/hub/permissions/registry.go`)
- Added `ResourceQuota = "quota"` constant
- Added 4 permission entries: `quota.read`, `quota.create`, `quota.update`, `quota.delete`
- All use `CapabilityScope` kind

### Hub-Admin Role (`pkg/hub/seed.go`)
- Added all 4 quota permissions to the `hubAdminPermissionIDs` curated set

### API Handlers (`pkg/hub/handlers_quota.go` — new, ~470 lines)
- **Limit Definitions:** GET/POST `/admin/limits`, GET/PUT/DELETE `/admin/limits/:id`
- **Entitlement Bindings:** GET/POST `/admin/limits/:id/entitlements`, GET/PUT/DELETE `/admin/entitlements/:id`
- **Usage Queries:** GET `/admin/usage`, GET `/admin/usage/:limitID`, GET `/usage/me`
- System-seeded limit definitions (`System == true`) are protected from deletion (returns 403)
- Write operations use inline `Decide()` authorization checks
- Nested entitlements route parsed via path decomposition in `handleAdminLimitByID`

### Route Metadata (`pkg/hub/route_metadata.go`)
- 6 new entries: 5 `RouteHubAdmin` quota routes + 1 `RouteAuthenticated` for `/usage/me`

### Route Registration (`pkg/hub/server.go`)
- 6 new `HandleFunc` calls with `guarded()` wrappers

### Tests (`pkg/hub/handlers_quota_test.go` — new, ~420 lines)
- 33 tests covering:
  - Full CRUD for limit definitions (create, read, list, update, delete)
  - Cannot-delete-system-seeded invariant
  - Full CRUD for entitlement bindings
  - Usage summary and per-limit usage queries
  - `/usage/me` accessible by non-admin users
  - Admin endpoints denied for non-admin users and unauthenticated requests
  - Hub-admin access verification
  - Method-not-allowed checks

### Route Classification Test (`pkg/hub/route_classification_test.go`)
- Added 6 new route entries to `routePermissionClassifications` map

## Verification

- `make ci` passes (fmt-check, lint, compat-literals, check-authz-guards, test-fast, build)
- All 33 new tests pass

## Design Decisions

1. **Usage queries at system scope:** The `/usage/me` endpoint resolves effective limits and counts active reservations at system scope. Project-scoped usage would require iterating over the user's projects, which is deferred to a future enhancement.

2. **Inline write authorization:** Following the `handlers_lifecycle_hooks.go` pattern, write operations (POST/PUT/DELETE) use `requireWritePermissionForQuota()` with inline `Decide()` calls, since the route guard only checks the read permission at the route metadata level.

3. **No store interface changes:** Per the brief's boundaries, no modifications were made to the QuotaStore interface. The handlers compose existing store methods.
