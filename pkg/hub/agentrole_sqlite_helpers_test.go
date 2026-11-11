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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupAgentRoleTest creates a server with a member user and project.
// The user is the project creator and is in the hub-members group.
func setupAgentRoleTest(t *testing.T) (*Server, store.Store, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-role-test"),
		Email:       "roletest@test.com",
		DisplayName: "Role Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-role-test"),
		Name:      "role-test-project",
		Slug:      "role-test-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, user, project
}

// doAgentReadRequest makes a GET request to the given path using an agent token
// with the specified scopes.
func doAgentReadRequest(t *testing.T, srv *Server, agentID, projectID, path string, scopes []AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agentID, projectID, scopes, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupReadScopeTest creates a server, user, project, and agent for read scope tests.
func setupReadScopeTest(t *testing.T) (*Server, store.Store, *store.Agent, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-read-scope"),
		Email:       "readscope@test.com",
		DisplayName: "Read Scope Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-read-scope"),
		Name:      "read-scope-project",
		Slug:      "read-scope-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	agent := &store.Agent{
		ID:        tid("agent-read-scope"),
		Slug:      "read-scope-agent",
		Name:      "Read Scope Agent",
		ProjectID: project.ID,
		Phase:     "running",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, agent, project
}
