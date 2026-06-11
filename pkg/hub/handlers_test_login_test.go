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
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testLoginStore struct {
	store.Store
	users       map[string]*store.User
	errOnLookup error
}

func newTestLoginStore() *testLoginStore {
	return &testLoginStore{users: make(map[string]*store.User)}
}

func (s *testLoginStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if s.errOnLookup != nil {
		return nil, s.errOnLookup
	}
	if u, ok := s.users[email]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
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

// newTestLoginWebServer creates a WebServer for test-login tests and returns
// the UserTokenService so callers can mint challenge tokens.
func newTestLoginWebServer(t *testing.T, enableTestLogin bool) (*WebServer, *UserTokenService) {
	t.Helper()
	cfg := WebServerConfig{
		EnableTestLogin: enableTestLogin,
	}
	ws := NewWebServer(cfg)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(newTestLoginStore())
	return ws, tokenSvc
}

// testLoginAuthHeader mints a valid test-login challenge token and returns
// the value for the Authorization header ("Bearer <token>").
func testLoginAuthHeader(t *testing.T, svc *UserTokenService) string {
	t.Helper()
	token, err := svc.GenerateTestLoginToken("test")
	require.NoError(t, err)
	return "Bearer " + token
}

func TestHandleTestLogin_Success(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"admin","displayName":"Test User"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
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
	ws, tokenSvc := newTestLoginWebServer(t, true)

	body := `{"email":"member@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp TestLoginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "member", resp.User.Role)
	assert.Equal(t, "member@example.com", resp.User.DisplayName)
}

func TestHandleTestLogin_Disabled(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, false)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleTestLogin_MethodNotAllowed(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/test-login", nil)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleTestLogin_MissingEmail(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	body := `{"role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_InvalidEmail(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	body := `{"email":"nope","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "email must contain @")
}

func TestHandleTestLogin_DBError(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)
	mockStore := ws.store.(*testLoginStore)
	mockStore.errOnLookup = fmt.Errorf("connection refused")

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "failed to look up user")
}

func TestHandleTestLogin_InvalidRole(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"superadmin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_InvalidJSON(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleTestLogin_ExistingUser(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

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
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
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
			ws, tokenSvc := newTestLoginWebServer(t, true)

			body := fmt.Sprintf(`{"email":"user@example.com","role":"%s"}`, role)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
			rec := httptest.NewRecorder()

			ws.handleTestLogin(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)

			var resp TestLoginResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, role, resp.User.Role)
		})
	}
}

// --- Auth failure tests ---

func TestHandleTestLogin_MissingAuth(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "authorization required")
}

func TestHandleTestLogin_InvalidToken(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid test-login token")
}

func TestHandleTestLogin_WrongSigningKey(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	// Mint a token with a different signing key
	otherSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	token, err := otherSvc.GenerateTestLoginToken("attacker")
	require.NoError(t, err)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid test-login token")
}

func TestHandleTestLogin_WrongAudience(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	// Mint a regular user access token (audience "scion-hub-api") instead of
	// a test-login token (audience "scion-test-login").
	userToken, _, _, err := tokenSvc.GenerateTokenPair(
		"uid", "test@example.com", "Test", "admin", ClientTypeWeb,
	)
	require.NoError(t, err)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid test-login token")
}

func TestHandleTestLogin_ExpiredToken(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	// Manually create an expired test-login token using the same signing key.
	// We reach into the UserTokenService's signer to build an expired JWT.
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: ws.userTokenSvc.config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	past := time.Now().Add(-10 * time.Minute)
	claims := jwt.Claims{
		Issuer:    UserTokenIssuer,
		Subject:   "test",
		Audience:  jwt.Audience{TestLoginAudience},
		IssuedAt:  jwt.NewNumericDate(past),
		Expiry:    jwt.NewNumericDate(past.Add(5 * time.Minute)), // expired 5 min ago
		NotBefore: jwt.NewNumericDate(past),
		ID:        "expired-test-id",
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid test-login token")
}

func TestHandleTestLogin_AuthNotBearer(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "authorization required")
}

func TestHandleTestLogin_BearerCaseInsensitive(t *testing.T) {
	ws, tokenSvc := newTestLoginWebServer(t, true)

	token, err := tokenSvc.GenerateTestLoginToken("test")
	require.NoError(t, err)

	// RFC 7235: auth scheme is case-insensitive
	for _, scheme := range []string{"bearer", "BEARER", "Bearer"} {
		t.Run(scheme, func(t *testing.T) {
			body := `{"email":"ci@example.com","role":"member"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", scheme+" "+token)
			rec := httptest.NewRecorder()

			ws.handleTestLogin(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code, "scheme %q should be accepted", scheme)
		})
	}
}

func TestHandleTestLogin_NoExpiryClaim(t *testing.T) {
	ws, _ := newTestLoginWebServer(t, true)

	// Craft a token with no exp claim to verify it is rejected.
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: ws.userTokenSvc.config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	now := time.Now()
	claims := jwt.Claims{
		Issuer:   UserTokenIssuer,
		Subject:  "test",
		Audience: jwt.Audience{TestLoginAudience},
		IssuedAt: jwt.NewNumericDate(now),
		// Expiry intentionally omitted
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	body := `{"email":"test@example.com","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	ws.handleTestLogin(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid test-login token")
}
