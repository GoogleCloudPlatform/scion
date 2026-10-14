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

// grantAuditFaultStore is a switch-gated wrapper for the group rollback
// tests below. It injects its faults while fault is armed and heal is not:
// the test arms fault for the failing request and heal for the request
// that must succeed, so srv.store is installed once, before any request,
// and never reassigned (ptone/scion#3435). auditErrFor fails
// CreateMutationAudit for records of that mutation type; ownerAddFails
// fails every owner-role AddGroupMember. Both apply inside and outside
// transactions.
type grantAuditFaultStore struct {
	store.Store
	fault, heal   *storeFaultSwitch
	auditErrFor   string
	ownerAddFails bool
}

// newGrantAuditFaultServer is a test server whose store is a disarmed
// grantAuditFaultStore configured by cfg. It returns the raw store for
// setup and assertions, and the fault and heal switches.
func newGrantAuditFaultServer(t *testing.T, cfg grantAuditFaultStore) (*Server, store.Store, *storeFaultSwitch, *storeFaultSwitch) {
	t.Helper()
	heal := &storeFaultSwitch{}
	srv, real, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *grantAuditFaultStore {
		w := cfg
		w.Store, w.fault, w.heal = inner, f, heal
		return &w
	})
	return srv, real, fault, heal
}

func (s *grantAuditFaultStore) injecting() bool {
	return s.fault.Active() && !s.heal.Active()
}

func (s *grantAuditFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		c := *s
		c.Store = tx
		return fn(&c)
	})
}

func (s *grantAuditFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.injecting() && s.auditErrFor != "" && r.MutationType == s.auditErrFor {
		return errors.New("injected mutation audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

func (s *grantAuditFaultStore) AddGroupMember(ctx context.Context, m *store.GroupMember) error {
	if s.injecting() && s.ownerAddFails && m.Role == store.GroupMemberRoleOwner {
		return errors.New("injected group owner add fault")
	}
	return s.Store.AddGroupMember(ctx, m)
}

// Group create with a parent writes the parent membership's audit record
// in the same transaction; an injected audit failure leaves no group and
// no child edge under the parent.
func TestCreateGroupWithParentAuditFailureRollsBack(t *testing.T) {
	srv, real, fault, heal := newGrantAuditFaultServer(t, grantAuditFaultStore{auditErrFor: "group_member_add"})
	ctx := context.Background()
	parent := &store.Group{ID: tid("cg-audit-parent"), Name: "cg audit parent", Slug: "cg-audit-parent", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, parent))
	req := CreateGroupRequest{Name: "cg audit child", Slug: "cg-audit-child", ParentID: parent.ID}

	fault.Arm()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err := real.GetGroupBySlug(ctx, req.Slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no group after the audit failure")
	children, err := real.ListGroups(ctx, store.GroupFilter{ParentID: parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, children.Items, "no child edge after the audit failure")
	assert.Empty(t, grantAudits(t, real, "group_member_add", parent.ID))

	heal.Arm()
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
	srv, real, fault, heal := newGrantAuditFaultServer(t, grantAuditFaultStore{auditErrFor: "group_member_remove"})
	ctx := context.Background()
	group := &store.Group{ID: tid("gm-rm-audit-group"), Name: "gm rm audit", Slug: "gm-rm-audit", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, group))
	userID := tid("gm-rm-audit-user")
	seedRolesTestUser(t, real, userID, "gm-rm-audit@test.com")
	require.NoError(t, real.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: store.GroupMemberRoleMember,
	}))
	path := "/api/v1/groups/" + group.ID + "/members/" + store.GroupMemberTypeUser + "/" + userID

	fault.Arm()
	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err := real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	require.NoError(t, err, "membership kept after the audit failure")
	assert.Empty(t, grantAudits(t, real, "group_member_remove", group.ID))

	heal.Arm()
	rec = doRequest(t, srv, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	audits := grantAudits(t, real, "group_member_remove", group.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Contains(t, audits[0].BeforeSummary, userID)
	assert.NotEmpty(t, audits[0].ActorPrincipalID)
}

// Group create adds the creator's owner membership and its audit record in
// the same transaction as the group; a failure of either write leaves no
// group. Each case runs on its own server, so the store is installed once
// and the fault is switched on and off rather than swapped.
func TestCreateGroupCreatorOwnerFailureRollsBack(t *testing.T) {
	req := CreateGroupRequest{Name: "cg owner audit", Slug: "cg-owner-audit"}

	for name, cfg := range map[string]grantAuditFaultStore{
		"owner add fails":   {ownerAddFails: true},
		"owner audit fails": {auditErrFor: "group_member_add"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, real, fault, heal := newGrantAuditFaultServer(t, cfg)
			ctx := context.Background()

			fault.Arm()
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			_, err := real.GetGroupBySlug(ctx, req.Slug)
			assert.ErrorIs(t, err, store.ErrNotFound, "no group after the failure")

			heal.Arm()
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups", req)
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
		})
	}
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

// groupDeleteFixture creates a group with one user member on the raw store
// and returns the group, the member's ID, and the group's delete path.
func groupDeleteFixture(t *testing.T, s store.Store, name string) (*store.Group, string, string) {
	t.Helper()
	ctx := context.Background()
	group := &store.Group{ID: tid(name + "-group"), Name: name + " group", Slug: name, GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, group))
	userID := tid(name + "-user")
	seedRolesTestUser(t, s, userID, name+"@test.com")
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: store.GroupMemberRoleMember,
	}))
	return group, userID, "/api/v1/groups/" + group.ID
}

// Group delete writes its audit record in the same transaction; an
// injected audit failure answers 500 and leaves the group and its
// membership in place. The fault store is installed before any setup and
// never swapped.
func TestGroupDeleteAuditFailureRollsBack(t *testing.T) {
	srv, real := testServer(t)
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *createTxFaultStore {
		return &createTxFaultStore{Store: inner, auditErrFor: "group_delete"}
	})
	ctx := context.Background()
	group, userID, path := groupDeleteFixture(t, real, "gd-audit-fault")
	parent := &store.Group{ID: tid("gd-audit-fault-parent"), Name: "gd audit fault parent", Slug: "gd-audit-fault-parent", GroupType: store.GroupTypeExplicit}
	require.NoError(t, real.CreateGroup(ctx, parent))
	require.NoError(t, real.AddGroupMember(ctx, &store.GroupMember{
		GroupID: parent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: group.ID, Role: store.GroupMemberRoleMember,
	}))
	rd := newDeliverRoleDefinition(t, real)
	binding, err := real.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      group.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	_, err = real.GetGroup(ctx, group.ID)
	require.NoError(t, err, "group kept after the audit failure")
	_, err = real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	require.NoError(t, err, "membership kept after the audit failure")
	children, err := real.ListGroups(ctx, store.GroupFilter{ParentID: parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, children.Items, 1, "parent edge kept after the audit failure")
	assert.Equal(t, group.ID, children.Items[0].ID)
	bindings, err := real.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, group.ID)
	require.NoError(t, err)
	require.Len(t, bindings, 1, "role binding kept after the audit failure")
	assert.Equal(t, binding.ID, bindings[0].ID)
	assert.Empty(t, grantAudits(t, real, "group_delete", group.ID))
}

// A group delete writes exactly one group_delete record, synchronously,
// attributed to the request actor and naming the deleted group.
func TestGroupDeleteWritesAudit(t *testing.T) {
	srv, real := testServer(t)
	ctx := context.Background()
	group, userID, path := groupDeleteFixture(t, real, "gd-audit-ok")

	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := real.GetGroup(ctx, group.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = real.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	audits := grantAudits(t, real, "group_delete", group.ID)
	require.Len(t, audits, 1, "exactly one audit record, written synchronously")
	assert.Equal(t, "group", audits[0].TargetType)
	assert.Equal(t, DevUserID, audits[0].ActorPrincipalID)
	var before map[string]string
	require.NoError(t, json.Unmarshal([]byte(audits[0].BeforeSummary), &before))
	assert.Equal(t, group.ID, before["group_id"])
	assert.Equal(t, group.Name, before["group_name"])
	assert.Equal(t, group.Slug, before["slug"])
}
