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
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// doRequestWithUAT makes an HTTP request authenticated with a real scoped UAT.
func doRequestWithUAT(t *testing.T, srv *Server, uatKey, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+uatKey)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// rs4MintContext returns a context with the actor identity and credential
// context required by the RS4 bounded UAT service for audit record creation.
func rs4MintContext(userID string) context.Context {
	identity := NewAuthenticatedUser(userID, userID+"@test.com", "Test User", "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
}

// mintScopedUAT mints a real scoped UAT through the production token service.
// RS4: The issuer must have role bindings with the requested scopes in the
// target project; the context must carry the actor identity for audit.
func mintScopedUAT(t *testing.T, srv *Server, userID, projectID string, scopes []string) string {
	t.Helper()
	ctx := rs4MintContext(userID)
	key, _, err := srv.uatService.CreateToken(
		ctx, userID, "test-uat", projectID, scopes, nil,
	)
	require.NoError(t, err, "failed to mint scoped UAT")
	return key
}

// noDBStore wraps a store.Store but does NOT expose DB(). This simulates
// the case where the store doesn't support raw database access.
type noDBStore struct {
	store.Store
}

func findRS1RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find repo root (no go.mod found)")
		}
		dir = parent
	}
}

// createRS1Project creates a project with a permanent owner.
func createRS1Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, ownerID)

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      "RS1 Test Project " + projectID,
		Slug:      fmt.Sprintf("rs1-test-%s", projectID[:8]),
		CreatedBy: ownerID,
	}))

	// Create owner role binding.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create owner binding: %v", err)
	}
}

// createRS1UserWithRole creates a user and assigns them a project role.
func createRS1UserWithRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email,
		DisplayName: "User", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}
