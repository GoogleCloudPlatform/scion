# Project Log: PR-A1 Admin Permission Registry + D4 Infrastructure

**Date:** 2026-08-27
**Agent:** dev-pf-p2a-infra
**Branch:** scion/pf-p2a-infra
**Base:** 3aeb772 (Permissions Foundation Phase 1)

## Summary

Implemented Layer 1 of the D4 (admin as code bypass) resolution: the infrastructure that enables incremental handler conversion from inline admin checks to permission-based authorization.

## What Was Built

### 1. Permission Registry Additions (`pkg/hub/permissions/registry.go`)

- Added `ResourceHub = "hub"` resource type constant
- Added 5 new action constants: `ActionInvite`, `ActionSuspend`, `ActionPromote`, `ActionClone`, `ActionExecute`
- Added 28 new hub-level permissions covering settings, config, maintenance, diagnostics, health, admin mode, integrations, lifecycle hooks, allow list, project defaults, auth reset, scheduler, federation, teams manifest, validate, GitHub app, and metrics
- Added 7 extensions to existing resource types: `user.invite`, `user.suspend`, `user.promote`, `user.list`, `project.clone`, `project.list`, `skill.register`
- Total: 35 new permission IDs in the registry (57 → 92)

### 2. routeGuard Update (`pkg/hub/route_metadata.go`)

Updated the `RouteHubAdmin` case in `routeGuard` to:
- Check if `meta.Permission` is set (non-empty string)
- If set: call `Decide()` with the declared permission, constructing an `AuthzRequest` with the permission ID
- If NOT set: fall back to existing `requireAdmin` behavior

This makes incremental conversion safe — routes with no Permission in their metadata behave exactly as before.

### 3. Role Binding Permission Evaluation (`pkg/hub/authz.go`)

- Added `Permission` field to `AuthzRequest` struct for canonical permission ID
- Added role binding permission check step in `checkAccessForUser()` (between SA assign baseline and policy evaluation)
- When a permission ID is provided, the system resolves the user's effective permissions from role bindings and checks if the requested permission is granted
- This enables the D4 "second path": non-super-admin users with the correct permissions through role bindings get access

### 4. Hub-Admin Role Definition

- Added `SystemRoleHubAdmin = "hub-admin"` constant (`pkg/store/models.go`)
- Added `hubAdminPermissionIDs()` function with curated permission set (`pkg/hub/seed.go`)
- Added hub-admin to `seedRoleDefinitions()` between super-admin and hub-member

**Hub-admin includes:** user management (read/list/update/invite), all group operations, hub settings/config/health, integrations, lifecycle hooks, allow list, project defaults, scheduler, federation, teams manifest, GitHub app, metrics, validate, project oversight, skill registries

**Excluded (super-admin only):** maintenance, auth reset, diagnostics, admin mode, all policy operations, user suspend/promote

### 5. Tests

**Bypass Census Test** (`pkg/hub/bypass_census_test.go`):
- Scans all non-test `.go` files in `pkg/hub/` for admin bypass patterns
- Maintains an allowlist of ~80 authorized bypass locations
- Fails when a new bypass appears outside the allowlist
- Allowlist will shrink as handlers are converted in PR-A2 through PR-A6

**RouteGuard Permission Test** (`pkg/hub/routeguard_permission_test.go`):
- 8 test cases covering both the permission-based and fallback paths
- Validates super-admin allowed through admin bypass
- Validates hub-admin allowed through role binding grant (D4 second path!)
- Validates member denied, unauthenticated denied
- Validates hub-admin denied for super-admin-only permissions
- Validates requireAdmin fallback for unconverted routes

### 6. Frontend UAT Scope Update

Updated `web/src/components/shared/token-list.ts` AVAILABLE_SCOPES to include new UAT scopes (hub:settings:read/update, hub:config:read/update, hub:health:read, hub:integrations:read/update, project:clone, user:invite).

## Decisions Made

1. **Role binding evaluation position:** Added after the SA assign baseline (step 2.7) and before policy evaluation (step 3), maintaining the existing authorization hierarchy
2. **Permission field on AuthzRequest:** Added as an optional field rather than modifying the Resource/Action contract, preserving backward compatibility for all existing CheckAccess callers
3. **Hub-admin permissions as explicit set:** Used a curated allowlist rather than a derivation rule, matching the architect's recommendation that the permission set is a product decision

## Pre-existing Issues

- `TestHandleAdminServerConfig_Get` fails on main (pre-existing, not caused by this PR)

## Files Changed

- `pkg/hub/permissions/registry.go` — 35 new permission entries + 6 new constants
- `pkg/hub/route_metadata.go` — routeGuard RouteHubAdmin case updated
- `pkg/hub/authz.go` — Permission field on AuthzRequest, role binding check step, 5 new Action constants
- `pkg/hub/seed.go` — hub-admin role seeding + hubAdminPermissionIDs()
- `pkg/store/models.go` — SystemRoleHubAdmin constant
- `pkg/hub/bypass_census_test.go` — new test file
- `pkg/hub/routeguard_permission_test.go` — new test file
- `web/src/components/shared/token-list.ts` — updated AVAILABLE_SCOPES
