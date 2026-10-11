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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireLastOwnerDenial asserts a 409 last_owner response whose
// details.projects lists exactly the given project.
func requireLastOwnerDenial(t *testing.T, rec *httptest.ResponseRecorder, project *store.Project) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Projects []lastOwnerProjectRef `json:"projects"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeLastOwner, resp.Error.Code)
	assert.NotEmpty(t, resp.Error.Message)
	assert.Equal(t, []lastOwnerProjectRef{{ID: project.ID, Name: project.Name}}, resp.Error.Details.Projects)
}

// grantSystemRole gives the user a system-scoped binding for the named
// system role directly in the store.
func grantSystemRole(t *testing.T, s store.Store, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
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

// newInvitedUser creates a user in invited status, as the allow-list
// endpoints manage.
func newInvitedUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(id), Email: email, DisplayName: id,
		Role: store.UserRoleMember, Status: store.UserStatusInvited, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// createOwnerBinding gives the principal a project-owner binding directly in
// the store, optionally pending (notBefore) or expired (expiresAt).
func createOwnerBinding(t *testing.T, s store.Store, userID, projectID string, notBefore, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}
