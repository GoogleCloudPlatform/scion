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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testLoginStore struct {
	store.Store
	users map[string]*store.User
}

func newTestLoginStore() *testLoginStore {
	return &testLoginStore{users: make(map[string]*store.User)}
}

func (s *testLoginStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if u, ok := s.users[email]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user not found")
}

func (s *testLoginStore) CreateUser(_ context.Context, user *store.User) error {
	s.users[user.Email] = user
	return nil
}

func (s *testLoginStore) UpdateUser(_ context.Context, user *store.User) error {
	s.users[user.Email] = user
	return nil
}

func (s *testLoginStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, fmt.Errorf("not found")
}

func newTestLoginWebServer(t *testing.T, enableTestLogin bool) *WebServer {
	t.Helper()
	cfg := WebServerConfig{
		EnableTestLogin: enableTestLogin,
	}
	ws := NewWebServer(cfg)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(newTestLoginStore())
	return ws
}

func TestHandleTestLogin_Success(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"admin","displayName":"Test User"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp TestLoginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "test@example.com", resp.User.Email)
	assert.Equal(t, "admin", resp.User.Role)
	assert.Equal(t, "Test User", resp.User.DisplayName)
	assert.NotEmpty(t, resp.AccessToken)
	assert.NotEmpty(t, resp.RefreshToken)
	assert.Greater(t, resp.ExpiresIn, int64(0))

	cookies := rec.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == webSessionName {
			found = true
			break
		}
	}
	assert.True(t, found, "session cookie should be set")
}

func TestHandleTestLogin_DefaultRole(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	body := `{"email":"member@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp TestLoginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "member", resp.User.Role)
	assert.Equal(t, "member@example.com", resp.User.DisplayName)
}

func TestHandleTestLogin_Disabled(t *testing.T) {
	ws := newTestLoginWebServer(t, false)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleTestLogin_MethodNotAllowed(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/test-login", nil)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleTestLogin_MissingEmail(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	body := `{"role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_InvalidRole(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"superadmin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_InvalidJSON(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_ExistingUser(t *testing.T) {
	ws := newTestLoginWebServer(t, true)

	// Pre-populate a user
	mockStore := ws.store.(*testLoginStore)
	mockStore.users["existing@example.com"] = &store.User{
		ID:          "existing-id",
		Email:       "existing@example.com",
		DisplayName: "Old Name",
		Role:        "member",
		Status:      "active",
		Created:     time.Now().Add(-24 * time.Hour),
	}

	body := `{"email":"existing@example.com","role":"admin","displayName":"New Name"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp TestLoginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "existing-id", resp.User.ID)
	assert.Equal(t, "admin", resp.User.Role)
}

func TestHandleTestLogin_AllRoles(t *testing.T) {
	for _, role := range []string{"admin", "member", "viewer"} {
		t.Run(role, func(t *testing.T) {
			ws := newTestLoginWebServer(t, true)

			body := fmt.Sprintf(`{"email":"user@example.com","role":"%s"}`, role)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			ws.handleTestLogin(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)

			var resp TestLoginResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, role, resp.User.Role)
		})
	}
}
