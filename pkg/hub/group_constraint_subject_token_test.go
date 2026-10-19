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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing a member from, or deleting, a group that is the subject of an
// access constraint is session only. The caller of these tests holds
// access_constraint.admin and every group permission at system scope, and
// owns each group.

// constraintSubjectTokenScopes are the scopes of the pin token.
// TODO: add access_constraint:admin once that selector exists.
var constraintSubjectTokenScopes = []string{"group:removeMember", "group:delete"}

// newConstraintSubjectFixture is the group token fixture whose caller also
// holds access_constraint.admin through a system-scoped custom role.
func newConstraintSubjectFixture(t *testing.T, prefix string) *groupTokenFixture {
	t.Helper()
	f := newGroupTokenFixture(t, prefix)
	ctx := context.Background()
	role, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: prefix + "-constraint-admin", Description: "access constraint administration", ScopeType: store.RoleScopeSystem,
		Permissions: []string{PermissionConstraintAdmin},
	})
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.callerID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	return f
}

// constraintSubjectGroup creates a group owned by the caller with one user
// member and returns both. When subject is true the group is the subject
// of an access constraint.
func constraintSubjectGroup(t *testing.T, f *groupTokenFixture, name string, subject bool) (*store.Group, string) {
	t.Helper()
	g := f.group(t, name)
	m := f.user(t, name+"-member")
	require.NoError(t, f.s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: m, Role: store.GroupMemberRoleMember,
	}))
	if subject {
		createGroupClosureConstraint(t, f.s, g.ID)
	}
	return g, m
}

func (f *groupTokenFixture) groupExists(t *testing.T, g *store.Group) bool {
	t.Helper()
	_, err := f.s.GetGroup(context.Background(), g.ID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

// TestConstraintSubjectGroupToken_MemberRemoveSessionOnly pins that a user
// access token carrying group:removeMember is refused with the session-only
// reason when it removes a member from a group that is the subject of an
// access constraint, and that the membership stays.
func TestConstraintSubjectGroupToken_MemberRemoveSessionOnly(t *testing.T) {
	f := newConstraintSubjectFixture(t, "csrm")
	g, m := constraintSubjectGroup(t, f, "csrm-group", true)

	rec := doRequestWithUAT(t, f.srv, f.mint(t, hubBoundary(), constraintSubjectTokenScopes...),
		http.MethodDelete, "/api/v1/groups/"+g.ID+"/members/user/"+m, nil)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "token member remove on a constraint subject group")
	assert.True(t, f.isMember(t, g, store.GroupMemberTypeUser, m), "the membership stays")
}

// TestConstraintSubjectGroupToken_DeleteSessionOnly pins that a user access
// token carrying group:delete is refused with the session-only reason when
// it deletes a group that is the subject of an access constraint, and that
// the group stays.
func TestConstraintSubjectGroupToken_DeleteSessionOnly(t *testing.T) {
	f := newConstraintSubjectFixture(t, "csdel")
	g, _ := constraintSubjectGroup(t, f, "csdel-group", true)

	rec := doRequestWithUAT(t, f.srv, f.mint(t, hubBoundary(), constraintSubjectTokenScopes...),
		http.MethodDelete, "/api/v1/groups/"+g.ID, nil)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "token delete of a constraint subject group")
	assert.True(t, f.groupExists(t, g), "the group stays")
}

// TestConstraintSubjectGroupSession_MemberRemoveAndDeleteAdmitted pins that
// an interactive session of the same caller removes a member from, and
// deletes, a group that is the subject of an access constraint.
func TestConstraintSubjectGroupSession_MemberRemoveAndDeleteAdmitted(t *testing.T) {
	f := newConstraintSubjectFixture(t, "cssess")
	g, m := constraintSubjectGroup(t, f, "cssess-group", true)

	code := f.session(t, http.MethodDelete, "/api/v1/groups/"+g.ID+"/members/user/"+m, nil)
	assert.Equal(t, http.StatusNoContent, code, "a session removes the member")
	assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, m), "the member is removed")

	code = f.session(t, http.MethodDelete, "/api/v1/groups/"+g.ID, nil)
	assert.Equal(t, http.StatusNoContent, code, "a session deletes the group")
	assert.False(t, f.groupExists(t, g), "the group is deleted")
}

// TestGroupToken_NonConstraintSubjectMemberRemoveAndDeleteAdmitted pins
// that the same token removes a member from, and deletes, a group that is
// not the subject of an access constraint.
func TestGroupToken_NonConstraintSubjectMemberRemoveAndDeleteAdmitted(t *testing.T) {
	f := newConstraintSubjectFixture(t, "csnon")
	g, m := constraintSubjectGroup(t, f, "csnon-group", false)
	key := f.mint(t, hubBoundary(), constraintSubjectTokenScopes...)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodDelete, "/api/v1/groups/"+g.ID+"/members/user/"+m, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "token member remove: %s", rec.Body.String())
	assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, m), "the member is removed")

	rec = doRequestWithUAT(t, f.srv, key, http.MethodDelete, "/api/v1/groups/"+g.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "token delete: %s", rec.Body.String())
	assert.False(t, f.groupExists(t, g), "the group is deleted")
}
