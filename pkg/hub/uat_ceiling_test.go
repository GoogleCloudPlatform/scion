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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyScopedIdentity builds a *ScopedUserIdentity the way an unbackfilled
// row is normalized: NewScopedUserIdentity derives the ceiling from scopes
// via the frozen legacy snapshot, exactly what
// store.UserAccessToken.NormalizedCeiling does for such a row. Tests that
// need the production load path instead (ValidateToken against a real
// stored row) use seedLegacyTokenInStore.
func legacyScopedIdentity(base UserIdentity, projectID string, scopes []string) *ScopedUserIdentity {
	return NewScopedUserIdentity(base, projectID, scopes)
}

// seedLegacyTokenInStore inserts a user_access_tokens row directly through
// the real store, the way a row exists before its ceiling has ever been
// computed: CeilingVersion/CeilingPermissionIDs are left at their zero
// values. It returns the plaintext key (for ValidateToken) and the row ID.
func seedLegacyTokenInStore(t *testing.T, s store.Store, userID, projectID string, scopes []string) (plaintext, tokenID string) {
	t.Helper()
	randomBytes := make([]byte, UATRandomBytes)
	_, err := rand.Read(randomBytes)
	require.NoError(t, err)
	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.UATPrefix + keyBody
	prefix := store.UATPrefix + keyBody[:UATPrefixLength]
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	future := time.Now().Add(90 * 24 * time.Hour)
	token := &store.UserAccessToken{
		ID: uuid.New().String(), UserID: userID, Name: "legacy", Prefix: prefix, KeyHash: hashStr,
		ProjectID: projectID, Scopes: scopes, ExpiresAt: &future, Created: time.Now(),
	}
	require.NoError(t, s.CreateUserAccessToken(context.Background(), token))
	return fullKey, token.ID
}

// TestUATCeiling_Decide_LegacyAttachOnlyDeniesLifecycle characterizes the
// Decide path for an unversioned attach-only UAT: it is denied a lifecycle
// action on its own project's agent. No scope implies another.
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

	// OwnerID must be set: agent.attach is not part of the project-owner
	// role's flat permissions (miller79/scion#88) — the owner reaches their
	// OWN agent's attach only through the resource-owner relationship
	// grant, which is what the "control" assertion below exercises.
	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.False(t, decision.Allowed, "attach-only UAT must not gain lifecycle on the Decide path")

	// Control: the same token IS allowed the action it actually holds.
	attachDecision := authz.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.True(t, attachDecision.Allowed, "attach-only UAT must still be allowed agent.attach")
}

// TestUATCeiling_Decide_LegacyManageAliasAllowsLifecycle characterizes
// legacy manage-alias expansion separately: an unversioned token minted with
// agent:manage stores the mint-time expanded concrete scopes (including
// agent:lifecycle explicitly), so — unlike attach-only — it is allowed
// lifecycle, without any implication expansion being involved.
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
	// This is what expandScopes("agent:manage") persists at mint time — the
	// stored Scopes column never holds the raw alias.
	storedScopes := permissions.UATManageScopesFor(permissions.ResourceAgent)
	scoped := legacyScopedIdentity(base, project.ID, storedScopes)

	// OwnerID is set so the attach denial below comes from the token's
	// ceiling, not from the absence of the resource-owner relationship grant
	// (see the comment at the attach-only test above).
	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.True(t, decision.Allowed, "an agent:manage token must keep its explicitly-expanded lifecycle scope")

	// The manage alias still deliberately excludes attach/port_access.
	attachDecision := authz.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.False(t, attachDecision.Allowed, "an agent:manage token must not gain attach (excluded from the alias)")
}

// TestUATCeiling_CanDelegate_LegacyAttachOnlyDeniesLifecycle: CanDelegate
// applies the same ceiling as Decide. A project owner who holds
// agent.lifecycle through their real project-owner role, but whose
// credential ceiling carries only agent.attach, must not be able to
// delegate agent.lifecycle.
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
	assert.False(t, decision.Allowed, "an attach-only credential must not be able to delegate agent.lifecycle")
}

// TestUATCeiling_CanDelegate_LegacyManageAliasAllowsLifecycle is the
// companion positive case: an agent:manage token's persisted concrete
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
	assert.True(t, decision.Allowed, "a credential holding agent.lifecycle explicitly must be able to delegate it")
}

// TestUATCeiling_CanDelegate_EmptyCeilingDeniesEverything: a ScopedUserIdentity
// whose ceiling has no permission IDs must not fall through to "unrestricted"
// in intersectCredentialCaveats.
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

// TestUATCeiling_Decide_NewlyIssuedAttachOnlyDeniesLifecycle covers the
// "newly issued" half of the rule at CeilingVersionV1: a V1 ceiling
// containing only agent.attach denies agent.lifecycle and allows
// agent.attach, through the same ceilingRestriction path a legacy ceiling
// goes through.
//
// Built via NewScopedUserIdentityWithCeiling rather than minted through
// CreateTokenWithParams: agent.attach is granted only through the
// resource-owner/ancestor relationship, never through a flat role
// permission (miller79/scion#88), and mint validation only checks flat role
// permissions today — no role can mint a bare agent:attach token.
func TestUATCeiling_Decide_NewlyIssuedAttachOnlyDeniesLifecycle(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("v1-attach-owner")
	project := &store.Project{ID: tid("v1-attach-project"), Name: "p", Slug: "v1-attach-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "v1a@example.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{ID: tid("v1-attach-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "v1a@example.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithCeiling(base, project.ID, []string{"agent:attach"}, "", permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: []string{"agent.attach"},
	})

	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.False(t, decision.Allowed, "a V1 attach-only token must not gain lifecycle")
	attachDecision := authz.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.True(t, attachDecision.Allowed, "a V1 attach-only token must still be allowed attach")

	cdDecision := authz.CanDelegate(ctx, scoped, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.lifecycle"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project.ID,
	})
	assert.False(t, cdDecision.Allowed, "a V1 attach-only token must not be able to delegate lifecycle")
}

// TestUATCeiling_LegacyRowThroughValidateToken exercises the production
// load path — a row inserted through the store, validated by
// UserAccessTokenService.ValidateToken — rather than a test-constructed
// identity, both before and after the ceiling backfill runs against that
// row. The authorization outcome must be identical at both stages.
func TestUATCeiling_LegacyRowThroughValidateToken(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("legacy-load-owner")
	project := &store.Project{ID: tid("legacy-load-project"), Name: "p", Slug: "legacy-load-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "ll@example.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{ID: tid("legacy-load-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	plaintext, tokenID := seedLegacyTokenInStore(t, s, ownerID, project.ID, []string{"agent:attach"})
	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}

	checkDeniesLifecycleAllowsAttach := func(t *testing.T) {
		t.Helper()
		identity, err := srv.uatService.ValidateToken(ctx, plaintext)
		require.NoError(t, err)

		decision := srv.authzService.CheckAccess(ctx, identity, resource, ActionLifecycle)
		assert.False(t, decision.Allowed, "a legacy attach-only row must not gain lifecycle through the production load path")
		attachDecision := srv.authzService.CheckAccess(ctx, identity, resource, ActionAttach)
		assert.True(t, attachDecision.Allowed, "a legacy attach-only row must still be allowed attach through the production load path")

		cdDecision := srv.authzService.CanDelegate(ctx, identity, GrantDescriptor{
			Type:            GrantTypeRoleBinding,
			RolePermissions: []string{"agent.lifecycle"},
			ScopeType:       store.RoleScopeProject,
			ScopeID:         project.ID,
		})
		assert.False(t, cdDecision.Allowed, "a legacy attach-only row must not be able to delegate lifecycle through the production load path")
	}

	t.Run("before backfill", checkDeniesLifecycleAllowsAttach)

	stored, err := s.GetUserAccessToken(ctx, tokenID)
	require.NoError(t, err)
	require.Nil(t, stored.CeilingPermissionIDs, "precondition: row must not be backfilled yet")

	// Force the ceiling backfill to reprocess: testServer's own setup
	// already ran Migrate once, before this row existed, and recorded
	// completion.
	require.NoError(t, s.DeleteHubSetting(ctx, "migration_uat_ceiling_backfill_v1"))
	require.NoError(t, s.Migrate(ctx))

	stored, err = s.GetUserAccessToken(ctx, tokenID)
	require.NoError(t, err)
	require.NotNil(t, stored.CeilingPermissionIDs, "row must be backfilled by the forced re-migrate")

	t.Run("after backfill", checkDeniesLifecycleAllowsAttach)
}

// TestUATCeiling_LegacyRow_AliasChangeCannotWidenBetweenValidations pins the
// sharpest form of "a Registry or alias change never widens an existing
// token": an unbackfilled legacy row recomputes its ceiling on every
// ValidateToken call, so an alias change happening between two requests for
// the SAME token must not change what it is authorized for. The mutation's
// effectiveness is confirmed via ResolveSelector before the negative result
// is trusted.
func TestUATCeiling_LegacyRow_AliasChangeCannotWidenBetweenValidations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("nowiden-alias-owner")
	project := &store.Project{ID: tid("nowiden-alias-project"), Name: "p", Slug: "nowiden-alias-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "nwa@example.com", project.ID, store.ProjectRoleOwner)

	plaintext, _ := seedLegacyTokenInStore(t, s, ownerID, project.ID, []string{"agent:attach"})

	before, err := srv.uatService.ValidateToken(ctx, plaintext)
	require.NoError(t, err)
	beforeDecision := srv.authzService.CanDelegate(ctx, before, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.lifecycle"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project.ID,
	})
	require.False(t, beforeDecision.Allowed)

	// Retarget agent:attach as a manage alias for "agent": buildSelectorRegistry
	// processes aliases after plain UATScope entries, so this alias
	// candidate wins the same map key and "agent:attach" would resolve to
	// the full agent:manage expansion — including agent.lifecycle — if
	// anything re-consulted ResolveSelector live.
	mutatedAliases := make(map[string]string, len(permissions.UATManageAliases)+1)
	for k, v := range permissions.UATManageAliases {
		mutatedAliases[k] = v
	}
	mutatedAliases["agent:attach"] = permissions.ResourceAgent
	t.Cleanup(permissions.OverrideSelectorInputsForTest(permissions.Registry, mutatedAliases))

	live, ok := permissions.ResolveSelector("agent:attach")
	require.True(t, ok)
	require.Contains(t, live.PermissionIDs, "agent.lifecycle", "test setup: expected the alias mutation to be effective")

	after, err := srv.uatService.ValidateToken(ctx, plaintext)
	require.NoError(t, err)
	afterDecision := srv.authzService.CanDelegate(ctx, after, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.lifecycle"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project.ID,
	})
	assert.False(t, afterDecision.Allowed, "an alias mutation must not let re-validating the same never-backfilled legacy token gain lifecycle")
}

// TestUATCeiling_V1Token_RegistryChangeCannotWidenBetweenValidations is the
// CeilingVersionV1 counterpart: a minted token's persisted
// CeilingPermissionIDs must survive a later Registry mutation unchanged
// across repeated ValidateToken calls.
func TestUATCeiling_V1Token_RegistryChangeCannotWidenBetweenValidations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("nowiden-v1-owner")
	project := &store.Project{ID: tid("nowiden-v1-project"), Name: "p", Slug: "nowiden-v1-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "nwv1@example.com", project.ID, store.ProjectRoleOwner)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "v1", ProjectID: project.ID, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	before, err := srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	require.True(t, before.Ceiling().Allows("agent.read"))
	require.False(t, before.Ceiling().Allows("agent.list"))

	// Retarget "agent:read" to resolve to agent.list instead, by appending a
	// later Registry entry with the same UATScope: buildSelectorRegistry's
	// last-write-wins map assignment means the appended entry decides what
	// ResolveSelector("agent:read") returns from here on. agent.list already
	// has a reviewed boundary entry, so the selector still resolves.
	mutated := append([]permissions.Permission(nil), permissions.Registry...)
	mutated = append(mutated, permissions.Permission{
		ID: "agent.list", Resource: permissions.ResourceAgent, Action: permissions.ActionRead, UATScope: "agent:read",
	})
	t.Cleanup(permissions.OverrideSelectorInputsForTest(mutated, permissions.UATManageAliases))

	live, ok := permissions.ResolveSelector("agent:read")
	require.True(t, ok)
	require.Equal(t, []string{"agent.list"}, live.PermissionIDs, "test setup: expected the Registry mutation to be effective")

	after, err := srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	assert.True(t, after.Ceiling().Allows("agent.read"), "a V1 token's persisted ceiling must survive a later Registry mutation unchanged")
	assert.False(t, after.Ceiling().Allows("agent.list"), "a Registry mutation must not let re-validating an existing V1 token gain a new permission")

	// Behavior-level confirmation, not just the ceiling's own bookkeeping:
	// the owner role holds agent.list, so CanDelegate would allow delegating
	// it if the mutation had actually widened this token's ceiling.
	cdDecision := srv.authzService.CanDelegate(ctx, after, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.list"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         project.ID,
	})
	assert.False(t, cdDecision.Allowed, "a Registry mutation must not let re-validating an existing V1 token delegate a new permission")
}

// TestCreateTokenWithParams_PersistsCeiling_ExplicitScope pins that mint
// validation and the persisted ceiling agree for an ordinary,
// explicitly-selected scope. Uses agent:read/agent:list — flat permissions
// a project-owner role actually holds; agent.attach is deliberately NOT
// part of that flat grant (miller79/scion#88), so it belongs in the
// mint-denial characterization, not here.
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
// ValidateToken builds a ScopedUserIdentity whose Ceiling() is the row's
// PERSISTED ceiling, not one re-derived from Scopes. The row is seeded
// directly (as TestBackfillUATCeilings_LeavesExistingV1RowsUntouched does),
// with Scopes normalizing to a DIFFERENT permission than the persisted V1
// ceiling, so the assertions cannot pass by the two coincidentally agreeing.
func TestValidateToken_UsesPersistedCeiling(t *testing.T) {
	srv, s := testServer(t)
	ownerID := tid("validate-ceiling-owner")
	project := &store.Project{ID: tid("validate-ceiling-project"), Name: "p", Slug: "validate-ceiling-project"}
	require.NoError(t, s.CreateProject(context.Background(), project))
	createTestUserWithProjectRole(t, s, ownerID, "vc@example.com", project.ID, store.ProjectRoleOwner)

	randomBytes := make([]byte, UATRandomBytes)
	_, err := rand.Read(randomBytes)
	require.NoError(t, err)
	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.UATPrefix + keyBody
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	token := &store.UserAccessToken{
		ID: uuid.New().String(), UserID: ownerID, Name: "validate", Prefix: store.UATPrefix + keyBody[:UATPrefixLength],
		KeyHash: hashStr, ProjectID: project.ID, Scopes: []string{"agent:attach"},
		CeilingVersion: permissions.CeilingVersionV1, CeilingPermissionIDs: []string{"agent.read"},
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUserAccessToken(context.Background(), token))

	identity, err := srv.uatService.ValidateToken(context.Background(), fullKey)
	require.NoError(t, err)
	ceiling := identity.Ceiling()
	assert.Equal(t, permissions.CeilingVersionV1, ceiling.Version)
	assert.True(t, ceiling.Allows("agent.read"), "ceiling must come from the persisted CeilingPermissionIDs")
	assert.False(t, ceiling.Allows("agent.attach"), "ceiling must NOT be re-derived from Scopes, which normalize to a different permission")
}

// TestCredentialContextForIdentity_TypedNilScopedUserIdentityFailsClosed pins
// a guard against a Go interface footgun: a typed-nil *ScopedUserIdentity
// satisfies identity.(*ScopedUserIdentity) with ok == true and scoped == nil,
// even though the identity interface value itself is not nil. Calling this
// function must not panic dereferencing the nil scoped pointer, and must not
// silently drop CredentialKindUAT down to the zero Kind — the zero Kind
// reads downstream as "no credential restriction," which would authorize a
// request exactly as if it carried no UAT credential at all (see the
// Decide-level test below, which pins that distinction through the kernel).
func TestCredentialContextForIdentity_TypedNilScopedUserIdentityFailsClosed(t *testing.T) {
	var nilScoped *ScopedUserIdentity
	var cc CredentialContext
	require.NotPanics(t, func() {
		cc = credentialContextForIdentity(nilScoped)
	})
	assert.Equal(t, CredentialKindUAT, cc.Kind)
	assert.False(t, cc.Ceiling.Allows("agent.read"), "a typed-nil scoped identity must deny every permission, never carry an unrestricted ceiling")
	assert.False(t, cc.Ceiling.Allows(""), "an empty permission ID must also deny")
}

// TestUATCeiling_Decide_TypedNilScopedIdentityCredentialDeniesRatherThanLiftingRestriction
// proves the fail-closed property through the real kernel: a Credential
// built from a broken (typed-nil) scoped identity must not authorize a
// request that an ordinary, unrestricted principal would be allowed. The
// positive control shows the same principal IS allowed without this
// credential attached; reverting credentialContextForIdentity's guard to the
// zero CredentialContext (Kind == "") would make credential.Kind ==
// CredentialKindUAT false, skip Decide step 7a's ceiling restriction
// entirely, and allow this request — the fail-open regression this test
// pins against.
func TestUATCeiling_Decide_TypedNilScopedIdentityCredentialDeniesRatherThanLiftingRestriction(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("typed-nil-cred-owner")
	project := &store.Project{ID: tid("typed-nil-cred-project"), Name: "p", Slug: "typed-nil-cred-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "tnc@example.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{ID: tid("typed-nil-cred-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := NewAuthenticatedUser(ownerID, "tnc@example.com", "Owner", "member", "api")
	principal := principalContextForIdentity(identity)
	resource := Resource{Type: "agent", ID: agent.ID, ParentType: "project", ParentID: project.ID}

	control := authz.Decide(ctx, AuthzRequest{
		Principal: principal, Credential: credentialContextForIdentity(identity), Resource: resource, Action: ActionRead,
	})
	require.True(t, control.Allowed, "positive control: the owner must be allowed without a UAT credential")

	var nilScoped *ScopedUserIdentity
	decision := authz.Decide(ctx, AuthzRequest{
		Principal: principal, Credential: credentialContextForIdentity(nilScoped), Resource: resource, Action: ActionRead,
	})
	assert.False(t, decision.Allowed, "a credential built from a broken (typed-nil) scoped identity must deny, not fall back to an unrestricted request")
}
