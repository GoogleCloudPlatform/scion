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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// grantAudits returns the mutation audit records of mutationType for
// targetID.
func grantAudits(t *testing.T, s store.Store, mutationType, targetID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID == targetID {
			out = append(out, r)
		}
	}
	return out
}

// roleBindingFixture is a server, a custom system role and a user to bind.
func roleBindingFixture(t *testing.T, name string) (*Server, store.Store, *store.RoleDefinition, string) {
	t.Helper()
	srv, s := testServer(t)
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name: "rb-audit-" + name, ScopeType: store.RoleScopeSystem, Permissions: []string{"project.read"},
	})
	userID := tid("rb-audit-user-" + name)
	seedRolesTestUser(t, s, userID, "rb-audit-"+name+"@test.com")
	return srv, s, role, userID
}

func userRoleBindings(t *testing.T, s store.Store, userID, roleID string) []*store.RoleBinding {
	t.Helper()
	all, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range all {
		if b.RoleDefinitionID == roleID {
			out = append(out, b)
		}
	}
	return out
}

// Role-binding create writes exactly one audit record in the same
// transaction; an injected audit failure leaves no binding.
func TestRoleBindingAuditFailureRollsBack(t *testing.T) {
	srv, real, role, userID := roleBindingFixture(t, "create")
	req := createRoleBindingRequest{RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID, ScopeType: store.RoleScopeSystem}

	srv.store = &createTxFaultStore{Store: real, auditErrFor: "role_binding_create"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Empty(t, userRoleBindings(t, real, userID, role.ID), "no binding after the audit failure")

	srv.store = real
	rb := createBindingViaAPI(t, srv, req)
	audits := grantAudits(t, real, "role_binding_create", rb.ID)
	require.Len(t, audits, 1, "exactly one audit record")
	assert.Equal(t, "allow", audits[0].CanDelegateResult)
	assert.NotEmpty(t, audits[0].ActorPrincipalID)
	assert.Contains(t, audits[0].AfterSummary, userID)
}

// Role-binding delete writes exactly one audit record in the same
// transaction; an injected audit failure leaves the binding in place.
func TestRoleBindingDeleteAuditFailureRollsBack(t *testing.T) {
	srv, real, role, userID := roleBindingFixture(t, "delete")
	rb := createBindingViaAPI(t, srv, createRoleBindingRequest{RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID, ScopeType: store.RoleScopeSystem})

	srv.store = &createTxFaultStore{Store: real, auditErrFor: "role_binding_delete"}
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+rb.ID, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err := real.GetRoleBinding(context.Background(), rb.ID)
	require.NoError(t, err, "binding kept after the audit failure")
	assert.Empty(t, grantAudits(t, real, "role_binding_delete", rb.ID))

	srv.store = real
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+rb.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = real.GetRoleBinding(context.Background(), rb.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	audits := grantAudits(t, real, "role_binding_delete", rb.ID)
	require.Len(t, audits, 1, "exactly one audit record")
	assert.Contains(t, audits[0].BeforeSummary, userID)
}

// Group-member add writes exactly one audit record in the same
// transaction; an injected audit failure leaves no membership.
func TestGroupMemberAuditFailureRollsBack(t *testing.T) {
	srv, real := testServer(t)
	ctx := context.Background()
	group := &store.Group{ID: tid("gm-audit-group"), Name: "gm audit", Slug: "gm-audit", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, group))
	userID := tid("gm-audit-user")
	seedRolesTestUser(t, real, userID, "gm-audit@test.com")
	req := AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: store.GroupMemberRoleMember}
	path := "/api/v1/groups/" + group.ID + "/members"

	srv.store = &createTxFaultStore{Store: real, auditErrFor: "group_member_add"}
	rec := doRequest(t, srv, http.MethodPost, path, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	members, err := real.GetGroupMembers(ctx, group.ID)
	require.NoError(t, err)
	for _, m := range members {
		assert.NotEqual(t, userID, m.MemberID, "no membership after the audit failure")
	}
	assert.Empty(t, grantAudits(t, real, "group_member_add", group.ID))

	srv.store = real
	rec = doRequest(t, srv, http.MethodPost, path, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got GroupMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, userID, got.MemberID)
	audits := grantAudits(t, real, "group_member_add", group.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Contains(t, audits[0].AfterSummary, userID)
	assert.NotEmpty(t, audits[0].ActorPrincipalID)
}

// Group create with a parent writes the parent membership's audit record
// in the same transaction; an injected audit failure leaves no group and
// no child edge under the parent.
func TestCreateGroupWithParentAuditFailureRollsBack(t *testing.T) {
	srv, real := testServer(t)
	ctx := context.Background()
	parent := &store.Group{ID: tid("cg-audit-parent"), Name: "cg audit parent", Slug: "cg-audit-parent", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, parent))
	req := CreateGroupRequest{Name: "cg audit child", Slug: "cg-audit-child", ParentID: parent.ID}

	srv.store = &createTxFaultStore{Store: real, auditErrFor: "group_member_add"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err := real.GetGroupBySlug(ctx, req.Slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no group after the audit failure")
	children, err := real.ListGroups(ctx, store.GroupFilter{ParentID: parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, children.Items, "no child edge after the audit failure")
	assert.Empty(t, grantAudits(t, real, "group_member_add", parent.ID))

	srv.store = real
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var child store.Group
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&child))
	audits := grantAudits(t, real, "group_member_add", parent.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Contains(t, audits[0].AfterSummary, child.ID)
	assert.NotEmpty(t, audits[0].ActorPrincipalID)
}

// Group-member removal writes exactly one audit record in the same
// transaction; an injected audit failure leaves the membership in place.
func TestGroupMemberRemoveAuditFailureRollsBack(t *testing.T) {
	srv, real := testServer(t)
	ctx := context.Background()
	group := &store.Group{ID: tid("gm-rm-audit-group"), Name: "gm rm audit", Slug: "gm-rm-audit", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, group))
	userID := tid("gm-rm-audit-user")
	seedRolesTestUser(t, real, userID, "gm-rm-audit@test.com")
	require.NoError(t, real.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: store.GroupMemberRoleMember,
	}))
	path := "/api/v1/groups/" + group.ID + "/members/" + store.GroupMemberTypeUser + "/" + userID

	srv.store = &createTxFaultStore{Store: real, auditErrFor: "group_member_remove"}
	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err := real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	require.NoError(t, err, "membership kept after the audit failure")
	assert.Empty(t, grantAudits(t, real, "group_member_remove", group.ID))

	srv.store = real
	rec = doRequest(t, srv, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	audits := grantAudits(t, real, "group_member_remove", group.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Contains(t, audits[0].BeforeSummary, userID)
	assert.NotEmpty(t, audits[0].ActorPrincipalID)
}

// groupOwnerAddFaultStore fails every owner-role group membership add,
// inside and outside transactions.
type groupOwnerAddFaultStore struct {
	store.Store
}

func (s *groupOwnerAddFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(&groupOwnerAddFaultStore{Store: tx}) })
}

func (s *groupOwnerAddFaultStore) AddGroupMember(ctx context.Context, m *store.GroupMember) error {
	if m.Role == store.GroupMemberRoleOwner {
		return errors.New("injected group owner add fault")
	}
	return s.Store.AddGroupMember(ctx, m)
}

// Group create adds the creator's owner membership and its audit record in
// the same transaction as the group; a failure of either write leaves no
// group.
func TestCreateGroupCreatorOwnerFailureRollsBack(t *testing.T) {
	srv, real := testServer(t)
	ctx := context.Background()
	req := CreateGroupRequest{Name: "cg owner audit", Slug: "cg-owner-audit"}

	for name, faulty := range map[string]store.Store{
		"owner add fails":   &groupOwnerAddFaultStore{Store: real},
		"owner audit fails": &createTxFaultStore{Store: real, auditErrFor: "group_member_add"},
	} {
		t.Run(name, func(t *testing.T) {
			srv.store = faulty
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			_, err := real.GetGroupBySlug(ctx, req.Slug)
			assert.ErrorIs(t, err, store.ErrNotFound, "no group after the failure")
		})
	}

	srv.store = real
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var group store.Group
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&group))
	owner, err := real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, DevUserID)
	require.NoError(t, err, "the creator is an owner of the new group")
	assert.Equal(t, store.GroupMemberRoleOwner, owner.Role)
	audits := grantAudits(t, real, "group_member_add", group.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Contains(t, audits[0].AfterSummary, DevUserID)
	assert.Contains(t, audits[0].AfterSummary, `"role":"owner"`)
	assert.Equal(t, DevUserID, audits[0].ActorPrincipalID)
}

// The role-binding audit summary is valid JSON for any field value,
// including control characters and invalid UTF-8, and round-trips valid
// UTF-8 values unchanged.
func TestRoleBindingSummaryIsJSON(t *testing.T) {
	b := &store.RoleBinding{
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      "user\x01\"quoted\"\\",
		RoleDefinitionID: "role\nname\x7f\tend",
		ScopeType:        store.RoleScopeProject,
		ScopeID:          "scope-\u2028-id",
	}
	summary := roleBindingSummary(b)
	require.True(t, json.Valid([]byte(summary)), summary)
	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(summary), &got))
	assert.Equal(t, map[string]string{
		"principal_type":     b.PrincipalType,
		"principal_id":       b.PrincipalID,
		"role_definition_id": b.RoleDefinitionID,
		"scope_type":         b.ScopeType,
		"scope_id":           b.ScopeID,
	}, got)

	b.RoleDefinitionID = "role\xff\xfe"
	summary = roleBindingSummary(b)
	assert.True(t, json.Valid([]byte(summary)), summary)
}
