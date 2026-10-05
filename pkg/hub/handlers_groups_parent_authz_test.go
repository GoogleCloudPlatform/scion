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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parentGroupFixture is a hub server with a parent group (created by the
// dev admin) and a spare group that can be offered as a group member.
type parentGroupFixture struct {
	srv    *Server
	store  store.Store
	parent *store.Group
	spare  *store.Group
}

func newParentGroupFixture(t *testing.T, name string) *parentGroupFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	mk := func(slug string) *store.Group {
		g := &store.Group{
			ID:        api.NewUUID(),
			Name:      slug,
			Slug:      slug,
			GroupType: store.GroupTypeExplicit,
			OwnerID:   DevUserID,
			CreatedBy: DevUserID,
		}
		require.NoError(t, s.CreateGroup(ctx, g))
		return g
	}
	return &parentGroupFixture{
		srv:    srv,
		store:  s,
		parent: mk(name + "-parent"),
		spare:  mk(name + "-spare"),
	}
}

// newSystemRoleUser creates a member user holding a custom system-scope role
// with exactly the given permissions.
func (f *parentGroupFixture) newSystemRoleUser(t *testing.T, name string, perms []string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          tid(name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, u))
	ensureHubMembership(ctx, f.store, u.ID)

	rd, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name + "-role",
		Description: "test role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

func (f *parentGroupFixture) addMember(t *testing.T, groupID, userID, role string) {
	t.Helper()
	require.NoError(t, f.store.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       role,
	}))
}

// bindHubAdminToParent gives the parent group a system-scope hub-admin
// binding, so a membership of the parent carries authority the actor lacks.
func (f *parentGroupFixture) bindHubAdminToParent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      f.parent.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func (f *parentGroupFixture) createChild(t *testing.T, actor *store.User, slug, parentID string) (int, string) {
	t.Helper()
	body := map[string]interface{}{"name": slug, "slug": slug}
	if parentID != "" {
		body["parentId"] = parentID
	}
	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/groups", body)
	return rec.Code, rec.Body.String()
}

// addSpareAsMember adds the spare group to the parent through the members
// endpoint: the reference response for the same caller and parent.
func (f *parentGroupFixture) addSpareAsMember(t *testing.T, actor *store.User) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": f.spare.ID})
	return rec.Code, rec.Body.String()
}

func apiErrorCode(t *testing.T, body string) string {
	t.Helper()
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(body), &errResp), "body: %s", body)
	return errResp.Error.Code
}

// assertNothingCreated checks that a refused create left no group, no
// creator membership and no child edge under the parent.
func (f *parentGroupFixture) assertNothingCreated(t *testing.T, actor *store.User, slug string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.GetGroupBySlug(ctx, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "refused create must not create the group")

	children, err := f.store.ListGroups(ctx, store.GroupFilter{ParentID: f.parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, children.Items, "refused create must not add a child under the parent")

	memberships, err := f.store.GetUserGroups(ctx, actor.ID)
	require.NoError(t, err)
	for _, m := range memberships {
		assert.NotEqual(t, store.GroupMemberRoleOwner, m.Role,
			"refused create must not add the caller as owner of any group (group %s)", m.GroupID)
	}
}

// assertRefusedLikeAddMember runs a create with parentId that must be refused with the same
// status and error code as adding a group member to that parent.
func (f *parentGroupFixture) assertRefusedLikeAddMember(t *testing.T, actor *store.User, slug, wantMsg string) {
	t.Helper()
	code, body := f.createChild(t, actor, slug, f.parent.ID)
	assert.Equal(t, http.StatusForbidden, code, "create with parentId: %s", body)
	assert.Contains(t, body, wantMsg)
	f.assertNothingCreated(t, actor, slug)

	refCode, refBody := f.addSpareAsMember(t, actor)
	assert.Equal(t, refCode, code, "status must match addGroupMember (ref body: %s)", refBody)
	assert.Equal(t, apiErrorCode(t, refBody), apiErrorCode(t, body), "error code must match addGroupMember")
	assert.Contains(t, refBody, wantMsg, "addGroupMember refuses for the same reason")
}

// TestCreateGroup_ParentID_RequiresAddMemberOnParent: a caller with only
// group.create cannot create a group under a parent they cannot add members
// to.
func TestCreateGroup_ParentID_RequiresAddMemberOnParent(t *testing.T) {
	f := newParentGroupFixture(t, "cg-addmember")
	actor := f.newSystemRoleUser(t, "cg-create-only", []string{"group.create"})

	f.assertRefusedLikeAddMember(t, actor, "cg-addmember-child", "")
}

// TestCreateGroup_ParentID_AppliesRoleHierarchy: a caller holding
// group.create and group.addMember through a custom role, but who is neither
// owner nor admin of the parent, is refused by the group role hierarchy.
func TestCreateGroup_ParentID_AppliesRoleHierarchy(t *testing.T) {
	f := newParentGroupFixture(t, "cg-hierarchy")
	actor := f.newSystemRoleUser(t, "cg-create-add", []string{"group.create", "group.addMember"})

	f.assertRefusedLikeAddMember(t, actor, "cg-hierarchy-child", "Only group owners or admins can add members")
}

// TestCreateGroup_ParentID_AppliesCanDelegate: the owner of the parent group
// who is not a member of it, and so lacks the authority bound to the parent,
// cannot create a group under it.
func TestCreateGroup_ParentID_AppliesCanDelegate(t *testing.T) {
	f := newParentGroupFixture(t, "cg-delegate")
	actor := f.newSystemRoleUser(t, "cg-delegate-actor", []string{"group.create"})
	f.parent.OwnerID = actor.ID
	require.NoError(t, f.store.UpdateGroup(context.Background(), f.parent))
	f.bindHubAdminToParent(t)

	f.assertRefusedLikeAddMember(t, actor, "cg-delegate-child", "Cannot grant authority you do not hold")
}

// TestCreateGroup_ParentID_AllowedForParentAdmin: a caller who can add
// members to the parent (group.addMember and admin of the parent) can create
// a group under it.
func TestCreateGroup_ParentID_AllowedForParentAdmin(t *testing.T) {
	f := newParentGroupFixture(t, "cg-allowed")
	actor := f.newSystemRoleUser(t, "cg-parent-admin", []string{"group.create", "group.addMember"})
	f.addMember(t, f.parent.ID, actor.ID, store.GroupMemberRoleAdmin)

	code, body := f.createChild(t, actor, "cg-allowed-child", f.parent.ID)
	require.Equal(t, http.StatusCreated, code, body)

	child, err := f.store.GetGroupBySlug(context.Background(), "cg-allowed-child")
	require.NoError(t, err)
	children, err := f.store.ListGroups(context.Background(), store.GroupFilter{ParentID: f.parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, children.Items, 1)
	assert.Equal(t, child.ID, children.Items[0].ID, "the new group is a child of the parent")

	// Reference: the same caller may add a group member to the parent.
	refCode, refBody := f.addSpareAsMember(t, actor)
	assert.Equal(t, http.StatusCreated, refCode, refBody)
}

// TestCreateGroup_NoParentID_Unchanged: without parentId, group.create alone
// is enough, as before.
func TestCreateGroup_NoParentID_Unchanged(t *testing.T) {
	f := newParentGroupFixture(t, "cg-noparent")
	actor := f.newSystemRoleUser(t, "cg-noparent-actor", []string{"group.create"})

	code, body := f.createChild(t, actor, "cg-noparent-group", "")
	require.Equal(t, http.StatusCreated, code, body)

	g, err := f.store.GetGroupBySlug(context.Background(), "cg-noparent-group")
	require.NoError(t, err)
	m, err := f.store.GetGroupMembership(context.Background(), g.ID, store.GroupMemberTypeUser, actor.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleOwner, m.Role)
}

// TestCreateGroup_ParentID_UnknownParent: a parentId that names no group is
// a validation error and creates nothing.
func TestCreateGroup_ParentID_UnknownParent(t *testing.T) {
	f := newParentGroupFixture(t, "cg-unknown")

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-unknown-child", "slug": "cg-unknown-child", "parentId": api.NewUUID()})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeValidationError, apiErrorCode(t, rec.Body.String()))
	_, err := f.store.GetGroupBySlug(context.Background(), "cg-unknown-child")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
