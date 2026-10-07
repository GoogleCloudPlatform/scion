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
