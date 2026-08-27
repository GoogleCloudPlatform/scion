# PR-A4 Operations Handler Non-blocking Fixes

**Date:** 2026-08-27
**Branch:** `scion/pf-p2a-ops`
**Commit:** `fix: move method checks earlier + extract shared allRoutes helper (PR-A4)`

## Changes

### Fix 1: Move HTTP method checks earlier in two handlers

In `pkg/hub/admin_maintenance.go`, two handlers (`handleAdminRestart` and `handleAdminMaintenanceMigrations`) were extracting the user identity from the request context before checking the HTTP method. This meant a `MethodNotAllowed` response would still pay the cost of identity extraction for invalid methods.

Moved the `r.Method != http.MethodPost` guard to the very first line of each handler, before `GetUserIdentityFromContext`.

### Fix 2: Extract shared `allHubAdminRoutes()` helper

In `pkg/hub/route_classification_test.go`, `TestHubAdminRoutesRejectScopedAdminUAT` built its hub-admin routes slice inline. Extracted this into a reusable `allHubAdminRoutes()` function that returns a sorted slice of all routes classified as `hub-admin:*`, enabling future tests to share the same list without duplicating the logic.

## Verification

- `make ci` passes (format, vet, tests, build).
