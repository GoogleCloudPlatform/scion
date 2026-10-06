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

// deletedUserTokenFixture is a hub server with a project owned by the dev
// user, a member user, and an agent owned by that member.
type deletedUserTokenFixture struct {
	srv     *Server
	store   store.Store
	user    *store.User
	agentID string
	token   string
}

func newDeletedUserTokenFixture(t *testing.T, name string) *deletedUserTokenFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid(name + "-user"),
		Email:       name + "@test.com",
		DisplayName: "Token Lifecycle User",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid(name + "-project"),
		Name:      name + "-project",
		Slug:      name + "-project",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	agentID := tid(name + "-agent")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)

	// A CLI token: the long-lived user token type.
	token, _, err := srv.userTokenService.GenerateAccessToken(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI,
	)
	require.NoError(t, err)

	return &deletedUserTokenFixture{srv: srv, store: s, user: user, agentID: agentID, token: token}
}

func (f *deletedUserTokenFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), "body: %s", rec.Body.String())
	return errResp.Error.Code
}

// TestJWTAuth_TokenStopsWorkingAfterUserDelete covers the full flow: a CLI
// token works for its user (including the user's own agent), the user is
// deleted through the admin API, and the same unexpired token is then
// rejected with 401 user_not_found on every endpoint.
func TestJWTAuth_TokenStopsWorkingAfterUserDelete(t *testing.T) {
	f := newDeletedUserTokenFixture(t, "jwt-deleted")
	agentPath := "/api/v1/agents/" + f.agentID

	// Control: the live user's token works.
	rec := f.get(t, "/api/v1/auth/me")
	require.Equal(t, http.StatusOK, rec.Code, "auth/me before delete: %s", rec.Body.String())
	rec = f.get(t, agentPath)
	require.Equal(t, http.StatusOK, rec.Code, "own agent before delete: %s", rec.Body.String())

	// Delete the user through the admin API.
	del := doRequest(t, f.srv, http.MethodDelete, "/api/v1/users/"+f.user.ID, nil)
	require.Equal(t, http.StatusNoContent, del.Code, "delete user: %s", del.Body.String())

	// The same, still-unexpired token is now rejected before any handler.
	for _, path := range []string{"/api/v1/auth/me", agentPath, "/api/v1/agents"} {
		rec := f.get(t, path)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s after delete: %s", path, rec.Body.String())
		assert.Equal(t, ErrCodeUserNotFound, errorCodeOf(t, rec), "%s after delete", path)
		assert.Contains(t, rec.Body.String(), "no user record for this token", "%s after delete", path)
	}
}

// TestJWTAuth_SuspendedUserTokenUnchanged is the control for the deleted-user
// rule: a suspended user's token keeps the existing 403 user_suspended
// response.
func TestJWTAuth_SuspendedUserTokenUnchanged(t *testing.T) {
	f := newDeletedUserTokenFixture(t, "jwt-suspended")

	f.user.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(context.Background(), f.user))

	rec := f.get(t, "/api/v1/agents/"+f.agentID)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "user_suspended", errorCodeOf(t, rec))
}
