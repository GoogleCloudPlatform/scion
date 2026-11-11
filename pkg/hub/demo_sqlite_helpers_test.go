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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// doRequestAsUser creates a user token and performs an HTTP request as that user.
func doRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupDemoPolicyTest creates a test server with two users and a project.
// User "alice" is a project member (project creator); user "bob" is not.
// Both are hub-members. Returns the server, store, users, and project.
func setupDemoPolicyTest(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	alice, bob, project := setupDemoPolicyOn(t, srv, s)
	return srv, s, alice, bob, project
}

// setupDemoPolicyTestWithFault is setupDemoPolicyTest with a switch-gated
// store wrapper (see installStoreFault) installed before the fixture's
// audited setup (seedProjectCreatorMembership emits a mutation audit whose
// goroutine reads srv.store). Tests call fault.Arm() where they used to
// assign srv.store, which would race that goroutine (ptone/scion#3184).
func setupDemoPolicyTestWithFault[W store.Store](t *testing.T, wrap func(inner store.Store, fault *storeFaultSwitch) W) (*Server, store.Store, *store.User, *store.User, *store.Project, W, *storeFaultSwitch) {
	t.Helper()
	srv, s, wrapped, fault := testServerWithStoreFault(t, wrap)
	alice, bob, project := setupDemoPolicyOn(t, srv, s)
	return srv, s, alice, bob, project, wrapped, fault
}

func setupDemoPolicyOn(t *testing.T, srv *Server, s store.Store) (alice, bob *store.User, project *store.Project) {
	t.Helper()
	ctx := context.Background()

	// Create users
	alice = &store.User{
		ID:          tid("user-alice"),
		Email:       "alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          tid("user-bob"),
		Email:       "bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	// Add both to hub-members group (simulates login)
	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	// Create a project owned by alice
	project = &store.Project{
		ID:        tid("project-demo"),
		Name:      "Demo Project",
		Slug:      "demo-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create the creator's owner binding and the members group (simulates
	// what the project creation handler does).
	srv.seedProjectCreatorMembership(ctx, project)

	return alice, bob, project
}
