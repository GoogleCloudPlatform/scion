# Phase 1E: RoleDefinition, RoleBinding, and Dual-Read Authorization

**Date**: 2026-08-25
**Agent**: pf-1e-dev
**Branch**: scion/auth-refactor

## Summary

Implemented Phase 1E of the Permissions Foundation auth refactor: added
RoleDefinition and RoleBinding Ent schemas with full store adapters, seeded
system role definitions, backfilled role bindings from legacy User.Role and
group memberships, updated the authorization pipeline to use role bindings
with legacy fallback, added transactional project membership via role bindings
on project creation, and wrote comprehensive tests.

## Work Items Completed

### 1. Ent Schemas (`pkg/ent/schema/`)

- **RoleDefinition** (`roledefinition.go`): UUID primary key, name, description,
  scope_type (enum: system/project), permissions (JSON `[]string`), system flag,
  created/updated timestamps. Unique index on (name, scope_type). Edge to
  role_bindings.
- **RoleBinding** (`rolebinding.go`): UUID primary key, role_definition_id
  (optional/nillable UUID FK), principal_type (enum: user/agent), principal_id,
  scope_type (enum: system/project), scope_id, created_by, created timestamp.
  Indexes on (principal_type, principal_id), (role_definition_id),
  (scope_type, scope_id), and a unique composite index on all five binding
  dimensions. Edge from role_definition.
- Ran `go generate ./pkg/ent/...` to produce all generated Ent code.

### 2. Store Models and Interface (`pkg/store/`)

- **models.go**: Added `RoleDefinition`, `RoleBinding`, `ProjectMembership`
  structs. Added scope constants (`RoleScopeSystem`, `RoleScopeProject`),
  system role names (super-admin, hub-member, hub-viewer), project role names
  (owner, admin, member), agent role definition names (none, readonly, baseline,
  full), and principal type constants (user, agent).
- **store.go**: Added `RoleStore` sub-interface with 12 methods covering CRUD
  for role definitions, role bindings, project membership queries, and an
  `IsProjectMember` convenience method. Composed into the `Store` interface.

### 3. Store Adapter (`pkg/store/entadapter/role_store.go`)

- `RoleStore` struct embedding `*ent.Client`.
- Conversion functions: `entRoleDefinitionToStore`, `entRoleBindingToStore`.
- All 12 `RoleStore` interface methods implemented using Ent queries.
- `GetProjectMembership` resolves the highest-privilege role via a `roleRank`
  helper (owner > admin > member).
- `IsProjectMember` uses an efficient `Count` query.
- Integrated into `CompositeStore` via embedding in `composite.go`.

### 4. System Role Seeding (`pkg/hub/seed.go`)

- `seedRoleDefinitions()`: Seeds 10 system role definitions covering hub-level
  roles (super-admin, hub-member, hub-viewer), project-scoped roles (owner,
  admin, member), and agent role definitions (none, readonly, baseline, full).
- Permission sets computed from the canonical permission registry using helper
  functions: `allPermissionIDs`, `permissionIDsByActions`,
  `projectScopedPermissionIDs`, `projectPermissionIDsExcluding`,
  `projectMemberPermissionIDs`, `agentRolePermissionIDs`.
- Idempotent: uses lookup-before-create pattern, skips `ErrAlreadyExists`.
- Called from `seedIfNeeded` in `server.go`.

### 5. Role Binding Backfill (`pkg/hub/seed.go`)

- `BackfillRoleBindings()`: Creates role bindings from two legacy sources:
  - **User.Role**: Maps admin → super-admin + hub-member, user → hub-member.
  - **Group memberships**: Maps group members to project-scoped role bindings
    (owner/admin → project-owner, member → project-member) for groups that
    have an associated project.
- Idempotent: skips existing bindings via `ErrAlreadyExists`.
- Called from `seedIfNeeded` in `server.go`.

### 6. Dual-Read Authorization (`pkg/hub/authz.go`)

- Replaced `isProjectOwnerOrAdmin` with a dual-read implementation: first
  queries role bindings via `store.GetProjectMembership`, falls back to
  `legacyIsProjectOwnerOrAdmin` (preserved original logic).
- Added `getEffectivePermissions()`: resolves all permissions from a user's
  role bindings, filters by scope, and deduplicates.
- Legacy path preserved as `legacyIsProjectOwnerOrAdmin` for backward
  compatibility during the migration period.

### 7. Transactional Project Membership (`pkg/hub/handlers_projects_core.go`)

- Added `createProjectOwnerRoleBinding()`: creates a project-owner role binding
  for the creating user immediately after project creation.
- Called in `createProject` after `s.store.CreateProject`, before group/policy
  creation, using `project.CreatedBy` as the principal ID.

### 8. GroupMembership Separation (`pkg/hub/handlers_groups.go`)

- Added documentation comment clarifying that `GroupMembership.Role` is now
  governance-only (controls group-level privileges), with project authorization
  handled exclusively through role bindings.

### 9. Identity Documentation (`pkg/hub/identity.go`)

- Added Phase 1E/1F documentation comment to `IsUnscopedLocalPlatformAdmin`
  explaining the future migration path to role-binding-based admin checks.

### 10. Comprehensive Tests (`pkg/hub/role_binding_test.go`)

- 24 test cases covering:
  - Role definition seeding verification
  - CRUD operations for definitions and bindings
  - Project membership by ID and group membership without role binding
  - Backfill from User.Role and group memberships
  - Backfill idempotency
  - User.Role compatibility
  - Admin bypass regression
  - `getEffectivePermissions` with system and project scopes
  - GroupMembership governance separation
- Fixed `mockAuthzStore` in `sse_authz_test.go` to implement new `RoleStore`
  methods, preventing nil pointer panics.

## Verification

- `go generate ./pkg/ent/...` — clean
- `go test ./pkg/hub -run "TestRoleBinding|TestRoleDefinition|TestProjectMembership|TestBackfill|TestUserRole|TestAdminBypass|TestGetEffective|TestGroupMembership"` — all 24 tests pass
- `make ci` (fmt-check, lint, compat-literals, check-authz-guards, test-fast, build) — passes
- Pre-existing flaky test failures in TestSkillsResolve, TestMultipartPublish,
  TestFSList, TestWorkspace* are unrelated to these changes

## Design Decisions

1. **Dual-read pattern**: Role bindings are checked first; legacy group-based
   check is the fallback. This allows gradual migration without breaking
   existing access.
2. **Optional role_definition_id**: Made nillable to support future direct
   permission bindings without requiring a role definition.
3. **roleRank helper**: When a user has multiple project-scoped role bindings,
   the highest-privilege role wins (owner > admin > member).
4. **Idempotent seeding**: Both role definition seeding and backfill use
   lookup-before-create with `ErrAlreadyExists` handling, making them safe
   to re-run.
5. **No PolicyBoundary**: As specified in the brief, PolicyBoundary
   implementation is deferred to a later phase.

## Commits

- `72837f2` feat: add RoleDefinition and RoleBinding schemas, models, and store adapters
- `f003370` feat: seed system role definitions and backfill role bindings
- `3bf128a` feat: update authorization to use role bindings with legacy fallback
- `c676e39` feat: add transactional role binding on project creation
- `d71bd90` test: add comprehensive tests for role bindings and fix mock store
