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
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// bellPreloadSpyStore records store method invocations while active, and
// forwards DB()/Dialect() from the underlying store so webChatStore attaches
// to the same database.
type bellPreloadSpyStore struct {
	store.Store
	dbSrc  store.Store
	mu     sync.Mutex
	active bool
	counts map[string]int64
}

func newBellPreloadSpyStore(inner, dbSrc store.Store) *bellPreloadSpyStore {
	return &bellPreloadSpyStore{Store: inner, dbSrc: dbSrc, counts: make(map[string]int64)}
}

func (c *bellPreloadSpyStore) DB() *sql.DB {
	if d, ok := c.dbSrc.(interface{ DB() *sql.DB }); ok {
		return d.DB()
	}
	return nil
}

func (c *bellPreloadSpyStore) Dialect() string {
	if d, ok := c.dbSrc.(interface{ Dialect() string }); ok {
		return d.Dialect()
	}
	return ""
}

func (c *bellPreloadSpyStore) Reset(active bool) {
	c.mu.Lock()
	c.active = active
	c.counts = make(map[string]int64)
	c.mu.Unlock()
}

func (c *bellPreloadSpyStore) inc(name string) {
	c.mu.Lock()
	if c.active {
		c.counts[name]++
	}
	c.mu.Unlock()
}

func (c *bellPreloadSpyStore) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

func (c *bellPreloadSpyStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	c.inc("GetProject")
	return c.Store.GetProject(ctx, id)
}

func (c *bellPreloadSpyStore) ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.RoleBinding, error) {
	c.inc("ListRoleBindingsForScope")
	return c.Store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
}

func (c *bellPreloadSpyStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	c.inc("ListRoleBindingsForPrincipal")
	return c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (c *bellPreloadSpyStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	c.inc("ListRoleBindingsForPrincipals")
	return c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (c *bellPreloadSpyStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	c.inc("GetRoleDefinition")
	return c.Store.GetRoleDefinition(ctx, id)
}

func (c *bellPreloadSpyStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	c.inc("GetRoleDefinitionsByIDs")
	return c.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

func (c *bellPreloadSpyStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	c.inc("GetGroupMembers")
	return c.Store.GetGroupMembers(ctx, groupID)
}

func (c *bellPreloadSpyStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	c.inc("GetEffectiveGroups")
	return c.Store.GetEffectiveGroups(ctx, userID)
}

func (c *bellPreloadSpyStore) GetEffectiveGroupsForUsers(ctx context.Context, userIDs []string) (map[string][]string, error) {
	c.inc("GetEffectiveGroupsForUsers")
	return c.Store.GetEffectiveGroupsForUsers(ctx, userIDs)
}

func (c *bellPreloadSpyStore) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	c.inc("GetParentGroups")
	return c.Store.GetParentGroups(ctx, groupID)
}

func (c *bellPreloadSpyStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.inc("GetUser")
	return c.Store.GetUser(ctx, id)
}

func (c *bellPreloadSpyStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	c.inc("GetUsersByIDs")
	return c.Store.GetUsersByIDs(ctx, ids)
}

func (c *bellPreloadSpyStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	c.inc("ListAccessConstraints")
	return c.Store.ListAccessConstraints(ctx, limit, offset)
}

// newBellCountingSQLiteServer builds a migrated in-memory SQLite hub server
// instrumented with both a SQL-level dialect.Driver counter and a store-method
// spy.
func newBellCountingSQLiteServer(t *testing.T) (*Server, *entadapter.CompositeStore, *bellQueryCountingDriver, *bellPreloadSpyStore) {
	t.Helper()
	rawStore, client, err := newTestStoreWithClient(t)
	require.NoError(t, err)
	countingDrv := &bellQueryCountingDriver{Driver: client.Driver()}
	countedStore := entadapter.NewCompositeStore(ent.NewClient(ent.Driver(countingDrv)))
	spy := newBellPreloadSpyStore(countedStore, rawStore)
	srv, _ := testServerOnMigratedStore(t, spy)
	return srv, rawStore, countingDrv, spy
}

// TestClampAllWith_CombinedEqualsSplit verifies the membership-parity
// invariant from design §3.7: clamping a single user's combined
// [user:u, group:g...] binding list once and splitting by principal type
// produces the exact same (directBindings, groupBindings) as clamping direct
// and group bindings separately, across every user kind and admin-tier state.
func TestClampAllWith_CombinedEqualsSplit(t *testing.T) {
	ctx := context.Background()
	rawStore, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	srv, _ := testServerOnMigratedStore(t, rawStore)

	hubAdminRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	hubMemberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	projMemberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	now := time.Now().UTC()
	future := now.Add(time.Hour)
	farFuture := now.Add(2 * time.Hour)
	past := now.Add(-time.Hour)
	projID := api.NewUUID()
	uid := api.NewUUID()
	gid := api.NewUUID()

	direct := []*store.RoleBinding{
		{ID: "d-proj", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeProject, ScopeID: projID, RoleDefinitionID: projMemberRD.ID},
		{ID: "d-sys-member", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubMemberRD.ID, ExpiresAt: &future},
		{ID: "d-sys-admin-ok", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubAdminRD.ID, ExpiresAt: &future},
		{ID: "d-sys-admin-noexp", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubAdminRD.ID},
		{ID: "d-sys-admin-overexp", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubAdminRD.ID, ExpiresAt: &farFuture},
	}
	group := []*store.RoleBinding{
		{ID: "g-proj", PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: gid, ScopeType: store.RoleScopeProject, ScopeID: projID, RoleDefinitionID: projMemberRD.ID},
		{ID: "g-sys-member", PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: gid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubMemberRD.ID},
		{ID: "g-sys-admin", PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: gid, ScopeType: store.RoleScopeSystem, RoleDefinitionID: hubAdminRD.ID},
	}
	combined := append(append([]*store.RoleBinding(nil), direct...), group...)

	userOnlyRefs := []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: uid}}
	combinedRefs := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: uid},
		{Type: store.RoleBindingPrincipalGroup, ID: gid},
	}

	bindingIDs := func(bs []*store.RoleBinding) []string {
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = b.ID
		}
		return out
	}

	cases := []struct {
		name      string
		user      *store.User
		tierOn    bool
		wantError bool
	}{
		{name: "human", user: &store.User{ID: uid, Kind: store.UserKindHuman}, tierOn: true},
		{name: "test_fixture", user: &store.User{ID: uid, Kind: store.UserKindTestFixture, ExpiresAt: &future}, tierOn: true},
		{name: "test_hub_admin_enabled_live", user: &store.User{ID: uid, Kind: store.UserKindTestHubAdmin, ExpiresAt: &future}, tierOn: true},
		{name: "test_hub_admin_disabled", user: &store.User{ID: uid, Kind: store.UserKindTestHubAdmin, ExpiresAt: &future}, tierOn: false},
		{name: "test_hub_admin_expired", user: &store.User{ID: uid, Kind: store.UserKindTestHubAdmin, ExpiresAt: &past}, tierOn: true},
		{name: "test_super_admin_enabled_live", user: &store.User{ID: uid, Kind: store.UserKindTestSuperAdmin, ExpiresAt: &future}, tierOn: true},
		{name: "unknown_kind", user: &store.User{ID: uid, Kind: "future_unknown_kind"}, tierOn: true, wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := &testFixtureClampCache{
				adminTier: func(string) bool { return tc.tierOn },
				now:       func() time.Time { return now },
			}
			clamp := &testFixtureGrantClamp{Store: srv.store, cache: cache}
			kindOf := func(_ context.Context, _ string) (principalKind, error) {
				return classifyUserKind(tc.user)
			}

			gotCombined, errComb := clamp.clampAllWith(ctx, combinedRefs, combined, kindOf)
			gotDirect, errDir := clamp.clampAllWith(ctx, userOnlyRefs, direct, kindOf)
			gotGroup, errGrp := clamp.clampAllWith(ctx, combinedRefs, group, kindOf)

			if tc.wantError {
				require.Error(t, errComb)
				require.Error(t, errDir)
				require.Error(t, errGrp)
				return
			}
			require.NoError(t, errComb)
			require.NoError(t, errDir)
			require.NoError(t, errGrp)

			splitDirect, splitGroup := splitUserAndGroupBindings(gotCombined)
			assert.Equal(t, bindingIDs(gotDirect), bindingIDs(splitDirect), "direct bindings must match")
			assert.Equal(t, bindingIDs(gotGroup), bindingIDs(splitGroup), "group bindings must match")
		})
	}
}

// TestExpandDecisionBellTopic_AuthzParityMatrix implements design §7 item 6:
// a mixed-batch parity matrix covering every user kind and binding shape,
// asserting that the preload path's (membership, read, chat_post) and
// expandDecisionBellTopic result match the sequential oracle for every user,
// that test identities / expired test admins / unknown kinds never affect
// other users in the batch, and that zero GetUser calls occur on the preload
// path.
func TestExpandDecisionBellTopic_AuthzParityMatrix(t *testing.T) {
	ctx := context.Background()

	runMatrix := func(t *testing.T, adminTierEnabled bool) {
		srv, rawStore, _, spy := newBellCountingSQLiteServer(t)
		alice, bob, demoProj := setupDemoPolicyOn(t, srv, rawStore)
		f := newDecisionsFixtureOn(t, srv, spy, alice, bob, demoProj)
		setDecisionsExperiment(t, f.srv, true)

		now := time.Now().UTC()
		future := now.Add(time.Hour)
		past := now.Add(-time.Hour)

		setAuthzTestAdminTier(srv.authzService, func(string) bool { return adminTierEnabled }, func() time.Time { return now })
		t.Cleanup(func() { setAuthzTestAdminTier(srv.authzService, nil, nil) })

		proj := &store.Project{
			ID:        api.NewUUID(),
			Name:      "Parity Matrix Project",
			Slug:      "parity-matrix-" + api.NewUUID()[:8],
			CreatedBy: alice.ID,
			Created:   now,
			Updated:   now,
		}
		require.NoError(t, rawStore.CreateProject(ctx, proj))

		srv.mu.RLock()
		wcs := srv.webChatStore
		srv.mu.RUnlock()
		require.NotNil(t, wcs)
		topicID := api.NewUUID()
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID:        topicID,
			ProjectID: proj.ID,
			Name:      "parity-topic",
			CreatedBy: alice.ID,
			CreatedAt: now,
		}))

		memberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		viewerRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
		require.NoError(t, err)
		visitorRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
		require.NoError(t, err)
		ownerRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		require.NoError(t, err)
		hubMemberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
		require.NoError(t, err)
		hubAdminRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		superAdminRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)

		// Groups:
		// - grpDirectMember -> project-member
		// - grpNestedParent -> project-member, contains grpNestedChild
		// - grpOwner -> project-owner (groups must never confer owner)
		// - grpViewer -> project-viewer
		// - grpConstrained -> NOT bound to project, targeted by AccessConstraint denying project.chat_post
		mkGroup := func(name string) *store.Group {
			g := &store.Group{
				ID:        api.NewUUID(),
				Name:      name,
				Slug:      name + "-" + api.NewUUID()[:8],
				GroupType: store.GroupTypeExplicit,
				Created:   now,
				Updated:   now,
			}
			require.NoError(t, rawStore.CreateGroup(ctx, g))
			return g
		}
		grpDirectMember := mkGroup("grp-direct-member")
		grpNestedParent := mkGroup("grp-nested-parent")
		grpNestedChild := mkGroup("grp-nested-child")
		grpOwner := mkGroup("grp-owner")
		grpViewer := mkGroup("grp-viewer")
		grpConstrained := mkGroup("grp-constrained")

		require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
			GroupID: grpNestedParent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: grpNestedChild.ID, Role: store.GroupMemberRoleMember, AddedAt: now,
		}))

		bind := func(pType, pID, scopeType, scopeID, rdID string, nb, exp *time.Time) {
			_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
				ID:               api.NewUUID(),
				PrincipalType:    pType,
				PrincipalID:      pID,
				RoleDefinitionID: rdID,
				ScopeType:        scopeType,
				ScopeID:          scopeID,
				NotBefore:        nb,
				ExpiresAt:        exp,
				CreatedAt:        now,
			})
			require.NoError(t, berr)
		}

		bind(store.RoleBindingPrincipalGroup, grpDirectMember.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalGroup, grpNestedParent.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)
		// Insert a legacy/bypassed group-bound project-owner row directly so we
		// exercise the defensive "groups never confer owner" rule in both paths
		// (CreateRoleBinding rejects group-bound project-owner at write time).
		_, err = rawStore.DB().ExecContext(ctx,
			`INSERT INTO role_bindings (id, principal_type, principal_id, role_definition_id, scope_type, scope_id, created_by, membership_kind, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			api.NewUUID(), store.RoleBindingPrincipalGroup, grpOwner.ID, ownerRD.ID, store.RoleScopeProject, proj.ID, "test", "member", now,
		)
		require.NoError(t, err)
		bind(store.RoleBindingPrincipalGroup, grpViewer.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// AccessConstraint targeting grpConstrained (which is NOT bound to proj):
		// caps permissions to project.read only (denying project.chat_post).
		_, err = rawStore.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name:               "deny-chat-post-constrained",
			SubjectKind:        store.ConstraintSubjectGroupClosure,
			SubjectGroupID:     &grpConstrained.ID,
			ScopeType:          store.RoleScopeProject,
			ScopeID:            proj.ID,
			MaximumPermissions: []string{ChatActionRead},
			Purpose:            "test constraint",
			CreatedBy:          alice.ID,
		})
		require.NoError(t, err)

		type matrixUser struct {
			label string
			user  *store.User
		}
		var allUsers []matrixUser

		mkUser := func(label, status, kind string, exp *time.Time) *store.User {
			u := &store.User{
				ID:          api.NewUUID(),
				Email:       fmt.Sprintf("%s-%s@example.com", label, api.NewUUID()[:6]),
				DisplayName: label,
				Role:        store.UserRoleMember,
				Status:      status,
				Kind:        kind,
				ExpiresAt:   exp,
				Created:     now,
			}
			switch kind {
			case store.UserKindFederated:
				issuer := "https://remote-hub.example.com"
				fedEmail := u.Email
				u.Email = ""
				u.Role = store.UserRoleViewer
				u.HomeIssuer = &issuer
				u.FederatedEmail = &fedEmail
				require.NoError(t, rawStore.CreateFederatedUser(ctx, u))
			case store.UserKindTestFixture:
				u.Email = fmt.Sprintf("%s-%s@%s", label, api.NewUUID()[:6], store.TestFixtureEmailDomain)
				issuedBy := alice.ID
				u.IssuedBy = &issuedBy
				require.NoError(t, rawStore.CreateTestFixtureUser(ctx, u))
			case store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin:
				domain := store.TestHubAdminEmailDomain
				rdID := hubAdminRD.ID
				if kind == store.UserKindTestSuperAdmin {
					domain = store.TestSuperAdminEmailDomain
					rdID = superAdminRD.ID
				}
				u.Email = fmt.Sprintf("%s-%s@%s", label, api.NewUUID()[:6], domain)
				u.Role = store.TestAdminRole(kind)
				issuedBy := alice.ID
				issuanceID := api.NewUUID()
				purpose := "parity matrix test"
				u.IssuedBy = &issuedBy
				u.IssuanceID = &issuanceID
				u.Purpose = &purpose
				createExp := future
				u.ExpiresAt = &createExp
				_, err := rawStore.CreateTestAdminUser(ctx, u, rdID)
				require.NoError(t, err)
				if exp != nil && exp.Before(now) {
					_, uerr := rawStore.DB().ExecContext(ctx, "UPDATE users SET expires_at = ? WHERE id = ?", *exp, u.ID)
					require.NoError(t, uerr)
					u.ExpiresAt = exp
				}
			case "future_unknown_kind":
				u.Kind = store.UserKindHuman
				require.NoError(t, rawStore.CreateUser(ctx, u))
				_, uerr := rawStore.DB().ExecContext(ctx, "UPDATE users SET kind = ? WHERE id = ?", kind, u.ID)
				require.NoError(t, uerr)
				u.Kind = kind
			default:
				require.NoError(t, rawStore.CreateUser(ctx, u))
			}
			allUsers = append(allUsers, matrixUser{label: label, user: u})
			return u
		}
		addToGroup := func(gid, uid string) {
			require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
				GroupID: gid, MemberType: store.GroupMemberTypeUser, MemberID: uid, Role: store.GroupMemberRoleMember, AddedAt: now,
			}))
		}

		// 1. Direct project-member (human)
		uDirectMember := mkUser("direct-member", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uDirectMember.ID, store.RoleScopeSystem, "", hubMemberRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalUser, uDirectMember.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 2. Direct project-viewer (human)
		uDirectViewer := mkUser("direct-viewer", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uDirectViewer.ID, store.RoleScopeSystem, "", hubMemberRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalUser, uDirectViewer.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// 3. Direct project-visitor (federated account — visitor grants bind only to federated users)
		uDirectVisitor := mkUser("direct-visitor", store.UserStatusActive, store.UserKindFederated, nil)
		_, err = rawStore.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
			ID:               api.NewUUID(),
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      uDirectVisitor.ID,
			RoleDefinitionID: visitorRD.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
			CreatedAt:        now,
		})
		require.NoError(t, err)

		// 4. Direct project-owner (human)
		uDirectOwner := mkUser("direct-owner", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uDirectOwner.ID, store.RoleScopeProject, proj.ID, ownerRD.ID, nil, nil)

		// 5. Group-bound project-member
		uGroupMember := mkUser("group-member", store.UserStatusActive, store.UserKindHuman, nil)
		addToGroup(grpDirectMember.ID, uGroupMember.ID)

		// 6. Nested-group-bound project-member
		uNestedMember := mkUser("nested-group-member", store.UserStatusActive, store.UserKindHuman, nil)
		addToGroup(grpNestedChild.ID, uNestedMember.ID)

		// 7. Group-bound project-owner (groups never confer owner)
		uGroupOwner := mkUser("group-owner", store.UserStatusActive, store.UserKindHuman, nil)
		addToGroup(grpOwner.ID, uGroupOwner.ID)

		// 8. Group-bound project-viewer
		uGroupViewer := mkUser("group-viewer", store.UserStatusActive, store.UserKindHuman, nil)
		addToGroup(grpViewer.ID, uGroupViewer.ID)

		// 9. Expired direct binding
		uExpiredBinding := mkUser("expired-binding", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uExpiredBinding.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, &past)

		// 10. Future direct binding
		uFutureBinding := mkUser("future-binding", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uFutureBinding.ID, store.RoleScopeProject, proj.ID, memberRD.ID, &future, nil)

		// 11. System hub-admin with project-viewer (can post via system hub-admin)
		uSysHubAdminViewer := mkUser("sys-hub-admin-viewer", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uSysHubAdminViewer.ID, store.RoleScopeSystem, "", hubAdminRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalUser, uSysHubAdminViewer.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// 12. Constrained via unbound group
		uConstrained := mkUser("constrained-unbound-group", store.UserStatusActive, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uConstrained.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)
		addToGroup(grpConstrained.ID, uConstrained.ID)

		// 13. test_fixture with project-member + system hub-admin (system admin clamped, project-member allows post)
		uFixtureMember := mkUser("test-fixture-member", store.UserStatusActive, store.UserKindTestFixture, &future)
		bind(store.RoleBindingPrincipalUser, uFixtureMember.ID, store.RoleScopeSystem, "", hubAdminRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalUser, uFixtureMember.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 14. test_fixture with project-viewer + system hub-admin (system admin clamped -> cannot post!)
		uFixtureViewer := mkUser("test-fixture-viewer", store.UserStatusActive, store.UserKindTestFixture, &future)
		bind(store.RoleBindingPrincipalUser, uFixtureViewer.ID, store.RoleScopeSystem, "", hubAdminRD.ID, nil, nil)
		bind(store.RoleBindingPrincipalUser, uFixtureViewer.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// 15. test_hub_admin (live) with project-viewer (CreateTestAdminUser binds system hub-admin with matching expiry)
		uTestHubAdminLive := mkUser("test-hub-admin-live", store.UserStatusActive, store.UserKindTestHubAdmin, &future)
		bind(store.RoleBindingPrincipalUser, uTestHubAdminLive.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// 16. test_hub_admin (expired) with project-member (all bindings dropped!)
		uTestHubAdminExp := mkUser("test-hub-admin-expired", store.UserStatusActive, store.UserKindTestHubAdmin, &past)
		bind(store.RoleBindingPrincipalUser, uTestHubAdminExp.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 17. test_super_admin (live) with project-viewer (CreateTestAdminUser binds system super-admin with matching expiry)
		uTestSuperAdminLive := mkUser("test-super-admin-live", store.UserStatusActive, store.UserKindTestSuperAdmin, &future)
		bind(store.RoleBindingPrincipalUser, uTestSuperAdminLive.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)

		// 18. Unknown Kind with project-member (fails closed for this user only, never for the batch)
		uUnknownKind := mkUser("unknown-kind", store.UserStatusActive, "future_unknown_kind", nil)
		bind(store.RoleBindingPrincipalUser, uUnknownKind.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 19. Suspended human with project-member
		uSuspended := mkUser("suspended", store.UserStatusSuspended, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uSuspended.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 20. Invited human with project-member
		uInvited := mkUser("invited", store.UserStatusInvited, store.UserKindHuman, nil)
		bind(store.RoleBindingPrincipalUser, uInvited.ID, store.RoleScopeProject, proj.ID, memberRD.ID, nil, nil)

		// 21-23. Shared group carrying a raw-inserted system hub-admin binding
		// plus a project-viewer binding on proj, containing a human, a
		// test_fixture, and an expired test_hub_admin together. Verifies that
		// per-user clamping over shared group bindings preserves the human's
		// system hub-admin grant while clamping the fixture to viewer-only and
		// dropping all bindings for the expired test_hub_admin without
		// cross-contaminating the batch.
		grpSharedSysAdmin := mkGroup("grp-shared-sys-admin")
		bind(store.RoleBindingPrincipalGroup, grpSharedSysAdmin.ID, store.RoleScopeProject, proj.ID, viewerRD.ID, nil, nil)
		_, err = rawStore.DB().ExecContext(ctx,
			`INSERT INTO role_bindings (id, principal_type, principal_id, role_definition_id, scope_type, scope_id, created_by, membership_kind, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			api.NewUUID(), store.RoleBindingPrincipalGroup, grpSharedSysAdmin.ID, hubAdminRD.ID, store.RoleScopeSystem, "", "test", "", now,
		)
		require.NoError(t, err)
		uSharedGrpHuman := mkUser("shared-grp-human", store.UserStatusActive, store.UserKindHuman, nil)
		uSharedGrpFixture := mkUser("shared-grp-fixture", store.UserStatusActive, store.UserKindTestFixture, &future)
		uSharedGrpExpAdmin := mkUser("shared-grp-exp-admin", store.UserStatusActive, store.UserKindTestHubAdmin, &past)
		addToGroup(grpSharedSysAdmin.ID, uSharedGrpHuman.ID)
		addToGroup(grpSharedSysAdmin.ID, uSharedGrpFixture.ID)
		addToGroup(grpSharedSysAdmin.ID, uSharedGrpExpAdmin.ID)

		// Sequential oracle for every user in the matrix (no preload, fresh ctx).
		type userVerdict struct {
			active   bool
			isMember bool
			memErr   bool
			canRead  bool
			canPost  bool
		}
		seqVerdicts := make(map[string]userVerdict, len(allUsers))
		var seqPosting []string

		for _, mu := range allUsers {
			u := mu.user
			loaded, uerr := checkUserAccount(ctx, rawStore, PrincipalContext{Kind: PrincipalKindUser, ID: u.ID})
			active := classifyUserAccount(loaded, uerr) == accountAllowed && loaded != nil
			if !active {
				seqVerdicts[u.ID] = userVerdict{active: false}
				continue
			}
			mem := srv.CheckEffectiveMembership(ctx, u.ID, proj.ID)
			viewer := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
			access := srv.chatUserAccess(ctx, viewer, proj, []string{ChatActionRead}, chatThreadWriteActions, nil, false, false)
			v := userVerdict{
				active:   true,
				isMember: mem.IsMember,
				memErr:   mem.Err != nil,
				canRead:  access.Can(ChatActionRead),
				canPost:  access.Can(ChatActionPost),
			}
			seqVerdicts[u.ID] = v
			if !v.memErr && v.isMember && v.canRead && v.canPost {
				seqPosting = append(seqPosting, u.ID)
			}
		}
		sort.Strings(seqPosting)

		// Preload path: per-user parity check + zero GetUser assertion.
		activeMap := make(map[string]*store.User)
		for _, mu := range allUsers {
			if seqVerdicts[mu.user.ID].active {
				activeMap[mu.user.ID] = mu.user
			}
		}

		spy.Reset(true)
		preloadCtx := withAuthzInputMemo(ctx)
		preloaded, err := srv.authzService.preloadUserAuthzInputs(preloadCtx, activeMap)
		require.NoError(t, err)

		for _, mu := range allUsers {
			u := mu.user
			want := seqVerdicts[u.ID]
			if !want.active {
				continue
			}
			in := preloaded[u.ID]
			require.NotNil(t, in, "preloaded entry missing for %s", mu.label)
			if in.err != nil {
				assert.True(t, want.memErr, "%s: preload errored (%v) but sequential did not", mu.label, in.err)
				continue
			}
			directB, groupB := splitUserAndGroupBindings(in.bindings)
			mem := evaluateEffectiveMembership(u.ID, proj.ID, now, directB, groupB, in.rawRoleDefs)
			viewer := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
			access := srv.chatUserAccess(preloadCtx, viewer, proj, []string{ChatActionRead}, chatThreadWriteActions, nil, false, false)

			assert.Equal(t, want.memErr, mem.Err != nil, "%s: memErr mismatch", mu.label)
			assert.Equal(t, want.isMember, mem.IsMember, "%s: isMember mismatch", mu.label)
			assert.Equal(t, want.canRead, access.Can(ChatActionRead), "%s: canRead mismatch", mu.label)
			assert.Equal(t, want.canPost, access.Can(ChatActionPost), "%s: canPost mismatch", mu.label)
		}
		preloadSnap := spy.Snapshot()
		spy.Reset(false)

		assert.Zero(t, preloadSnap["GetUser"], "preload path must issue zero GetUser calls")
		assert.Zero(t, preloadSnap["GetEffectiveGroups"], "preload path must issue zero per-user GetEffectiveGroups calls")
		assert.Equal(t, int64(1), preloadSnap["GetEffectiveGroupsForUsers"], "preload path must issue one batched GetEffectiveGroupsForUsers call")

		// Full expandDecisionBellTopic parity + zero GetUser check.
		spy.Reset(true)
		gotPosting := srv.expandDecisionBellTopic(ctx, proj.ID, topicID)
		expandSnap := spy.Snapshot()
		spy.Reset(false)

		sort.Strings(gotPosting)
		assert.Equal(t, seqPosting, gotPosting, "expandDecisionBellTopic must match sequential oracle exactly")
		assert.Zero(t, expandSnap["GetUser"], "expandDecisionBellTopic must issue zero GetUser calls")

		// Explicit sanity assertions on key security outcomes.
		postingSet := make(map[string]bool, len(gotPosting))
		for _, id := range gotPosting {
			postingSet[id] = true
		}
		assert.True(t, postingSet[uDirectMember.ID], "direct human member must post")
		assert.True(t, postingSet[uGroupMember.ID], "group human member must post")
		assert.True(t, postingSet[uNestedMember.ID], "nested group human member must post")
		assert.True(t, postingSet[uSysHubAdminViewer.ID], "system hub-admin with project-viewer must post")
		assert.True(t, postingSet[uFixtureMember.ID], "test_fixture with project-member must post")
		assert.False(t, postingSet[uFixtureViewer.ID], "test_fixture with project-viewer + clamped hub-admin must NOT post")
		assert.False(t, postingSet[uDirectViewer.ID], "direct viewer must NOT post")
		assert.False(t, postingSet[uDirectVisitor.ID], "visitor must NOT post")
		assert.False(t, postingSet[uGroupOwner.ID], "group-bound owner must NOT post")
		assert.False(t, postingSet[uConstrained.ID], "constrained via unbound group must NOT post")
		assert.False(t, postingSet[uTestHubAdminExp.ID], "expired test_hub_admin must NOT post")
		assert.False(t, postingSet[uUnknownKind.ID], "unknown Kind must fail closed")
		assert.False(t, postingSet[uSuspended.ID], "suspended user must NOT post")
		assert.False(t, postingSet[uInvited.ID], "invited user must NOT post")
		assert.Equal(t, adminTierEnabled, postingSet[uTestHubAdminLive.ID], "live test_hub_admin follows admin tier switch")
		assert.Equal(t, adminTierEnabled, postingSet[uTestSuperAdminLive.ID], "live test_super_admin follows admin tier switch")
		assert.True(t, postingSet[uSharedGrpHuman.ID], "human in shared sys-admin + viewer group must post")
		assert.False(t, postingSet[uSharedGrpFixture.ID], "test_fixture in shared sys-admin + viewer group must be clamped and NOT post")
		assert.False(t, postingSet[uSharedGrpExpAdmin.ID], "expired test_hub_admin in shared sys-admin + viewer group must NOT post")
	}

	t.Run("admin_tier_enabled", func(t *testing.T) { runMatrix(t, true) })
	t.Run("admin_tier_disabled", func(t *testing.T) { runMatrix(t, false) })
}

// TestPreloadUserAuthzInputs_SkipsPartiallyPopulatedMemoEntry verifies that
// preloadUserAuthzInputs seeds refs+bindings+roleDefs atomically only when a
// principal's memo entry is completely empty, leaving a pre-populated entry
// untouched rather than mixing a pre-existing closure with preloaded bindings.
func TestPreloadUserAuthzInputs_SkipsPartiallyPopulatedMemoEntry(t *testing.T) {
	ctx := context.Background()
	srv, rawStore, _, _ := newBellCountingSQLiteServer(t)

	u := &store.User{
		ID:          api.NewUUID(),
		Email:       "partial-memo@example.com",
		DisplayName: "Partial Memo",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindHuman,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, rawStore.CreateUser(ctx, u))

	memoCtx := withAuthzInputMemo(ctx)
	memo := authzInputMemoFromContext(memoCtx)
	require.NotNil(t, memo)

	sentinelRefs := []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: u.ID}}
	entry := memo.entryFor(principalKey{normType: store.RoleBindingPrincipalUser, id: u.ID})
	memo.mu.Lock()
	entry.refs = sentinelRefs
	entry.refsOK = true
	memo.mu.Unlock()

	_, err := srv.authzService.preloadUserAuthzInputs(memoCtx, map[string]*store.User{u.ID: u})
	require.NoError(t, err)

	memo.mu.Lock()
	defer memo.mu.Unlock()
	assert.True(t, entry.refsOK)
	assert.False(t, entry.bindingsOK, "partially populated memo entry must not have bindings seeded")
	assert.False(t, entry.roleDefsOK, "partially populated memo entry must not have roleDefs seeded")
}

// TestExpandDecisionBellTopic_QueryCountConstantInMembers implements design §7
// item 4: asserts that SQL query count for topic expansion at 50 members equals
// that at 300 members (for both direct and group-bound topologies), stays
// within the <= 20 + 3*G acceptance bar, and issues zero GetUser calls.
func TestExpandDecisionBellTopic_QueryCountConstantInMembers(t *testing.T) {
	ctx := context.Background()

	measure := func(t *testing.T, direct, grouped int) (int64, int, map[string]int64) {
		t.Helper()
		srv, rawStore, drv, spy := newBellCountingSQLiteServer(t)
		alice, bob, demoProj := setupDemoPolicyOn(t, srv, rawStore)
		f := newDecisionsFixtureOn(t, srv, spy, alice, bob, demoProj)
		setDecisionsExperiment(t, f.srv, true)

		now := time.Now().UTC()
		proj := &store.Project{
			ID:        api.NewUUID(),
			Name:      fmt.Sprintf("QCount d%d-g%d", direct, grouped),
			Slug:      fmt.Sprintf("qcount-d%d-g%d-%s", direct, grouped, api.NewUUID()[:6]),
			CreatedBy: alice.ID,
			Created:   now,
			Updated:   now,
		}
		require.NoError(t, rawStore.CreateProject(ctx, proj))

		srv.mu.RLock()
		wcs := srv.webChatStore
		srv.mu.RUnlock()
		topicID := api.NewUUID()
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID:        topicID,
			ProjectID: proj.ID,
			Name:      "general",
			CreatedBy: alice.ID,
			CreatedAt: now,
		}))

		memberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		hubMemberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
		require.NoError(t, err)

		mkUser := func(prefix string, i int) *store.User {
			u := &store.User{
				ID:          api.NewUUID(),
				Email:       fmt.Sprintf("%s-%d-%s@example.com", prefix, i, api.NewUUID()[:6]),
				DisplayName: fmt.Sprintf("%s %d", prefix, i),
				Role:        store.UserRoleMember,
				Status:      store.UserStatusActive,
				Kind:        store.UserKindHuman,
				Created:     now,
			}
			require.NoError(t, rawStore.CreateUser(ctx, u))
			_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
				ID:               api.NewUUID(),
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      u.ID,
				RoleDefinitionID: hubMemberRD.ID,
				ScopeType:        store.RoleScopeSystem,
				CreatedAt:        now,
			})
			require.NoError(t, berr)
			return u
		}

		for i := 0; i < direct; i++ {
			u := mkUser("dir", i)
			_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
				ID:               api.NewUUID(),
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      u.ID,
				RoleDefinitionID: memberRD.ID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          proj.ID,
				CreatedAt:        now,
			})
			require.NoError(t, berr)
		}

		if grouped > 0 {
			parentGrp := &store.Group{ID: api.NewUUID(), Name: "Parent", Slug: "p-" + api.NewUUID()[:8], GroupType: store.GroupTypeExplicit, Created: now, Updated: now}
			childGrp := &store.Group{ID: api.NewUUID(), Name: "Child", Slug: "c-" + api.NewUUID()[:8], GroupType: store.GroupTypeExplicit, Created: now, Updated: now}
			require.NoError(t, rawStore.CreateGroup(ctx, parentGrp))
			require.NoError(t, rawStore.CreateGroup(ctx, childGrp))
			require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
				GroupID: parentGrp.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childGrp.ID, Role: store.GroupMemberRoleMember, AddedAt: now,
			}))
			_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
				ID:               api.NewUUID(),
				PrincipalType:    store.RoleBindingPrincipalGroup,
				PrincipalID:      parentGrp.ID,
				RoleDefinitionID: memberRD.ID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          proj.ID,
				CreatedAt:        now,
			})
			require.NoError(t, berr)
			for i := 0; i < grouped; i++ {
				u := mkUser("grp", i)
				target := parentGrp.ID
				if i%2 == 1 {
					target = childGrp.ID
				}
				require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
					GroupID: target, MemberType: store.GroupMemberTypeUser, MemberID: u.ID, Role: store.GroupMemberRoleMember, AddedAt: now,
				}))
			}
		}

		drv.Reset(true)
		spy.Reset(true)
		got := srv.expandDecisionBellTopic(ctx, proj.ID, topicID)
		sqlCount := drv.Count()
		snap := spy.Snapshot()
		drv.Reset(false)
		spy.Reset(false)
		return sqlCount, len(got), snap
	}

	t.Run("direct_50_vs_300", func(t *testing.T) {
		q50, n50, s50 := measure(t, 50, 0)
		q300, n300, s300 := measure(t, 300, 0)
		t.Logf("direct: 50 members -> %d ent SQL queries (%d posting); 300 members -> %d ent SQL queries (%d posting)", q50, n50, q300, n300)
		assert.Equal(t, 50, n50)
		assert.Equal(t, 300, n300)
		assert.Equal(t, q50, q300, "SQL query count must be constant between 50 and 300 direct members")
		// +1 for wcs.GetTopic outside ent driver.
		assert.LessOrEqual(t, q300+1, int64(20), "direct expansion must stay <= 20 SQL queries")
		assert.Zero(t, s50["GetUser"])
		assert.Zero(t, s300["GetUser"])
		assert.Zero(t, s300["GetEffectiveGroups"])
	})

	t.Run("grouped_50_vs_300", func(t *testing.T) {
		q50, n50, s50 := measure(t, 0, 50)
		q300, n300, s300 := measure(t, 0, 300)
		t.Logf("grouped (G=2): 50 members -> %d ent SQL queries (%d posting); 300 members -> %d ent SQL queries (%d posting)", q50, n50, q300, n300)
		assert.Equal(t, 50, n50)
		assert.Equal(t, 300, n300)
		assert.Equal(t, q50, q300, "SQL query count must be constant between 50 and 300 group-bound members")
		// G=2 bound groups -> <= 20 + 3*2 = 26 SQL queries (+1 for wcs.GetTopic).
		assert.LessOrEqual(t, q300+1, int64(20+3*2), "grouped expansion must stay <= 20 + 3*G SQL queries")
		assert.Zero(t, s50["GetUser"])
		assert.Zero(t, s300["GetUser"])
		assert.Zero(t, s300["GetEffectiveGroups"])
	})
}

// TestDecisionsBellTopicExpansionScale is the opt-in scale benchmark for
// ptone/scion#8143 (promoted from the investigation benchmark). Set
// SCION_DECISIONS_SCALE=1 to run; runs against in-memory SQLite by default and
// against Postgres when SCION_TEST_POSTGRES_DSN is set.
func TestDecisionsBellTopicExpansionScale(t *testing.T) {
	if os.Getenv("SCION_DECISIONS_SCALE") != "1" {
		t.Skip("set SCION_DECISIONS_SCALE=1 to run the decisions bell topic-expansion scale benchmark")
	}

	ctx := context.Background()
	pgDSN := os.Getenv("SCION_TEST_POSTGRES_DSN")

	type scenario struct {
		name      string
		direct    int
		grouped   int
		viewers   int
		suspended int
	}
	scenarios := []scenario{
		{name: "direct_50", direct: 50},
		{name: "direct_200", direct: 200},
		{name: "direct_500", direct: 500},
		{name: "grouped_500", grouped: 500},
		{name: "mixed_500", direct: 300, grouped: 160, viewers: 25, suspended: 15},
	}

	newScaleServer := func(t *testing.T) (*Server, *entadapter.CompositeStore, *bellQueryCountingDriver, *bellPreloadSpyStore, string) {
		t.Helper()
		if pgDSN == "" {
			srv, rawStore, drv, spy := newBellCountingSQLiteServer(t)
			return srv, rawStore, drv, spy, "sqlite"
		}
		schema := fmt.Sprintf("bell_scale_%d_%s", time.Now().UnixNano(), uuid.NewString()[:8])
		cfg, err := pgx.ParseConfig(pgDSN)
		require.NoError(t, err)
		cfg.RuntimeParams["search_path"] = schema
		db := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { _ = db.Close() })

		_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })

		baseDrv := entsql.OpenDB(dialect.Postgres, db)
		baseClient := ent.NewClient(ent.Driver(baseDrv))
		require.NoError(t, entc.AutoMigrate(ctx, baseClient))
		rawStore := entadapter.NewCompositeStore(baseClient)

		countingDrv := &bellQueryCountingDriver{Driver: baseDrv}
		countedStore := entadapter.NewCompositeStore(ent.NewClient(ent.Driver(countingDrv)))
		spy := newBellPreloadSpyStore(countedStore, rawStore)
		srv, _ := testServerOnMigratedStore(t, spy)
		return srv, rawStore, countingDrv, spy, "postgres"
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			srv, rawStore, drv, spy, backend := newScaleServer(t)
			alice, bob, demoProj := setupDemoPolicyOn(t, srv, rawStore)
			f := newDecisionsFixtureOn(t, srv, spy, alice, bob, demoProj)
			setDecisionsExperiment(t, f.srv, true)

			now := time.Now().UTC()
			proj := &store.Project{
				ID:        api.NewUUID(),
				Name:      "Scale " + sc.name,
				Slug:      "scale-" + sc.name + "-" + api.NewUUID()[:6],
				CreatedBy: alice.ID,
				Created:   now,
				Updated:   now,
			}
			require.NoError(t, rawStore.CreateProject(ctx, proj))

			srv.mu.RLock()
			wcs := srv.webChatStore
			srv.mu.RUnlock()
			require.NotNil(t, wcs)
			topicID := api.NewUUID()
			require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
				ID:        topicID,
				ProjectID: proj.ID,
				Name:      "general",
				CreatedBy: alice.ID,
				CreatedAt: now,
			}))

			memberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)
			viewerRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
			require.NoError(t, err)
			hubMemberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
			require.NoError(t, err)

			createUser := func(prefix string, i int, status string) *store.User {
				u := &store.User{
					ID:          api.NewUUID(),
					Email:       fmt.Sprintf("%s-%s-%d-%s@example.com", sc.name, prefix, i, api.NewUUID()[:6]),
					DisplayName: fmt.Sprintf("%s %d", prefix, i),
					Role:        store.UserRoleMember,
					Status:      status,
					Kind:        store.UserKindHuman,
					Created:     now,
				}
				require.NoError(t, rawStore.CreateUser(ctx, u))
				_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
					ID:               api.NewUUID(),
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      u.ID,
					RoleDefinitionID: hubMemberRD.ID,
					ScopeType:        store.RoleScopeSystem,
					CreatedAt:        now,
				})
				require.NoError(t, berr)
				return u
			}
			bindUser := func(uid, rdID string) {
				_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
					ID:               api.NewUUID(),
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      uid,
					RoleDefinitionID: rdID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          proj.ID,
					CreatedAt:        now,
				})
				require.NoError(t, berr)
			}

			for i := 0; i < sc.direct; i++ {
				bindUser(createUser("direct", i, store.UserStatusActive).ID, memberRD.ID)
			}
			for i := 0; i < sc.viewers; i++ {
				bindUser(createUser("viewer", i, store.UserStatusActive).ID, viewerRD.ID)
			}
			for i := 0; i < sc.suspended; i++ {
				bindUser(createUser("susp", i, store.UserStatusSuspended).ID, memberRD.ID)
			}
			if sc.grouped > 0 {
				parentGrp := &store.Group{ID: api.NewUUID(), Name: "Parent " + sc.name, Slug: "parent-" + sc.name + "-" + api.NewUUID()[:6], GroupType: store.GroupTypeExplicit, Created: now, Updated: now}
				childGrp := &store.Group{ID: api.NewUUID(), Name: "Child " + sc.name, Slug: "child-" + sc.name + "-" + api.NewUUID()[:6], GroupType: store.GroupTypeExplicit, Created: now, Updated: now}
				require.NoError(t, rawStore.CreateGroup(ctx, parentGrp))
				require.NoError(t, rawStore.CreateGroup(ctx, childGrp))
				require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
					GroupID: parentGrp.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childGrp.ID, Role: store.GroupMemberRoleMember, AddedAt: now,
				}))
				_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
					ID:               api.NewUUID(),
					PrincipalType:    store.RoleBindingPrincipalGroup,
					PrincipalID:      parentGrp.ID,
					RoleDefinitionID: memberRD.ID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          proj.ID,
					CreatedAt:        now,
				})
				require.NoError(t, berr)
				for i := 0; i < sc.grouped; i++ {
					u := createUser("grp", i, store.UserStatusActive)
					target := parentGrp.ID
					if i%2 == 1 {
						target = childGrp.ID
					}
					require.NoError(t, rawStore.AddGroupMember(ctx, &store.GroupMember{
						GroupID: target, MemberType: store.GroupMemberTypeUser, MemberID: u.ID, Role: store.GroupMemberRoleMember, AddedAt: now,
					}))
				}
			}

			// Cold pass.
			drv.Reset(true)
			spy.Reset(true)
			t0 := time.Now()
			coldRes := srv.expandDecisionBellTopic(ctx, proj.ID, topicID)
			coldDur := time.Since(t0)
			coldSQL := drv.Count() + 1 // +1 for wcs.GetTopic
			coldSnap := spy.Snapshot()
			drv.Reset(false)
			spy.Reset(false)

			// 5 warm passes (median).
			warmDurs := make([]time.Duration, 0, 5)
			var warmSQL int64
			for iter := 0; iter < 5; iter++ {
				drv.Reset(true)
				t1 := time.Now()
				warmRes := srv.expandDecisionBellTopic(ctx, proj.ID, topicID)
				dur := time.Since(t1)
				if iter == 0 {
					warmSQL = drv.Count() + 1
				}
				drv.Reset(false)
				require.Len(t, warmRes, len(coldRes))
				warmDurs = append(warmDurs, dur)
			}
			sort.Slice(warmDurs, func(i, j int) bool { return warmDurs[i] < warmDurs[j] })

			t.Logf("[%s/%s] returned=%d cold=%v warm_median=%v (min=%v max=%v) sql_cold=%d sql_warm=%d store_calls=%v",
				backend, sc.name, len(coldRes),
				coldDur.Round(time.Microsecond),
				warmDurs[len(warmDurs)/2].Round(time.Microsecond),
				warmDurs[0].Round(time.Microsecond),
				warmDurs[len(warmDurs)-1].Round(time.Microsecond),
				coldSQL, warmSQL, coldSnap)
		})
	}
}
