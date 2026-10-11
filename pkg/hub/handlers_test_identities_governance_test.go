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
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Constraint governance reads role bindings outside the authorization
// service. A hub test identity must never count as a constraint admin
// there: not as a surviving admin in the lockout check, and not as an
// admin for suspension review.

// tiConstraintAdminSetup seeds a system role with access_constraint.admin
// bound directly to realAdmin and to a group holding a test identity.
func tiConstraintAdminSetup(t *testing.T, s store.Store, name string) (realAdmin string, group *store.Group, fixture *store.User, adminRD *store.RoleDefinition) {
	t.Helper()
	realAdmin = pvSeedUser(t, s, name+"-real-admin")
	adminRD = createTestRoleDefinition(t, s, name+"-constraint-admin", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", realAdmin, store.RoleScopeSystem, "")
	group = tiGroupWithSystemRole(t, s, name+"-admin-group", adminRD.Name, DevUserID)
	fixture = tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	tiAddToGroup(t, s, group.ID, fixture.ID, store.GroupMemberRoleMember)
	return realAdmin, group, fixture, adminRD
}

// A constraint that removes constraint-admin from every real admin (here
// the dev super-admin and a direct constraint admin, targeted through one
// group) is a lockout, even though a test identity in an admin-granting
// group holds a (clamped) constraint-admin grant and is not targeted.
// Control: with an untargeted real admin in the admin group, the same
// change is safe.
func TestTestIdentity_ConstraintLockoutIgnoresTestIdentities(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()
	realAdmin, group, fixture, _ := tiConstraintAdminSetup(t, s, "ti-lockout")
	targets := tiGroupWithSystemRole(t, s, "ti-lockout-targets", "", DevUserID)
	tiAddToGroup(t, s, targets.ID, DevUserID, store.GroupMemberRoleMember)
	tiAddToGroup(t, s, targets.ID, realAdmin, store.GroupMemberRoleMember)

	draft := func(name string) *store.AccessConstraint {
		return &store.AccessConstraint{
			Name: name, SubjectKind: store.ConstraintSubjectGroupClosure, SubjectGroupID: pvStrPtr(targets.ID),
			ScopeType: store.RoleScopeSystem, MaximumPermissions: []string{"agent.read"},
			Purpose: "test identity lockout",
		}
	}

	admins, err := ps.resolveAdminUsers(ctx, store.RoleScopeSystem, "")
	require.NoError(t, err)
	for _, a := range admins {
		assert.NotEqual(t, fixture.ID, a.userID, "a test identity is never a resolved constraint admin")
	}

	res, err := ps.GeneratePreview(ctx, PreviewRequest{Operation: "create", Draft: draft("ti-lockout-1"), Actor: pvTestActor(realAdmin)})
	require.NoError(t, err)
	require.NotNil(t, res.Lockout.Safe)
	assert.False(t, *res.Lockout.Safe, "blocking every real admin is a lockout")
	assert.NotNil(t, res.CommitBlocked)

	// Control: an untargeted real admin in the admin group survives.
	other := pvSeedUser(t, s, "ti-lockout-real-admin-2")
	tiAddToGroup(t, s, group.ID, other, store.GroupMemberRoleMember)
	res, err = ps.GeneratePreview(ctx, PreviewRequest{Operation: "create", Draft: draft("ti-lockout-2"), Actor: pvTestActor(realAdmin)})
	require.NoError(t, err)
	require.NotNil(t, res.Lockout.Safe)
	assert.True(t, *res.Lockout.Safe, "control: an untargeted real admin survives")
}

// isConstraintAdmin (suspension review) does not treat a test identity in
// an admin-granting group as a constraint admin; a real member does.
func TestTestIdentity_NotAConstraintAdminForGovernance(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()
	_, group, fixture, _ := tiConstraintAdminSetup(t, s, "ti-gov")
	human := pvSeedUser(t, s, "ti-gov-human")
	tiAddToGroup(t, s, group.ID, human, store.GroupMemberRoleMember)

	isAdmin, err := gs.isConstraintAdmin(ctx, fixture.ID)
	require.NoError(t, err)
	assert.False(t, isAdmin)
	check, err := gs.CheckUserSuspension(ctx, fixture.ID)
	require.NoError(t, err)
	assert.False(t, check.ReviewRequired)

	isAdmin, err = gs.isConstraintAdmin(ctx, human)
	require.NoError(t, err)
	assert.True(t, isAdmin, "control: a real member of the group is a constraint admin")
}

// The admin resolution fails closed when a user's kind cannot be read.
func TestTestIdentity_ConstraintAdminResolutionFailsClosed(t *testing.T) {
	_, authz, s := previewTestSetup(t)
	ctx := context.Background()
	tiConstraintAdminSetup(t, s, "ti-failclosed-gov")
	ps := NewPreviewServiceWithKey(kindErrStore{Store: s}, authz, slog.Default(), []byte("test-preview-hmac-key-32-bytes!!"))
	_, err := ps.resolveAdminUsers(ctx, store.RoleScopeSystem, "")
	assert.Error(t, err, "an unreadable kind must not let a user count as an admin")
}

// ReconcileSuperAdminBindings' pre-scan does not count a test identity
// listed in admin_emails toward the intended admin set (pass 2 never
// promotes it), so it cannot make demoting every real admin look safe.
// Control: a real user listed in admin_emails does count.
func TestTestIdentity_ReconcileDoesNotCountTestIdentityAsIntendedAdmin(t *testing.T) {
	_, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	fixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))

	safe, err := ReconcileSuperAdminBindings(ctx, s, []string{fixture.Email}, store.UserRoleMember)
	require.NoError(t, err)
	assert.False(t, safe, "only a test identity in admin_emails: demotion must not be considered safe")
	dev, err := s.GetUser(ctx, DevUserID)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleAdmin, dev.Role, "the existing admin is not demoted")

	realID := pvSeedUser(t, s, "ti-reconcile-real")
	realRow, err := s.GetUser(ctx, realID)
	require.NoError(t, err)
	safe, err = ReconcileSuperAdminBindings(ctx, s, []string{fixture.Email, realRow.Email}, store.UserRoleMember)
	require.NoError(t, err)
	assert.True(t, safe, "control: a real user in admin_emails counts toward the intended admin set")
}

// The in-transaction hub role-binding authority check (used by the
// custom-role path of project membership) does not grant a test identity
// authority through a group with a system role carrying role_binding.*;
// a real member of the same group keeps it.
func TestTestIdentity_NoHubRoleBindingAuthorityThroughGroup(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	role := tiCustomSystemRole(t, s, "ti-rb-authority", PermRoleBindingCreate, PermRoleBindingDelete)
	group := tiGroupWithSystemRole(t, s, "ti-rb-group", role, DevUserID)
	fixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	tiAddToGroup(t, s, group.ID, fixture.ID, store.GroupMemberRoleMember)
	human := pvSeedUser(t, s, "ti-rb-human")
	tiAddToGroup(t, s, group.ID, human, store.GroupMemberRoleMember)

	ok, err := srv.membershipService.actorHasHubRoleBindingAuthorityTx(ctx, s, fixture.ID, MembershipOpAdd)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = srv.membershipService.actorHasHubRoleBindingAuthorityTx(ctx, s, human, MembershipOpAdd)
	require.NoError(t, err)
	assert.True(t, ok, "control: a real member of the group holds the authority")

	_, err = srv.membershipService.actorHasHubRoleBindingAuthorityTx(ctx, kindErrStore{Store: s}, human, MembershipOpAdd)
	assert.Error(t, err, "an unreadable kind fails closed")
}

// The preview's base permissions apply the test-identity clamp: a test
// identity in an admin-granting group does not show the group's
// system-scoped grants; a human member of the same group does.
func TestTestIdentity_PreviewBasePermissionsClamped(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()
	_, group, fixture, _ := tiConstraintAdminSetup(t, s, "ti-preview-base")
	human := pvSeedUser(t, s, "ti-preview-base-human")
	tiAddToGroup(t, s, group.ID, human, store.GroupMemberRoleMember)

	perms := func(userID string) []string {
		groups, err := s.GetEffectiveGroups(ctx, userID)
		require.NoError(t, err)
		got, err := ps.getBasePermissions(ctx, resolvedPrincipal{principalType: "user", principalID: userID, groupIDs: groups},
			store.RoleScopeSystem, "", time.Now())
		require.NoError(t, err)
		return got
	}
	assert.NotContains(t, perms(fixture.ID), PermissionConstraintAdmin, "a test identity's group-derived system grant is clamped")
	assert.Contains(t, perms(human), PermissionConstraintAdmin, "control: a human member keeps the group's grant")
}

// groupsErrStore fails group resolution.
type groupsErrStore struct{ store.Store }

func (groupsErrStore) GetEffectiveGroups(context.Context, string) ([]string, error) {
	return nil, errors.New("groups unavailable")
}

// isConstraintAdmin returns a group-resolution error instead of judging
// the user on direct bindings alone.
func TestTestIdentity_ConstraintAdminGroupErrorReturned(t *testing.T) {
	_, ps, authz, s := govTestSetup(t)
	human := pvSeedUser(t, s, "ti-gov-grouperr")
	gs := NewGovernanceService(groupsErrStore{Store: s}, ps, authz, slog.Default())
	_, err := gs.isConstraintAdmin(context.Background(), human)
	assert.Error(t, err)
	_, err = gs.CheckUserSuspension(context.Background(), human)
	assert.Error(t, err)
}
