# Phase 1D: Declarative Route Guards and Authorized Lists

**Date**: 2026-08-25
**Agent**: pf-1d-dev
**Branch**: scion/auth-refactor

## Summary

Implemented Phase 1D of the auth refactor: declarative route metadata, guard
wrappers for all registered routes, and extension of the `authorizedList`
filtering pattern to agents and projects.

## Work Items Completed

### 1. Route Metadata Table (`pkg/hub/route_metadata.go`)

- Defined `RouteClassification` enum with 8 categories: `RoutePublic`,
  `RouteAuthenticated`, `RoutePolicy`, `RouteHubAdmin`, `RouteWorkstation`,
  `RouteBrokerHMAC`, `RouteAgentToken`, `RouteWebhook`.
- Defined `RouteMetadata` struct carrying `Pattern`, `RouteID`,
  `Classification`, `Permission`, `Resource`, and `Action`.
- Built `routeMetadataTable` covering all 110+ registered routes with
  classification and, for policy routes, the canonical permission ID.

### 2. Declarative Route Guard Wrappers

- Implemented `routeGuard()` method that switches on classification to apply
  the appropriate auth check before the handler runs.
- `guarded()` helper looks up metadata and wraps the handler; fails closed
  with 500 if a route has no metadata entry.
- Wrapped all route registrations in `registerRoutes()` with `s.guarded()`.
- Removed redundant `requireAdminHandler()` wrappers from admin routes (the
  guard handles it).
- Removed redundant `requireWorkstation` wrappers (the guard handles it).

**Classification decisions:**

| Route | Classification | Rationale |
|-------|---------------|-----------|
| Pre-start hooks | `RouteAuthenticated` | GET allows any authenticated user (non-admins see redacted scripts); POST/PUT/DELETE require admin enforced by handler |
| Hub injected skills | `RouteAuthenticated` | GET allows any authenticated user; PUT requires admin enforced by handler |
| Policy routes | Pass-through guard | Multiple auth models (user, agent, broker); some support anonymous access (skills); handler performs per-resource authorization |
| Broker HMAC routes | Pass-through guard | Already wrapped with broker auth middleware at the mux level |
| Webhook routes | Pass-through guard | Signatures verified inside the handler |

### 3. Authorized List Filtering Extension

**Agents (`pkg/hub/handlers_agents_core.go`):**
- Non-admin callers now go through `authorizedList` with `AuthorizeReadBatch`
  for policy-enforced filtering.
- Admin callers use direct store query (unchanged behavior).
- Capabilities computed only for authorized items.

**Projects (`pkg/hub/handlers_projects_core.go`):**
- Same pattern as agents: non-admin through `authorizedList`, admin direct.

**Store cursor migration:**
- Migrated `ListAgents` and `ListProjects` in `pkg/store/entadapter/` to the
  Phase 1B stabilized cursor pattern: `decodeListCursor`/`encodeListCursor`
  with `CursorBinding`, `keysetBeforeCursor` predicate, `SkipTotalCount`.
- Order changed to `(created DESC, id DESC)` for stable keyset pagination.

### 4. Route Coverage Test Enhancement

- `TestRegisteredRoutesHaveRouteMetadata`: verifies every registered route
  has an entry in `routeMetadataTable` with non-empty Classification and
  RouteID.
- `TestRouteMetadataPermissionsAreValid`: verifies `RoutePolicy` entries
  reference valid permission IDs from the registry.
- `TestRouteGuardsDenyUnauthorized`: tests representative routes from each
  classification to verify guards correctly deny unauthorized access.

## Key Design Decisions

1. **RoutePolicy guard is pass-through**: `DecideFromContext` was initially
   used for scope-level permission checks on policy routes, but it
   default-denied all non-admin users because access flows through ownership
   and project membership, not scope-level policy grants. Skills also support
   anonymous access for public resources, and broker identity uses a separate
   context key not visible to `GetIdentityFromContext`. The guard classifies
   the route; enforcement stays in the handler.

2. **Original limit parsing preserved**: The `parseAuthorizedListLimit`
   function (default 50, cap 100) is appropriate for the internal
   `authorizedList` batching, but the user-facing limit for agents and
   projects retains the original default of 500 with no cap to preserve
   backward compatibility.

3. **Guards are defense-in-depth**: Existing handler-local authorization
   checks are NOT removed. Guards are a second layer of protection.

## Commits

1. `9b9350d` feat: add declarative route metadata and guard wrappers
2. `9f537cf` feat: extend authorized list filtering to agents
3. `648f168` feat: extend authorized list filtering to projects
4. `e275a31` feat: enhance route coverage tests for declarative metadata
5. `825011a` fix: correct route classifications and restore original limit parsing
6. `702052a` style: apply gofmt to route metadata table
7. `5ffcfd0` chore: add route_metadata.go to legacy grove allowlist

## Test Results

- `go test ./pkg/hub -timeout=600s -count=1`: All pass (one pre-existing
  flaky test `TestCheckWorkspaceStorageHealth_GKEUnverifiableMountStaysReady`
  fails intermittently under full parallel execution but passes in isolation).
- `make ci`: Passes.
