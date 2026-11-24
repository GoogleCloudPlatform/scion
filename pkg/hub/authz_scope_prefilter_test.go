//go:build !no_sqlite

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

package hub

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func newAuthzPrefilterTestServer(t *testing.T) (*Server, *entadapter.CompositeStore, string) {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("SCION_TEST_POSTGRES_DSN"); dsn != "" {
		schemaName := "authz_prefilter_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		adminClient, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2})
		require.NoError(t, err)
		adminCS := entadapter.NewCompositeStore(adminClient)
		_, err = adminCS.DB().ExecContext(ctx, "CREATE SCHEMA "+schemaName)
		require.NoError(t, err)
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		scopedDSN := dsn + sep + "search_path=" + schemaName
		base, err := entc.OpenPostgres(scopedDSN, entc.PoolConfig{MaxOpenConns: 4})
		if err != nil {
			_, _ = adminCS.DB().ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
			_ = adminCS.Close()
			require.NoError(t, err)
		}
		require.NoError(t, entc.AutoMigrate(ctx, base))
		cs := entadapter.NewCompositeStore(base)
		t.Cleanup(func() {
			_ = cs.Close()
			_, _ = adminCS.DB().ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
			_ = adminCS.Close()
		})
		srv, _ := testServerOnMigratedStore(t, cs)
		alice, _, _ := setupDemoPolicyOn(t, srv, srv.store)
		return srv, cs, alice.ID
	}
	rawStore, _, err := newTestStoreWithClient(t)
	require.NoError(t, err)
	srv, _ := testServerOnMigratedStore(t, rawStore)
	alice, _, _ := setupDemoPolicyOn(t, srv, srv.store)
	return srv, rawStore, alice.ID
}

// TestEvaluate_PrecomputedSortedPermsByteIdentical verifies that precomputing
// sortedPerms on RolePermissions produces byte-identical KernelDecision and
// KernelProvenance compared to a RolePermissions without precomputed keys.
func TestEvaluate_PrecomputedSortedPermsByteIdentical(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	perms := []string{"project.update", "project.read", "project.chat_post", "agent.list", "agent.read"}

	precomputed := NewRolePermissions("r-member", "project-member", ScopeTypeProject, perms)
	permMap := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		permMap[p] = struct{}{}
	}
	unprecomputed := &RolePermissions{
		RoleID:            "r-member",
		RoleName:          "project-member",
		ScopeType:         ScopeTypeProject,
		Permissions:       permMap,
		ConfersMembership: true,
	}

	candidates := []CandidateBinding{
		{
			BindingID:        "b-match",
			RoleDefinitionID: "r-member",
			PrincipalType:    "user",
			PrincipalID:      "u1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "p1",
		},
		{
			BindingID:        "b-other-proj",
			RoleDefinitionID: "r-member",
			PrincipalType:    "user",
			PrincipalID:      "u1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "p2",
		},
	}

	reqPre := KernelRequest{
		Permission:        "project.read",
		PrincipalClosure:  map[string]struct{}{"user:u1": {}},
		MembershipPaths:   map[string][]string{"user:u1": {"u1"}},
		Resource:          ResourceContext{ResourceType: "project", ResourceID: "p1", ProjectID: "p1"},
		CandidateBindings: candidates,
		RoleDefinitions:   map[string]*RolePermissions{"r-member": precomputed},
		Now:               now,
	}
	reqUnpre := reqPre
	reqUnpre.RoleDefinitions = map[string]*RolePermissions{"r-member": unprecomputed}

	gotPre := Evaluate(reqPre)
	gotUnpre := Evaluate(reqUnpre)
	assert.Equal(t, gotUnpre, gotPre, "precomputed sortedPerms must produce byte-identical KernelDecision and KernelProvenance")
	assert.True(t, gotPre.Allowed)
	assert.Len(t, gotPre.Provenance.GrantingBindings, 1)
	assert.Len(t, gotPre.Provenance.RejectedCandidates, 1)
	assert.Equal(t, []string{"agent.list", "agent.read", "project.chat_post", "project.read", "project.update"},
		gotPre.Provenance.GrantingBindings[0].Permissions)
}

// TestAuthzScopePrefilter_ParityMatrix verifies that non-explain decide() with
// authzInputMemo's BindingsForProject scope pre-filter produces per-project
// decisions identical to unmemoized full-binding decide() across every identity
// kind, binding shape, access constraint shape, test identity, account status,
// UAT scope/boundary, and View-as-member session (ptone/scion#9157).
func TestAuthzScopePrefilter_ParityMatrix(t *testing.T) {
	ctx := context.Background()
	srv, rawStore, issuerID := newAuthzPrefilterTestServer(t)
	s := srv.store
	d := rawStore.Dialect()
	var err error

	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	projMemberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	projViewerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)
	projAdminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	newProj := func(slug string) *store.Project {
		p := &store.Project{
			ID:      api.NewUUID(),
			Name:    "Proj " + slug,
			Slug:    slug,
			Created: time.Now().UTC(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		return p
	}
	pMember := newProj("p-member")
	pViewer := newProj("p-viewer")
	pAdmin := newProj("p-admin")
	pDirectGroup := newProj("p-direct-group")
	pNestedGroup := newProj("p-nested-group")
	pExpired := newProj("p-expired")
	pFuture := newProj("p-future")
	pConstrainedReadOnly := newProj("p-constrained-ro")
	pGroupConstrainedRO := newProj("p-group-constrained-ro")
	pGroupConstrainedDeny := newProj("p-group-constrained-deny")
	pNoGrant := newProj("p-no-grant")

	allProjects := []*store.Project{
		pMember, pViewer, pAdmin, pDirectGroup, pNestedGroup,
		pExpired, pFuture, pConstrainedReadOnly, pGroupConstrainedRO,
		pGroupConstrainedDeny, pNoGrant,
	}
	allResources := make([]Resource, len(allProjects))
	for i, p := range allProjects {
		allResources[i] = projectResource(p)
	}

	directGrp := &store.Group{ID: api.NewUUID(), Name: "direct-grp", Slug: "direct-grp", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, directGrp))
	childGrp := &store.Group{ID: api.NewUUID(), Name: "child-grp", Slug: "child-grp", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, childGrp))
	parentGrp := &store.Group{ID: api.NewUUID(), Name: "parent-grp", Slug: "parent-grp", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, parentGrp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: parentGrp.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childGrp.ID, Role: store.GroupMemberRoleMember,
	}))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projMemberRD.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: directGrp.ID,
		ScopeType: store.RoleScopeProject, ScopeID: pDirectGroup.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projMemberRD.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: parentGrp.ID,
		ScopeType: store.RoleScopeProject, ScopeID: pNestedGroup.ID, CreatedBy: "test",
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	past := now.Add(-1 * time.Hour)
	future := now.Add(2 * time.Hour)

	createUser := func(email, status, kind string, expiresAt *time.Time) *store.User {
		u := &store.User{
			ID:          api.NewUUID(),
			Email:       email,
			DisplayName: email,
			Role:        store.UserRoleMember,
			Status:      status,
			Kind:        kind,
			ExpiresAt:   expiresAt,
			Created:     now,
		}
		switch kind {
		case store.UserKindTestFixture:
			u.Email = fmt.Sprintf("fixture-%s@%s", api.NewUUID()[:6], store.TestFixtureEmailDomain)
			u.IssuedBy = &issuerID
			require.NoError(t, rawStore.CreateTestFixtureUser(ctx, u))
		case store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin:
			domain := store.TestHubAdminEmailDomain
			rdID := hubAdminRD.ID
			if kind == store.UserKindTestSuperAdmin {
				domain = store.TestSuperAdminEmailDomain
				rdID = superAdminRD.ID
			}
			u.Email = fmt.Sprintf("tadmin-%s@%s", api.NewUUID()[:6], domain)
			u.Role = store.TestAdminRole(kind)
			issuanceID := api.NewUUID()
			purpose := "authz scope prefilter parity matrix"
			u.IssuedBy = &issuerID
			u.IssuanceID = &issuanceID
			u.Purpose = &purpose
			createExp := future
			u.ExpiresAt = &createExp
			_, cerr := rawStore.CreateTestAdminUser(ctx, u, rdID)
			require.NoError(t, cerr)
			if expiresAt != nil && expiresAt.Before(now) {
				q := "UPDATE users SET expires_at = ? WHERE id = ?"
				if d == dialect.Postgres {
					q = "UPDATE users SET expires_at = $1 WHERE id = $2"
				}
				_, uerr := rawStore.DB().ExecContext(ctx, q, *expiresAt, u.ID)
				require.NoError(t, uerr)
				u.ExpiresAt = expiresAt
			}
		default:
			require.NoError(t, rawStore.CreateUser(ctx, u))
		}
		return u
	}

	bindMixedProjects := func(uid string) {
		for _, spec := range []struct {
			projID string
			rdID   string
			nb     *time.Time
			exp    *time.Time
		}{
			{pMember.ID, projMemberRD.ID, nil, nil},
			{pViewer.ID, projViewerRD.ID, nil, nil},
			{pAdmin.ID, projAdminRD.ID, nil, nil},
			{pExpired.ID, projMemberRD.ID, nil, &past},
			{pFuture.ID, projMemberRD.ID, &future, nil},
			{pConstrainedReadOnly.ID, projMemberRD.ID, nil, nil},
			{pGroupConstrainedRO.ID, projMemberRD.ID, nil, nil},
			{pGroupConstrainedDeny.ID, projMemberRD.ID, nil, nil},
		} {
			_, berr := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: spec.rdID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid,
				ScopeType: store.RoleScopeProject, ScopeID: spec.projID,
				NotBefore: spec.nb, ExpiresAt: spec.exp, CreatedBy: "test",
			})
			require.NoError(t, berr)
		}
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: directGrp.ID, MemberType: store.GroupMemberTypeUser, MemberID: uid, Role: store.GroupMemberRoleMember,
		}))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: childGrp.ID, MemberType: store.GroupMemberTypeUser, MemberID: uid, Role: store.GroupMemberRoleMember,
		}))
	}

	strPtr := func(v string) *string { return &v }

	// 1. Regular human user with mixed direct + group + nested group + constraints.
	human := createUser("human@test.local", store.UserStatusActive, store.UserKindHuman, nil)
	bindMixedProjects(human.ID)

	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		ID:                   api.NewUUID(),
		Name:                 "user-ro-constraint",
		SubjectKind:          "principal",
		SubjectPrincipalType: strPtr("user"),
		SubjectPrincipalID:   strPtr(human.ID),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              pConstrainedReadOnly.ID,
		MaximumPermissions:   []string{"project.read"},
		CreatedBy:            "test",
	})
	require.NoError(t, err)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		ID:                 api.NewUUID(),
		Name:               "group-ro-constraint",
		SubjectKind:        "group_closure",
		SubjectGroupID:     strPtr(directGrp.ID),
		ScopeType:          store.RoleScopeProject,
		ScopeID:            pGroupConstrainedRO.ID,
		MaximumPermissions: []string{"project.read"},
		CreatedBy:          "test",
	})
	require.NoError(t, err)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		ID:                 api.NewUUID(),
		Name:               "nested-group-deny-constraint",
		SubjectKind:        "group_closure",
		SubjectGroupID:     strPtr(parentGrp.ID),
		ScopeType:          store.RoleScopeProject,
		ScopeID:            pGroupConstrainedDeny.ID,
		MaximumPermissions: []string{"agent.list"},
		CreatedBy:          "test",
	})
	require.NoError(t, err)

	// 2. System hub-admin (also tested with View-as-member).
	sysAdmin := createUser("sysadmin@test.local", store.UserStatusActive, store.UserKindHuman, nil)
	sysAdmin.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, sysAdmin))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: sysAdmin.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projMemberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: sysAdmin.ID,
		ScopeType: store.RoleScopeProject, ScopeID: pMember.ID, CreatedBy: "test",
	})
	require.NoError(t, err)

	// 3. System-constrained user.
	sysConstrained := createUser("sysconstrained@test.local", store.UserStatusActive, store.UserKindHuman, nil)
	bindMixedProjects(sysConstrained.ID)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		ID:                   api.NewUUID(),
		Name:                 "sys-ro-constraint",
		SubjectKind:          "principal",
		SubjectPrincipalType: strPtr("user"),
		SubjectPrincipalID:   strPtr(sysConstrained.ID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"project.read"},
		CreatedBy:            "test",
	})
	require.NoError(t, err)

	// 4. Test identities: test_fixture, test_hub_admin (live/expired), test_super_admin.
	// CreateTestAdminUser already creates the system role binding for test_hub_admin
	// and test_super_admin.
	testFixture := createUser("fixture@test.local", store.UserStatusActive, store.UserKindTestFixture, &future)
	bindMixedProjects(testFixture.ID)
	_, err = rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: testFixture.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)

	testHubAdminLive := createUser("tha-live@test.local", store.UserStatusActive, store.UserKindTestHubAdmin, &future)
	testHubAdminExpired := createUser("tha-exp@test.local", store.UserStatusActive, store.UserKindTestHubAdmin, &past)
	testSuperAdmin := createUser("tsa@test.local", store.UserStatusActive, store.UserKindTestSuperAdmin, &future)

	// 5. Suspended, invited, and unknown-Kind users.
	suspended := createUser("suspended@test.local", store.UserStatusSuspended, store.UserKindHuman, nil)
	bindMixedProjects(suspended.ID)

	invited := createUser("invited@test.local", store.UserStatusInvited, store.UserKindHuman, nil)
	bindMixedProjects(invited.ID)

	unknownKind := createUser("unknown-kind@test.local", store.UserStatusActive, store.UserKindHuman, nil)
	bindMixedProjects(unknownKind.ID)
	qUnknown := `UPDATE users SET kind = 'future_unknown_kind' WHERE id = ?`
	if d == dialect.Postgres {
		qUnknown = `UPDATE users SET kind = 'future_unknown_kind' WHERE id = $1`
	}
	_, err = rawStore.DB().ExecContext(ctx, qUnknown, unknownKind.ID)
	require.NoError(t, err)

	// 6. Scoped UAT identities for human.
	humanIdent := NewAuthenticatedUser(human.ID, human.Email, human.DisplayName, human.Role, string(ClientTypeWeb))
	uatHubReadWrite := NewScopedUserIdentityWithBoundaryAndDecoration(humanIdent, hubBoundary(), []string{"project:read", "inbox:write"}, "tok-hub-rw", bearerCeiling(t, "project:read", "inbox:write"), nil)
	uatHubReadOnly := NewScopedUserIdentityWithBoundaryAndDecoration(humanIdent, hubBoundary(), []string{"project:read"}, "tok-hub-ro", bearerCeiling(t, "project:read"), nil)
	uatHubNoRead := NewScopedUserIdentityWithBoundaryAndDecoration(humanIdent, hubBoundary(), []string{"inbox:read"}, "tok-hub-noread", bearerCeiling(t, "inbox:read"), nil)
	uatProjMember := NewScopedUserIdentityWithBoundaryAndDecoration(humanIdent, projectBoundary(pMember.ID), []string{"project:read", "inbox:write"}, "tok-proj-member", bearerCeiling(t, "project:read", "inbox:write"), nil)

	// Parity checker: compares memoized (BindingsForProject pre-filtered)
	// evaluation against maskAllAuthzMemo (unfiltered full-binding evaluation)
	// across all 11 projects for CheckAccess, ComputeCapabilitiesForActions,
	// and chatUserAccess.
	checkParity := func(t *testing.T, evalCtx context.Context, ident UserIdentity, tokenScope bool) {
		t.Helper()
		memoCtx := withAuthzInputMemo(evalCtx)
		unfilteredCtx := maskAllAuthzMemo(evalCtx)

		gotCaps := srv.authzService.ComputeCapabilitiesForActions(memoCtx, ident, allResources, []Action{ActionRead, ActionChatPost})
		wantCaps := srv.authzService.ComputeCapabilitiesForActions(unfilteredCtx, ident, allResources, []Action{ActionRead, ActionChatPost})
		for i, p := range allProjects {
			assert.Equal(t, wantCaps[i].Actions, gotCaps[i].Actions,
				"project %s (%s) capabilities mismatch", p.Slug, p.ID)

			// Also verify full Decision fields (Allowed, Reason, BindingID, RoleName, Scope, DenyCause).
			for _, act := range []Action{ActionRead, ActionChatPost} {
				var gotD, wantD Decision
				if IsScopedUserIdentity(ident) {
					gotD = srv.authzService.DecideFromContext(contextWithIdentity(memoCtx, ident), allResources[i], act)
					wantD = srv.authzService.DecideFromContext(contextWithIdentity(unfilteredCtx, ident), allResources[i], act)
				} else {
					gotD = srv.authzService.CheckAccess(memoCtx, ident, allResources[i], act)
					wantD = srv.authzService.CheckAccess(unfilteredCtx, ident, allResources[i], act)
				}
				assert.Equal(t, wantD.Allowed, gotD.Allowed, "project %s action %s Allowed mismatch", p.Slug, act)
				assert.Equal(t, wantD.Reason, gotD.Reason, "project %s action %s Reason mismatch", p.Slug, act)
				assert.Equal(t, wantD.BindingID, gotD.BindingID, "project %s action %s BindingID mismatch", p.Slug, act)
				assert.Equal(t, wantD.RoleName, gotD.RoleName, "project %s action %s RoleName mismatch", p.Slug, act)
				assert.Equal(t, wantD.Scope, gotD.Scope, "project %s action %s Scope mismatch", p.Slug, act)
				assert.Equal(t, wantD.DenyCause, gotD.DenyCause, "project %s action %s DenyCause mismatch", p.Slug, act)
			}

			gotChat := srv.chatUserAccess(memoCtx, ident, p,
				[]string{ChatActionRead}, chatThreadWriteActions, nil, tokenScope, false)
			wantChat := srv.chatUserAccess(unfilteredCtx, ident, p,
				[]string{ChatActionRead}, chatThreadWriteActions, nil, tokenScope, false)
			assert.Equal(t, wantChat.Actions, gotChat.Actions,
				"project %s (%s) ChatAccess.Actions mismatch (tokenScope=%v)", p.Slug, p.ID, tokenScope)
			assert.Equal(t, wantChat.Reason, gotChat.Reason,
				"project %s (%s) ChatAccess.Reason mismatch (tokenScope=%v)", p.Slug, p.ID, tokenScope)
		}
	}

	userIdent := func(u *store.User) UserIdentity {
		return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
	}

	t.Run("human_mixed_direct_group_nested_constraints", func(t *testing.T) {
		checkParity(t, ctx, humanIdent, false)
		memoCtx := withAuthzInputMemo(ctx)
		caps := srv.authzService.ComputeCapabilitiesForActions(memoCtx, humanIdent, allResources, []Action{ActionRead, ActionChatPost})
		assert.Equal(t, []string{"read", "chat_post"}, caps[0].Actions, "pMember")
		assert.Equal(t, []string{"read"}, caps[1].Actions, "pViewer")
		assert.Equal(t, []string{"read", "chat_post"}, caps[2].Actions, "pAdmin")
		assert.Equal(t, []string{"read", "chat_post"}, caps[3].Actions, "pDirectGroup")
		assert.Equal(t, []string{"read", "chat_post"}, caps[4].Actions, "pNestedGroup")
		assert.Empty(t, caps[5].Actions, "pExpired")
		assert.Empty(t, caps[6].Actions, "pFuture")
		assert.Equal(t, []string{"read"}, caps[7].Actions, "pConstrainedReadOnly")
		assert.Equal(t, []string{"read"}, caps[8].Actions, "pGroupConstrainedRO")
		assert.Empty(t, caps[9].Actions, "pGroupConstrainedDeny")
		assert.Empty(t, caps[10].Actions, "pNoGrant")
	})

	t.Run("explain_preserves_all_inactive_grants", func(t *testing.T) {
		memoCtx := withAuthzInputMemo(ctx)
		unfilteredCtx := maskAllAuthzMemo(ctx)
		req := AuthzRequest{
			Principal: principalContextForIdentity(humanIdent),
			Resource:  allResources[0],
			Action:    ActionRead,
			Explain:   true,
		}
		got := srv.authzService.Decide(memoCtx, req)
		want := srv.authzService.Decide(unfilteredCtx, req)
		assert.Equal(t, want.Allowed, got.Allowed)
		require.NotNil(t, got.Provenance)
		require.NotNil(t, want.Provenance)
		assert.Len(t, got.Provenance.InactiveGrants, len(want.Provenance.InactiveGrants),
			"Explain=true must keep all InactiveGrants unfiltered")
	})

	t.Run("system_hub_admin", func(t *testing.T) {
		checkParity(t, ctx, userIdent(sysAdmin), false)
	})

	t.Run("view_as_member_session", func(t *testing.T) {
		vamCtx := contextWithViewAsMember(ctx, sysAdmin.ID)
		vamIdent := NewAuthenticatedUser(sysAdmin.ID, sysAdmin.Email, sysAdmin.DisplayName, store.UserRoleAdmin, string(ClientTypeWeb)).WithViewAsMember()
		checkParity(t, vamCtx, vamIdent, false)
		memoCtx := withAuthzInputMemo(vamCtx)
		caps := srv.authzService.ComputeCapabilitiesForActions(memoCtx, vamIdent, allResources, []Action{ActionRead, ActionChatPost})
		assert.Equal(t, []string{"read", "chat_post"}, caps[0].Actions, "view-as-member retains direct pMember")
		assert.Empty(t, caps[10].Actions, "view-as-member loses system-admin access to pNoGrant")
	})

	t.Run("system_scoped_access_constraint", func(t *testing.T) {
		checkParity(t, ctx, userIdent(sysConstrained), false)
	})

	t.Run("test_fixture", func(t *testing.T) {
		checkParity(t, ctx, userIdent(testFixture), false)
	})

	t.Run("test_hub_admin_enabled_disabled_expired", func(t *testing.T) {
		clamp, ok := srv.authzService.store.(*testFixtureGrantClamp)
		require.True(t, ok, "expected testFixtureGrantClamp wrapping authz store")

		origAdminTier := clamp.cache.adminTier
		t.Cleanup(func() { clamp.cache.adminTier = origAdminTier })

		clamp.cache.adminTier = func(string) bool { return true }
		checkParity(t, ctx, userIdent(testHubAdminLive), false)
		checkParity(t, ctx, userIdent(testHubAdminExpired), false)
		checkParity(t, ctx, userIdent(testSuperAdmin), false)

		clamp.cache.adminTier = func(string) bool { return false }
		checkParity(t, ctx, userIdent(testHubAdminLive), false)
		checkParity(t, ctx, userIdent(testSuperAdmin), false)
	})

	t.Run("suspended_invited_unknown_kind", func(t *testing.T) {
		checkParity(t, ctx, userIdent(suspended), false)
		checkParity(t, ctx, userIdent(invited), false)
		checkParity(t, ctx, userIdent(unknownKind), false)

		for _, u := range []*store.User{suspended, invited, unknownKind} {
			caps := srv.authzService.ComputeCapabilitiesForActions(withAuthzInputMemo(ctx), userIdent(u), allResources, []Action{ActionRead, ActionChatPost})
			for i, p := range allProjects {
				assert.Empty(t, caps[i].Actions, "%s must be denied on %s", u.Email, p.Slug)
			}
		}
	})

	t.Run("uat_scoped_tokens", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			tok        *ScopedUserIdentity
			tokenScope bool
		}{
			{"hub_rw_tokenScope_false", uatHubReadWrite, false},
			{"hub_rw_tokenScope_true", uatHubReadWrite, true},
			{"hub_ro_tokenScope_true", uatHubReadOnly, true},
			{"hub_noread_tokenScope_false", uatHubNoRead, false},
			{"proj_member_tokenScope_true", uatProjMember, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				checkParity(t, ctx, tc.tok, tc.tokenScope)
			})
		}
	})
}

// TestAuthzScopePrefilter_RegressionGuard fails if per-project authorization
// work regresses back to O(P * B) binding evaluation or per-evaluation
// sortedKeys allocation (ptone/scion#9157).
func TestAuthzScopePrefilter_RegressionGuard(t *testing.T) {
	// 1. BindingsForProject returns O(1) bindings per project, independent of
	// total project bindings (5 vs 100 projects).
	t.Run("bindings_for_project_constant_in_project_count", func(t *testing.T) {
		measure := func(numProjects int) (filteredLen int, storeCalls map[string]int64) {
			ctx := context.Background()
			srv, rawStore, _, spy := newBellCountingSQLiteServer(t)
			_, bob, _ := setupDemoPolicyOn(t, srv, srv.store)

			memberRole, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)

			var targetProjID string
			for i := 0; i < numProjects; i++ {
				p := &store.Project{
					ID:      api.NewUUID(),
					Name:    fmt.Sprintf("reg-p-%03d", i),
					Slug:    fmt.Sprintf("reg-p-%03d", i),
					Created: time.Now().UTC(),
				}
				require.NoError(t, rawStore.CreateProject(ctx, p))
				_, err = rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: bob.ID,
					ScopeType: store.RoleScopeProject, ScopeID: p.ID, CreatedBy: "test",
				})
				require.NoError(t, err)
				if i == 0 {
					targetProjID = p.ID
				}
			}

			bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, string(ClientTypeWeb))
			memoCtx := withAuthzInputMemo(ctx)

			spy.Reset(true)
			h := srv.authzService.inputsFor(memoCtx, bobIdent)
			_, err = h.Principals()
			require.NoError(t, err)
			filtered, err := h.BindingsForProject(targetProjID)
			require.NoError(t, err)
			_, err = h.RoleDefs()
			require.NoError(t, err)

			// Calling BindingsForProject across another 20 projects must issue 0 extra store calls.
			for i := 0; i < 20; i++ {
				h2 := srv.authzService.inputsFor(memoCtx, bobIdent)
				_, err = h2.Principals()
				require.NoError(t, err)
				_, err = h2.BindingsForProject(targetProjID)
				require.NoError(t, err)
				_, err = h2.RoleDefs()
				require.NoError(t, err)
			}

			snap := spy.Snapshot()
			spy.Reset(false)
			return len(filtered), snap
		}

		len5, store5 := measure(5)
		len100, store100 := measure(100)

		assert.Equal(t, len5, len100, "BindingsForProject slice length must be constant across 5 vs 100 projects")
		assert.LessOrEqual(t, len100, 5, "BindingsForProject must only return system + matching project bindings (got %d for 100 projects)", len100)
		assert.Equal(t, store5["GetEffectiveGroups"], store100["GetEffectiveGroups"])
		assert.Equal(t, store5["ListRoleBindingsForPrincipals"], store100["ListRoleBindingsForPrincipals"])
		assert.Equal(t, store5["GetRoleDefinitionsByIDs"], store100["GetRoleDefinitionsByIDs"])
		assert.LessOrEqual(t, store100["ListRoleBindingsForPrincipals"], int64(1))
	})

	// 2. evaluateBinding must not allocate/sort role.Permissions per evaluation
	// when the role was built with NewRolePermissions.
	t.Run("evaluateBinding_no_per_eval_sortedKeys_alloc", func(t *testing.T) {
		perms := make([]string, 30)
		for i := range perms {
			perms[i] = fmt.Sprintf("perm.%02d", i)
		}
		role := NewRolePermissions("r1", "member", ScopeTypeProject, perms)
		req := KernelRequest{
			Permission:       "perm.00",
			PrincipalClosure: map[string]struct{}{"user:u1": {}},
			MembershipPaths:  map[string][]string{"user:u1": {"u1"}},
			Resource:         ResourceContext{ResourceType: "project", ResourceID: "p1", ProjectID: "p1"},
			RoleDefinitions:  map[string]*RolePermissions{"r1": role},
			Now:              time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		}
		cb := &CandidateBinding{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "u1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "p1",
		}
		allocs := testing.AllocsPerRun(100, func() {
			_ = evaluateBinding(req, cb)
		})
		// Before P6, sortedKeys allocated the 30-element slice + sort scratch
		// on every binding evaluation; with precomputed sortedPerms, only
		// compositeKey string concat allocates.
		assert.LessOrEqual(t, allocs, 2.0, "evaluateBinding must not re-allocate/sort role.Permissions (got %.1f allocs)", allocs)
	})
}
