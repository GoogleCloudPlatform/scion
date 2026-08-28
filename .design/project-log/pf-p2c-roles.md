# PR-C1: Role/Binding Management Endpoints

**Date:** 2026-08-28
**Agent:** dev-pf-p2c-roles
**Branch:** scion/pf-p2c-roles

## Summary

Implemented CRUD API endpoints for role definitions, role bindings, and a
permissions registry endpoint. This enables the scoped admin model by allowing
hub-admins to manage roles and assign them to users through the API.

## Changes

### 1. Permissions Registry (`pkg/hub/permissions/registry.go`)
- Added `ResourceRole` and `ResourceRoleBinding` constants
- Added 7 new permissions: `role.read`, `role.create`, `role.update`,
  `role.delete`, `role_binding.read`, `role_binding.create`, `role_binding.delete`
- All use `CapabilityScope` kind

### 2. Hub-Admin Seed (`pkg/hub/seed.go`)
- Added all 7 role/binding permissions to `hubAdminPermissionIDs()`

### 3. Store Layer (`pkg/store/store.go`, `pkg/store/entadapter/role_store.go`)
- Added `UpdateRoleDefinition`, `DeleteRoleDefinition`, `ListAllRoleBindings`
  to the `RoleStore` interface
- Implemented in ent adapter with proper guards:
  - System roles cannot be updated or deleted
  - Roles with active bindings cannot be deleted

### 4. API Handlers (`pkg/hub/handlers_roles.go` — new file)
- Role definitions: GET/POST `/admin/roles`, GET/PUT/DELETE `/admin/roles/:id`
- Role bindings: GET/POST `/admin/role-bindings`, DELETE `/admin/role-bindings/:id`,
  GET `/admin/role-bindings/user/:userID`
- Permissions: GET `/admin/permissions`
- CanDelegate enforced on role binding creation (GrantTypeRoleBinding) and
  custom role creation (GrantTypeCustomRole)
- D10 super-admin binding guard enforced at store level

### 5. Route Metadata (`pkg/hub/route_metadata.go`)
- All 5 new route patterns use `RouteHubAdmin` classification (not RoutePolicy)

### 6. Route Registration (`pkg/hub/server.go`)
- Registered all 5 route handlers via `s.mux.HandleFunc`

### 7. Tests (`pkg/hub/handlers_roles_test.go` — new file)
- 25+ test cases covering CRUD, system role immutability, invalid permissions,
  super-admin binding rejection, unauthenticated access, validation

### 8. Route Classification (`pkg/hub/route_classification_test.go`)
- Added new routes to `routePermissionClassifications` map

## Verification

- `make ci` passes (build, lint, format, all tests)

## Boundaries Respected

- Did NOT modify super-admin bypass in checkAccessForUser
- Did NOT build PolicyBoundary
- Did NOT allow scoped credentials to carry super-admin bypass
- Did NOT modify CanDelegate logic
- Did NOT touch policy handlers
- Used RouteHubAdmin (not RoutePolicy) for all new routes
