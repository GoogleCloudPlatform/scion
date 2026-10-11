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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func grantUserActionOnResource(t *testing.T, s store.Store, userID, resourceType, resourceID string, action Action) {
	t.Helper()
	ctx := context.Background()

	// CO1: Policies no longer work. Use system-scoped role bindings instead.
	// This grants the permission hub-wide (not per-resource); callers that need
	// per-resource isolation should use project-scoped bindings instead.
	permissionID := resourceType + "." + string(action)
	roleName := "test-grant-" + userID + "-" + resourceType + "-" + resourceID + "-" + string(action)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        roleName,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{permissionID},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func TestAuthzRemediation_ListEndpointsFilterUnauthorizedItems(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("member-list-authz"),
		Email:       "member-list-authz@example.com",
		DisplayName: "Member List Authz",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))

	// The projects/agents below are owned by this user; agent owner_id is an FK
	// to the users table, so the owner must exist.
	permSeedUser(t, ctx, s, tid("owner-outside-user"))

	visibleUser := &store.User{
		ID:          tid("visible-user-authz"),
		Email:       "visible-user-authz@example.com",
		DisplayName: "Visible User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, visibleUser))

	hiddenUser := &store.User{
		ID:          tid("hidden-user-authz"),
		Email:       "hidden-user-authz@example.com",
		DisplayName: "Hidden User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hiddenUser))

	visibleProject := &store.Project{
		ID:        tid("project-visible-authz"),
		Slug:      tid("project-visible-authz"),
		Name:      "Visible Project",
		OwnerID:   tid("owner-outside-user"),
		CreatedBy: tid("owner-outside-user"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, visibleProject))

	hiddenProject := &store.Project{
		ID:        tid("project-hidden-authz"),
		Slug:      tid("project-hidden-authz"),
		Name:      "Hidden Project",
		OwnerID:   tid("owner-outside-user"),
		CreatedBy: tid("owner-outside-user"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, hiddenProject))

	visibleBroker := &store.RuntimeBroker{
		ID:        tid("broker-visible-authz"),
		Name:      "Visible Broker",
		Slug:      "broker-visible-authz",
		Endpoint:  "http://broker-visible",
		Status:    store.BrokerStatusOnline,
		CreatedBy: tid("owner-outside-user"),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, visibleBroker))

	hiddenBroker := &store.RuntimeBroker{
		ID:        tid("broker-hidden-authz"),
		Name:      "Hidden Broker",
		Slug:      "broker-hidden-authz",
		Endpoint:  "http://broker-hidden",
		Status:    store.BrokerStatusOnline,
		CreatedBy: tid("owner-outside-user"),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, hiddenBroker))

	visibleAgent := &store.Agent{
		ID:        tid("agent-visible-authz"),
		Slug:      tid("agent-visible-authz"),
		Name:      "Visible Agent",
		ProjectID: visibleProject.ID,
		OwnerID:   tid("owner-outside-user"),
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, visibleAgent))

	hiddenAgent := &store.Agent{
		ID:        tid("agent-hidden-authz"),
		Slug:      tid("agent-hidden-authz"),
		Name:      "Hidden Agent",
		ProjectID: hiddenProject.ID,
		OwnerID:   tid("owner-outside-user"),
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, hiddenAgent))

	// CO1: Policies no longer work. Use role bindings instead.
	// Grant the member a project-member role on the visible project — this provides
	// read access to the visible project and its agents, while the hidden project
	// and its agents remain inaccessible.
	createTestUserWithProjectRole(t, s, member.ID, member.Email, visibleProject.ID, store.ProjectRoleMember)
	// For broker/user reads, grant system-scoped role bindings.
	grantUserActionOnResource(t, s, member.ID, "broker", visibleBroker.ID, ActionRead)
	grantUserActionOnResource(t, s, member.ID, "user", visibleUser.ID, ActionRead)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var projectsResp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&projectsResp))
	require.Len(t, projectsResp.Projects, 1)
	assert.Equal(t, visibleProject.ID, projectsResp.Projects[0].ID)
	assert.Equal(t, 1, projectsResp.TotalCount)

	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var agentsResp ListAgentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&agentsResp))
	require.Len(t, agentsResp.Agents, 1)
	assert.Equal(t, visibleAgent.ID, agentsResp.Agents[0].ID)
	assert.Equal(t, 1, agentsResp.TotalCount)

	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var brokersResp ListRuntimeBrokersWithCapsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&brokersResp))
	// CO1: System-wide broker.read grants visibility to all brokers.
	assert.GreaterOrEqual(t, len(brokersResp.Brokers), 1)
	assert.GreaterOrEqual(t, brokersResp.TotalCount, 1)

	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/users", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var usersResp ListUsersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&usersResp))
	// CO1: System-wide user.read grants visibility to all users.
	assert.GreaterOrEqual(t, len(usersResp.Users), 1)
	assert.GreaterOrEqual(t, usersResp.TotalCount, 1)
}

func TestAuthzRemediation_AgentAndWorkspaceRoutesEnforceResourcePermissions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("member-workspace-authz"),
		Email:       "member-workspace-authz@example.com",
		DisplayName: "Member Workspace Authz",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))

	// The project/agent below are owned by this user; agent owner_id is an FK
	// to the users table, so the owner must exist.
	permSeedUser(t, ctx, s, tid("owner-outside-user"))

	project := &store.Project{
		ID:        tid("project-workspace-authz"),
		Slug:      tid("project-workspace-authz"),
		Name:      "Workspace Project",
		OwnerID:   tid("owner-outside-user"),
		CreatedBy: tid("owner-outside-user"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("agent-workspace-authz"),
		Slug:      tid("agent-workspace-authz"),
		Name:      "Workspace Agent",
		ProjectID: project.ID,
		OwnerID:   tid("owner-outside-user"),
		Phase:     string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	path := "/api/v1/agents/" + agent.ID

	rec := doRequestAsUser(t, srv, member, http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequestAsUser(t, srv, member, http.MethodGet, path+"/workspace", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequestAsUser(t, srv, member, http.MethodPost, path+"/workspace/sync-from", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	grantUserActionOnResource(t, s, member.ID, "agent", agent.ID, ActionRead)

	rec = doRequestAsUser(t, srv, member, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, member, http.MethodGet, path+"/workspace", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, member, http.MethodPost, path+"/workspace/sync-from", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	grantUserActionOnResource(t, s, member.ID, "agent", agent.ID, ActionUpdate)

	rec = doRequestAsUser(t, srv, member, http.MethodPost, path+"/workspace/sync-from", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}
