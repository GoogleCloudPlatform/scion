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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupCredentialTestServer creates a test server and required fixtures
// (user, project, hub membership) for credential revocation tests.
func setupCredentialTestServer(t *testing.T) (*Server, store.Store, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	return setupCredentialTestServerOn(t, srv, s)
}

// setupCredentialTestServerOn seeds the credential-test user and project on
// an already constructed test server and its (unwrapped) store.
func setupCredentialTestServerOn(t *testing.T, srv *Server, s store.Store) (*Server, store.Store, *store.User, *store.Project) {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-cred-test"),
		Email:       "credtest@test.com",
		DisplayName: "Credential Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-cred-test"),
		Name:      "cred-test-project",
		Slug:      "cred-test-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, user, project
}

// createCredTestAgent creates a minimal agent in the store for testing.
func createCredTestAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:        agentID,
		Name:      "test-agent-" + agentID[:8],
		Slug:      "test-agent-" + agentID[:8],
		ProjectID: projectID,
		OwnerID:   ownerID,
		Phase:     "running",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}
