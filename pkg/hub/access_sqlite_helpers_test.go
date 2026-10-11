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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func commitAuditedConstraint(
	t *testing.T,
	ctx context.Context,
	gs *GovernanceService,
	ps *PreviewService,
	draft *store.AccessConstraint,
	actor PrincipalContext,
) (*CommitResult, error) {
	t.Helper()
	preview, err := ps.GeneratePreview(context.Background(), PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	return gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: preview.PreviewToken,
		PreviewID:    preview.PreviewID,
		DraftHash:    preview.DraftHash,
		Actor:        actor,
		AuditRequest: &auditevent.RequestRef{
			ID:      "request-audit-create",
			Method:  "POST",
			Route:   "/api/v1/admin/access-constraints",
			Surface: "api",
		},
	})
}

// govTestSetup creates a GovernanceService backed by an in-memory SQLite store
// with standard test data: a super-admin user, roles, and permissions.
func govTestSetup(t *testing.T) (*GovernanceService, *PreviewService, *AuthzService, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	authz := srv.authzService
	logger := slog.Default()
	key := []byte("test-governance-hmac-key-32byte!")
	ps := NewPreviewServiceWithKey(s, authz, logger, key)
	ps.nowFunc = func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	gs := NewGovernanceService(s, ps, authz, logger)
	gs.nowFunc = ps.nowFunc
	return gs, ps, authz, s
}

// govSeedAdminUser creates a user and grants them constraint-admin permission.
func govSeedAdminUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	userID := pvSeedUser(t, s, name)
	// Create a role with constraint-admin permission.
	rd := createTestRoleDefinition(t, s, "admin-role-"+name, store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")
	return userID
}

// govSeedNonAdminUser creates a user with basic permissions but NOT constraint-admin.
func govSeedNonAdminUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	userID := pvSeedUser(t, s, name)
	rd := createTestRoleDefinition(t, s, "basic-role-"+name, store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")
	return userID
}

// govCreateAndCommit generates a preview for the given draft and commits it.
func govCreateAndCommit(t *testing.T, gs *GovernanceService, ps *PreviewService, draft *store.AccessConstraint, actor PrincipalContext) *store.AccessConstraint {
	t.Helper()
	ctx := context.Background()

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	commitResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.NoError(t, err)
	return commitResult.Constraint
}

// pvSeedUser creates a user in the store (preview-test-scoped helper).
func pvSeedUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateUser(context.Background(), &store.User{
		ID:          id,
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        "member",
		Status:      "active",
	})
	require.NoError(t, err)
	return id
}

// pvSeedGroup creates a group in the store (preview-test-scoped helper).
func pvSeedGroup(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateGroup(context.Background(), &store.Group{
		ID:        id,
		Name:      name,
		Slug:      name,
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)
	return id
}

// pvSeedGroupMember adds a member to a group (preview-test-scoped helper).
func pvSeedGroupMember(t *testing.T, s store.Store, groupID, memberType, memberID string) {
	t.Helper()
	err := s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID:    groupID,
		MemberType: memberType,
		MemberID:   memberID,
		Role:       "member",
		AddedBy:    "test",
	})
	require.NoError(t, err)
}

// pvSeedRoleBinding creates a role binding (preview-test-scoped helper).
func pvSeedRoleBinding(t *testing.T, s store.Store, roleDefID, principalType, principalID, scopeType, scopeID string) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// pvSeedProject creates a project in the store (preview-test-scoped helper).
func pvSeedProject(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateProject(context.Background(), &store.Project{
		ID:   id,
		Name: name,
		Slug: name,
	})
	require.NoError(t, err)
	return id
}

// pvTestActor returns a PrincipalContext for testing.
func pvTestActor(userID string) PrincipalContext {
	return PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   userID,
	}
}

// pvStrPtr returns a pointer to a string (preview-test-scoped helper).
func pvStrPtr(s string) *string { return &s }
