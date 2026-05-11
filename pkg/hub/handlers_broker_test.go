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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupBrokerAuthzTest creates a test server with three users:
//   - alice: member, will own a broker
//   - bob: member, non-owner
//   - admin: admin user
//
// It also creates a project (owned by alice) and a broker (owned by alice) directly
// in the store, and links the broker as a provider to the project.
func setupBrokerAuthzTest(t *testing.T) (srv *Server, s store.Store, alice, bob, admin *store.User, project *store.Project, broker *store.RuntimeBroker) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID:          "user-broker-alice",
		Email:       "broker-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          "user-broker-bob",
		Email:       "broker-bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	admin = &store.User{
		ID:          "user-broker-admin",
		Email:       "broker-admin@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Add alice and bob to hub-members group
	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	// Create a project owned by alice
	project = &store.Project{
		ID:        "project-broker-test",
		Name:      "Broker Test Project",
		Slug:      "broker-test-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Add bob as a project member so he can create agents (project-level authz)
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	_ = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleMember,
	})

	// Create a broker owned by alice directly in the store
	broker = &store.RuntimeBroker{
		ID:        "broker-alice-owned",
		Name:      "Alice Broker",
		Slug:      "alice-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Link broker as a provider to the project and set as default
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	return
}

// ============================================================================
// Broker Registration Authorization Tests
// ============================================================================

func TestBrokerAuthz_Registration_MemberCanRegister(t *testing.T) {
	srv, _, alice, _, _, _, _ := setupBrokerAuthzTest(t)

	// Non-admin member should be able to register a broker
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{Name: "Alice New Broker"})

	assert.Equal(t, http.StatusCreated, rec.Code,
		"member should be able to register a broker; got: %s", rec.Body.String())

	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.BrokerID)
	assert.NotEmpty(t, resp.JoinToken)
}

func TestBrokerAuthz_Registration_UnauthenticatedDenied(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers",
		CreateBrokerRegistrationRequest{Name: "Unauthenticated Broker"})

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated user should get 401; got: %s", rec.Body.String())
}

// ============================================================================
// Broker Dispatch Authorization Tests
// ============================================================================

func TestBrokerAuthz_Dispatch_OwnerAllowed(t *testing.T) {
	srv, _, alice, _, _, project, _ := setupBrokerAuthzTest(t)

	// Alice (broker owner) should pass dispatch authorization.
	// The request may fail downstream (no dispatcher), but NOT with 403.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "dispatch-owner-test",
		ProjectID: project.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"broker owner should not get 403; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Dispatch_NonOwnerDenied(t *testing.T) {
	srv, _, _, bob, _, project, _ := setupBrokerAuthzTest(t)

	// Bob (not the broker owner) should be denied dispatch.
	// The default broker is owned by alice so resolveRuntimeBroker will skip it,
	// resulting in a "no broker available" error rather than 403.
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "dispatch-nonowner-test",
		ProjectID: project.ID,
	})
	// Bob should not succeed — he can't dispatch to alice's broker
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.NotEqual(t, http.StatusCreated, rec.Code)
}

func TestBrokerAuthz_Dispatch_AutoProvide_NonOwnerAllowed(t *testing.T) {
	srv, s, _, bob, _, project, broker := setupBrokerAuthzTest(t)
	ctx := context.Background()

	// Mark the broker as auto-provide — shared infrastructure available to all users
	broker.AutoProvide = true
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	// Bob (not the broker owner) should be allowed to dispatch on an auto-provide broker.
	// The request may fail downstream (no dispatcher), but NOT with 403 or "no broker available".
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "dispatch-autoprovide-test",
		ProjectID: project.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"non-owner should not get 403 on auto-provide broker; got: %s", rec.Body.String())
	// Should not get "no broker available" either — the auto-provide broker should be selected
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code,
		"auto-provide broker should be available to non-owner; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Dispatch_AdminBypass(t *testing.T) {
	srv, _, _, _, admin, project, _ := setupBrokerAuthzTest(t)

	// Admin should bypass broker dispatch authorization.
	// The request may fail downstream (no dispatcher), but NOT with 403.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "dispatch-admin-test",
		ProjectID: project.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"admin should not get 403; got: %s", rec.Body.String())
}

// ============================================================================
// Broker Update Authorization Tests
// ============================================================================

func TestBrokerAuthz_Update_OwnerAllowed(t *testing.T) {
	srv, _, alice, _, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch,
		"/api/v1/runtime-brokers/"+broker.ID, map[string]string{
			"name": "Renamed by Owner",
		})
	assert.Equal(t, http.StatusOK, rec.Code,
		"owner should be able to update broker; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Update_NonOwnerDenied(t *testing.T) {
	srv, _, _, bob, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, bob, http.MethodPatch,
		"/api/v1/runtime-brokers/"+broker.ID, map[string]string{
			"name": "Renamed by Non-Owner",
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403 for update; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Update_AdminBypass(t *testing.T) {
	srv, _, _, _, admin, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, admin, http.MethodPatch,
		"/api/v1/runtime-brokers/"+broker.ID, map[string]string{
			"name": "Renamed by Admin",
		})
	assert.Equal(t, http.StatusOK, rec.Code,
		"admin should be able to update any broker; got: %s", rec.Body.String())
}

// ============================================================================
// Broker Delete Authorization Tests
// ============================================================================

func TestBrokerAuthz_Delete_NonOwnerDenied(t *testing.T) {
	srv, _, _, bob, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, bob, http.MethodDelete,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403 for delete; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Delete_OwnerAllowed(t *testing.T) {
	srv, _, alice, _, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"owner should be able to delete broker; got: %s", rec.Body.String())
}

func TestBrokerAuthz_Delete_AdminBypass(t *testing.T) {
	srv, _, _, _, admin, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"admin should be able to delete any broker; got: %s", rec.Body.String())
}

// ============================================================================
// Broker Capabilities Tests
// ============================================================================

func TestBrokerAuthz_Capabilities_OwnerSeesDispatch(t *testing.T) {
	srv, _, alice, _, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RuntimeBrokerWithCapabilities
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Cap, "capabilities should be present")
	assert.Contains(t, resp.Cap.Actions, "dispatch",
		"owner should see 'dispatch' in capabilities")
	assert.Contains(t, resp.Cap.Actions, "update",
		"owner should see 'update' in capabilities")
	assert.Contains(t, resp.Cap.Actions, "delete",
		"owner should see 'delete' in capabilities")
}

func TestBrokerAuthz_Capabilities_AutoProvide_NonOwnerSeesDispatch(t *testing.T) {
	srv, s, _, bob, _, _, broker := setupBrokerAuthzTest(t)
	ctx := context.Background()

	// Mark the broker as auto-provide
	broker.AutoProvide = true
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RuntimeBrokerWithCapabilities
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Cap, "capabilities should be present")
	assert.Contains(t, resp.Cap.Actions, "dispatch",
		"non-owner should see 'dispatch' on auto-provide broker")
	// Non-owner should still NOT see update/delete
	assert.NotContains(t, resp.Cap.Actions, "update",
		"non-owner should NOT see 'update' even on auto-provide broker")
	assert.NotContains(t, resp.Cap.Actions, "delete",
		"non-owner should NOT see 'delete' even on auto-provide broker")
}

func TestBrokerAuthz_Capabilities_NonOwnerNoDispatch(t *testing.T) {
	srv, _, _, bob, _, _, broker := setupBrokerAuthzTest(t)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/runtime-brokers/"+broker.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RuntimeBrokerWithCapabilities
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Cap, "capabilities should be present")
	assert.NotContains(t, resp.Cap.Actions, "dispatch",
		"non-owner should NOT see 'dispatch' in capabilities")
	assert.NotContains(t, resp.Cap.Actions, "update",
		"non-owner should NOT see 'update' in capabilities")
	assert.NotContains(t, resp.Cap.Actions, "delete",
		"non-owner should NOT see 'delete' in capabilities")
}

// ============================================================================
// Pre-existing Broker Resolution Tests
// ============================================================================

func TestAgentCreate_BrokerResolution(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "broker_id_123",
		Name:   "My Laptop",
		Slug:   "my-laptop",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a project
	project := &store.Project{
		ID:      "project_1",
		Slug:    "test-project",
		Name:    "Test Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Register broker as provider
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	t.Run("Resolve by ID", func(t *testing.T) {
		body := map[string]interface{}{
			"name":            "Agent ID",
			"projectId":         project.ID,
			"runtimeBrokerId": "broker_id_123",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "broker_id_123", resp.Agent.RuntimeBrokerID)
	})

	t.Run("Resolve by Name", func(t *testing.T) {
		body := map[string]interface{}{
			"name":            "Agent Name",
			"projectId":         project.ID,
			"runtimeBrokerId": "My Laptop",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "broker_id_123", resp.Agent.RuntimeBrokerID)
	})

	t.Run("Resolve by Slug", func(t *testing.T) {
		body := map[string]interface{}{
			"name":            "Agent Slug",
			"projectId":         project.ID,
			"runtimeBrokerId": "my-laptop",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "broker_id_123", resp.Agent.RuntimeBrokerID)
	})

	t.Run("Invalid broker", func(t *testing.T) {
		body := map[string]interface{}{
			"name":            "Agent Invalid",
			"projectId":         project.ID,
			"runtimeBrokerId": "non-existent",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})
}
