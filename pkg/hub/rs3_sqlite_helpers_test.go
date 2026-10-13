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
	"fmt"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func createRS3Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, ownerID)

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      "RS3 Test Project " + projectID,
		Slug:      fmt.Sprintf("rs3-test-%s", projectID[:8]),
		CreatedBy: ownerID,
	}))

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

// newTestAuthzService creates an AuthzService from the given store for use in
// failure injection tests where we can't use the full server.
func newTestAuthzService(s store.Store) *AuthzService {
	return NewAuthzService(s, slog.Default())
}

// setTestIdentity sets the identity in context for the test.
func setTestIdentity(ctx context.Context, user UserIdentity) context.Context {
	return contextWithIdentity(ctx, user)
}
