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
	"net/http"
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
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: groupID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
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

	// (a) A group already bound to super-admin; the fixture is added after
	// issuance.
	superGroup := tiGroupWithSystemRole(t, s, "ti-clamp-super", store.SystemRoleSuperAdmin, DevUserID)
	tiAddToGroup(t, s, superGroup.ID, fid, store.GroupMemberRoleMember)
	tiAddToGroup(t, s, superGroup.ID, human.ID, store.GroupMemberRoleMember)

	// The fixture's own token: 403 on admin routes, through the live
	// binding resolution of every request.
	for _, path := range []string{"/api/v1/admin/server-config", "/api/v1/admin/health/summary"} {
		rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
	}
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodPost, "/api/v1/admin/role-bindings", map[string]string{
		"roleDefinitionId": superRD.ID, "principalType": "user", "principalId": human.ID, "scopeType": "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "fixture granting super-admin: %s", rec.Body.String())

	// The inherited path through Decide, IsSystemAdmin and CanDelegate.
	assert.False(t, IsUnscopedLocalPlatformAdmin(fixtureIdent))
	assert.False(t, tiHubDecision(srv, fixtureIdent, "hub.config.read").Allowed, "Decide: fixture inherits no super-admin")
	assert.False(t, srv.authzService.IsSystemAdmin(ctx, fid))
	assert.False(t, srv.authzService.CanDelegate(ctx, fixtureIdent, GrantDescriptor{
		Type: GrantTypeRoleBinding, RoleDefinitionID: superRD.ID, ScopeType: store.RoleScopeSystem}).Allowed,
		"CanDelegate: fixture cannot delegate inherited super-admin")

	// Negative control: the human member keeps every inherited grant.
	assert.True(t, tiHubDecision(srv, humanIdent, "hub.config.read").Allowed, "Decide: human inherits super-admin")
	assert.True(t, srv.authzService.IsSystemAdmin(ctx, human.ID))
	assert.True(t, srv.authzService.CanDelegate(ctx, humanIdent, GrantDescriptor{
		Type: GrantTypeRoleBinding, RoleDefinitionID: superRD.ID, ScopeType: store.RoleScopeSystem}).Allowed)
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
	tiBindSystemRoleToGroup(t, s, owned.ID, store.SystemRoleSuperAdmin)
	assert.False(t, srv.authzService.IsSystemAdmin(ctx, fid))
	assert.False(t, tiHubDecision(srv, fixtureIdent, "hub.config.read").Allowed)

	// The member grants stay: the fixture still creates a project.
	projectID := tiCreateProject(t, srv, fx.AccessToken, "ti-clamp-project")
	assert.NotEmpty(t, projectID)
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)
}

// Message authorization honours the cap: a test identity in a super-admin
// group cannot message an agent in a project it has no access to; a human
// in the same group can.
func TestTestIdentity_MessageAuthzHonoursCap(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-msg-issuer")
	fx := tiIssue(t, srv, issuerTok, nil)
	group := tiGroupWithSystemRole(t, s, "ti-msg-super", store.SystemRoleSuperAdmin, DevUserID)
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
