# Phase 1F: CanDelegate Admission Gate

**Date**: 2026-08-25
**Agent**: pf-1f-dev
**Branch**: scion/auth-refactor
**Commit**: 49ef1b4

## Summary

Implemented Phase 1F of the Permissions Foundation auth refactor: the
CanDelegate admission gate that prevents actors from delegating authority
they do not themselves possess. This is the core privilege-escalation guard
for the authorization system. Also resolved Phase 1E deferred items by
removing the legacy group-based project membership fallback and adding
startup reconciliation for super-admin role bindings.

## Work Items Completed

### 1. Phase 1E Deferred Items (Part A)

- **Removed legacy group-based fallback** (`pkg/hub/authz.go`): Deleted
  `legacyIsProjectOwnerOrAdmin` function entirely. `isProjectOwnerOrAdmin`
  now uses role bindings as the sole source of truth for project membership.
- **Added `IsSystemAdmin` method** (`pkg/hub/authz.go`): Explicit
  role-binding-based super-admin check that queries system-scoped
  super-admin role bindings. Complements the fast-path
  `IsUnscopedLocalPlatformAdmin` which uses `User.Role`.
- **Added `ReconcileSuperAdminBindings`** (`pkg/hub/seed.go`): Startup
  reconciliation function (~90 lines) that ensures consistency between
  `User.Role=="admin"` and system-scoped super-admin role bindings. Forward
  direction creates missing bindings; reverse direction logs warnings for
  orphaned bindings (does not auto-remove for safety).
- **Updated startup sequence** (`pkg/hub/server.go`): Added
  `ReconcileSuperAdminBindings` call after `BackfillRoleBindings`.
- **Updated test helpers** (`pkg/hub/authz_project_owner_test.go`):
  `addProjectMemberWithRole` now creates corresponding project role bindings
  (maps group roles to project role definitions) to work with the new
  role-binding-only membership checks.
- **Updated project creation** (`pkg/hub/handlers_projects_core.go`):
  `createProjectMembersGroupAndPolicy` now also creates role bindings for
  the project creator. Role binding creation failure elevated from warning
  to HTTP 500 error.

### 2. CanDelegate Core Logic (Part B)

- **New file: `pkg/hub/authz_candelegate.go`** (429 lines): Core
  CanDelegate implementation with GrantDescriptor pattern.
- **GrantType constants**: `GrantTypeRoleBinding`, `GrantTypeGroupMembership`,
  `GrantTypeAgentDelegation`, `GrantTypeCustomRole`, `GrantTypePolicy`,
  `GrantTypeProjectMembership`.
- **GrantDescriptor struct**: Describes a specific authority grant with
  fields for each grant type (role definition ID, group info, agent role,
  project ID, custom role permissions).
- **`CanDelegate` method**: Main entry point. Checks nil actor, enforces
  UAT scope constraints, provides super-admin bypass (unscoped local
  platform admins only), then routes to type-specific delegation checks.
- **Type-specific checks**:
  - `canDelegateRoleBinding`: Resolves target role's permissions, verifies
    actor holds all of them via role bindings and policies.
  - `canDelegateGroupMembership`: Maps group role to project role, checks
    actor holds equivalent permissions.
  - `canDelegateAgent`: For agent callers, checks scope containment. For
    user callers, checks `agent.create` permission (role ceiling governs
    effective role separately).
  - `canDelegateCustomRole`: Verifies actor holds all permissions in the
    custom role.
  - `canDelegatePolicy`: Returns deny for non-super-admin (policy authoring
    is super-admin-only).
  - `canDelegateProjectMembership`: Checks actor is project owner or admin.
- **Permission resolution**: `actorHoldsAllPermissions` checks both
  role-binding-granted and policy-granted permissions.
  `getPolicyGrantedPermissions` resolves permissions via the policy engine.

### 3. CanDelegate Wiring (Part C)

- **Agent creation** (`pkg/hub/handlers_agents_core.go`): CanDelegate
  check in `createAgentInProject` after effective role computation and
  before NoAuth mapping. Returns HTTP 403 with code
  `INSUFFICIENT_DELEGATION_AUTHORITY`.
- **Group membership** (`pkg/hub/handlers_groups.go`): CanDelegate check
  in `addGroupMember` for user callers adding user-type members. Agent
  callers are excluded (already constrained by role-hierarchy).
- **Scheduled agent dispatch** (`pkg/hub/server.go`): CanDelegate checks
  at fire time in `authorizeScheduledAgentCreate` for both agent and user
  callers.
- **Policy authoring** (`pkg/hub/handlers_policies.go`): Documented that
  CanDelegate enforcement is implicit via the existing `requireAdmin` gate
  (super-admin-only).

### 4. Comprehensive Tests (Part D)

- **New file: `pkg/hub/authz_candelegate_test.go`** (726 lines, 30+ tests):
  - Role binding delegation: super-admin bypass, project admin delegation,
    insufficient permissions, non-existent role definitions.
  - Group membership delegation: project-scoped groups, admin-to-member
    downward delegation, member cannot add admin.
  - Agent delegation: user creating agents, agent creating sub-agents
    (scope containment), scope escalation prevention.
  - Custom role delegation: actor with superset, actor missing permissions.
  - Policy authoring: non-admin denied, super-admin allowed.
  - UAT scope constraints: project-scoped credentials cannot create
    system-scoped grants, cannot delegate outside their bound project.
  - Phase 1E deferred items: role-binding-only membership, IsSystemAdmin
    verification.
  - Edge cases: nil identity, empty actor ID.

## Design Decisions

1. **GrantDescriptor pattern over interface hierarchy**: A single struct
   with type tag is simpler and more extensible than a type hierarchy.
   Matches the established pattern in the codebase.

2. **Agent delegation checks `agent.create` not all agent permissions**:
   Initially checked if user holds ALL permissions an agent role grants.
   This was too strict - project members couldn't create full-role agents
   because they lack `agent.delete`, `agent.attach`. Changed to verify
   `agent.create` permission only; the role ceiling logic
   (`ResolveEffectiveRole`) separately governs the effective agent role.

3. **Permission resolution from both role bindings and policies**: Users
   acquire permissions from two sources - role bindings and policies.
   CanDelegate checks both to avoid false denials for users who have
   permissions via policy but not role binding.

4. **UAT constraints**: Scoped credentials cannot create system-scoped
   grants or delegate outside their project. This prevents token theft
   from escalating beyond the token's intended scope.

5. **Agent callers excluded from group membership CanDelegate**: Agent
   callers adding group members were being blocked by CanDelegate. Since
   agents are already constrained by role-hierarchy checks, the CanDelegate
   gate only applies to user callers.

6. **ReconcileSuperAdminBindings logs but doesn't auto-remove**: Orphaned
   role bindings (binding exists but `User.Role != "admin"`) are logged as
   warnings but not removed. Auto-removal could be dangerous if the user
   table is temporarily inconsistent.

## Files Changed

| File | Change |
|------|--------|
| `pkg/hub/authz.go` | Removed legacy fallback, added IsSystemAdmin |
| `pkg/hub/authz_candelegate.go` | **NEW** - Core CanDelegate implementation |
| `pkg/hub/authz_candelegate_test.go` | **NEW** - 30+ delegation tests |
| `pkg/hub/authz_project_owner_test.go` | Updated test helper for role bindings |
| `pkg/hub/demo_policy_test.go` | Updated comment |
| `pkg/hub/handlers_agents_core.go` | Wired CanDelegate to agent creation |
| `pkg/hub/handlers_groups.go` | Wired CanDelegate to group membership |
| `pkg/hub/handlers_policies.go` | Added documentation comment |
| `pkg/hub/handlers_projects_core.go` | Role binding error handling, binding in group setup |
| `pkg/hub/identity.go` | Updated IsUnscopedLocalPlatformAdmin comment |
| `pkg/hub/role_binding_test.go` | Fixed duplicate binding creation |
| `pkg/hub/seed.go` | Added ReconcileSuperAdminBindings |
| `pkg/hub/server.go` | Added reconciliation call, CanDelegate in scheduled dispatch |

## Test Coverage

- 30+ new tests in `authz_candelegate_test.go`
- All new tests pass (`go test ./pkg/hub/ -run TestCanDelegate`)
- Full test suite (`go test ./pkg/hub/ -timeout=600s -count=1`) passes
  except for pre-existing flaky tests (different tests fail each run, all
  pass individually - confirmed by running against main branch)
- `make ci` passes cleanly

## Risks and Known Issues

1. **Pre-existing flaky tests**: 7-12 tests in the full hub suite fail
   non-deterministically due to test parallelism/ordering issues. These
   are pre-existing and unrelated to Phase 1F changes. All pass when run
   individually.

2. **ReconcileSuperAdminBindings is append-only**: It creates missing
   bindings but does not remove orphaned ones. If a user is demoted from
   admin, their system-scoped super-admin role binding will persist until
   manually cleaned up. The log warning surfaces this for operators.

3. **Policy-granted permissions in delegation**: The
   `getPolicyGrantedPermissions` function evaluates policies to determine
   what permissions an actor has. If policy evaluation is slow or complex,
   this could add latency to delegation checks. Current implementation
   queries all policies and evaluates each.
