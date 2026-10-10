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
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Hub-level grants through groups are capped for test identities ---------

// tiGroupWithSystemRole creates an explicit group, optionally owned by
// ownerID, and binds the named system role to it.
func tiGroupWithSystemRole(t *testing.T, s store.Store, name, roleName, ownerID string) *store.Group {
	t.Helper()
	ctx := context.Background()
	g := &store.Group{ID: tid(name), Name: name, Slug: name, GroupType: store.GroupTypeExplicit,
		Created: time.Now(), Updated: time.Now(), CreatedBy: ownerID, OwnerID: ownerID}
	require.NoError(t, s.CreateGroup(ctx, g))
	if roleName != "" {
		tiBindSystemRoleToGroup(t, s, g.ID, roleName)
	}
	return g
}

func tiBindSystemRoleToGroup(t *testing.T, s store.Store, groupID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	createdBy := "test"
	if roleName == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy // the store's rule for super-admin bindings
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: groupID, ScopeType: store.RoleScopeSystem, CreatedBy: createdBy})
	require.NoError(t, err)
}

// tiCustomSystemRole creates a system-scoped custom role with perms and
// returns its name. Super-admin is direct-user-only, so a group carries
// hub-level authority through hub-admin or a custom system role.
func tiCustomSystemRole(t *testing.T, s store.Store, name string, perms ...string) string {
	t.Helper()
	_, err := s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		ID: generateID(), Name: name, Description: "test identity clamp test role",
		ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	return name
}

func tiAddToGroup(t *testing.T, s store.Store, groupID, userID, role string) {
	t.Helper()
	require.NoError(t, s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: role, AddedAt: time.Now()}))
}

func tiHubDecision(srv *Server, user UserIdentity, permission string) Decision {
	return srv.authzService.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     ActionRead,
		Permission: permission,
	})
}

// A test identity in a group bound to super-admin, in a group that gains
// hub-admin after the identity joined, and in a group it owns that is then
// bound to super-admin, stays at member: every inherited system grant
// except hub-member is dropped where authorization reads bindings. A human
// member in the same groups keeps the inherited grants.
func TestTestIdentity_GroupGrantsClampedToMember(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-clamp-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member"})
	fid := fx.Identity.ID
	fixtureIdent := NewAuthenticatedUser(fid, fx.Identity.Email, "", store.UserRoleMember, string(ClientTypeAPI))

	human := &store.User{ID: tid("ti-clamp-human"), Email: "ti-clamp-human@test.com", DisplayName: "h", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	ensureHubMembership(ctx, s, human.ID)
	humanIdent := NewAuthenticatedUser(human.ID, human.Email, "", store.UserRoleMember, string(ClientTypeAPI))

	// (a) A group already bound to a broad custom system role; the fixture
	// is added after issuance.
	adminRole := tiCustomSystemRole(t, s, "ti-clamp-admin-role", "hub.config.read", "hub.health.read",
		"role_binding.create", permissionTestIdentityIssue)
	superGroup := tiGroupWithSystemRole(t, s, "ti-clamp-super", adminRole, DevUserID)
	tiAddToGroup(t, s, superGroup.ID, fid, store.GroupMemberRoleMember)
	tiAddToGroup(t, s, superGroup.ID, human.ID, store.GroupMemberRoleMember)

	// The fixture's own token: 403 on admin routes, through the live
	// binding resolution of every request.
	for _, path := range []string{"/api/v1/admin/server-config", "/api/v1/admin/health/summary"} {
		rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
	}
	adminRD, err := s.GetRoleDefinitionByName(ctx, adminRole, store.RoleScopeSystem)
	require.NoError(t, err)
	rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodPost, "/api/v1/admin/role-bindings", map[string]string{
		"roleDefinitionId": adminRD.ID, "principalType": "user", "principalId": human.ID, "scopeType": "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "fixture granting a system role: %s", rec.Body.String())
	rec = tiPost(t, srv, fx.AccessToken, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "inherited issuer grant is clamped: %s", rec.Body.String())

	// The inherited path through Decide and CanDelegate.
	grant := GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: adminRD.ID, RolePermissions: adminRD.Permissions, ScopeType: store.RoleScopeSystem}
	assert.False(t, IsUnscopedLocalPlatformAdmin(fixtureIdent))
	assert.False(t, tiHubDecision(srv, fixtureIdent, "hub.config.read").Allowed, "Decide: fixture inherits nothing through the group")
	assert.False(t, srv.authzService.CanDelegate(ctx, fixtureIdent, grant).Allowed,
		"CanDelegate: fixture cannot delegate an inherited system role")

	// Negative control: the human member keeps every inherited grant.
	assert.True(t, tiHubDecision(srv, humanIdent, "hub.config.read").Allowed, "Decide: human inherits the group's role")
	assert.True(t, srv.authzService.CanDelegate(ctx, humanIdent, grant).Allowed, "CanDelegate: human holds the inherited role")
	humanRow, err := s.GetUser(ctx, human.ID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, srv, humanRow, http.MethodGet, "/api/v1/admin/server-config", nil).Code)

	// (b) A group that gains hub-admin after the fixture joined it.
	later := tiGroupWithSystemRole(t, s, "ti-clamp-later", "", DevUserID)
	tiAddToGroup(t, s, later.ID, fid, store.GroupMemberRoleMember)
	other := &store.User{ID: tid("ti-clamp-human2"), Email: "ti-clamp-human2@test.com", DisplayName: "h2", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, other))
	tiAddToGroup(t, s, later.ID, other.ID, store.GroupMemberRoleMember)
	tiBindSystemRoleToGroup(t, s, later.ID, store.SystemRoleHubAdmin)
	assert.False(t, srv.authzService.IsHubAdmin(ctx, fid))
	assert.True(t, srv.authzService.IsHubAdmin(ctx, other.ID), "control: a human in the same group becomes hub-admin")

	// (c) A group the fixture owns, later bound to super-admin.
	owned := tiGroupWithSystemRole(t, s, "ti-clamp-owned", "", fid)
	tiAddToGroup(t, s, owned.ID, fid, store.GroupMemberRoleOwner)
	tiBindSystemRoleToGroup(t, s, owned.ID, store.SystemRoleHubAdmin)
	tiBindSystemRoleToGroup(t, s, owned.ID, adminRole)
	assert.False(t, srv.authzService.IsHubAdmin(ctx, fid))
	assert.False(t, tiHubDecision(srv, fixtureIdent, "hub.config.read").Allowed)

	// The member grants stay: the fixture still creates a project.
	projectID := tiCreateProject(t, srv, fx.AccessToken, "ti-clamp-project")
	assert.NotEmpty(t, projectID)
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)
}

// Message authorization honours the cap: a test identity in a group with a
// system-scoped agent.message grant cannot message an agent in a project it has no access to; a human
// in the same group can.
func TestTestIdentity_MessageAuthzHonoursCap(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-msg-issuer")
	fx := tiIssue(t, srv, issuerTok, nil)
	group := tiGroupWithSystemRole(t, s, "ti-msg-super", tiCustomSystemRole(t, s, "ti-msg-role", "agent.message"), DevUserID)
	tiAddToGroup(t, s, group.ID, fx.Identity.ID, store.GroupMemberRoleMember)
	human := &store.User{ID: tid("ti-msg-human"), Email: "ti-msg-human@test.com", DisplayName: "h", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	ensureHubMembership(ctx, s, human.ID)
	tiAddToGroup(t, s, group.ID, human.ID, store.GroupMemberRoleMember)

	project := &store.Project{ID: tid("ti-msg-project"), Name: "ti-msg-project", Slug: "ti-msg-project", OwnerID: DevUserID, CreatedBy: DevUserID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: tid("ti-msg-agent"), Slug: "ti-msg-agent", Name: "ti msg agent", ProjectID: project.ID,
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject, StateVersion: 1, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, agent))

	fixtureIdent := NewAuthenticatedUser(fx.Identity.ID, fx.Identity.Email, "", store.UserRoleMember, string(ClientTypeAPI))
	allowed, reason, _ := srv.authorizeAgentMessage(ctx, fixtureIdent, agent, false)
	assert.False(t, allowed, "fixture in a super-admin group: %s", reason)

	humanIdent := NewAuthenticatedUser(human.ID, human.Email, "", store.UserRoleMember, string(ClientTypeAPI))
	allowed, reason, _ = srv.authorizeAgentMessage(ctx, humanIdent, agent, false)
	assert.True(t, allowed, "control: human in the same group: %s", reason)
}

// unwrapTestFixtureClamp returns the store under the authorization
// service's test-identity grant clamp.
func unwrapTestFixtureClamp(s store.Store) store.Store {
	if c, ok := s.(*testFixtureGrantClamp); ok {
		return c.Store
	}
	return s
}

// The authorization service always reads bindings through the clamp.
func TestTestIdentity_AuthzStoreIsClamped(t *testing.T) {
	srv, _ := testServer(t)
	_, ok := srv.authzService.store.(*testFixtureGrantClamp)
	assert.True(t, ok)
	_, ok = srv.authzFor(&testFixtureGrantClamp{}).store.(*testFixtureGrantClamp)
	assert.True(t, ok, "a transaction-bound authorization service is clamped too")
	assert.Nil(t, NewAuthzService(nil, nil).store)
}

// kindErrStore fails every user lookup with a store error.
type kindErrStore struct{ store.Store }

func (kindErrStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, errors.New("store unavailable")
}

// If the kind lookup errors, the clamp fails closed: the binding read
// errors, so authorization denies instead of treating the user as human.
func TestTestIdentity_ClampFailsClosedOnKindLookupError(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	group := tiGroupWithSystemRole(t, s, "ti-failclosed-super", store.SystemRoleHubAdmin, DevUserID)
	human := &store.User{ID: tid("ti-failclosed-human"), Email: "ti-failclosed@test.com", DisplayName: "h", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	tiAddToGroup(t, s, group.ID, human.ID, store.GroupMemberRoleMember)

	authz := NewAuthzService(kindErrStore{Store: s}, srv.authzService.logger)
	groups, err := s.GetEffectiveGroups(ctx, human.ID)
	require.NoError(t, err)
	principals := []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: human.ID}}
	for _, g := range groups {
		principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: g})
	}
	_, err = authz.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	require.Error(t, err, "an unreadable kind must not resolve to an ordinary user")
	assert.False(t, authz.IsHubAdmin(ctx, human.ID), "fails closed")

	_, err = lookupUserIsTestFixture(ctx, kindErrStore{Store: s}, human.ID)
	assert.Error(t, err)
	isFx, err := lookupUserIsTestFixture(ctx, s, generateID())
	require.NoError(t, err)
	assert.False(t, isFx, "a missing user has no row and no grants")

	// The same lookup through the working store: not a fixture, grants kept.
	assert.True(t, srv.authzService.IsHubAdmin(ctx, human.ID))
}

// countingKindStore counts user and role-definition lookups.
type countingKindStore struct {
	store.Store
	mu        sync.Mutex
	getUser   int
	roleByIDs int
	failRoles bool
}

func (c *countingKindStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.mu.Lock()
	c.getUser++
	c.mu.Unlock()
	return c.Store.GetUser(ctx, id)
}

func (c *countingKindStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	c.mu.Lock()
	c.roleByIDs++
	fail := c.failRoles
	c.mu.Unlock()
	if fail {
		return nil, errors.New("store unavailable")
	}
	return c.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

// R2-2: the clamp's caches are shared with transaction-bound
// authorization services; kinds and role classes are read once; a failed
// role lookup drops the test identity's bindings and is retried, not
// cached.
func TestTestIdentity_ClampCachesShared(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	group := tiGroupWithSystemRole(t, s, "ti-cache-hubadmin", store.SystemRoleHubAdmin, DevUserID)
	human := &store.User{ID: tid("ti-cache-human"), Email: "ti-cache@test.com", DisplayName: "h", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	tiAddToGroup(t, s, group.ID, human.ID, store.GroupMemberRoleMember)
	fixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	tiAddToGroup(t, s, group.ID, fixture.ID, store.GroupMemberRoleMember)

	counting := &countingKindStore{Store: s}
	main := NewAuthzService(counting, srv.authzService.logger)
	require.True(t, main.IsHubAdmin(ctx, human.ID))
	require.Equal(t, 1, counting.getUser, "a privileged binding loads the kind once")
	roleReads := counting.roleByIDs

	// A transaction-bound service (as Server.authzFor builds) shares the cache.
	tx := NewAuthzService(counting, srv.authzService.logger)
	shareTestFixtureClampCache(tx.store, main.store)
	require.True(t, tx.IsHubAdmin(ctx, human.ID))
	assert.Equal(t, 1, counting.getUser, "the shared cache avoids a second kind read")
	assert.Equal(t, roleReads, counting.roleByIDs, "role classes are cached and shared too")
	srvTx := srv.authzFor(&countingKindStore{Store: s})
	assert.Same(t, srv.authzService.store.(*testFixtureGrantClamp).cache, srvTx.store.(*testFixtureGrantClamp).cache)

	// A failed role lookup drops the test identity's bindings and is
	// retried later, not cached.
	failing := &countingKindStore{Store: s, failRoles: true}
	fx := NewAuthzService(failing, srv.authzService.logger)
	assert.False(t, fx.IsHubAdmin(ctx, fixture.ID))
	assert.True(t, fx.IsHubAdmin(ctx, human.ID), "control: a failed role lookup leaves an ordinary user's grants unclamped")
	c := fx.store.(*testFixtureGrantClamp)
	assert.Empty(t, c.cache.roleAllowed, "a failed lookup is not cached")
	failing.mu.Lock()
	failing.failRoles = false
	failing.mu.Unlock()
	assert.False(t, fx.IsHubAdmin(ctx, fixture.ID), "hub-admin is still dropped once classified")
	assert.NotEmpty(t, c.cache.roleAllowed)
	before := failing.roleByIDs
	assert.False(t, fx.IsHubAdmin(ctx, fixture.ID))
	assert.Equal(t, before, failing.roleByIDs, "classified roles are cached")
}

// R2-6: the admin effective-access view shows a test identity only the
// system grants authorization honours.
func TestTestIdentity_EffectiveAccessViewClamped(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-ea-issuer")
	fx := tiIssue(t, srv, issuerTok, nil)
	human := &store.User{ID: tid("ti-ea-human"), Email: "ti-ea@test.com", DisplayName: "h", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	ensureHubMembership(ctx, s, human.ID)
	group := tiGroupWithSystemRole(t, s, "ti-ea-hubadmin", store.SystemRoleHubAdmin, DevUserID)
	tiAddToGroup(t, s, group.ID, fx.Identity.ID, store.GroupMemberRoleMember)
	tiAddToGroup(t, s, group.ID, human.ID, store.GroupMemberRoleMember)

	count := func(userID string) int {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/effective-access?principalType=user&principalId="+userID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp adminEffectiveAccessResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.ActiveBindingCount
	}
	fixtureCount, humanCount := count(fx.Identity.ID), count(human.ID)
	assert.Equal(t, humanCount-1, fixtureCount, "the fixture's inherited hub-admin binding is not shown")
}
