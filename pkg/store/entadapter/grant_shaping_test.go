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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Grant shaping in the store (ptone/scion#4240, admin tier A3). Every
// identity here is synthetic, in an in-memory store.

// gsEnv is a store with one admin-kind test identity, its shaping, and a
// human.
type gsEnv struct {
	cs      *CompositeStore
	admin   *store.User
	human   *store.User
	shaping store.GrantShaping
	// shaped is the context of a request the admin-kind identity makes.
	shaped context.Context
	// humanCtx is the context of a request the human makes.
	humanCtx context.Context
}

func newGSEnv(t *testing.T, kind string) gsEnv {
	t.Helper()
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	roleName := store.SystemRoleHubAdmin
	if kind == store.UserKindTestSuperAdmin {
		roleName = store.SystemRoleSuperAdmin
	}
	rd := newTestAdminSystemRole(t, cs, roleName)
	exp := time.Now().Add(30 * time.Minute)
	admin := newTestAdminUser(kind, uuid.NewString(), exp)
	_, err := cs.CreateTestAdminUser(ctx, admin, rd.ID)
	require.NoError(t, err)
	admin, err = cs.GetUser(ctx, admin.ID)
	require.NoError(t, err)
	human := &store.User{ID: uuid.NewString(), Email: "gs-" + uuid.NewString()[:8] + "@example.com", DisplayName: "h"}
	require.NoError(t, cs.CreateUser(ctx, human))
	g := store.GrantShaping{IdentityID: admin.ID, IssuanceID: *admin.IssuanceID, ExpiresAt: *admin.ExpiresAt}
	shaped := store.ContextWithGrantShaping(store.ContextWithGrantActor(ctx, admin.ID), g)
	return gsEnv{cs: cs, admin: admin, human: human, shaping: g, shaped: shaped, humanCtx: store.ContextWithGrantActor(ctx, human.ID)}
}

func gsProjectRole(t *testing.T, cs *CompositeStore, name string) string {
	t.Helper()
	rd, err := cs.GetRoleDefinitionByName(context.Background(), name, store.RoleScopeProject)
	if err == nil {
		return rd.ID
	}
	rd, err = cs.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		ID: uuid.NewString(), Name: name, ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	return rd.ID
}

func gsGroup(t *testing.T, cs *CompositeStore, fixture store.FixtureProvenance) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, cs.CreateGroup(context.Background(), &store.Group{ID: id, Name: "g", Slug: "g-" + id[:8], GroupType: store.GroupTypeExplicit, Fixture: fixture}))
	return id
}

// A role binding written for an admin-kind identity expires no later than
// the identity and carries its issuance; the same write for a human is
// unchanged; a value the caller puts on the model is never stored.
func TestGrantShaping_RoleBindingExpiryAndStamp(t *testing.T) {
	for _, kind := range []string{store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin} {
		t.Run(kind, func(t *testing.T) {
			e := newGSEnv(t, kind)
			proj := newStampTestProject(t, e.cs, store.FixtureProvenance{})
			identityExp := testAdminExpiry(*e.admin.ExpiresAt)
			for _, roleName := range []string{store.ProjectRoleOwner, store.ProjectRoleAdmin} {
				rdID := gsProjectRole(t, e.cs, roleName)
				later := identityExp.Add(time.Hour)
				earlier := identityExp.Add(-10 * time.Minute).UTC().Truncate(time.Second)
				for _, tc := range []struct {
					name      string
					requested *time.Time
					want      time.Time
				}{
					{"no expiry", nil, identityExp},
					{"later than the identity", &later, identityExp},
					{"earlier than the identity", &earlier, earlier},
				} {
					grantee := &store.User{ID: uuid.NewString(), Email: "gs-g-" + uuid.NewString()[:8] + "@example.com", DisplayName: "g"}
					require.NoError(t, e.cs.CreateUser(context.Background(), grantee))
					bogus := "client-supplied"
					b, err := e.cs.CreateRoleBinding(e.shaped, &store.RoleBinding{
						RoleDefinitionID: rdID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: grantee.ID,
						ScopeType: store.RoleScopeProject, ScopeID: proj.ID, ExpiresAt: tc.requested, CreatedBy: e.admin.ID, IssuanceID: &bogus,
					})
					require.NoError(t, err, "%s %s", roleName, tc.name)
					got, err := e.cs.GetRoleBinding(context.Background(), b.ID)
					require.NoError(t, err)
					require.NotNil(t, got.ExpiresAt, "%s %s: forced expiry", roleName, tc.name)
					assert.True(t, got.ExpiresAt.Equal(tc.want), "%s %s: stored %v want %v", roleName, tc.name, got.ExpiresAt, tc.want)
					require.NotNil(t, got.IssuanceID, "%s %s: stamped", roleName, tc.name)
					assert.Equal(t, e.shaping.IssuanceID, *got.IssuanceID, "%s %s: the issuance, not the client value", roleName, tc.name)
					assert.Equal(t, e.admin.ID, got.CreatedBy, "created_by stays the acting identity")

					// It lapses at expiry: the membership reads do not count it then.
					e.cs.RoleStore.now = func() time.Time { return got.ExpiresAt.Add(time.Second) }
					m, err := e.cs.GetProjectMembership(context.Background(), proj.ID, grantee.ID)
					assert.True(t, err != nil || m == nil, "%s %s: no membership after expiry", roleName, tc.name)
					e.cs.RoleStore.now = time.Now
				}
			}

			// Human control: no expiry forced, no stamp, a client value ignored.
			grantee := &store.User{ID: uuid.NewString(), Email: "gs-h-" + uuid.NewString()[:8] + "@example.com", DisplayName: "g"}
			require.NoError(t, e.cs.CreateUser(context.Background(), grantee))
			bogus := "client-supplied"
			b, err := e.cs.CreateRoleBinding(e.humanCtx, &store.RoleBinding{
				RoleDefinitionID: gsProjectRole(t, e.cs, store.ProjectRoleOwner), PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: grantee.ID,
				ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: e.human.ID, IssuanceID: &bogus,
			})
			require.NoError(t, err)
			got, err := e.cs.GetRoleBinding(context.Background(), b.ID)
			require.NoError(t, err)
			assert.Nil(t, got.ExpiresAt, "human: no forced expiry")
			assert.Nil(t, got.IssuanceID, "human: no stamp, the client value is not stored")
		})
	}
}

// Fail-closed: a grant write whose actor is an admin-kind identity (the
// request's grant actor, or the user the row records as its creator) is
// refused without a matching shaping. System writers (issuance,
// reconcile) are not affected.
func TestGrantShaping_UnshapedAdminActorRefused(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	proj := newStampTestProject(t, e.cs, store.FixtureProvenance{})
	rdID := gsProjectRole(t, e.cs, store.ProjectRoleAdmin)
	bind := func(ctx context.Context, createdBy string) error {
		grantee := &store.User{ID: uuid.NewString(), Email: "gs-u-" + uuid.NewString()[:8] + "@example.com", DisplayName: "g"}
		require.NoError(t, e.cs.CreateUser(context.Background(), grantee))
		_, err := e.cs.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rdID, PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID: grantee.ID, ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: createdBy})
		return err
	}
	other := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), time.Now().Add(30*time.Minute))
	_, err := e.cs.CreateTestAdminUser(ctx, other, newTestAdminSystemRole(t, e.cs, store.SystemRoleHubAdmin).ID)
	require.NoError(t, err)
	otherShaping := store.GrantShaping{IdentityID: other.ID, IssuanceID: *other.IssuanceID, ExpiresAt: *other.ExpiresAt}

	cases := []struct {
		name      string
		ctx       context.Context
		createdBy string
	}{
		{"actor on the context, no shaping", store.ContextWithGrantActor(ctx, e.admin.ID), e.human.ID},
		{"recorded creator, no context at all", ctx, e.admin.ID},
		{"shaping of another identity", store.ContextWithGrantShaping(store.ContextWithGrantActor(ctx, e.admin.ID), otherShaping), e.admin.ID},
		{"incomplete shaping", store.ContextWithGrantShaping(store.ContextWithGrantActor(ctx, e.admin.ID), store.GrantShaping{IdentityID: e.admin.ID}), e.admin.ID},
	}
	for _, tc := range cases {
		err := bind(tc.ctx, tc.createdBy)
		assert.ErrorIs(t, err, store.ErrTestAdminGrantUnshaped, tc.name)
	}

	// Each other grant table refuses the same way.
	g := gsGroup(t, e.cs, store.FixtureProvenance{})
	unshaped := store.ContextWithGrantActor(ctx, e.admin.ID)
	assert.ErrorIs(t, e.cs.AddGroupMember(unshaped, &store.GroupMember{GroupID: g, MemberType: store.GroupMemberTypeUser, MemberID: e.human.ID, Role: "member"}),
		store.ErrTestAdminGrantUnshaped, "group membership")
	_, err = e.cs.CreateRoleDefinition(unshaped, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-r-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject})
	assert.ErrorIs(t, err, store.ErrTestAdminGrantUnshaped, "role definition")
	assert.ErrorIs(t, e.cs.CreateInviteCode(ctx, &store.InviteCode{ID: uuid.NewString(), CodeHash: uuid.NewString(), CodePrefix: "scion_in", MaxUses: 1,
		ExpiresAt: time.Now().Add(time.Hour), CreatedBy: e.admin.ID}), store.ErrTestAdminGrantUnshaped, "invite code")
	_, err = e.cs.UpsertJoinToken(ctx, &store.BrokerJoinToken{BrokerID: uuid.NewString(), TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), CreatedBy: e.admin.ID})
	assert.ErrorIs(t, err, store.ErrTestAdminGrantUnshaped, "join token")

	// System writers with no admin-kind actor are unaffected: a reconcile
	// sentinel and a human actor write unshaped grants as before.
	require.NoError(t, bind(ctx, store.SystemReconcileCreatedBy), "system writer")
	require.NoError(t, bind(e.humanCtx, e.human.ID), "human writer")
}

// A super-admin binding stays refused under shaping: the shaped CreatedBy
// is the identity, never a reconciler sentinel (D10).
func TestGrantShaping_SuperAdminBindingRefused(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	superRD := newTestAdminSystemRole(t, e.cs, store.SystemRoleSuperAdmin)
	_, err := e.cs.CreateRoleBinding(e.shaped, &store.RoleBinding{RoleDefinitionID: superRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: e.admin.ID})
	assert.ErrorIs(t, err, store.ErrSuperAdminBindingRestricted)
	// Even naming a reconciler sentinel: no super-admin binding is written
	// under shaping.
	_, err = e.cs.CreateRoleBinding(e.shaped, &store.RoleBinding{RoleDefinitionID: superRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy})
	assert.ErrorIs(t, err, store.ErrSuperAdminBindingRestricted)
	bs, err := e.cs.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, e.human.ID)
	require.NoError(t, err)
	assert.Empty(t, bs)
	// Control: the reconciler itself (no admin-kind actor) still writes it.
	_, err = e.cs.CreateRoleBinding(context.Background(), &store.RoleBinding{RoleDefinitionID: superRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy})
	assert.NoError(t, err, "reconcile is unaffected")
}

// group_memberships.expires_at: a shaped membership expires with the
// identity, and every membership read of the store ignores it after
// expiry. Re-adding the member after expiry works.
func TestGrantShaping_GroupMembershipExpiry(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	parent := gsGroup(t, e.cs, store.FixtureProvenance{})
	g := gsGroup(t, e.cs, store.FixtureProvenance{})
	require.NoError(t, e.cs.AddGroupMember(ctx, &store.GroupMember{GroupID: parent, MemberType: store.GroupMemberTypeGroup, MemberID: g, Role: "member"}))
	proj := newStampTestProject(t, e.cs, store.FixtureProvenance{})
	agentID := uuid.NewString()
	require.NoError(t, e.cs.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "a-" + agentID[:8], Name: "a", ProjectID: proj.ID, Phase: "created"}))

	m := &store.GroupMember{GroupID: g, MemberType: store.GroupMemberTypeUser, MemberID: e.human.ID, Role: "owner", AddedBy: e.admin.ID}
	require.NoError(t, e.cs.AddGroupMember(e.shaped, m))
	am := &store.GroupMember{GroupID: g, MemberType: store.GroupMemberTypeAgent, MemberID: agentID, Role: "member", AddedBy: e.admin.ID}
	require.NoError(t, e.cs.AddGroupMember(e.shaped, am))
	want := testAdminExpiry(*e.admin.ExpiresAt)
	require.NotNil(t, m.ExpiresAt)
	assert.True(t, m.ExpiresAt.Equal(want))
	require.NotNil(t, m.IssuanceID)
	assert.Equal(t, e.shaping.IssuanceID, *m.IssuanceID)

	reads := func() (effective, agentEffective []string, userGroups, members, owners int, direct error) {
		effective, err := e.cs.GetEffectiveGroups(ctx, e.human.ID)
		require.NoError(t, err)
		agentEffective, err = e.cs.GetEffectiveGroupsForAgent(ctx, agentID)
		require.NoError(t, err)
		ug, err := e.cs.GetUserGroups(ctx, e.human.ID)
		require.NoError(t, err)
		gm, err := e.cs.GetGroupMembers(ctx, g)
		require.NoError(t, err)
		n := 0
		for _, x := range gm {
			if x.MemberType != store.GroupMemberTypeGroup {
				n++
			}
		}
		owners, err = e.cs.CountGroupMembersByRole(ctx, g, "owner")
		require.NoError(t, err)
		_, direct = e.cs.GetGroupMembership(ctx, g, store.GroupMemberTypeUser, e.human.ID)
		return effective, agentEffective, len(ug), n, owners, direct
	}
	eff, aEff, ug, members, owners, direct := reads()
	assert.ElementsMatch(t, []string{g, parent}, eff, "live: the group and its parent")
	assert.Contains(t, aEff, g)
	assert.Equal(t, 1, ug)
	assert.Equal(t, 2, members)
	assert.Equal(t, 1, owners)
	assert.NoError(t, direct)

	e.cs.GroupStore.now = func() time.Time { return want.Add(time.Second) }
	eff, aEff, ug, members, owners, direct = reads()
	assert.Empty(t, eff, "expired: no effective group, so the parent's roles are not reached either")
	assert.NotContains(t, aEff, g, "expired agent membership")
	assert.Zero(t, ug)
	assert.Zero(t, members)
	assert.Zero(t, owners)
	assert.ErrorIs(t, direct, store.ErrNotFound)

	// Re-adding after expiry replaces the expired row (unshaped human
	// write: no expiry).
	require.NoError(t, e.cs.AddGroupMember(e.humanCtx, &store.GroupMember{GroupID: g, MemberType: store.GroupMemberTypeUser, MemberID: e.human.ID, Role: "member", AddedBy: e.human.ID}))
	eff, _, _, _, _, direct = reads()
	assert.Contains(t, eff, g)
	assert.NoError(t, direct)
	e.cs.GroupStore.now = time.Now
}

// Changes that cannot be bounded by the identity's expiry are refused:
// nesting a group it did not create, re-roling a membership it did not
// add, changing a role it did not define. Its own rows may be changed.
func TestGrantShaping_UnboundableChangesRefused(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	own := store.FixtureProvenance{RootID: e.admin.ID, IssuerID: *e.admin.IssuedBy}
	parent := gsGroup(t, e.cs, store.FixtureProvenance{})
	humanGroup := gsGroup(t, e.cs, store.FixtureProvenance{})
	ownGroup := gsGroup(t, e.cs, own)

	assert.ErrorIs(t, e.cs.AddGroupMember(e.shaped, &store.GroupMember{GroupID: parent, MemberType: store.GroupMemberTypeGroup, MemberID: humanGroup, Role: "member"}),
		store.ErrTestAdminGrantRefused, "nesting a group without the identity's stamp")
	assert.NoError(t, e.cs.AddGroupMember(e.shaped, &store.GroupMember{GroupID: parent, MemberType: store.GroupMemberTypeGroup, MemberID: ownGroup, Role: "member"}),
		"nesting its own group")
	assert.NoError(t, e.cs.AddGroupMember(e.humanCtx, &store.GroupMember{GroupID: parent, MemberType: store.GroupMemberTypeGroup, MemberID: humanGroup, Role: "member"}),
		"human control")

	childID := uuid.NewString()
	assert.ErrorIs(t, e.cs.CreateGroup(e.shaped, &store.Group{ID: childID, Name: "c", Slug: "c-" + childID[:8], ParentID: parent, CreatedBy: e.admin.ID}),
		store.ErrTestAdminGrantRefused, "a child group without the stamp")
	childID = uuid.NewString()
	assert.NoError(t, e.cs.CreateGroup(e.shaped, &store.Group{ID: childID, Name: "c", Slug: "c-" + childID[:8], ParentID: parent, CreatedBy: e.admin.ID, Fixture: own}))

	// Membership role change.
	require.NoError(t, e.cs.AddGroupMember(e.humanCtx, &store.GroupMember{GroupID: humanGroup, MemberType: store.GroupMemberTypeUser, MemberID: e.human.ID, Role: "member", AddedBy: e.human.ID}))
	assert.ErrorIs(t, e.cs.UpdateGroupMemberRole(e.shaped, humanGroup, store.GroupMemberTypeUser, e.human.ID, "owner"), store.ErrTestAdminGrantRefused)
	other := &store.User{ID: uuid.NewString(), Email: "gs-o-" + uuid.NewString()[:8] + "@example.com", DisplayName: "o"}
	require.NoError(t, e.cs.CreateUser(ctx, other))
	require.NoError(t, e.cs.AddGroupMember(e.shaped, &store.GroupMember{GroupID: humanGroup, MemberType: store.GroupMemberTypeUser, MemberID: other.ID, Role: "member", AddedBy: e.admin.ID}))
	assert.NoError(t, e.cs.UpdateGroupMemberRole(e.shaped, humanGroup, store.GroupMemberTypeUser, other.ID, "admin"), "its own membership")
	assert.NoError(t, e.cs.UpdateGroupMemberRole(e.humanCtx, humanGroup, store.GroupMemberTypeUser, e.human.ID, "owner"), "human control")

	// Role definitions.
	humanRole, err := e.cs.CreateRoleDefinition(e.humanCtx, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-hr-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"}})
	require.NoError(t, err)
	assert.Nil(t, humanRole.IssuanceID)
	humanRole.Permissions = []string{"project.read", "project.update"}
	_, err = e.cs.UpdateRoleDefinition(e.shaped, humanRole)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "a role it did not define")
	ownRole, err := e.cs.CreateRoleDefinition(e.shaped, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-or-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"}})
	require.NoError(t, err)
	require.NotNil(t, ownRole.IssuanceID)
	assert.Equal(t, e.shaping.IssuanceID, *ownRole.IssuanceID, "a custom role carries the issuance")
	ownRole.Permissions = []string{"project.read", "project.update"}
	_, err = e.cs.UpdateRoleDefinition(e.shaped, ownRole)
	assert.NoError(t, err, "its own role")
	_, err = e.cs.UpdateRoleDefinition(e.humanCtx, humanRole)
	assert.NoError(t, err, "human control")
	_, err = e.cs.CreateRoleDefinition(e.shaped, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-sys-" + uuid.NewString()[:8], ScopeType: store.RoleScopeSystem, System: true})
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "never a system role")
	assert.ErrorIs(t, e.cs.UpdateSystemRoleDefinitionPermissions(e.shaped, humanRole.ID, []string{"project.read"}), store.ErrTestAdminGrantRefused)
}

// Invite codes and broker join tokens: expiry forced to the identity's,
// issuance stamped; a re-mint replaces the stamp without updating it.
func TestGrantShaping_InvitesAndJoinTokens(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestHubAdmin)
	ctx := context.Background()
	want := testAdminExpiry(*e.admin.ExpiresAt)

	inv := &store.InviteCode{ID: uuid.NewString(), CodeHash: uuid.NewString(), CodePrefix: "scion_in", MaxUses: 1,
		ExpiresAt: time.Now().Add(5 * 24 * time.Hour), CreatedBy: e.admin.ID}
	require.NoError(t, e.cs.CreateInviteCode(e.shaped, inv))
	got, err := e.cs.GetInviteCode(ctx, inv.ID)
	require.NoError(t, err)
	assert.True(t, got.ExpiresAt.Equal(want), "invite expiry forced: %v", got.ExpiresAt)
	require.NotNil(t, got.IssuanceID)
	assert.Equal(t, e.shaping.IssuanceID, *got.IssuanceID)
	hinv := &store.InviteCode{ID: uuid.NewString(), CodeHash: uuid.NewString(), CodePrefix: "scion_in", MaxUses: 1,
		ExpiresAt: time.Now().Add(5 * 24 * time.Hour), CreatedBy: e.human.ID}
	require.NoError(t, e.cs.CreateInviteCode(e.humanCtx, hinv))
	got, err = e.cs.GetInviteCode(ctx, hinv.ID)
	require.NoError(t, err)
	assert.Nil(t, got.IssuanceID, "human control")
	assert.True(t, got.ExpiresAt.After(want), "human control: expiry as requested")

	broker := uuid.NewString()
	_, err = e.cs.UpsertJoinToken(e.shaped, &store.BrokerJoinToken{BrokerID: broker, TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(24 * time.Hour), CreatedBy: e.admin.ID})
	require.NoError(t, err)
	jt := gsJoinToken(t, e.cs, broker)
	assert.True(t, jt.ExpiresAt.Equal(want), "join token expiry forced: %v", jt.ExpiresAt)
	require.NotNil(t, jt.IssuanceID)
	// A human re-mint replaces the row: the stamp goes.
	_, err = e.cs.UpsertJoinToken(e.humanCtx, &store.BrokerJoinToken{BrokerID: broker, TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(24 * time.Hour), CreatedBy: e.human.ID})
	require.NoError(t, err)
	jt = gsJoinToken(t, e.cs, broker)
	assert.Nil(t, jt.IssuanceID, "a human re-mint carries no stamp")
	assert.Equal(t, e.human.ID, jt.CreatedBy)
	// And a shaped re-mint over a human token stamps it.
	_, err = e.cs.UpsertJoinToken(e.shaped, &store.BrokerJoinToken{BrokerID: broker, TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(24 * time.Hour), CreatedBy: e.admin.ID})
	require.NoError(t, err)
	jt = gsJoinToken(t, e.cs, broker)
	require.NotNil(t, jt.IssuanceID)
	assert.Equal(t, e.shaping.IssuanceID, *jt.IssuanceID)
}

func gsJoinToken(t *testing.T, cs *CompositeStore, brokerID string) *store.BrokerJoinToken {
	t.Helper()
	row, err := cs.client.BrokerJoinToken.Get(context.Background(), uuid.MustParse(brokerID))
	require.NoError(t, err)
	return entJoinTokenToStore(row)
}

// ListIssuanceStampedGrants selects by the stamp only, and
// DeleteIssuanceStampedGrant deletes a row only while it carries the
// stamp; a stamped role still bound is kept.
func TestIssuanceStampedGrants_ListAndDelete(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	proj := newStampTestProject(t, e.cs, store.FixtureProvenance{})
	g := gsGroup(t, e.cs, store.FixtureProvenance{})
	require.NoError(t, e.cs.AddGroupMember(e.shaped, &store.GroupMember{GroupID: g, MemberType: store.GroupMemberTypeUser, MemberID: e.human.ID, Role: "member", AddedBy: e.admin.ID}))
	ownRole, err := e.cs.CreateRoleDefinition(e.shaped, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-l-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"}})
	require.NoError(t, err)
	// A human binds the stamped role: the role must survive the purge.
	_, err = e.cs.CreateRoleBinding(e.humanCtx, &store.RoleBinding{RoleDefinitionID: ownRole.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: e.human.ID})
	require.NoError(t, err)
	stamped, err := e.cs.CreateRoleBinding(e.shaped, &store.RoleBinding{RoleDefinitionID: gsProjectRole(t, e.cs, store.ProjectRoleAdmin), PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: e.admin.ID})
	require.NoError(t, err)
	require.NoError(t, e.cs.CreateInviteCode(e.shaped, &store.InviteCode{ID: uuid.NewString(), CodeHash: uuid.NewString(), CodePrefix: "scion_in", MaxUses: 1, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: e.admin.ID}))
	_, err = e.cs.UpsertJoinToken(e.shaped, &store.BrokerJoinToken{BrokerID: uuid.NewString(), TokenHash: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), CreatedBy: e.admin.ID})
	require.NoError(t, err)

	grants, truncated, err := e.cs.ListIssuanceStampedGrants(ctx, e.shaping.IssuanceID, 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	kinds := map[store.IssuanceGrantKind]int{}
	for _, gr := range grants {
		kinds[gr.Kind]++
	}
	assert.Equal(t, map[store.IssuanceGrantKind]int{
		store.IssuanceGrantRoleBinding: 1, store.IssuanceGrantGroupMembership: 1, store.IssuanceGrantRoleDefinition: 1,
		store.IssuanceGrantInviteCode: 1, store.IssuanceGrantBrokerJoinToken: 1,
	}, kinds, "exactly the stamped rows; the human binding is not listed")
	_, _, err = e.cs.ListIssuanceStampedGrants(ctx, "", 0)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Wrong issuance: nothing deleted.
	ok, err := e.cs.DeleteIssuanceStampedGrant(ctx, store.IssuanceStampedGrant{Kind: store.IssuanceGrantRoleBinding, ID: stamped.ID}, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, ok)
	for _, gr := range grants {
		ok, err := e.cs.DeleteIssuanceStampedGrant(ctx, gr, e.shaping.IssuanceID)
		if gr.Kind == store.IssuanceGrantRoleDefinition {
			assert.ErrorIs(t, err, store.ErrStampedRoleInUse, "a role a human binding names is kept")
			continue
		}
		require.NoError(t, err, gr.Kind)
		assert.True(t, ok, gr.Kind)
	}
	left, _, err := e.cs.ListIssuanceStampedGrants(ctx, e.shaping.IssuanceID, 0)
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, store.IssuanceGrantRoleDefinition, left[0].Kind)
	hb, err := e.cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, e.human.ID)
	require.NoError(t, err)
	assert.Len(t, hb, 1, "the human's own binding is untouched")
}

// The reaper's query selects the admin kinds only when asked.
func TestListExpiredTestFixtureUsers_IncludeAdminKinds(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestHubAdmin)
	ctx := context.Background()
	after := e.admin.ExpiresAt.Add(time.Minute)
	got, err := e.cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: after})
	require.NoError(t, err)
	assert.Empty(t, got, "default: test_fixture only")
	got, err = e.cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: after, IncludeAdminKinds: true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, e.admin.ID, got[0].ID)
}

// Review R2: a role the identity defined may change only while no role
// binding without its issuance names it. A person's binding of the role
// makes the shaped update refused (the change would outlive the identity
// through that binding, and the purge keeps the role); with only stamped
// bindings it is allowed. The stamped-grant listing marks such a role.
func TestGrantShaping_StampedRoleBoundByPersonIsFrozen(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	proj := newStampTestProject(t, e.cs, store.FixtureProvenance{})
	role, err := e.cs.CreateRoleDefinition(e.shaped, &store.RoleDefinition{ID: uuid.NewString(), Name: "gs-fr-" + uuid.NewString()[:8],
		ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"}})
	require.NoError(t, err)

	// Only a stamped binding: the update is allowed.
	other := &store.User{ID: uuid.NewString(), Email: "gs-fr-o-" + uuid.NewString()[:8] + "@example.com", DisplayName: "o"}
	require.NoError(t, e.cs.CreateUser(ctx, other))
	_, err = e.cs.CreateRoleBinding(e.shaped, &store.RoleBinding{RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: other.ID, ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: e.admin.ID})
	require.NoError(t, err)
	role.Permissions = []string{"project.read", "project.update"}
	_, err = e.cs.UpdateRoleDefinition(e.shaped, role)
	require.NoError(t, err, "only stamped bindings name the role")
	grants, _, err := e.cs.ListIssuanceStampedGrants(ctx, e.shaping.IssuanceID, 0)
	require.NoError(t, err)
	for _, g := range grants {
		if g.Kind == store.IssuanceGrantRoleDefinition {
			assert.False(t, g.BoundOutsideIssuance)
		}
	}

	// A person binds the role: the shaped update is refused and the role
	// is unchanged.
	_, err = e.cs.CreateRoleBinding(e.humanCtx, &store.RoleBinding{RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: e.human.ID, ScopeType: store.RoleScopeProject, ScopeID: proj.ID, CreatedBy: e.human.ID})
	require.NoError(t, err)
	role.Permissions = []string{"project.read", "project.update", "project.manage"}
	_, err = e.cs.UpdateRoleDefinition(e.shaped, role)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused)
	got, err := e.cs.GetRoleDefinition(ctx, role.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"project.read", "project.update"}, got.Permissions, "unchanged")
	grants, _, err = e.cs.ListIssuanceStampedGrants(ctx, e.shaping.IssuanceID, 0)
	require.NoError(t, err)
	var marked bool
	for _, g := range grants {
		if g.Kind == store.IssuanceGrantRoleDefinition && g.ID == role.ID {
			marked = g.BoundOutsideIssuance
		}
	}
	assert.True(t, marked, "the listing marks the role a person's binding names")
	// Control: the person (a human actor) may change it.
	_, err = e.cs.UpdateRoleDefinition(e.humanCtx, role)
	assert.NoError(t, err)
}

// Upstream's visitor binding window update (forward merge): under grant
// shaping an admin-kind identity changes the window only of a visitor
// binding it made, and the window still ends no later than the identity; a
// visitor binding a person made is refused. A person's update is the
// control.
func TestGrantShaping_VisitorWindowUpdate(t *testing.T) {
	e := newGSEnv(t, store.UserKindTestSuperAdmin)
	ctx := context.Background()
	noMembership := false
	_, err := e.cs.CreateRoleDefinition(ctx, &store.RoleDefinition{Name: store.ProjectRoleVisitor, ScopeType: store.RoleScopeProject,
		Permissions: []string{"agent.read"}, System: true, ConfersMembership: &noMembership})
	require.NoError(t, err)
	projectID := uuid.NewString()
	fed := func() *store.User {
		u := newFederatedUser("https://issuer.example", "v-"+uuid.NewString()[:8]+"@corp.com", store.UserStatusActive)
		require.NoError(t, e.cs.CreateFederatedUser(ctx, u))
		return u
	}
	bind := func(c context.Context, u *store.User, createdBy string) *store.RoleBinding {
		b, err := e.cs.CreateVisitorRoleBinding(c, &store.RoleBinding{PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: u.ID,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: createdBy})
		require.NoError(t, err)
		return b
	}
	later := time.Now().Add(48 * time.Hour)

	personMade := bind(e.humanCtx, fed(), e.human.ID)
	_, err = e.cs.UpdateVisitorRoleBindingWindow(e.shaped, personMade.ID, nil, &later)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "a visitor binding a person made")

	own := bind(e.shaped, fed(), e.admin.ID)
	require.NotNil(t, own.IssuanceID)
	got, err := e.cs.UpdateVisitorRoleBindingWindow(e.shaped, own.ID, nil, &later)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, got.ExpiresAt.Equal(testAdminExpiry(*e.admin.ExpiresAt)), "clamped: %v", got.ExpiresAt)
	got, err = e.cs.UpdateVisitorRoleBindingWindow(e.shaped, own.ID, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt, "no unbounded window under shaping")

	// A binding made under another admin-kind identity's issuance is
	// refused, and the row is left unchanged.
	other := newTestAdminUser(store.UserKindTestSuperAdmin, uuid.NewString(), time.Now().Add(20*time.Minute))
	_, err = e.cs.CreateTestAdminUser(ctx, other, newTestAdminSystemRole(t, e.cs, store.SystemRoleSuperAdmin).ID)
	require.NoError(t, err)
	other, err = e.cs.GetUser(ctx, other.ID)
	require.NoError(t, err)
	otherShaped := store.ContextWithGrantShaping(store.ContextWithGrantActor(ctx, other.ID),
		store.GrantShaping{IdentityID: other.ID, IssuanceID: *other.IssuanceID, ExpiresAt: *other.ExpiresAt})
	otherMade := bind(otherShaped, fed(), other.ID)
	require.NotNil(t, otherMade.IssuanceID)
	require.NotNil(t, otherMade.ExpiresAt)
	_, err = e.cs.UpdateVisitorRoleBindingWindow(e.shaped, otherMade.ID, nil, &later)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "a visitor binding of a different issuance")
	stored, err := e.cs.GetRoleBinding(ctx, otherMade.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.ExpiresAt)
	assert.True(t, stored.ExpiresAt.Equal(*otherMade.ExpiresAt), "row unchanged: %v", stored.ExpiresAt)
	assert.Equal(t, *otherMade.IssuanceID, *stored.IssuanceID, "stamp unchanged")

	got, err = e.cs.UpdateVisitorRoleBindingWindow(e.humanCtx, personMade.ID, nil, &later)
	require.NoError(t, err, "control: a person changes the window")
	require.NotNil(t, got.ExpiresAt)
	assert.WithinDuration(t, later, *got.ExpiresAt, time.Second)
}
