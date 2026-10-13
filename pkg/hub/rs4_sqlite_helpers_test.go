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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// rs4Project creates a project, user, and role binding for RS4 tests.
func rs4Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	createRS1Project(t, s, projectID, ownerID)
}

// rs4AddProjectRole adds a project role binding for an already-existing user (e.g. DevUserID).
func rs4AddProjectRole(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()
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
		t.Fatalf("failed to create project role binding: %v", err)
	}
}

// rs4ExtractError extracts the error code from a JSON error response.
func rs4ExtractError(resp map[string]interface{}) string {
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		if code, ok := errObj["code"].(string); ok {
			return code
		}
	}
	return ""
}

// rs4FailingStore extends the RS1 failingStore pattern for UAT operations.
type rs4FailingStore struct {
	store.Store
	createMutationAuditErr   error
	createUserAccessTokenErr error
	commitErr                error
}

func (f *rs4FailingStore) CreateMutationAudit(ctx context.Context, record *store.MutationAuditRecord) error {
	if f.createMutationAuditErr != nil {
		return f.createMutationAuditErr
	}
	return f.Store.CreateMutationAudit(ctx, record)
}

func (f *rs4FailingStore) CreateUserAccessToken(ctx context.Context, token *store.UserAccessToken) error {
	if f.createUserAccessTokenErr != nil {
		return f.createUserAccessTokenErr
	}
	return f.Store.CreateUserAccessToken(ctx, token)
}

func (f *rs4FailingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		wrappedTx := &rs4FailingStore{
			Store:                    tx,
			createMutationAuditErr:   f.createMutationAuditErr,
			createUserAccessTokenErr: f.createUserAccessTokenErr,
		}
		if err := fn(wrappedTx); err != nil {
			return err
		}
		if f.commitErr != nil {
			return f.commitErr
		}
		return nil
	})
}
