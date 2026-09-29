// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// A.2 characterization and acceptance tests (ptone/scion#2118).
//
// Characterization (before-change pins, still true after the change — Decide
// is not widened):
//   - TestUATCeiling_Decide_LegacyAttachOnlyDeniesLifecycle
//   - TestUATCeiling_Decide_LegacyManageAliasAllowsLifecycle
//
// Ruling alignment (CanDelegate narrowed to match Decide's actual behavior —
// a2-legacy-attach-ruling.md):
//   - TestUATCeiling_CanDelegate_LegacyAttachOnlyDeniesLifecycle
//   - TestUATCeiling_CanDelegate_LegacyManageAliasAllowsLifecycle
//
// Acceptance criteria:
//   - Registry/alias changes never widen an existing token:
//     TestUATCeiling_RegistryAdditionNeverWidensExistingToken
//   - Unknown/malformed/empty ceilings deny:
//     TestUATCeiling_Decide_UnknownVersionDenies,
//     TestUATCeiling_Decide_EmptyCeilingDenies
//   - Mint validation and the persisted ceiling agree, and manage-alias
//     expansion is persisted while attach/port_access require explicit
//     selection: TestCreateTokenWithParams_PersistsCeiling*
// ---------------------------------------------------------------------------

// legacyScopedIdentity builds a *ScopedUserIdentity the way a
// CeilingVersionUnspecified row is normalized: NewScopedUserIdentity derives
// the ceiling from raw scopes via the frozen legacy snapshot, exactly what
// store.UserAccessToken.NormalizedCeiling does for an unbackfilled row.
func legacyScopedIdentity(base UserIdentity, projectID string, scopes []string) *ScopedUserIdentity {
	return NewScopedUserIdentity(base, projectID, scopes)
}

// TestUATCeiling_Decide_LegacyAttachOnlyDeniesLifecycle characterizes the
// Decide path for a legacy (unversioned) attach-only UAT: it must be denied
// a lifecycle action on its own project's agent, both before and after A.2 —
// Decide's exact-scope behavior is preserved, never widened.
func TestUATCeiling_Decide_LegacyAttachOnlyDeniesLifecycle(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("legacy-attach-owner")
	project := &store.Project{ID: tid("legacy-attach-project"), Name: "p", Slug: "legacy-attach-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "owner@example.com", project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{ID: tid("legacy-attach-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "owner@example.com", "Owner", "member", "api")
	scoped := legacyScopedIdentity(base, project.ID, []string{"agent:attach"})

	// OwnerID must be set: agent.attach is deliberately excluded from the
	// project-owner role's flat permissions (miller79/scion#88) — the owner
	// reaches their OWN agent's attach only through the resource-owner
	// relationship grant, which is what the "control" assertion below
	// exercises.
	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.False(t, decision.Allowed, "legacy attach-only UAT must not gain lifecycle on the Decide path")

	// Control: the same token IS allowed the action it actually holds.
	attachDecision := authz.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.True(t, attachDecision.Allowed, "legacy attach-only UAT must still be allowed agent.attach")
}

// TestUATCeiling_Decide_LegacyManageAliasAllowsLifecycle characterizes
// legacy manage-alias expansion separately, as required by the A.2 brief: a
// legacy token minted with agent:manage stores the mint-time expanded
// concrete scopes (including agent:lifecycle explicitly), so — unlike
// attach-only — it IS allowed lifecycle, without any implication expansion
// being involved.
func TestUATCeiling_Decide_LegacyManageAliasAllowsLifecycle(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("legacy-manage-owner")
	project := &store.Project{ID: tid("legacy-manage-project"), Name: "p", Slug: "legacy-manage-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "owner2@example.com", project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{ID: tid("legacy-manage-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "owner2@example.com", "Owner", "member", "api")
	// This is what expandScopes("agent:manage") persisted at mint time —
	// a legacy row's Scopes column never holds the raw alias.
	storedScopes := permissions.UATManageScopesFor(permissions.ResourceAgent)
	scoped := legacyScopedIdentity(base, project.ID, storedScopes)

	resource := Resource{Type: "agent", ID: agent.ID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.True(t, decision.Allowed, "legacy agent:manage token must keep its explicitly-expanded lifecycle scope")

	// The manage alias still deliberately excludes attach/port_access.
	attachDecision := authz.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.False(t, attachDecision.Allowed, "legacy agent:manage token must not gain attach (excluded from the alias)")
}

// TestUATCeiling_CanDelegate_LegacyAttachOnlyDeniesLifecycle is the ruling
// test: CanDelegate is narrowed to match Decide's exact-scope behavior. A
// project owner who minted a legacy attach-only UAT holds agent.lifecycle
// through their real project-owner role, but the credential ceiling must
// still prevent that UAT from delegating it.
func TestUATCeiling_CanDelegate_LegacyAttachOnlyDeniesLifecycle(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	ownerID := tid("cd-legacy-attach-owner")
	project := tid("cd-legacy-attach-project")
	createDelegateTestProject(t, s, project, "cd-legacy-attach-project", "test")
	createTestUserWithProjectRole(t, s, ownerID, "cd-owner@example.com", project, store.ProjectRoleOwner)

	base := NewAuthenticatedUser(ownerID, "cd-owner@example.com", "Owner", "member", "api")
	scoped := legacyScopedIdentity(base, project, []string{"agent:attach"})

	decision := authz.CanDelegate(ctx, scoped, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.lifecycle"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project,
	})
	assert.False(t, decision.Allowed, "legacy attach-only UAT must not be able to delegate agent.lifecycle")
}

// TestUATCeiling_CanDelegate_LegacyManageAliasAllowsLifecycle is the
// companion positive case: a legacy manage-alias token's persisted concrete
// scopes already include agent.lifecycle, so CanDelegate allows it — this is
// not an implication, it was always explicitly in the ceiling.
func TestUATCeiling_CanDelegate_LegacyManageAliasAllowsLifecycle(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	ownerID := tid("cd-legacy-manage-owner")
	project := tid("cd-legacy-manage-project")
	createDelegateTestProject(t, s, project, "cd-legacy-manage-project", "test")
	createTestUserWithProjectRole(t, s, ownerID, "cd-owner2@example.com", project, store.ProjectRoleOwner)

	base := NewAuthenticatedUser(ownerID, "cd-owner2@example.com", "Owner", "member", "api")
	scoped := legacyScopedIdentity(base, project, permissions.UATManageScopesFor(permissions.ResourceAgent))

	decision := authz.CanDelegate(ctx, scoped, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.lifecycle"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project,
	})
	assert.True(t, decision.Allowed, "legacy agent:manage token must be able to delegate its explicitly-held agent.lifecycle")
}

// TestUATCeiling_CanDelegate_EmptyCeilingDeniesEverything pins the F-8
// over-permissive-path fix directly: a ScopedUserIdentity whose ceiling has
// no permission IDs must not fall through to "unrestricted" in
// intersectCredentialCaveats.
func TestUATCeiling_CanDelegate_EmptyCeilingDeniesEverything(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	ownerID := tid("cd-empty-ceiling-owner")
	project := tid("cd-empty-ceiling-project")
	createDelegateTestProject(t, s, project, "cd-empty-ceiling-project", "test")
	createTestUserWithProjectRole(t, s, ownerID, "cd-owner3@example.com", project, store.ProjectRoleOwner)

	base := NewAuthenticatedUser(ownerID, "cd-owner3@example.com", "Owner", "member", "api")
	// No scopes at all — an empty ceiling, not "unrestricted".
	scoped := NewScopedUserIdentityWithCeiling(base, project, nil, "", permissions.FrozenPermissionCeiling{
		Version: permissions.CeilingVersionV1,
	})

	decision := authz.CanDelegate(ctx, scoped, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.read"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project,
	})
	assert.False(t, decision.Allowed, "an empty ceiling must deny delegation of every permission")
}

// TestUATCeiling_Decide_UnknownVersionDenies and
// TestUATCeiling_Decide_EmptyCeilingDenies pin the AC directly at the Decide
// integration level: an unknown ceiling version, or an explicit empty
// permission list, denies rather than becoming unrestricted.
func TestUATCeiling_Decide_UnknownVersionDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("unknown-version-owner")
	project := &store.Project{ID: tid("unknown-version-project"), Name: "p", Slug: "unknown-version-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "uv@example.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{ID: tid("unknown-version-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "uv@example.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithCeiling(base, project.ID, []string{"agent:read"}, "", permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersion(77),
		PermissionIDs: []string{"agent.read"},
	})

	resource := Resource{Type: "agent", ID: agent.ID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionRead)
	assert.False(t, decision.Allowed, "an unknown ceiling version must deny even a permission present in PermissionIDs")
}

func TestUATCeiling_Decide_EmptyCeilingDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("empty-ceiling-owner")
	project := &store.Project{ID: tid("empty-ceiling-project"), Name: "p", Slug: "empty-ceiling-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "ec@example.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{ID: tid("empty-ceiling-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "ec@example.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithCeiling(base, project.ID, []string{"agent:read"}, "", permissions.FrozenPermissionCeiling{
		Version: permissions.CeilingVersionV1, // explicit empty PermissionIDs
	})

	resource := Resource{Type: "agent", ID: agent.ID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionRead)
	assert.False(t, decision.Allowed, "an explicit empty V1 ceiling must deny, not become unrestricted")
}

// TestUATCeiling_RegistryAdditionNeverWidensExistingToken is the direct AC
// test: mint a real token, capture its persisted ceiling, mutate the live
// Registry to add a new permission that (if live-resolved) could plausibly
// enlarge what the token's stored scope means, and confirm the previously
// minted token's authorization outcome is unchanged.
func TestUATCeiling_RegistryAdditionNeverWidensExistingToken(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("no-widen-owner")
	project := &store.Project{ID: tid("no-widen-project"), Name: "p", Slug: "no-widen-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "nw@example.com", project.ID, store.ProjectRoleOwner)

	_, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "no-widen", ProjectID: project.ID, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	require.Equal(t, permissions.CeilingVersionV1, token.CeilingVersion)
	require.Equal(t, []string{"agent.read"}, token.CeilingPermissionIDs)

	originalRegistry := permissions.Registry
	t.Cleanup(func() { permissions.Registry = originalRegistry })
	mutated := append([]permissions.Permission(nil), originalRegistry...)
	mutated = append(mutated, permissions.Permission{
		ID: "agent.read.v2", Resource: permissions.ResourceAgent, Action: permissions.ActionRead, UATScope: "agent:read",
	})
	permissions.Registry = mutated

	ceiling := token.NormalizedCeiling()
	assert.True(t, ceiling.Allows("agent.read"))
	assert.False(t, ceiling.Allows("agent.read.v2"), "a Registry addition after mint must not widen an already-minted token")
}

// TestCreateTokenWithParams_PersistsCeiling_ExplicitScope pins that mint
// validation (resolveScopePermissionIDs, A.1's SelectorRegistry) and the
// persisted ceiling agree for an ordinary, explicitly-selected scope. Uses
// agent:read/agent:list — flat permissions a project-owner role actually
// holds; agent.attach is deliberately NOT part of that flat grant
// (miller79/scion#88), so it belongs in the mint-denial characterization,
// not here.
func TestCreateTokenWithParams_PersistsCeiling_ExplicitScope(t *testing.T) {
	srv, s := testServer(t)
	ownerID := tid("mint-explicit-owner")
	project := &store.Project{ID: tid("mint-explicit-project"), Name: "p", Slug: "mint-explicit-project"}
	require.NoError(t, s.CreateProject(context.Background(), project))
	createTestUserWithProjectRole(t, s, ownerID, "me@example.com", project.ID, store.ProjectRoleOwner)

	_, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "explicit", ProjectID: project.ID, Scopes: []string{"agent:read", "agent:list"},
	})
	require.NoError(t, err)
	assert.Equal(t, permissions.CeilingVersionV1, token.CeilingVersion)
	assert.ElementsMatch(t, []string{"agent.read", "agent.list"}, token.CeilingPermissionIDs)
}

// TestCreateTokenWithParams_PersistsCeiling_ManageAlias pins mint-time
// manage-alias expansion is persisted in the ceiling, while attach/
// port_access — excluded from the alias — are absent unless explicitly
// selected too.
func TestCreateTokenWithParams_PersistsCeiling_ManageAlias(t *testing.T) {
	srv, s := testServer(t)
	ownerID := tid("mint-manage-owner")
	project := &store.Project{ID: tid("mint-manage-project"), Name: "p", Slug: "mint-manage-project"}
	require.NoError(t, s.CreateProject(context.Background(), project))
	createTestUserWithProjectRole(t, s, ownerID, "mm@example.com", project.ID, store.ProjectRoleOwner)

	_, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "manage", ProjectID: project.ID, Scopes: []string{"agent:manage"},
	})
	require.NoError(t, err)
	assert.Equal(t, permissions.CeilingVersionV1, token.CeilingVersion)
	assert.Contains(t, token.CeilingPermissionIDs, "agent.lifecycle")
	assert.Contains(t, token.CeilingPermissionIDs, "agent.read")
	assert.NotContains(t, token.CeilingPermissionIDs, "agent.attach")
	assert.NotContains(t, token.CeilingPermissionIDs, "agent.port_access")

	ceiling := token.NormalizedCeiling()
	assert.True(t, ceiling.Allows("agent.lifecycle"))
	assert.False(t, ceiling.Allows("agent.attach"))
}

// TestValidateToken_UsesPersistedCeiling confirms the full round trip:
// ValidateToken builds a ScopedUserIdentity whose Ceiling() matches what was
// persisted at mint time, not a re-derivation from Scopes.
func TestValidateToken_UsesPersistedCeiling(t *testing.T) {
	srv, s := testServer(t)
	ownerID := tid("validate-ceiling-owner")
	project := &store.Project{ID: tid("validate-ceiling-project"), Name: "p", Slug: "validate-ceiling-project"}
	require.NoError(t, s.CreateProject(context.Background(), project))
	createTestUserWithProjectRole(t, s, ownerID, "vc@example.com", project.ID, store.ProjectRoleOwner)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "validate", ProjectID: project.ID, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	identity, err := srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err)
	ceiling := identity.Ceiling()
	assert.Equal(t, permissions.CeilingVersionV1, ceiling.Version)
	assert.True(t, ceiling.Allows("agent.read"))
	assert.False(t, ceiling.Allows("agent.lifecycle"))
}
