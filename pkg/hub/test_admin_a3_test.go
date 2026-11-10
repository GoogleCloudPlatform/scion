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
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin tier A3 (ptone/scion#4240): grant shaping, issuance stamps, the
// purge of admin kinds, the residue report and the agent scope clamp.
// Every identity, user, group and project here is synthetic, in an
// in-memory store.

// a3Kinds issues one identity of each admin kind, each by its own issuer.
func a3Kinds(t *testing.T, srv *Server, s store.Store, prefix string) (map[string]TestAdminIdentityTokenResponse, map[string]*store.User) {
	t.Helper()
	issuers := map[string]*store.User{
		store.UserKindTestSuperAdmin: taUser(t, s, prefix+"-iss-super", store.SystemRoleSuperAdmin),
		store.UserKindTestHubAdmin:   taUser(t, s, prefix+"-iss-hub", store.SystemRoleSuperAdmin),
	}
	return map[string]TestAdminIdentityTokenResponse{
		store.UserKindTestSuperAdmin: tsaIssue(t, srv, issuers[store.UserKindTestSuperAdmin], testAdminRoleSuperAdmin),
		store.UserKindTestHubAdmin:   tsaIssue(t, srv, issuers[store.UserKindTestHubAdmin], testAdminRoleHubAdmin),
	}, issuers
}

func a3Row(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, u.ExpiresAt)
	require.NotNil(t, u.IssuanceID)
	return u
}

func a3ProjectRoleID(t *testing.T, s store.Store, name string) string {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), name, store.RoleScopeProject)
	require.NoError(t, err)
	return rd.ID
}

// a3ProjectBinding returns userID's binding of roleName on projectID.
func a3ProjectBinding(t *testing.T, s store.Store, userID, projectID, roleName string) *store.RoleBinding {
	t.Helper()
	rdID := a3ProjectRoleID(t, s, roleName)
	bs, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bs {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == rdID {
			return b
		}
	}
	t.Fatalf("no %s binding for %s on %s", roleName, userID, projectID)
	return nil
}

// a3RawDB is the test store's database.
func a3RawDB(t *testing.T, s store.Store) (*sql.DB, string) {
	t.Helper()
	raw, ok := s.(interface {
		DB() *sql.DB
		Dialect() string
	})
	require.True(t, ok, "the test store exposes its database")
	return raw.DB(), raw.Dialect()
}

// AC A3 / carry item 1: a project-owner or project-admin membership issued
// by a test hub-admin or a test super-admin carries the identity's expiry
// and its issuance stamp; a requested later expiry is clamped. Both kinds,
// both roles, the members POST and PUT paths; the human control gets
// neither.
func TestTestAdminA3_ProjectRoleGrantsForcedExpiry(t *testing.T) {
	srv, s, _ := tsaServer(t)
	kinds, issuers := a3Kinds(t, srv, s, "a3-pr")
	for kind, k := range kinds {
		row := a3Row(t, s, k.Identity.ID)
		// The identity's own project: both kinds own it, so both may
		// appoint owners and admins.
		projectID := tiCreateProject(t, srv, k.AccessToken, "a3-pr-"+kind)
		for _, roleName := range []string{store.ProjectRoleOwner, store.ProjectRoleAdmin} {
			post := taUser(t, s, "a3-pr-post-"+roleName+"-"+kind, store.SystemRoleHubMember)
			rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
				addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, roleName), PrincipalType: "user", PrincipalID: post.ID})
			require.Less(t, rec.Code, 300, "%s members POST %s: %s", kind, roleName, rec.Body.String())
			put := taUser(t, s, "a3-pr-put-"+roleName+"-"+kind, store.SystemRoleHubMember)
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/projects/"+projectID+"/members/principals/user/"+put.ID,
				setMemberRolesRequestBody{RoleDefinitionIDs: []string{a3ProjectRoleID(t, s, roleName)}})
			require.Less(t, rec.Code, 300, "%s members PUT %s: %s", kind, roleName, rec.Body.String())
			for path, u := range map[string]*store.User{"POST": post, "PUT": put} {
				b := a3ProjectBinding(t, s, u.ID, projectID, roleName)
				require.NotNil(t, b.ExpiresAt, "%s %s %s: forced expiry", kind, path, roleName)
				assert.True(t, b.ExpiresAt.Equal(*row.ExpiresAt), "%s %s %s: expiry %v == identity %v", kind, path, roleName, b.ExpiresAt, row.ExpiresAt)
				require.NotNil(t, b.IssuanceID, "%s %s %s: stamped", kind, path, roleName)
				assert.Equal(t, *row.IssuanceID, *b.IssuanceID)
				assert.Equal(t, row.ID, b.CreatedBy)
			}
		}
		// A requested expiry later than the identity's is clamped; an
		// earlier one is kept (the role binding route takes an expiry).
		later := row.ExpiresAt.Add(24 * time.Hour)
		lu := taUser(t, s, "a3-pr-later-"+kind, store.SystemRoleHubMember)
		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: lu.ID,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, ExpiresAt: &later,
		})
		require.Less(t, rec.Code, 300, "%s role binding with a later expiry: %s", kind, rec.Body.String())
		b := a3ProjectBinding(t, s, lu.ID, projectID, store.ProjectRoleAdmin)
		require.NotNil(t, b.ExpiresAt)
		assert.True(t, b.ExpiresAt.Equal(*row.ExpiresAt), "%s: clamped to the identity's expiry", kind)

		// Human control: the issuer appoints an owner on its own project.
		hp := tid("a3-pr-human-" + kind)
		taProject(t, s, hp, issuers[kind].ID)
		hu := taUser(t, s, "a3-pr-hctl-"+kind, store.SystemRoleHubMember)
		rec = doRequestAsUser(t, srv, issuers[kind], http.MethodPost, "/api/v1/projects/"+hp+"/members",
			addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleOwner), PrincipalType: "user", PrincipalID: hu.ID})
		require.Less(t, rec.Code, 300, rec.Body.String())
		hb := a3ProjectBinding(t, s, hu.ID, hp, store.ProjectRoleOwner)
		assert.Nil(t, hb.ExpiresAt, "human control: no forced expiry")
		assert.Nil(t, hb.IssuanceID, "human control: no stamp")
	}
}

// AC A3 / carry items 1 and 6: grants made by a test admin lapse at its
// expiry. A project-admin membership and a group membership (the group
// holds project-member on another project) both confer nothing once the
// identity's expiry passes; a human's membership granted by a human stays.
func TestTestAdminA3_GrantsLapseAtIdentityExpiry(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-lapse-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	// Shorten the identity's lifetime to a few seconds (its row is the
	// source of the shaping on every request).
	db, dialect := a3RawDB(t, s)
	exp := time.Now().Add(10 * time.Second).UTC()
	_, err := db.ExecContext(ctx, taRebind(dialect, "UPDATE users SET expires_at = ? WHERE id = ?"), exp, k.Identity.ID)
	require.NoError(t, err)
	// Its issuance binding never outlives it (the A1 clamp drops one that
	// does): shorten it the same way.
	_, err = db.ExecContext(ctx, taRebind(dialect, "UPDATE role_bindings SET expires_at = ? WHERE principal_id = ? AND created_by = ?"), exp, k.Identity.ID, store.TestIdentityIssuanceCreatedBy)
	require.NoError(t, err)

	owner := taUser(t, s, "a3-lapse-owner", store.SystemRoleHubMember)
	p1, p2 := tid("a3-lapse-p1"), tid("a3-lapse-p2")
	taProject(t, s, p1, owner.ID)
	taProject(t, s, p2, owner.ID)
	g := &store.Group{ID: generateID(), Name: "a3 lapse", Slug: "a3-lapse-" + generateID()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, g))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleMember), PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: g.ID, ScopeType: store.RoleScopeProject, ScopeID: p2, CreatedBy: "test"})
	require.NoError(t, err)

	grantee := taUser(t, s, "a3-lapse-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+p1+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
		AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	// Control: the owner (a human) adds another human the same way.
	control := taUser(t, s, "a3-lapse-control", store.SystemRoleHubMember)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: control.ID, Role: "member", AddedBy: owner.ID}))

	roles, err := activeDirectProjectRoles(ctx, s, nil, false, grantee.ID, p1)
	require.NoError(t, err)
	require.True(t, roles[store.ProjectRoleAdmin], "live before expiry")
	groups, err := s.GetEffectiveGroups(ctx, grantee.ID)
	require.NoError(t, err)
	require.Contains(t, groups, g.ID, "live before expiry")
	d := srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(tsaIdentityFromRow(t, s, grantee.ID)),
		Credential: credentialContextForIdentity(tsaIdentityFromRow(t, s, grantee.ID)), Resource: Resource{Type: "project", ID: p2},
		Action: Action("read"), Permission: "project.read"})
	require.True(t, d.Allowed, "group grant live before expiry: %s", d.Reason)

	time.Sleep(time.Until(exp) + 500*time.Millisecond)

	roles, err = activeDirectProjectRoles(ctx, s, nil, false, grantee.ID, p1)
	require.NoError(t, err)
	assert.False(t, roles[store.ProjectRoleAdmin], "the project-admin grant lapsed at the identity's expiry")
	groups, err = s.GetEffectiveGroups(ctx, grantee.ID)
	require.NoError(t, err)
	assert.NotContains(t, groups, g.ID, "the group membership lapsed at the identity's expiry")
	d = srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(tsaIdentityFromRow(t, s, grantee.ID)),
		Credential: credentialContextForIdentity(tsaIdentityFromRow(t, s, grantee.ID)), Resource: Resource{Type: "project", ID: p2},
		Action: Action("read"), Permission: "project.read"})
	assert.False(t, d.Allowed, "no project access through the lapsed group membership")
	groups, err = s.GetEffectiveGroups(ctx, control.ID)
	require.NoError(t, err)
	assert.Contains(t, groups, g.ID, "control: a membership a human added stays")
}

// tsaIdentityFromRow is the authenticated identity of any user row.
func tsaIdentityFromRow(t *testing.T, s store.Store, id string) UserIdentity {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeAPI))
}

// AC A3: the system super-admin binding stays refused (A2's cage, not
// duplicated: the refusal names the test-admin policy), and so does a
// group membership that would confer super-admin. Carry item 5 (decided:
// keep): the built-in hub-admin role stays ungrantable by a test admin,
// with forced expiry or without, because expiry bounds the credential, not
// the durable effects of writes the grantee makes with it. A human
// super-admin is the control for each.
func TestTestAdminA3_SuperAdminAndHubAdminGrantsRefused(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-sa-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	sysRole := func(name string) string {
		rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeSystem)
		require.NoError(t, err)
		return rd.ID
	}
	human := taUser(t, s, "a3-sa-human", store.SystemRoleHubMember)
	exp := time.Now().Add(10 * time.Minute)
	for _, roleName := range []string{store.SystemRoleSuperAdmin, store.SystemRoleHubAdmin} {
		body := createRoleBindingRequest{RoleDefinitionID: sysRole(roleName), PrincipalType: "user", PrincipalID: human.ID, ScopeType: store.RoleScopeSystem, ExpiresAt: &exp}
		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/role-bindings", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s binding by the test super-admin: %s", roleName, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "test-admin policy", "%s: refused by the test-admin policy", roleName)
	}
	// Control: a human super-admin binds hub-admin (a super-admin binding is
	// reconciler-only for everyone, D10).
	rec := doRequestAsUser(t, srv, issuer, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{RoleDefinitionID: sysRole(store.SystemRoleHubAdmin), PrincipalType: "user", PrincipalID: human.ID, ScopeType: store.RoleScopeSystem})
	assert.Less(t, rec.Code, 300, "human control: hub-admin binding: %s", rec.Body.String())

	// A group bound to super-admin: adding a human is refused for the
	// test admin (the group closure holds deny-classified permissions).
	g := &store.Group{ID: generateID(), Name: "a3 super", Slug: "a3-super-" + generateID()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, g))
	taRawGroupBinding(t, s, g.ID, store.SystemRoleSuperAdmin)
	other := taUser(t, s, "a3-sa-other", store.SystemRoleHubMember)
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
		AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: other.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "test-admin policy")
	assert.False(t, srv.authzService.IsSystemAdmin(ctx, other.ID), "the human gained no super-admin")
	rec = doRequestAsUser(t, srv, issuer, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
		AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: other.ID})
	assert.Less(t, rec.Code, 300, "human control: group add: %s", rec.Body.String())
}

// AC A3: custom roles, invite codes and broker join tokens a test admin
// creates carry its issuance; invites and join tokens expire no later than
// it. Human controls carry neither.
func TestTestAdminA3_CustomRolesInvitesJoinTokens(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-cr-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)

	name := "a3-cr-" + generateID()[:8]
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
		map[string]any{"name": name, "scopeType": "project", "permissions": []string{"project.read"}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	require.NoError(t, err)
	require.NotNil(t, rd.IssuanceID, "custom role stamped")
	assert.Equal(t, *row.IssuanceID, *rd.IssuanceID)
	hname := "a3-cr-h-" + generateID()[:8]
	rec = doRequestAsUser(t, srv, issuer, http.MethodPost, "/api/v1/admin/roles",
		map[string]any{"name": hname, "scopeType": "project", "permissions": []string{"project.read"}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	hrd, err := s.GetRoleDefinitionByName(ctx, hname, store.RoleScopeProject)
	require.NoError(t, err)
	assert.Nil(t, hrd.IssuanceID, "human control")
	// Changing a role it did not define cannot be bounded: refused.
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/admin/roles/"+hrd.ID,
		map[string]any{"name": hname, "permissions": []string{"project.read", "project.update"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a test admin changing a human's role: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), store.ErrTestAdminGrantRefused.Error(), "the refusal is grant shaping's")
	rec = doRequestAsUser(t, srv, issuer, http.MethodPut, "/api/v1/admin/roles/"+hrd.ID,
		map[string]any{"name": hname, "permissions": []string{"project.read", "project.update"}})
	assert.Less(t, rec.Code, 300, "human control: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/invites", InviteCreateRequest{ExpiresIn: "96h", MaxUses: 1})
	require.Less(t, rec.Code, 300, rec.Body.String())
	invs, err := s.ListInviteCodes(ctx, store.ListOptions{})
	require.NoError(t, err)
	var found bool
	for _, inv := range invs.Items {
		if inv.CreatedBy != row.ID {
			continue
		}
		found = true
		got, err := s.GetInviteCode(ctx, inv.ID)
		require.NoError(t, err)
		assert.False(t, got.ExpiresAt.After(*row.ExpiresAt), "invite expiry %v clamped to the identity's %v", got.ExpiresAt, row.ExpiresAt)
		require.NotNil(t, got.IssuanceID)
		assert.Equal(t, *row.IssuanceID, *got.IssuanceID)
	}
	assert.True(t, found, "the test admin's invite is stored")

	// Broker join tokens: while the test admin tier is on, only the
	// co-located broker registers (A0/A2), so a test admin mints none
	// through the API; the store shapes any join token write anyway
	// (TestGrantShaping_InvitesAndJoinTokens).
	rec = doRequestWithTokenLoopback(t, srv, k.AccessToken, http.MethodPost, "/api/v1/brokers", map[string]any{"name": "a3-broker-" + generateID()[:8]})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	grants, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	kinds := map[store.IssuanceGrantKind]int{}
	for _, gr := range grants {
		kinds[gr.Kind]++
	}
	assert.Equal(t, 1, kinds[store.IssuanceGrantRoleDefinition])
	assert.Equal(t, 1, kinds[store.IssuanceGrantInviteCode])
	assert.Zero(t, kinds[store.IssuanceGrantBrokerJoinToken])
}

// Fail-closed second layer: CanDelegate refuses a grant by an admin-kind
// identity whose context carries no grant shaping for it, with the
// test-admin policy cause; with the request shaping it is admitted, and a
// human is unaffected.
func TestTestAdminA3_CanDelegateRequiresShaping(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "a3-cd-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	grant := GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleMember),
		ScopeType: store.RoleScopeProject, ScopeID: tid("a3-cd-p")}

	admin := tsaIdentity(t, s, row.ID)
	unshaped := contextWithIdentityAndUser(admin)
	d := srv.authzService.CanDelegate(unshaped, admin, grant)
	assert.False(t, d.Allowed)
	assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy)
	assert.Equal(t, testAdminDelegationReasonUnshaped, d.Reason)

	shaped := store.ContextWithGrantShaping(unshaped, store.GrantShaping{IdentityID: row.ID, IssuanceID: *row.IssuanceID, ExpiresAt: *row.ExpiresAt})
	d = srv.authzService.CanDelegate(shaped, admin, grant)
	assert.True(t, d.Allowed, d.Reason)
	other := store.ContextWithGrantShaping(unshaped, store.GrantShaping{IdentityID: issuer.ID, IssuanceID: "x", ExpiresAt: *row.ExpiresAt})
	d = srv.authzService.CanDelegate(other, admin, grant)
	assert.False(t, d.Allowed, "a shaping for another identity does not count")

	human := tsaIdentityFromRow(t, s, issuer.ID)
	d = srv.authzService.CanDelegate(contextWithIdentityAndUser(human), human, grant)
	assert.True(t, d.Allowed, "human control: %s", d.Reason)

	// The middleware puts the shaping on every request of the identity.
	ctx := contextWithTestAdminActor(contextWithIdentityAndUser(admin), row, testAdminCredentialBearer)
	ctx = contextWithGrantShaping(ctx)
	g, ok := store.GrantShapingFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, store.GrantShaping{IdentityID: row.ID, IssuanceID: *row.IssuanceID, ExpiresAt: *row.ExpiresAt}, g)
	assert.Equal(t, row.ID, store.GrantActorFromContext(ctx))
	hctx := contextWithGrantShaping(contextWithIdentityAndUser(human))
	_, ok = store.GrantShapingFromContext(hctx)
	assert.False(t, ok, "no shaping for a human")
	assert.Equal(t, issuer.ID, store.GrantActorFromContext(hctx), "the human is the grant actor")
}

// Requirement (b): the system paths that write grants are unaffected:
// issuance (the issuance binding), reconcile (a live test super-admin is
// left alone), and the member tier's issuance grants.
func TestTestAdminA3_SystemGrantPathsUnaffected(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-sys-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	bs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, row.ID)
	require.NoError(t, err)
	require.Len(t, bs, 1, "the issuance binding")
	assert.Equal(t, store.TestIdentityIssuanceCreatedBy, bs[0].CreatedBy)
	assert.Nil(t, bs[0].IssuanceID, "issuance runs in the issuer's request: no shaping")

	// Reconcile with a live test super-admin leaves its binding alone.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{issuer.Email}, "member")
	require.NoError(t, err)
	bs, err = s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, row.ID)
	require.NoError(t, err)
	assert.Len(t, bs, 1)

	// A member-tier identity issued by the human: its hub grants are
	// written unshaped.
	_, tok := tiIssuer(t, srv, s, "a3-sys-member-iss")
	fx := tiIssue(t, srv, tok, map[string]string{"role": "member", "purpose": "a3 system paths"})
	fbs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fx.Identity.ID)
	require.NoError(t, err)
	for _, b := range fbs {
		assert.Nil(t, b.IssuanceID, "member-tier issuance grant unshaped")
	}
}

// AC A3 purge: the purge of an admin kind removes every artifact stamped
// with its issuance and every resource stamped with it, and nothing else.
// A human it suspended stays suspended and appears in the residue report,
// as does a binding it removed from a human; the human's own grants stay.
// The run is written to the test admin audit sink.
func TestTestAdminA3_PurgeRemovesStampedOnly(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-pg-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)

	// Stamped artifacts.
	projectID := tiCreateProject(t, srv, k.AccessToken, "a3-pg-project")
	gid := fpCreateGroup(t, srv, k.AccessToken, map[string]any{"name": "a3 pg", "slug": "a3-pg-" + generateID()[:8]})
	owner := taUser(t, s, "a3-pg-owner", store.SystemRoleHubMember)
	hp := tid("a3-pg-hp")
	taProject(t, s, hp, owner.ID)
	grantee := taUser(t, s, "a3-pg-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+hp+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/groups/"+gid+"/members",
		AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/invites", InviteCreateRequest{ExpiresIn: "1h", MaxUses: 1})
	require.Less(t, rec.Code, 300, rec.Body.String())
	// Unstamped rows: a human's own project-member grant, and a human the
	// identity suspends.
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleMember), PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: owner.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)
	victim := taUser(t, s, "a3-pg-victim", store.SystemRoleHubMember)
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPatch, "/api/v1/users/"+victim.ID, map[string]any{"status": store.UserStatusSuspended})
	require.Less(t, rec.Code, 300, "suspend: %s", rec.Body.String())

	before, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	require.NotEmpty(t, before)

	// Residue before the purge: the suspension is listed.
	rec = doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/test-identities/admin/"+row.ID+"/residue", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var residue TestAdminResidueReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &residue))
	assert.True(t, a3ResidueHas(residue, victim.ID), "the suspended human is in the residue report: %s", rec.Body.String())
	assert.False(t, a3ResidueHas(residue, projectID), "its own project is stamped, not residue")
	assert.Len(t, residue.StampedGrants, len(before))
	assert.NotEmpty(t, residue.Limit)

	rec = doRequestAsUser(t, srv, issuer, http.MethodDelete, "/api/v1/test-identities/"+row.ID+"?purge=true", nil)
	report := fpPurgeReport(t, rec)
	assert.True(t, report.Complete, rec.Body.String())
	assert.True(t, report.IdentityDeleted)
	assert.Equal(t, len(before), report.Deleted.Grants, "every stamped grant removed")
	assert.Equal(t, 1, report.Deleted.Projects)
	assert.Equal(t, 1, report.Deleted.Groups)
	require.NotNil(t, report.Residue)
	assert.True(t, a3ResidueHas(*report.Residue, victim.ID))

	after, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	assert.Empty(t, after, "nothing stamped with the issuance is left")
	v, err := s.GetUser(ctx, victim.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusSuspended, v.Status, "unstamped changes are reported, never reverted")
	_, err = s.GetProject(ctx, hp)
	assert.NoError(t, err, "the human's project stays")
	roles, err := activeDirectProjectRoles(ctx, s, nil, false, owner.ID, hp)
	require.NoError(t, err)
	assert.True(t, roles[store.ProjectRoleOwner], "the human owner's grant stays")
	roles, err = activeDirectProjectRoles(ctx, s, nil, false, grantee.ID, hp)
	require.NoError(t, err)
	assert.False(t, roles[store.ProjectRoleAdmin], "the stamped project-admin grant is gone")
	for _, a := range fpPurgeAudits(t, s, testAdminGrantPurgeMutation, "") {
		assert.Contains(t, a.AfterSummary, `"issuance_id":"`+*row.IssuanceID+`"`)
	}
	assert.Len(t, fpPurgeAudits(t, s, testAdminGrantPurgeMutation, ""), len(before), "one audit row per removed grant")
	assert.NotEmpty(t, tsaSinkLines(buf, testAdminAuditEventPurge), "purge sink line")
	assert.NotEmpty(t, tsaSinkLines(buf, testAdminAuditEventResidue), "residue WARN sink line")

	// After the purge the residue report is still served (from the issuance
	// record), and still lists the suspension.
	rec = doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/test-identities/admin/"+row.ID+"/residue", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	residue = TestAdminResidueReport{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &residue))
	assert.True(t, a3ResidueHas(residue, victim.ID))
	assert.False(t, a3ResidueHas(residue, projectID), "a purged stamped project is not residue")
	assert.Empty(t, residue.StampedGrants)
}

func a3ResidueHas(r TestAdminResidueReport, targetID string) bool {
	for _, c := range r.UnstampedChanges {
		if c.TargetID == targetID {
			return true
		}
	}
	return false
}

// Carry item 3 (decided: callers stay human; a deliberate deviation from
// admin tier design §4.6): a test identity of any kind is refused as a
// purge caller with the test-identity cause. An admin-kind target is
// purged by its issuer or a human super-admin; another human (a hub-admin)
// gets the empty report and the identity stays.
func TestTestAdminA3_PurgeCallers(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	kinds, issuers := a3Kinds(t, srv, s, "a3-pc")
	target := kinds[store.UserKindTestHubAdmin]
	targetRow := a3Row(t, s, target.Identity.ID)
	// A stamped grant of the target, to show the control removes nothing.
	projectID := tiCreateProject(t, srv, target.AccessToken, "a3-pc-project")
	grantee := taUser(t, s, "a3-pc-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, target.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	stampedBefore, _, err := s.ListIssuanceStampedGrants(ctx, *targetRow.IssuanceID, 0)
	require.NoError(t, err)
	require.NotEmpty(t, stampedBefore)

	// Test identities of both admin kinds are refused as purge callers by
	// the test-admin policy on the route's permission (test_identity.issue,
	// deny for every admin kind), before any purge logic: the decision
	// names the policy and the sink records the refusal for the caller.
	for kind, caller := range kinds {
		policyLines := func() int {
			n := 0
			for _, l := range tsaSinkLines(buf, testAdminAuditEventPolicyDenial) {
				if l["user_id"] == caller.Identity.ID && l["class"] == "permission" && l["subject"] == permissionTestIdentityIssue &&
					l["issuance_id"] == caller.Identity.IssuanceID && l["route_id"] != nil && l["route_id"] != "" {
					n++
				}
			}
			return n
		}
		before := policyLines()
		rec := doRequestWithToken(t, srv, caller.AccessToken, http.MethodDelete, "/api/v1/test-identities/"+target.Identity.ID+"?purge=true", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s as purge caller: %s", kind, rec.Body.String())
		// The route guard refuses it (route permission test_identity.issue)
		// before the handler runs: the answer is the guard's structured
		// 403 for that permission, exactly.
		var body struct {
			Error struct {
				Code    string            `json:"code"`
				Message string            `json:"message"`
				Details map[string]string `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		assert.Equal(t, ErrCodeForbidden, body.Error.Code, kind)
		assert.Equal(t, "Insufficient permissions", body.Error.Message, "%s: the guard's message (it exposes no reason or denied_by)", kind)
		assert.Equal(t, map[string]string{"resource_type": "test_identity", "denied_action": "issue"}, body.Error.Details, "%s: refused on the route's permission", kind)
		// The HTTP request itself wrote the policy refusal: a new sink line
		// for this caller's issuance with the route ID (only a request
		// context carries them), counted before any direct Decide below.
		assert.Equal(t, before+1, policyLines(), "%s: the request's refusal is the test-admin policy's, in the sink", kind)
		d := srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(tsaIdentity(t, s, caller.Identity.ID)),
			Credential: credentialContextForIdentity(tsaIdentity(t, s, caller.Identity.ID)), Resource: Resource{Type: "test_identity"},
			Action: ActionIssue, Permission: permissionTestIdentityIssue, TargetEvidence: hubCollectionEvidence(permissionTestIdentityIssue)})
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, "%s: the decision names the test-admin policy: %s", kind, d.Reason)
	}

	// Control: a human who passes the route's test_identity.issue check
	// (a custom system role holding it) but is neither the issuer nor a
	// super-admin is not a purge caller for an admin-kind target: exactly
	// 200 with the empty report, and nothing is removed.
	issuerRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "a3-pc-issuer-" + generateID()[:8],
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue}})
	require.NoError(t, err)
	human := taUser(t, s, "a3-pc-human", store.SystemRoleHubMember)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: issuerRole.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, human, http.MethodDelete, "/api/v1/test-identities/"+target.Identity.ID+"?purge=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.Equal(t, *emptyTestIdentityPurgeReport(target.Identity.ID), r, "the empty report")
	_, err = s.GetUser(ctx, target.Identity.ID)
	require.NoError(t, err, "the identity stays")
	stampedAfter, _, err := s.ListIssuanceStampedGrants(ctx, *targetRow.IssuanceID, 0)
	require.NoError(t, err)
	assert.Equal(t, len(stampedBefore), len(stampedAfter), "the stamped grants stay")

	// The issuer purges its identity.
	rec = doRequestAsUser(t, srv, issuers[store.UserKindTestHubAdmin], http.MethodDelete, "/api/v1/test-identities/"+target.Identity.ID+"?purge=true", nil)
	assert.True(t, fpPurgeReport(t, rec).IdentityDeleted, rec.Body.String())
	// A human super-admin that is not the issuer purges the other one.
	other := taUser(t, s, "a3-pc-super", store.SystemRoleSuperAdmin)
	rec = doRequestAsUser(t, srv, other, http.MethodDelete, "/api/v1/test-identities/"+kinds[store.UserKindTestSuperAdmin].Identity.ID+"?purge=true", nil)
	assert.True(t, fpPurgeReport(t, rec).IdentityDeleted, rec.Body.String())
}

// AC A3 reaper: an expired admin-kind identity is purged by the reaper,
// with its stamped grants.
func TestTestAdminA3_ReaperPurgesAdminKinds(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-rp-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleHubAdmin)
	row := a3Row(t, s, k.Identity.ID)
	projectID := tiCreateProject(t, srv, k.AccessToken, "a3-rp-project")
	grantee := taUser(t, s, "a3-rp-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleOwner), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())

	res, err := srv.reapTestIdentities(ctx, row.ExpiresAt.Add(testIdentityReaperGrace+time.Minute))
	require.NoError(t, err)
	assert.Contains(t, res.Purged, row.ID, "the reaper purged the expired admin kind: %+v", res)
	_, err = s.GetUser(ctx, row.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	left, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	assert.Empty(t, left)
	_, err = s.GetProject(ctx, projectID)
	assert.Error(t, err, "its project went with it")
	lines := tsaSinkLines(buf, testAdminAuditEventPurge)
	require.NotEmpty(t, lines)
	assert.Equal(t, fixturePurgeExecutorReaper, lines[len(lines)-1]["executor"])
}

// Revoke-all also ends the grants each identity handed out.
func TestTestAdminA3_RevokeAllPurgesGrants(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-ra-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	owner := taUser(t, s, "a3-ra-owner", store.SystemRoleHubMember)
	hp := tid("a3-ra-hp")
	taProject(t, s, hp, owner.ID)
	grantee := taUser(t, s, "a3-ra-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+hp+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	taRevokeAll(t, srv, issuer)
	left, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	assert.Empty(t, left, "revoke-all removed the stamped grants")
	roles, err := activeDirectProjectRoles(ctx, s, nil, false, grantee.ID, hp)
	require.NoError(t, err)
	assert.False(t, roles[store.ProjectRoleAdmin])
}

// The residue route: a human super-admin session only.
func TestTestAdminA3_ResidueRoute(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "a3-rr-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	path := "/api/v1/test-identities/admin/" + k.Identity.ID + "/residue"
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, srv, issuer, http.MethodGet, path, nil).Code)
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "test admin caller: %s", rec.Body.String())
	hubAdmin := taUser(t, s, "a3-rr-hub", store.SystemRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, hubAdmin, http.MethodGet, path, nil).Code, "human hub-admin")
	human := taUser(t, s, "a3-rr-human", store.SystemRoleHubMember)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/test-identities/admin/"+human.ID+"/residue", nil).Code, "not an admin kind")
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/test-identities/admin/"+generateID()+"/residue", nil).Code)
}

// AE-1 (test-ident-lead, 10-14): a test super-admin removes a leftover
// human test-login-style user (synthetic, @scion-test.invalid) through the
// general user routes, one ID at a time. Those routes take no dry-run or
// reason; each request is written to the test admin audit sink with the
// issuance ID. Another user is untouched.
func TestTestAdminA3_AE1LeftoverUserCleanup(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-ae1-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	leftover := &store.User{ID: generateID(), Email: "tl-" + generateID()[:8] + "@scion-test.invalid", DisplayName: "leftover", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, leftover))
	bystander := taUser(t, s, "a3-ae1-bystander", store.SystemRoleHubMember)

	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, "/api/v1/users/"+leftover.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "read before delete: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodDelete, "/api/v1/users/"+leftover.ID, nil)
	require.Less(t, rec.Code, 300, "delete: %s", rec.Body.String())
	_, err := s.GetUser(ctx, leftover.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetUser(ctx, bystander.ID)
	assert.NoError(t, err, "only the named user is removed")
	var deletes int
	for _, l := range tsaSinkLines(buf, testAdminAuditEventRequest) {
		assert.Equal(t, *row.IssuanceID, l["issuance_id"], "every request carries the issuance ID")
		if l["method"] == http.MethodDelete {
			deletes++
		}
	}
	assert.Equal(t, 1, deletes, "one sink line for the one delete")
	// The deletion is in the residue report (an unstamped row).
	res, err := srv.testAdminResidue(ctx, row.ID, *row.IssuanceID)
	require.NoError(t, err)
	assert.True(t, a3ResidueHas(*res, leftover.ID))
}

// Carry items 2 and 7, advisor 7: an agent rooted at an admin-kind identity
// (and its sub-agent: the root is transitive) is never issued a scope that
// covers a deny-classified permission: project:secret:read (secret.use).
// A human-rooted agent in the same project keeps it.
func TestTestAdminA3_AgentScopeClamp(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-ag-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	projectID := tiCreateProject(t, srv, k.AccessToken, "a3-ag-project")
	tiAddProvider(t, s, projectID)
	a := fpCreateAgent(t, srv, k.AccessToken, projectID, "a3-ag-a")
	b := fpCreateSubAgent(t, srv, a, projectID, "a3-ag-b")
	aRow, bRow := fpAgent(t, s, a), fpAgent(t, s, b)
	assert.Equal(t, k.Identity.ID, aRow.Fixture.RootID)
	assert.Equal(t, k.Identity.ID, bRow.Fixture.RootID, "the sub-agent's root is the identity, not its parent")

	// Human-rooted control in the same project: the issuer (a human) is
	// made an owner and creates an agent.
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleOwner), PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: issuer.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)
	hc := fpCreateAgent(t, srv, taTokenFor(t, srv, issuer), projectID, "a3-ag-human")

	for name, id := range map[string]string{"admin-rooted agent": a, "admin-rooted sub-agent": b} {
		grant, err := srv.AuthorizeAgentToken(ctx, fpAgent(t, s, id))
		require.NoError(t, err, name)
		assert.NotContains(t, grant.Scopes, ScopeProjectSecretRead, "%s: project:secret:read clamped", name)
		assert.Contains(t, grant.Scopes, ScopeProjectRead, "%s: allowed scopes stay", name)
	}
	grant, err := srv.AuthorizeAgentToken(ctx, fpAgent(t, s, hc))
	require.NoError(t, err)
	assert.Contains(t, grant.Scopes, ScopeProjectSecretRead, "human-rooted control keeps project:secret:read")

	// No other door (advisor 8): a role bound directly on the admin-rooted
	// agent holding project.secret_read does not widen it (decide step 7b
	// intersects bindings with the token scopes).
	cr, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "a3-ag-secret-" + generateID()[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.secret_read", "project.read"}})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: cr.ID, PrincipalType: store.RoleBindingPrincipalAgent,
		PrincipalID: a, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)
	grant, err = srv.AuthorizeAgentToken(ctx, fpAgent(t, s, a))
	require.NoError(t, err)
	ident := &agentIdentityWrapper{&AgentTokenClaims{ProjectID: projectID, Scopes: grant.Scopes}}
	ident.Subject = a
	d := srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(ident), Credential: credentialContextForIdentity(ident),
		Resource: Resource{Type: "project", ID: projectID}, Action: Action("secret_read"), Permission: "project.secret_read"})
	assert.False(t, d.Allowed, "a direct binding does not restore the clamped permission: %s", d.Reason)
	// Control: the human-rooted agent, with the same binding, holds it.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: cr.ID, PrincipalType: store.RoleBindingPrincipalAgent,
		PrincipalID: hc, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)
	grant, err = srv.AuthorizeAgentToken(ctx, fpAgent(t, s, hc))
	require.NoError(t, err)
	hident := &agentIdentityWrapper{&AgentTokenClaims{ProjectID: projectID, Scopes: grant.Scopes}}
	hident.Subject = hc
	d = srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(hident), Credential: credentialContextForIdentity(hident),
		Resource: Resource{Type: "project", ID: projectID}, Action: Action("secret_read"), Permission: "project.secret_read"})
	assert.True(t, d.Allowed, "control: the human-rooted agent holds project.secret_read: %s", d.Reason)
}

// The scope census (advisor 4, 8): every agent token scope is classified
// for admin-rooted agents by testAdminScopeAllowed, from the permission
// registry; today exactly project:secret:read is refused. The empty-scope
// fallback of SignAgentToken (agent:status:update) is allowed. Every
// agent-delegable permission is evaluated against the issuer through decide
// (step 0c applies to an admin-kind issuer).
func TestTestAdminA3_ScopeCensus(t *testing.T) {
	all := map[AgentTokenScope]bool{}
	for _, r := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		for _, sc := range ScopesForRole(r) {
			all[sc] = true
		}
	}
	all[ScopeAgentStatusUpdate] = true
	all[ScopeIdentityToken] = true
	var refused []AgentTokenScope
	for sc := range all {
		if !testAdminScopeAllowed(sc) {
			refused = append(refused, sc)
		}
	}
	assert.Equal(t, []AgentTokenScope{ScopeProjectSecretRead}, refused)
	assert.True(t, testAdminScopeAllowed(ScopeAgentStatusUpdate), "the SignAgentToken empty-scope fallback is allowed")
	assert.True(t, testAdminScopeAllowed(AgentTokenScope(ScopeGCPTokenPrefix+"sa@example.iam.gserviceaccount.com")), "the GCP token scope maps to an allowed permission")
	assert.False(t, testAdminScopeAllowed("unknown:scope"), "an unknown scope fails closed")
}

// Runtime secret reads through the API are clamped for an admin-rooted
// agent (ti-adm-em 11:41Z: option 1), even in the project its root owns,
// where A2's membership rule admits it: the token carries no
// project:secret:read, so the material path refuses (check 4). A
// human-rooted agent in the same project is the control. Dispatch-time
// injection under A2's membership rule is unchanged
// (TestTestAdminPolicy_DispatchSecretsClamp).
func TestTestAdminA3_RuntimeSecretReadClamped(t *testing.T) {
	srv, s, _ := tsaServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()
	issuer := taUser(t, s, "a3-rs-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	root := a3Row(t, s, k.Identity.ID)
	own := tiCreateProject(t, srv, k.AccessToken, "a3-rs-own")
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleOwner), PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: issuer.ID, ScopeType: store.RoleScopeProject, ScopeID: own, CreatedBy: "test"})
	require.NoError(t, err)
	seedSecret(t, srv.secretBackend, "A3_SECRET", "a3-value", "", "", own)

	fetch := func(rootID string, stamp store.FixtureProvenance) *httptest.ResponseRecorder {
		agentID := generateID()
		a := &store.Agent{ID: agentID, Slug: "a3-rs-" + agentID[:6], Name: "a3 rs", ProjectID: own, Phase: string(state.PhaseRunning),
			StateVersion: 1, Ancestry: []string{rootID}, Fixture: stamp, Created: time.Now(), Updated: time.Now()}
		require.NoError(t, s.CreateAgent(ctx, a))
		scopes, err := srv.authzService.clampTestAdminRootedScopes(ctx, a, ScopesForRole(AgentRoleFull))
		require.NoError(t, err)
		tok, err := srv.agentTokenService.GenerateAgentToken(agentID, own, scopes, []string{rootID})
		require.NoError(t, err)
		return doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"A3_SECRET"}}, tok)
	}
	rec := fetch(root.ID, store.FixtureProvenance{RootID: root.ID, IssuerID: issuer.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, "admin-rooted agent, own project: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "a3-value")
	// The same with only the ancestry naming the admin kind (no stamp).
	rec = fetch(root.ID, store.FixtureProvenance{})
	assert.Equal(t, http.StatusForbidden, rec.Code, "ancestry head is the admin kind: %s", rec.Body.String())
	rec = fetch(issuer.ID, store.FixtureProvenance{})
	assert.Equal(t, http.StatusOK, rec.Code, "human-rooted control: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "a3-value")
}

// Review O1: a child-group edge has no expiry. A group a test admin creates
// under a person's group, with a member a person added (unshaped), passes
// the parent's authority on only until the identity expires: the reaper's
// hold step detaches it then, not a grace period later at the purge. A
// person's own child group under the same parent is untouched.
func TestTestAdminA3_ChildGroupEdgeEndsAtExpiry(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-ce-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	owner := taUser(t, s, "a3-ce-owner", store.SystemRoleHubMember)
	projectID := tid("a3-ce-project")
	taProject(t, s, projectID, owner.ID)
	parent := &store.Group{ID: generateID(), Name: "a3 ce parent", Slug: "a3-ce-parent-" + generateID()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, parent))
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleMember), PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: parent.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)

	// The test admin creates a child group of the person's group.
	child := fpCreateGroup(t, srv, k.AccessToken, map[string]any{"name": "a3 ce child", "slug": "a3-ce-child-" + generateID()[:8], "parentId": parent.ID})
	// A person adds a person to it (no shaping, no expiry).
	member := taUser(t, s, "a3-ce-member", store.SystemRoleHubMember)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: child, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: "member", AddedBy: owner.ID}))
	// Control: a person's own child group under the same parent.
	human := &store.Group{ID: generateID(), Name: "a3 ce human", Slug: "a3-ce-human-" + generateID()[:8], GroupType: store.GroupTypeExplicit, ParentID: parent.ID}
	require.NoError(t, s.CreateGroup(ctx, human))
	control := taUser(t, s, "a3-ce-control", store.SystemRoleHubMember)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: human.ID, MemberType: store.GroupMemberTypeUser, MemberID: control.ID, Role: "member", AddedBy: owner.ID}))

	groups, err := s.GetEffectiveGroups(ctx, member.ID)
	require.NoError(t, err)
	require.Contains(t, groups, parent.ID, "before expiry the child passes the parent's authority on")

	// One reaper tick inside the grace period after the expiry.
	_, err = srv.reapTestIdentities(ctx, row.ExpiresAt.Add(time.Minute))
	require.NoError(t, err)
	groups, err = s.GetEffectiveGroups(ctx, member.ID)
	require.NoError(t, err)
	assert.NotContains(t, groups, parent.ID, "after the expiry the edge is gone")
	assert.Contains(t, groups, child, "the membership itself (a person's) stays")
	groups, err = s.GetEffectiveGroups(ctx, control.ID)
	require.NoError(t, err)
	assert.Contains(t, groups, parent.ID, "control: a person's child group keeps its edge")
	assert.Len(t, fpPurgeAudits(t, s, testAdminGroupEdgeRemoveMutation, child), 1, "the removal is audited")
	_, err = s.GetUser(ctx, row.ID)
	assert.NoError(t, err, "the identity itself is purged only after the grace period")
}

// Review R2: a custom role the test admin defined that a person's binding
// names is frozen for the test admin (the store refuses the change; see
// TestGrantShaping_StampedRoleBoundByPersonIsFrozen), listed in the residue
// report's KeptRoles, and kept by the purge (KeptGrants), with the person's
// binding untouched.
func TestTestAdminA3_StampedRoleBoundByPersonKept(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-kr-iss", store.SystemRoleSuperAdmin)
	k := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	row := a3Row(t, s, k.Identity.ID)
	name := "a3-kr-" + generateID()[:8]
	rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
		map[string]any{"name": name, "scopeType": "project", "permissions": []string{"project.read"}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	require.NoError(t, err)
	owner := taUser(t, s, "a3-kr-owner", store.SystemRoleHubMember)
	projectID := tid("a3-kr-project")
	taProject(t, s, projectID, owner.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: owner.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: owner.ID})
	require.NoError(t, err)

	rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/admin/roles/"+rd.ID,
		map[string]any{"name": name, "permissions": []string{"project.read", "project.update"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), store.ErrTestAdminGrantRefused.Error())

	res, err := srv.testAdminResidue(ctx, row.ID, *row.IssuanceID)
	require.NoError(t, err)
	require.Len(t, res.KeptRoles, 1)
	assert.Equal(t, rd.ID, res.KeptRoles[0].ID)

	rec = doRequestAsUser(t, srv, issuer, http.MethodDelete, "/api/v1/test-identities/"+row.ID+"?purge=true", nil)
	report := fpPurgeReport(t, rec)
	assert.True(t, report.IdentityDeleted, rec.Body.String())
	require.Len(t, report.KeptGrants, 1)
	assert.Equal(t, rd.ID, report.KeptGrants[0].ID)
	assert.Equal(t, "role_in_use", report.KeptGrants[0].Reason)
	got, err := s.GetRoleDefinition(ctx, rd.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"project.read"}, got.Permissions, "the kept role is unchanged")
	roles, err := activeDirectProjectRoles(ctx, s, nil, false, owner.ID, projectID)
	require.NoError(t, err)
	assert.True(t, roles[store.ProjectRoleOwner], "the person's own grants stay")
}

// Review N1, at the place the refusal happens: the route guard
// (enforceRoutePermission) answers every refusal with the generic
// structured 403 for the route's permission, whatever the decision's
// cause. Driven deterministically: an authorization service whose user
// reads after the account-status gate's users-row read fail
// (a3LaterReadFaultStore) and whose admin tier is on (as Server.New wires
// it), and a request that has not passed the JWT row block (which would
// otherwise warm the kind cache from the real store). The decision is the
// test-admin policy's fail-closed kind-error refusal, and the person still
// sees only the neutral body: no test-identity wording, no reason, no
// denied_by.
// (The earlier end-to-end version never reached the failing read: the row
// block cached the kind through the real store first, and the swapped
// service had no tier switch; see the AC map.)
func TestTestAdminA3_UnverifiedKindGuardAnswerIsNeutral(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	human := taUser(t, s, "a3-uk-human", store.SystemRoleHubMember)
	ident := tsaIdentityFromRow(t, s, human.ID)
	meta, ok := routeMetadataTable["DELETE /api/v1/test-identities/{id}"]
	require.True(t, ok)

	saved := srv.authzService
	later := &a3LaterReadFaultStore{Store: s}
	svc := NewAuthzService(later, saved.logger)
	setAuthzTestAdminTier(svc, func(string) bool { return true }, time.Now)
	srv.authzService = svc
	t.Cleanup(func() { srv.authzService = saved })

	// The account-status gate's users-row read succeeds; the step 0c kind
	// read after it fails.
	later.budget(1)
	d := svc.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(ident), Credential: credentialContextForIdentity(ident),
		Resource: Resource{Type: meta.Resource}, Action: Action(meta.Action), Permission: meta.Permission, TargetEvidence: hubCollectionEvidence(meta.Permission)})
	require.False(t, d.Allowed)
	require.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy)
	require.Equal(t, testAdminPolicyKindErrorReason, d.Reason, "the cause is the unreadable kind")
	require.Equal(t, int32(2), later.calls.Load(), "user reads in decide: the status gate's row, then the step 0c kind")

	later.budget(1)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/test-identities/"+generateID()+"?purge=true", nil)
	req = req.WithContext(contextWithIdentityAndUser(ident))
	rec := httptest.NewRecorder()
	require.False(t, srv.enforceRoutePermission(rec, req, meta), "the guard refuses")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, body.Error.Code)
	assert.Equal(t, "Insufficient permissions", body.Error.Message, "the generic message")
	assert.Equal(t, map[string]string{"resource_type": "test_identity", "denied_action": "issue"}, body.Error.Details, "no reason, no denied_by")
}

// Kind-read fail-closed diagnostic (ti-adm-em, after the shard-2 run at
// df5e33dea): with the authorization service's user reads failing and no
// kind cached, a person holding test_identity.issue through a system-scope
// binding is refused: (0) with every user read failing, at the
// account-status gate, the first user read in decide; then, with that read
// succeeding and later user reads failing (a3LaterReadFaultStore), both
// (i) with the admin tier on (decide step 0c
// refuses on the unreadable kind) and (ii) with it off (step 0c is skipped;
// the grant clamp's kind read in step 3 refuses). (iii) No principal-inputs
// memo is in the context, so the bindings come from the store, through the
// clamp. (iv) The setup artefact of the earlier end-to-end test: a kind
// read through the server's real store first (what the JWT row block does)
// warms the same cache, and the clamp then never reaches the failing read.
func TestTestAdminA3_KindReadFailClosedDiagnostic(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuerRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "a3-kd-issuer-" + generateID()[:8],
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue}})
	require.NoError(t, err)
	human := taUser(t, s, "a3-kd-human", store.SystemRoleHubMember)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: issuerRole.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	ident := tsaIdentityFromRow(t, s, human.ID)
	req := AuthzRequest{Principal: principalContextForIdentity(ident), Credential: credentialContextForIdentity(ident),
		Resource: Resource{Type: "test_identity"}, Action: ActionIssue, Permission: permissionTestIdentityIssue,
		TargetEvidence: hubCollectionEvidence(permissionTestIdentityIssue), Explain: true}
	require.Nil(t, authzInputMemoFromContext(ctx), "(iii) no principal-inputs memo: bindings are read from the store")

	// Control: the real store admits the person.
	d := srv.authzService.Decide(ctx, req)
	require.True(t, d.Allowed, "control: %s", d.Reason)

	// (0) admin tier on, every user read failing: the account-status gate's
	// users-row read is the first user read in decide, and it refuses
	// before step 0c.
	unread := NewAuthzService(kindErrStore{Store: s}, srv.authzService.logger)
	setAuthzTestAdminTier(unread, func(string) bool { return true }, time.Now)
	d = unread.Decide(ctx, req)
	t.Logf("(0) user row unreadable, tier on: allowed=%v denied_by=%q reason=%q deny_cause=%q", d.Allowed, d.DeniedBy, d.Reason, d.DenyCause)
	assert.False(t, d.Allowed)
	assert.Equal(t, principalStatusLookupReason, d.Reason, "(0) refused at the account-status gate")
	assert.Equal(t, DenyCauseResolutionError, d.DenyCause)
	assert.Equal(t, DeniedBy(""), d.DeniedBy, "(0) the status gate refuses before step 0c")

	// (i) admin tier on, cold cache: the status gate's users-row read
	// succeeds and the step 0c kind read after it fails.
	laterOn := &a3LaterReadFaultStore{Store: s}
	on := NewAuthzService(laterOn, srv.authzService.logger)
	setAuthzTestAdminTier(on, func(string) bool { return true }, time.Now)
	laterOn.budget(1)
	d = on.Decide(ctx, req)
	t.Logf("(i) tier on: allowed=%v denied_by=%q reason=%q", d.Allowed, d.DeniedBy, d.Reason)
	assert.False(t, d.Allowed)
	assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy)
	assert.Equal(t, testAdminPolicyKindErrorReason, d.Reason)
	assert.Equal(t, int32(2), laterOn.calls.Load(), "(i) user reads: the status gate's row, then the step 0c kind")

	// (ii) admin tier off (step 0c skipped), cold cache: the status gate's
	// read succeeds and only the clamp's kind read can refuse.
	laterOff := &a3LaterReadFaultStore{Store: s}
	off := NewAuthzService(laterOff, srv.authzService.logger)
	laterOff.budget(1)
	d = off.Decide(ctx, req)
	t.Logf("(ii) tier off: allowed=%v denied_by=%q reason=%q deny_cause=%q", d.Allowed, d.DeniedBy, d.Reason, d.DenyCause)
	if d.Provenance != nil {
		t.Logf("(ii) provenance errors=%v deny_reasons=%v", d.Provenance.Errors, d.Provenance.DenyReasons)
	}
	assert.False(t, d.Allowed, "(ii) an unreadable kind must refuse at the clamp (fail-closed)")
	assert.Equal(t, DenyCauseResolutionError, d.DenyCause, "(ii) refused as a binding resolution error")
	assert.Equal(t, int32(2), laterOff.calls.Load(), "(ii) user reads: the status gate's row, then the clamp's kind")

	// (iv) the artefact: a successful kind read through the real store into
	// the same cache (as the row block does) turns the clamp's read into a
	// cache hit; the status gate's users-row read is the only store read.
	laterWarm := &a3LaterReadFaultStore{Store: s}
	warm := NewAuthzService(laterWarm, srv.authzService.logger)
	k, err := kindForPrincipalVia(ctx, s, warm.kindCache(), human.ID)
	require.NoError(t, err)
	require.True(t, k.isHuman())
	laterWarm.budget(1)
	d = warm.Decide(ctx, req)
	t.Logf("(iv) warmed cache, tier off: allowed=%v denied_by=%q reason=%q", d.Allowed, d.DeniedBy, d.Reason)
	assert.True(t, d.Allowed, "(iv) a kind cached from a successful read of the real store admits: the earlier test's setup, not a fail-open")
	assert.Equal(t, int32(1), laterWarm.calls.Load(), "(iv) user reads: the status gate's row only; the kind is a cache hit")
}

// a3KindFaultStore is the server's own store with GetUser failing for one
// user ID while armed. It is passed to New, so the JWT row block, the
// authorization service and its kind cache all read through it (the real
// wiring); DB and Dialect are forwarded for the code that needs them.
type a3KindFaultStore struct {
	store.Store
	mu   sync.Mutex
	fail string
	// allow is the number of reads of the armed user that still succeed;
	// calls counts every read of the armed user.
	allow atomic.Int32
	calls atomic.Int32
}

func (f *a3KindFaultStore) arm(userID string) { f.mu.Lock(); f.fail = userID; f.mu.Unlock() }

// budget lets the next n reads of the armed user succeed and resets calls.
func (f *a3KindFaultStore) budget(n int32) { f.allow.Store(n); f.calls.Store(0) }

func (f *a3KindFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail != "" && id == fail {
		f.calls.Add(1)
		if f.allow.Add(-1) >= 0 {
			return f.Store.GetUser(ctx, id)
		}
		return nil, errors.New("injected user read failure")
	}
	return f.Store.GetUser(ctx, id)
}

// a3LaterReadFaultStore lets the first allow user reads succeed and fails
// every later one; calls counts all of them. With allow=1 before a
// decision, the account-status gate's users-row read (the first user read
// in decide) succeeds and the next user read (the step 0c or step 3 clamp
// kind read) fails.
type a3LaterReadFaultStore struct {
	store.Store
	allow atomic.Int32
	calls atomic.Int32
}

// budget lets the next n user reads succeed and resets calls.
func (f *a3LaterReadFaultStore) budget(n int32) { f.allow.Store(n); f.calls.Store(0) }

func (f *a3LaterReadFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	f.calls.Add(1)
	if f.allow.Add(-1) >= 0 {
		return f.Store.GetUser(ctx, id)
	}
	return nil, errors.New("injected later user read failure")
}

func (f *a3KindFaultStore) DB() *sql.DB {
	if d, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return d.DB()
	}
	return nil
}

func (f *a3KindFaultStore) Dialect() string {
	if d, ok := f.Store.(interface{ Dialect() string }); ok {
		return d.Dialect()
	}
	return ""
}

// Kind-read fail-closed under the REAL wiring (ti-adm-em; A2's DEC-3
// promise): one store (the server's own, with an injected GetUser failure
// for one person), one kind cache, admin tier on. A person holding
// test_identity.issue through a system-scope binding:
//   - on the purge route over HTTP is refused (the JWT row block fails
//     closed on the unreadable kind before any handler);
//   - in decide, where the account-status gate's users-row read succeeds
//     and the next read of the person fails, with the tier on is refused
//     by step 0c (test-admin policy, kind-error reason);
//   - in decide with the tier off (step 0c skipped) is refused by the grant
//     clamp's kind read in step 3 (binding resolution error).
//
// With the fault cleared the same person is admitted (control).
func TestTestAdminA3_KindReadFailClosedRealWiring(t *testing.T) {
	inner, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	f := &a3KindFaultStore{Store: inner}
	srv, _ := testServerOnMigratedStore(t, f)
	enableTestIdentitiesForTest(srv)
	taEnableGate(srv)
	ctx := context.Background()
	issuerRole, err := inner.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "a3-rw-issuer-" + generateID()[:8],
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue}})
	require.NoError(t, err)
	human := taUser(t, inner, "a3-rw-human", store.SystemRoleHubMember)
	_, err = inner.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: issuerRole.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	ident := tsaIdentityFromRow(t, inner, human.ID)
	req := AuthzRequest{Principal: principalContextForIdentity(ident), Credential: credentialContextForIdentity(ident),
		Resource: Resource{Type: "test_identity"}, Action: ActionIssue, Permission: permissionTestIdentityIssue,
		TargetEvidence: hubCollectionEvidence(permissionTestIdentityIssue)}
	require.True(t, srv.testAdminTierOn(), "the admin tier is on")
	require.NotNil(t, srv.authzService.kindCache(), "the server's authorization service has the shared kind cache")

	f.arm(human.ID)
	f.budget(0)
	rec := doRequestAsUser(t, srv, human, http.MethodDelete, "/api/v1/test-identities/"+generateID()+"?purge=true", nil)
	t.Logf("HTTP purge route, kind unreadable: status=%d body=%s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "refused over HTTP: the JWT row block cannot verify the user")
	assert.Contains(t, rec.Body.String(), `"code":"store_error"`, "the row block's fail-closed answer")
	assert.NotContains(t, rec.Body.String(), `"identityId"`, "no purge report: the request never reached the handler")

	// In decide, the account-status gate's users-row read succeeds and the
	// next read of the person (step 0c or the clamp's kind read) fails.
	f.budget(1)
	d := srv.authzService.Decide(ctx, req)
	t.Logf("decide, tier on: allowed=%v denied_by=%q reason=%q deny_cause=%q", d.Allowed, d.DeniedBy, d.Reason, d.DenyCause)
	assert.False(t, d.Allowed, "tier on: refused")
	assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy)
	assert.Equal(t, testAdminPolicyKindErrorReason, d.Reason)
	assert.Equal(t, int32(2), f.calls.Load(), "tier on: user reads, the status gate's row then the step 0c kind")

	srv.testAdminGate = testAdminGate{}
	require.False(t, srv.testAdminTierOn())
	f.budget(1)
	d = srv.authzService.Decide(ctx, req)
	t.Logf("decide, tier off (step 3 clamp only): allowed=%v denied_by=%q reason=%q deny_cause=%q", d.Allowed, d.DeniedBy, d.Reason, d.DenyCause)
	assert.False(t, d.Allowed, "tier off: the clamp's kind read refuses")
	assert.Equal(t, DenyCauseResolutionError, d.DenyCause)
	assert.Equal(t, int32(2), f.calls.Load(), "tier off: user reads, the status gate's row then the clamp's kind")

	f.arm("")
	d = srv.authzService.Decide(ctx, req)
	assert.True(t, d.Allowed, "control: a readable kind admits: %s", d.Reason)
}

// The A3 agent mint census exempts conduit_relay_admit.go:writeRelayAdmission
// (relay admission claims of a user principal copy its scoped credential's
// scopes). That exemption holds only while no decision reads those claim
// scopes: a relay-held user session is rebuilt from the live user and token
// rows (relayUserIdentity) on every operation. This pins it: admission claims
// whose scopes and project differ from the live token yield an identity with
// the token's scopes, boundary and ceiling, for the same admin-kind user, and
// the test-admin policy still refuses that user a deny-classified permission.
func TestTestAdminA3_RelayUserIdentityIgnoresClaimScopes(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "a3-relay-iss", store.SystemRoleSuperAdmin)
	admin := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	projectID := tid("a3-relay-project")
	taProject(t, s, projectID, issuer.ID)
	expiry := time.Now().Add(time.Hour)
	tok := &store.UserAccessToken{ID: generateID(), UserID: admin.Identity.ID, Name: "a3-relay-uat", Prefix: "a3r_",
		KeyHash: "a3-relay-uat-key-hash", BoundaryKind: "project", ProjectID: projectID, Scopes: []string{"project:read"},
		ExpiresAt: &expiry, Created: time.Now()}
	require.NoError(t, s.CreateUserAccessToken(ctx, tok))
	live, err := s.GetUserAccessToken(ctx, tok.ID)
	require.NoError(t, err)

	claims := conduitAdmissionClaims{ID: admin.Identity.ID, AuthType: AuthTypeUAT, CredentialID: tok.ID,
		ScopeProject: tid("a3-relay-other"), Scopes: []string{"project:read", "project:update", "project:secret:read", "hub:admin"}}
	identity, verdict, reason := srv.relayUserIdentity(ctx, claims)
	require.Equal(t, conduitAuthzAllowed, verdict, reason)
	scoped, ok := identity.(*ScopedUserIdentity)
	require.True(t, ok, "a UAT session is rebuilt as a scoped user, got %T", identity)
	assert.Equal(t, admin.Identity.ID, scoped.ID(), "the same admin-kind user")
	assert.Equal(t, live.Scopes, scoped.ScopedScopes(), "scopes from the live token, not the admission claims")
	assert.NotEqual(t, claims.Scopes, scoped.ScopedScopes())
	assert.Equal(t, projectID, scoped.Boundary().ProjectID, "boundary from the live token, not the claims' ScopeProject")
	assert.Equal(t, live.NormalizedCeiling(), scoped.Ceiling(), "ceiling from the live token")

	d := srv.authzService.Decide(ctx, AuthzRequest{
		Principal: principalContextForIdentity(identity), Credential: credentialContextForIdentity(identity),
		Resource: Resource{Type: "project", ID: projectID}, Action: ActionManageVisitors, Permission: "project.manage_visitors",
	})
	assert.False(t, d.Allowed, "deny-classified permission for the rebuilt admin-kind user: %s", d.Reason)
	assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, "the test-admin policy applies to the rebuilt user (%s)", d.Reason)
}
