# Project Log: pf-p2a-integ-fix2 — Consolidate inline Decide boilerplate

**Date:** 2026-08-27
**Branch:** `scion/pf-p2a-integ`
**Agent:** dev-pf-p2a-integ-fix2

## Summary

Extracted a `requireWritePermission` Server helper method to consolidate
three instances of identical inline authorization boilerplate across the
integrations and lifecycle hooks handlers.

## Changes

### New file: `pkg/hub/authz_helpers.go`
- Added `Server.requireWritePermission(w, r, permission) (UserIdentity, bool)`
- Encapsulates: identity extraction, UserIdentity type assertion, Decide call
  with hub resource + update action + given permission string
- Returns the authenticated user alongside the bool so lifecycle hooks callers
  can pass it to CRUD methods without re-extracting

### Modified: `pkg/hub/handlers_integrations.go`
- `handleAdminIntegrationByName`: replaced 18-line inline Decide block with
  single `requireWritePermission` call for PUT/POST/DELETE methods

### Modified: `pkg/hub/handlers_lifecycle_hooks.go`
- `handleAdminLifecycleHooks` (POST case): replaced inline Decide block,
  using returned `user` for `createLifecycleHook`
- `handleAdminLifecycleHookByID` (PUT/DELETE case): replaced inline Decide
  block, using returned `user` for `updateLifecycleHook`/`deleteLifecycleHook`

### Fixed: `pkg/hub/handlers_integrations_test.go`
- `TestUpdateConfig_NonHA_NeedsConfigFile`: added missing `authzService`
  initialization (pre-existing nil-pointer bug — the test was panicking
  before this change as well)

## Verification

- `make ci` passes
- `go test ./pkg/hub/... -count=1` passes (all tests, non-cached)
- Pure refactor: no behavioral change, existing tests validate correctness
