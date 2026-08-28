# PR-D1: UAT Scope Expansion + Scope Endpoint

**Date:** 2026-08-28
**Agent:** dev-pf-p2d-scopes
**Branch:** scion/pf-p2d-scopes

## Summary

Added UAT scopes for 8 previously uncovered resource types in the permissions
registry and added a `/api/v1/auth/scopes` endpoint that returns all valid UAT
scopes. This enables granular token scoping for the full set of hub resources.

## Changes

### 1. Registry scope additions (`pkg/hub/permissions/registry.go`)

Added `UATScope` values to 30 existing permissions across 8 resource types:

| Resource Type | Scopes Added |
|---|---|
| skill | `skill:read`, `skill:create`, `skill:list`, `skill:update`, `skill:delete`, `skill:register` |
| template | `template:read`, `template:create`, `template:list`, `template:update`, `template:delete` |
| harness_config | `harness_config:read`, `harness_config:create`, `harness_config:list`, `harness_config:update`, `harness_config:delete` |
| group | `group:read`, `group:create`, `group:list`, `group:update`, `group:delete`, `group:addMember`, `group:removeMember` |
| user | `user:read`, `user:list` |
| broker | `broker:read`, `broker:list` |
| gcp_service_account | `gcp_service_account:read`, `gcp_service_account:list`, `gcp_service_account:verify`, `gcp_service_account:assign` |

**Deliberately excluded** (per architecture doc):
- `policy.*` — policy authoring stays super-admin-only, no UAT
- `user.update`, `user.suspend`, `user.promote` — authority-modifying operations
- `hub.maintenance.execute`, `hub.admin_mode.update`, `hub.auth_reset.execute` — admin operations
- `role.*`, `role_binding.*` — not exercisable by bearer tokens

### 2. Scope list endpoint (`pkg/hub/handlers_auth.go`)

Added `GET /api/v1/auth/scopes` — returns all valid UAT scopes grouped by
resource type plus the `agent:manage` alias expansion. Accessible to any
authenticated user (not admin-only).

### 3. Route metadata (`pkg/hub/route_metadata.go`)

Added `RouteAuthenticated` entry for `/api/v1/auth/scopes`.

### 4. Route registration (`pkg/hub/server.go`)

Added route registration in `registerRoutes()`.

### 5. Web UI sync (`web/src/components/shared/token-list.ts`)

Updated `AVAILABLE_SCOPES` array to include all new scopes, keeping the list
sorted alphabetically. The existing `TestTokenScopeSurfacesDoNotExposeStaleUATScopes`
drift test verifies this stays in sync with the registry.

### 6. Tests (`pkg/hub/handlers_auth_scopes_test.go`)

- `TestHandleAuthScopes_Authenticated` — endpoint returns scopes and aliases
- `TestHandleAuthScopes_Unauthenticated` — rejects unauthenticated requests (401)
- `TestHandleAuthScopes_MethodNotAllowed` — only GET accepted
- `TestHandleAuthScopes_NonAdmin` — accessible to non-admin users
- `TestHandleAuthScopes_ContainsNewScopes` — all new scopes present in response
- `TestUATScopes_NoPolicyScopesExist` — no policy UAT scopes
- `TestUATScopes_NoAuthorityEscalationScopes` — no authority-escalation scopes
- `TestUATScopes_ValidScopesIncludeNewResourceTypes` — ValidUATScopes() updated
- `TestUATScopes_FormatConsistency` — all scopes follow resource:action format
- `TestUATScopes_AgentManageAliasStillExpands` — agent:manage alias unchanged

## Compatibility

- Existing tokens with `agent:*` or `project:*` scopes are unchanged.
- `agent:manage` alias continues to expand to agent scopes only.
- New scopes are additive — no existing scope is removed or renamed.
- No schema migration required (registry is in-memory).
