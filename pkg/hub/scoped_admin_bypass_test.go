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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scopedAdminIdentity() *ScopedUserIdentity {
	return NewScopedUserIdentity(
		NewAuthenticatedUser(tid("scoped-admin"), "admin@example.com", "Admin", "admin", "api"),
		tid("project-1"),
		[]string{"group:addMember"},
	)
}

func federatedAdminIdentity() *FederatedUserIdentity {
	return NewFederatedUserIdentity("https://issuer.example", "admin", "admin@example.com", "Admin", "admin", nil)
}

// bindableFederatedAdmin is a federated identity whose ID is a local UUID so a
// test can grant an ordinary policy and reach a direct bypass branch. Production
// federation IDs intentionally include the issuer and are not bindable locally.
type bindableFederatedAdmin struct {
	UserIdentity
	issuerURL string
}

func (f *bindableFederatedAdmin) Type() string      { return "federated_user" }
func (f *bindableFederatedAdmin) IssuerURL() string { return f.issuerURL }

func newBindableFederatedAdmin(id string) *bindableFederatedAdmin {
	return &bindableFederatedAdmin{
		UserIdentity: NewAuthenticatedUser(id, "admin@example.com", "Admin", "admin", "api"),
		issuerURL:    "https://issuer.example",
	}
}

func TestAdminModeMiddlewareRejectsScopedAdmin(t *testing.T) {
	middleware := adminModeMiddleware(NewMaintenanceState(true, ""))(passthrough)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), scopedAdminIdentity()))
	rec := httptest.NewRecorder()

	middleware.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestStopAllAgentsRejectsScopedAdmin(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/stop-all", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), scopedAdminIdentity()))
	rec := httptest.NewRecorder()

	srv.handleStopAllAgents(rec, req, "")

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBrokerSecretRotationRejectsScopedAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("broker-1"), Name: "Broker", Slug: "broker-1", CreatedBy: tid("different-owner")}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), scopedAdminIdentity()))
	rec := httptest.NewRecorder()

	srv.handleBrokerRotateSecret(rec, req, broker.ID)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAddGroupMemberRejectsScopedAdminBypass(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	caller := scopedAdminIdentity()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: caller.ID(), Email: caller.Email(), DisplayName: caller.DisplayName(), Role: "admin", Status: "active"}))
	owner := &store.User{ID: tid("different-owner"), Email: "owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	target := &store.User{ID: tid("group-target"), Email: "target@example.com", DisplayName: "Target", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, target))
	group := &store.Group{ID: tid("group-1"), Name: "Group", Slug: "group-1", GroupType: store.GroupTypeExplicit, OwnerID: owner.ID}
	require.NoError(t, s.CreateGroup(ctx, group))
	policy := &store.Policy{ID: tid("group-add-member"), Name: "group add member", ScopeType: "hub", ResourceType: "group", Actions: []string{"addMember"}, Effect: "allow"}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{PolicyID: policy.ID, PrincipalType: "user", PrincipalID: caller.ID()}))
	body, err := json.Marshal(AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: target.ID, Role: store.GroupMemberRoleAdmin})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups/"+group.ID+"/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), caller))
	rec := httptest.NewRecorder()

	srv.addGroupMember(rec, req, group)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestFederatedAdminCannotUseDirectAdminShortcuts(t *testing.T) {
	t.Run("maintenance admission", func(t *testing.T) {
		middleware := adminModeMiddleware(NewMaintenanceState(true, ""))(passthrough)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), federatedAdminIdentity()))
		rec := httptest.NewRecorder()

		middleware.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})

	t.Run("global stop all", func(t *testing.T) {
		srv, _ := testServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/stop-all", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), federatedAdminIdentity()))
		rec := httptest.NewRecorder()

		srv.handleStopAllAgents(rec, req, "")

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("broker secret rotation", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		broker := &store.RuntimeBroker{ID: tid("fed-broker-1"), Name: "Broker", Slug: "fed-broker-1", CreatedBy: tid("different-owner")}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), federatedAdminIdentity()))
		rec := httptest.NewRecorder()

		srv.handleBrokerRotateSecret(rec, req, broker.ID)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestFederatedAdminCannotUseGroupHierarchyBypass(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	caller := newBindableFederatedAdmin(tid("federated-group-admin"))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: caller.ID(), Email: caller.Email(), DisplayName: caller.DisplayName(), Role: "member", Status: "active"}))
	owner := &store.User{ID: tid("federated-group-owner"), Email: "owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	target := &store.User{ID: tid("federated-group-target"), Email: "target@example.com", DisplayName: "Target", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	require.NoError(t, s.CreateUser(ctx, target))
	group := &store.Group{ID: tid("federated-group"), Name: "Group", Slug: "federated-group", GroupType: store.GroupTypeExplicit, OwnerID: owner.ID}
	require.NoError(t, s.CreateGroup(ctx, group))
	policy := &store.Policy{ID: tid("federated-group-add-member"), Name: "federated group add member", ScopeType: "hub", ResourceType: "group", Actions: []string{"addMember"}, Effect: "allow"}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{PolicyID: policy.ID, PrincipalType: "user", PrincipalID: caller.ID()}))
	body, err := json.Marshal(AddGroupMemberRequest{MemberType: store.GroupMemberTypeUser, MemberID: target.ID, Role: store.GroupMemberRoleAdmin})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups/"+group.ID+"/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), caller))
	rec := httptest.NewRecorder()

	srv.addGroupMember(rec, req, group)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestFederatedAdminCannotUseProjectStopAllBypass(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("federated-stop-project"), Name: "Project", Slug: "federated-stop-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	caller := newBindableFederatedAdmin(tid("federated-stop-admin"))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: caller.ID(), Email: caller.Email(), DisplayName: caller.DisplayName(), Role: "member", Status: "active"}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), caller))
	rec := httptest.NewRecorder()

	srv.handleStopAllAgents(rec, req, project.ID)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}
