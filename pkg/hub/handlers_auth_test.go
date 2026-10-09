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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenResponse_JSON(t *testing.T) {
	tr := TokenResponse{
		ID:        "t1",
		Name:      "ci-token",
		Prefix:    "scion_pat_abc1",
		ProjectID: "p1",
		Scopes:    []string{"agent:dispatch"},
		Created:   time.Now(),
	}

	data, err := json.Marshal(tr)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m))

	require.Equal(t, "p1", m["projectId"])
	_, hasGroveID := m["groveId"]
	require.False(t, hasGroveID, "legacy 'groveId' key present in marshal output, want absent")
}

func TestAuthLogin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.oauthService = &OAuthService{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != googleUserURL {
					return httpJSONResponse(http.StatusNotFound, `{"error":"not found"}`), nil
				}

				switch req.Header.Get("Authorization") {
				case "Bearer good-token":
					return httpJSONResponse(http.StatusOK, `{
						"id":"google-user-1",
						"email":"verified@example.com",
						"verified_email":true,
						"name":"Provider Name",
						"picture":"https://example.com/avatar.png"
					}`), nil
				case "Bearer good-token-2":
					return httpJSONResponse(http.StatusOK, `{
						"id":"google-user-1",
						"email":"verified@example.com",
						"verified_email":true,
						"name":"Provider Name 2",
						"picture":"https://example.com/avatar2.png"
					}`), nil
				default:
					return httpJSONResponse(http.StatusUnauthorized, `{"error":"invalid_token"}`), nil
				}
			}),
		},
	}

	// 1. Successful login (new user). Request-supplied identity fields are ignored.
	body := AuthLoginRequest{
		Provider:      "google",
		ProviderToken: "good-token",
		Email:         "forged@example.com",
		Name:          "Forged Name",
		Avatar:        "https://example.com/forged.png",
	}

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AuthLoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.User.Email != "verified@example.com" {
		t.Errorf("expected email 'verified@example.com', got %q", resp.User.Email)
	}

	if resp.AccessToken == "" {
		t.Error("expected access token to be set")
	}

	// Verify user was created from provider-verified identity, not request body.
	user, err := s.GetUserByEmail(ctx, "verified@example.com")
	if err != nil {
		t.Fatalf("failed to get user from store: %v", err)
	}
	if user.DisplayName != "Provider Name" {
		t.Errorf("expected display name 'Provider Name', got %q", user.DisplayName)
	}

	// 2. Successful login (existing user) - DisplayName should NOT be updated if already set
	body2 := AuthLoginRequest{
		Provider:      "google",
		ProviderToken: "good-token-2",
		Email:         "forged2@example.com",
		Name:          "Updated Name",
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec2.Code)
	}

	// Verify user was NOT updated (per implementation)
	user2, _ := s.GetUserByEmail(ctx, "verified@example.com")
	if user2.DisplayName != "Provider Name" {
		t.Errorf("expected display name 'Provider Name', got %q", user2.DisplayName)
	}

	// 3. Missing fields
	body3 := AuthLoginRequest{
		Provider: "google",
		// Missing ProviderToken
	}
	rec3 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing fields, got %d", rec3.Code)
	}

	// 4. Invalid provider token
	body4 := AuthLoginRequest{
		Provider:      "google",
		ProviderToken: "bad-token",
	}
	rec4 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body4)
	if rec4.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for invalid provider token, got %d: %s", rec4.Code, rec4.Body.String())
	}
}

func TestAuthMe(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user
	user := &store.User{
		ID:          tid("user_123"),
		Email:       "me@example.com",
		DisplayName: "Me",
		Role:        "admin",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Generate a token for this user
	token, _, _, _ := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)

	// Call /auth/me with the token
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != user.ID {
		t.Errorf("expected ID %q, got %q", user.ID, resp.ID)
	}
	if resp.Email != user.Email {
		t.Errorf("expected email %q, got %q", user.Email, resp.Email)
	}
}

// TestAuthRefreshRoleReevaluation covers the role re-evaluation that happens on
// every token refresh. The admin_emails config is additive-only: it promotes,
// but it never demotes a user who holds admin in the store.
func TestAuthRefreshRoleReevaluation(t *testing.T) {
	refresh := func(t *testing.T, srv *Server, refreshToken string) string {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/refresh",
			AuthRefreshRequest{RefreshToken: refreshToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp AuthRefreshResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		claims, err := srv.userTokenService.ValidateUserToken(resp.AccessToken)
		if err != nil {
			t.Fatalf("failed to validate refreshed access token: %v", err)
		}
		return claims.Role
	}

	t.Run("UI-promoted admin keeps admin role", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		// User is admin in the store (promoted via the admin UI) but is not
		// listed in admin_emails.
		user := &store.User{
			ID:      tid("user_ui_admin"),
			Email:   "ui-admin@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		if role := refresh(t, srv, refreshToken); role != "admin" {
			t.Errorf("expected refreshed token role 'admin', got %q", role)
		}

		stored, err := s.GetUserByEmail(ctx, user.Email)
		if err != nil {
			t.Fatalf("user not found: %v", err)
		}
		if stored.Role != "admin" {
			t.Errorf("expected stored role 'admin', got %q", stored.Role)
		}
	})

	t.Run("admin emails promotes member on refresh", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_promoted"),
			Email:   "promoted@example.com",
			Role:    "member",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = []string{"promoted@example.com"}

		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		if role := refresh(t, srv, refreshToken); role != "admin" {
			t.Errorf("expected refreshed token role 'admin', got %q", role)
		}

		stored, err := s.GetUserByEmail(ctx, user.Email)
		if err != nil {
			t.Fatalf("user not found: %v", err)
		}
		if stored.Role != "admin" {
			t.Errorf("expected stored role 'admin', got %q", stored.Role)
		}
	})

	t.Run("UI demotion is reflected in refreshed token", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_demoted"),
			Email:   "demoted@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		// Token still carries the stale "admin" role.
		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, "admin", ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		// Explicit demotion through the admin UI/API.
		user.Role = "member"
		if err := s.UpdateUser(ctx, user); err != nil {
			t.Fatalf("failed to demote user: %v", err)
		}

		if role := refresh(t, srv, refreshToken); role != "member" {
			t.Errorf("expected refreshed token role 'member', got %q", role)
		}
	})

	t.Run("UI-set viewer keeps viewer role", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_viewer"),
			Email:   "viewer@example.com",
			Role:    "viewer",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		if role := refresh(t, srv, refreshToken); role != "viewer" {
			t.Errorf("expected refreshed token role 'viewer', got %q", role)
		}

		stored, err := s.GetUserByEmail(ctx, user.Email)
		if err != nil {
			t.Fatalf("user not found: %v", err)
		}
		if stored.Role != "viewer" {
			t.Errorf("expected stored role 'viewer', got %q", stored.Role)
		}
	})

	t.Run("deleted admin cannot refresh into admin", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_deleted"),
			Email:   "deleted-admin@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		// Token carries the admin role the user held before offboarding.
		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, "admin", ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		if err := s.DeleteUser(ctx, user.ID); err != nil {
			t.Fatalf("failed to delete user: %v", err)
		}

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/refresh",
			AuthRefreshRequest{RefreshToken: refreshToken})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected status 401 for deleted user, got %d: %s", rec.Code, rec.Body.String())
		}

		// Belt and braces: even if the handler were changed to keep issuing
		// tokens, it must never hand back the admin role from the claim.
		if rec.Code == http.StatusOK {
			var resp AuthRefreshResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			claims, err := srv.userTokenService.ValidateUserToken(resp.AccessToken)
			if err != nil {
				t.Fatalf("failed to validate refreshed access token: %v", err)
			}
			if claims.Role == "admin" {
				t.Error("deleted user retained admin role from the JWT claim")
			}
		}
	})

	t.Run("suspended user cannot refresh", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_suspended"),
			Email:   "suspended@example.com",
			Role:    "admin",
			Status:  "suspended",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, "admin", ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/refresh",
			AuthRefreshRequest{RefreshToken: refreshToken})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected status 403 for suspended user, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("store error degrades to config-only role", func(t *testing.T) {
		// A transient store failure must not let the token's own role claim
		// stand in for the stored role: the refresh chain rotates, so trusting
		// claims.Role here would let a stale admin renew itself indefinitely.
		// The handler falls back to config-only evaluation instead.
		srv, s := testServer(t)
		ctx := context.Background()

		user := &store.User{
			ID:      tid("user_store_error"),
			Email:   "store-error@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		// Token carries the admin role the user currently holds in the store.
		_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, "admin", ClientTypeWeb,
		)
		if err != nil {
			t.Fatalf("failed to generate tokens: %v", err)
		}

		// The lookup now fails for a reason other than "not found" — a DB blip
		// rather than an offboarded user.
		srv.store = &failingUserLookupStore{Store: s, err: errors.New("database is locked")}

		if role := refresh(t, srv, refreshToken); role != "member" {
			t.Errorf("expected refreshed token role 'member' (config-only fallback), got %q", role)
		}
	})
}

// failingUserLookupStore wraps a store and makes the by-email user lookup fail
// with a non-ErrNotFound error, simulating a transient database failure.
type failingUserLookupStore struct {
	store.Store
	err error
}

func (f *failingUserLookupStore) GetUserByEmail(context.Context, string) (*store.User, error) {
	return nil, f.err
}

func TestAuthValidate(t *testing.T) {
	srv, s := testServer(t)

	if srv.userTokenService == nil {
		t.Fatal("userTokenService not initialized")
	}

	// The token's subject must have a user record to be reported valid.
	userID := tid("auth-validate-user")
	if err := s.CreateUser(context.Background(), &store.User{
		ID:          userID,
		Email:       "test@example.com",
		DisplayName: "Test",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Generate a token
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		userID, "test@example.com", "Test", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// Validate valid token
	body := AuthValidateRequest{Token: token}
	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AuthValidateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Valid {
		t.Error("expected token to be valid")
	}
	if resp.User == nil {
		t.Fatal("expected user to be set in response")
	}
	if resp.User.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", resp.User.Email)
	}

	// Validate invalid token
	body2 := AuthValidateRequest{Token: "invalid-token"}
	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", body2)

	var resp2 AuthValidateResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp2.Valid {
		t.Error("expected token to be invalid")
	}
}

// failingGetUserStore wraps a store and makes the by-ID user lookup return
// no user and the given error (which may be nil).
type failingGetUserStore struct {
	store.Store
	err error
}

func (f *failingGetUserStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, f.err
}

// TestAuthValidate_UserRecordStatus verifies that /auth/validate applies the
// same user-record check as hub JWT authentication: a live user's token is
// valid, while a deleted or suspended user's token is reported invalid, and
// a store failure is reported as 503 store_error.
func TestAuthValidate_UserRecordStatus(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	newUserToken := func(t *testing.T, name string) (*store.User, string) {
		t.Helper()
		u := &store.User{
			ID:          tid(name),
			Email:       name + "@test.com",
			DisplayName: name,
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		token, _, _, err := srv.userTokenService.GenerateTokenPair(
			u.ID, u.Email, u.DisplayName, u.Role, ClientTypeCLI)
		require.NoError(t, err)
		return u, token
	}
	validate := func(t *testing.T, token string) (int, AuthValidateResponse, string) {
		t.Helper()
		rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: token})
		var resp AuthValidateResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		}
		return rec.Code, resp, rec.Body.String()
	}

	t.Run("live user", func(t *testing.T) {
		u, token := newUserToken(t, "validate-live")
		code, resp, body := validate(t, token)
		require.Equal(t, http.StatusOK, code, body)
		assert.True(t, resp.Valid, body)
		require.NotNil(t, resp.User)
		assert.Equal(t, u.ID, resp.User.ID)
	})

	t.Run("deleted user", func(t *testing.T) {
		u, token := newUserToken(t, "validate-deleted")
		code, resp, body := validate(t, token)
		require.Equal(t, http.StatusOK, code, body)
		require.True(t, resp.Valid, "control: valid before delete: %s", body)

		require.NoError(t, s.DeleteUser(ctx, u.ID))
		code, resp, body = validate(t, token)
		require.Equal(t, http.StatusOK, code, body)
		assert.False(t, resp.Valid, "deleted user's token must not be valid: %s", body)
		assert.Nil(t, resp.User)
	})

	t.Run("suspended user", func(t *testing.T) {
		u, token := newUserToken(t, "validate-suspended")
		u.Status = store.UserStatusSuspended
		require.NoError(t, s.UpdateUser(ctx, u))
		code, resp, body := validate(t, token)
		require.Equal(t, http.StatusOK, code, body)
		assert.False(t, resp.Valid, "suspended user's token must not be valid: %s", body)
		assert.Nil(t, resp.User)
	})

	t.Run("store error", func(t *testing.T) {
		_, token := newUserToken(t, "validate-store-error")
		orig := srv.store
		srv.store = &failingGetUserStore{Store: s, err: errors.New("database is locked")}
		t.Cleanup(func() { srv.store = orig })
		code, _, body := validate(t, token)
		assert.Equal(t, http.StatusServiceUnavailable, code, body)
		assert.Contains(t, body, "store_error")
	})

	t.Run("nil user without error", func(t *testing.T) {
		_, token := newUserToken(t, "validate-nil-user")
		orig := srv.store
		srv.store = &failingGetUserStore{Store: s, err: nil}
		t.Cleanup(func() { srv.store = orig })
		code, resp, body := validate(t, token)
		require.Equal(t, http.StatusOK, code, body)
		assert.False(t, resp.Valid, "a token with no user record must not be valid: %s", body)
		assert.Nil(t, resp.User)
	})
}

func TestAuthToken(t *testing.T) {
	srv, _ := testServer(t)

	// 1. Missing required fields - code
	body1 := AuthTokenRequest{
		RedirectURI: "http://localhost:8080/callback",
		GrantType:   "authorization_code",
		Provider:    "google",
	}
	rec1 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body1)
	if rec1.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing code, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Missing required fields - redirectUri
	body2 := AuthTokenRequest{
		Code:      "test-code",
		GrantType: "authorization_code",
		Provider:  "google",
	}
	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing redirectUri, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. Missing required fields - grantType
	body3 := AuthTokenRequest{
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/callback",
		Provider:    "google",
	}
	rec3 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing grantType, got %d: %s", rec3.Code, rec3.Body.String())
	}

	// 4. Invalid grant type
	body4 := AuthTokenRequest{
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/callback",
		GrantType:   "client_credentials",
		Provider:    "google",
	}
	rec4 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body4)
	if rec4.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for unsupported grant type, got %d: %s", rec4.Code, rec4.Body.String())
	}
	// Verify error message
	var errResp4 struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(rec4.Body).Decode(&errResp4); err == nil {
		if errResp4.Message != "unsupported grant type" {
			t.Errorf("expected 'unsupported grant type' message, got %q", errResp4.Message)
		}
	}

	// 5. Invalid provider
	body5 := AuthTokenRequest{
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/callback",
		GrantType:   "authorization_code",
		Provider:    "facebook", // not supported
	}
	rec5 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body5)
	if rec5.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid provider, got %d: %s", rec5.Code, rec5.Body.String())
	}
	// Verify error code
	var errResp5 struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(rec5.Body).Decode(&errResp5); err == nil {
		if errResp5.Error != "invalid_provider" {
			t.Errorf("expected 'invalid_provider' error code, got %q", errResp5.Error)
		}
	}

	// 6. OAuth service not configured (default test server has no OAuth)
	body6 := AuthTokenRequest{
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/callback",
		GrantType:   "authorization_code",
		Provider:    "google",
	}
	rec6 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body6)
	if rec6.Code != http.StatusNotImplemented {
		t.Errorf("expected status 501 when OAuth not configured, got %d: %s", rec6.Code, rec6.Body.String())
	}
	// Verify error code
	var errResp6 struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(rec6.Body).Decode(&errResp6); err == nil {
		if errResp6.Error != "not_implemented" {
			t.Errorf("expected 'not_implemented' error code, got %q", errResp6.Error)
		}
	}
}

func TestAuthTokenProviderInference(t *testing.T) {
	srv, _ := testServer(t)

	// Test provider inference from redirect URI containing "github"
	body := AuthTokenRequest{
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/auth/callback/github",
		GrantType:   "authorization_code",
		// Provider not specified - should be inferred as "github"
	}
	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/token", body)

	// Should fail with "not_implemented" because OAuth is not configured,
	// but importantly, it should NOT fail with "invalid_provider"
	// This confirms the provider was correctly inferred as "github"
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("expected status 501 (OAuth not configured), got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err == nil {
		if errResp.Error == "invalid_provider" {
			t.Error("provider should have been inferred as 'github', but got 'invalid_provider' error")
		}
	}
}

func TestCLIDeviceAuthorize_OAuthNotConfigured(t *testing.T) {
	srv, _ := testServer(t)

	body := CLIDeviceAuthorizeRequest{Provider: "google"}
	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/cli/device", body)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("expected status 501 when OAuth not configured, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCLIDeviceAuthorize_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/cli/device", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for GET, got %d", rec.Code)
	}
}

func TestCLIAuthProviders_ReturnsConfiguredProviders(t *testing.T) {
	srv, _ := testServer(t)
	srv.oauthService = NewOAuthService(OAuthConfig{
		CLI: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "cli-gh-id",
				ClientSecret: "cli-gh-secret",
			},
		},
		Device: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "device-gh-id",
				ClientSecret: "device-gh-secret",
			},
		},
	}, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/auth/providers?clientType=device", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CLIAuthProvidersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ClientType != "device" {
		t.Fatalf("expected clientType device, got %q", resp.ClientType)
	}
	if len(resp.Providers) != 1 || resp.Providers[0] != "github" {
		t.Fatalf("expected providers [github], got %v", resp.Providers)
	}
}

func TestCLIAuthProviders_InvalidClientType(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/auth/providers?clientType=desktop", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCLIDeviceToken_MissingDeviceCode(t *testing.T) {
	srv, _ := testServer(t)

	body := CLIDeviceTokenRequest{Provider: "google"}
	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/cli/device/token", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing deviceCode, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCLIDeviceToken_OAuthNotConfigured(t *testing.T) {
	srv, _ := testServer(t)

	body := CLIDeviceTokenRequest{DeviceCode: "test-code", Provider: "google"}
	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/cli/device/token", body)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("expected status 501 when OAuth not configured, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCLIDeviceToken_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/cli/device/token", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for GET, got %d", rec.Code)
	}
}

func TestProvisionUser(t *testing.T) {
	ctx := context.Background()

	t.Run("creates new user", func(t *testing.T) {
		srv, s := testServer(t)

		info := &ExternalUserInfo{
			Email:       "new@example.com",
			DisplayName: "New User",
			AvatarURL:   "https://example.com/avatar.png",
		}

		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user.Email != "new@example.com" {
			t.Errorf("expected email new@example.com, got %q", user.Email)
		}
		if user.DisplayName != "New User" {
			t.Errorf("expected display name 'New User', got %q", user.DisplayName)
		}
		if user.AvatarURL != "https://example.com/avatar.png" {
			t.Errorf("expected avatar URL, got %q", user.AvatarURL)
		}
		if user.Status != "active" {
			t.Errorf("expected status 'active', got %q", user.Status)
		}
		if user.ID == "" {
			t.Error("expected non-empty user ID")
		}

		// Verify persisted in store
		stored, err := s.GetUserByEmail(ctx, "new@example.com")
		if err != nil {
			t.Fatalf("user not found in store: %v", err)
		}
		if stored.ID != user.ID {
			t.Errorf("stored user ID mismatch: %q vs %q", stored.ID, user.ID)
		}
	})

	t.Run("updates existing user last login", func(t *testing.T) {
		srv, s := testServer(t)

		// Pre-create user
		original := &store.User{
			ID:          generateID(),
			Email:       "existing@example.com",
			DisplayName: "Original Name",
			AvatarURL:   "https://example.com/original.png",
			Role:        "member",
			Status:      "active",
			Created:     time.Now().Add(-24 * time.Hour),
			LastLogin:   time.Now().Add(-24 * time.Hour),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}

		beforeLogin := time.Now()
		info := &ExternalUserInfo{
			Email:       "existing@example.com",
			DisplayName: "Updated Name",
			AvatarURL:   "https://example.com/updated.png",
		}

		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// LastLogin should be updated
		if user.LastLogin.Before(beforeLogin) {
			t.Error("expected LastLogin to be updated")
		}
		// DisplayName should NOT be updated (original was non-empty)
		if user.DisplayName != "Original Name" {
			t.Errorf("expected display name 'Original Name', got %q", user.DisplayName)
		}
		// AvatarURL should NOT be updated (original was non-empty)
		if user.AvatarURL != "https://example.com/original.png" {
			t.Errorf("expected original avatar URL, got %q", user.AvatarURL)
		}
	})

	t.Run("backfills empty display name and avatar", func(t *testing.T) {
		srv, s := testServer(t)

		// Pre-create user with empty display name and avatar
		original := &store.User{
			ID:      generateID(),
			Email:   "backfill@example.com",
			Role:    "member",
			Status:  "active",
			Created: time.Now().Add(-1 * time.Hour),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}

		info := &ExternalUserInfo{
			Email:       "backfill@example.com",
			DisplayName: "Backfilled Name",
			AvatarURL:   "https://example.com/backfilled.png",
		}

		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if user.DisplayName != "Backfilled Name" {
			t.Errorf("expected backfilled display name, got %q", user.DisplayName)
		}
		if user.AvatarURL != "https://example.com/backfilled.png" {
			t.Errorf("expected backfilled avatar URL, got %q", user.AvatarURL)
		}
	})

	t.Run("promotes member to admin when config changes", func(t *testing.T) {
		srv, s := testServer(t)

		// Pre-create user as member
		original := &store.User{
			ID:      generateID(),
			Email:   "admin@example.com",
			Role:    "member",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}

		// Configure server to recognize this email as admin
		srv.config.AdminEmails = []string{"admin@example.com"}

		info := &ExternalUserInfo{Email: "admin@example.com"}
		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if user.Role != "admin" {
			t.Errorf("expected role 'admin', got %q", user.Role)
		}
	})

	// admin_emails is additive-only: it can promote a user to admin but must
	// never demote one. A user promoted through the admin UI (or an admin whose
	// email was removed from the config) keeps their role across logins.
	t.Run("D11: demotes admin when removed from admin emails", func(t *testing.T) {
		srv, s := testServer(t)
		srv.demotionSafe.Store(true) // reconciler says demotion is safe

		// Pre-create user as admin (e.g. promoted via the admin UI or config)
		original := &store.User{
			ID:      generateID(),
			Email:   "ui-admin@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}

		// Admin emails list does NOT include this user — D11 requires demotion.
		srv.config.AdminEmails = []string{"other-admin@example.com"}

		info := &ExternalUserInfo{Email: "ui-admin@example.com"}
		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if user.Role != "member" {
			t.Errorf("expected role 'member' after D11 demotion, got %q", user.Role)
		}

		// Verify persisted in store
		stored, err := s.GetUserByEmail(ctx, "ui-admin@example.com")
		if err != nil {
			t.Fatalf("user not found in store: %v", err)
		}
		if stored.Role != "member" {
			t.Errorf("expected stored role 'member' after D11 demotion, got %q", stored.Role)
		}
	})

	t.Run("keeps member role when not in admin emails", func(t *testing.T) {
		srv, s := testServer(t)

		original := &store.User{
			ID:      generateID(),
			Email:   "plain-member@example.com",
			Role:    "member",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}

		srv.config.AdminEmails = []string{"other-admin@example.com"}

		info := &ExternalUserInfo{Email: "plain-member@example.com"}
		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if user.Role != "member" {
			t.Errorf("expected role 'member', got %q", user.Role)
		}
	})

	t.Run("respects UI demotion of a config-less admin", func(t *testing.T) {
		srv, s := testServer(t)

		original := &store.User{
			ID:      generateID(),
			Email:   "demoted@example.com",
			Role:    "admin",
			Status:  "active",
			Created: time.Now(),
		}
		if err := s.CreateUser(ctx, original); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		srv.config.AdminEmails = nil

		// Explicit demotion through the admin UI/API writes "member" to the DB.
		original.Role = "member"
		if err := s.UpdateUser(ctx, original); err != nil {
			t.Fatalf("failed to demote user: %v", err)
		}

		info := &ExternalUserInfo{Email: "demoted@example.com"}
		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if user.Role != "member" {
			t.Errorf("expected demotion to stick, got role %q", user.Role)
		}
	})

	t.Run("returns ErrAccessDenied for unauthorized domain", func(t *testing.T) {
		srv, _ := testServer(t)

		// Configure domain restriction
		srv.config.AuthorizedDomains = []string{"allowed.com"}
		srv.config.UserAccessMode = "domain_restricted"

		info := &ExternalUserInfo{Email: "user@forbidden.com"}
		_, err := srv.provisionUser(ctx, info)
		if !errors.Is(err, ErrAccessDenied) {
			t.Errorf("expected ErrAccessDenied, got %v", err)
		}
	})

	t.Run("returns ErrAccessDenied for invite-only mode", func(t *testing.T) {
		srv, _ := testServer(t)

		// Configure invite-only mode (user not on allow list)
		srv.config.UserAccessMode = "invite_only"

		info := &ExternalUserInfo{Email: "user@example.com"}
		_, err := srv.provisionUser(ctx, info)
		if !errors.Is(err, ErrAccessDenied) {
			t.Errorf("expected ErrAccessDenied, got %v", err)
		}
	})

	t.Run("admin bypasses domain restriction", func(t *testing.T) {
		srv, _ := testServer(t)

		// Configure domain restriction but also add admin email
		srv.config.AuthorizedDomains = []string{"allowed.com"}
		srv.config.UserAccessMode = "domain_restricted"
		srv.config.AdminEmails = []string{"admin@other.com"}

		info := &ExternalUserInfo{Email: "admin@other.com"}
		user, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("expected admin bypass, got error: %v", err)
		}
		if user.Role != "admin" {
			t.Errorf("expected role 'admin', got %q", user.Role)
		}
	})

	t.Run("idempotent - calling twice does not duplicate", func(t *testing.T) {
		srv, s := testServer(t)

		info := &ExternalUserInfo{
			Email:       "idempotent@example.com",
			DisplayName: "First Call",
		}

		user1, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("first call failed: %v", err)
		}

		user2, err := srv.provisionUser(ctx, info)
		if err != nil {
			t.Fatalf("second call failed: %v", err)
		}

		if user1.ID != user2.ID {
			t.Errorf("expected same user ID across calls, got %q and %q", user1.ID, user2.ID)
		}

		// Verify only one user exists
		u, err := s.GetUserByEmail(ctx, "idempotent@example.com")
		if err != nil {
			t.Fatalf("user not found: %v", err)
		}
		if u.ID != user1.ID {
			t.Error("store user ID does not match")
		}
	})
}

// TestColdStartSuperAdminBinding verifies that when the first admin user logs
// in on a fresh hub (empty user store), provisionUser creates both the
// User.Role="admin" AND the system-scoped super-admin RoleBinding immediately.
// Without the fix, ReconcileSuperAdminBindings runs at startup against an empty
// store, finds no matching users, and the binding is never created until the
// next restart.
func TestColdStartSuperAdminBinding(t *testing.T) {
	ctx := context.Background()

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-token"
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	// Configure the admin email BEFORE server creation. On a fresh start
	// ReconcileSuperAdminBindings will find zero users and create nothing.
	cfg.AdminEmails = []string{"first-admin@example.com"}
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() {
		_ = srv.Shutdown(ctx)
		_ = s.Close()
	})

	// Simulate the cold-start scenario: the admin user does not exist yet.
	// ReconcileSuperAdminBindings already ran at server startup and found no
	// matching users. Now the admin logs in for the first time.
	user, err := srv.provisionUser(ctx, &ExternalUserInfo{
		Email:       "first-admin@example.com",
		DisplayName: "First Admin",
	})
	if err != nil {
		t.Fatalf("provisionUser: %v", err)
	}

	// The user should have been assigned the admin role.
	if user.Role != "admin" {
		t.Fatalf("expected role 'admin', got %q", user.Role)
	}

	// Key assertion: the super-admin RoleBinding must exist IMMEDIATELY,
	// without requiring a second ReconcileSuperAdminBindings call or a hub
	// restart. This is the cold-start deadlock that this fix addresses.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("super-admin role definition not found: %v", err)
	}

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	if err != nil {
		t.Fatalf("failed to list bindings: %v", err)
	}

	var found bool
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID && b.ScopeType == store.RoleScopeSystem {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("cold-start deadlock: super-admin RoleBinding was NOT created at first login — " +
			"IsSystemAdmin will return false until the next hub restart")
	}

	// Double-check via AuthzService to confirm the full auth stack works.
	if !srv.authzService.IsSystemAdmin(ctx, user.ID) {
		t.Fatal("IsSystemAdmin must return true for the first admin immediately after provisioning")
	}
}

// D11-fix2: After login-time demotion (admin removed from AdminEmails),
// IsSystemAdmin must return false because the super-admin binding is deleted.
func TestD11Fix2_LoginDemotionDeletesBinding(t *testing.T) {
	ctx := context.Background()

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-token"
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	// AdminEmails does NOT include the user we will test.
	cfg.AdminEmails = []string{"real-admin@test.com"}
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() {
		_ = srv.Shutdown(ctx)
		_ = s.Close()
	})
	// The reconciler at startup found no user matching AdminEmails (the user is
	// created below), so demotionSafe is false. Override to test login-time
	// demotion behaviour in isolation from the guard (tested separately).
	srv.demotionSafe.Store(true)

	// Pre-create a user with Role="admin" and a super-admin binding.
	userID := generateID()
	if err := s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "demoted@test.com",
		DisplayName: "Demoted",
		Role:        "admin",
		Status:      "active",
		Created:     time.Now().Add(-time.Hour),
		LastLogin:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get role definition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	}); err != nil {
		t.Fatalf("create role binding: %v", err)
	}

	// Sanity: IsSystemAdmin returns true before login.
	if !srv.authzService.IsSystemAdmin(ctx, userID) {
		t.Fatal("pre-condition: user should be system admin before login")
	}

	// Trigger login (provisionUser), which should demote and delete binding.
	user, err := srv.provisionUser(ctx, &ExternalUserInfo{
		Email:       "demoted@test.com",
		DisplayName: "Demoted",
	})
	if err != nil {
		t.Fatalf("provisionUser: %v", err)
	}
	if user.Role != "member" {
		t.Fatalf("expected role 'member' after demotion, got %q", user.Role)
	}

	// Key assertion: IsSystemAdmin must be false AFTER login.
	if srv.authzService.IsSystemAdmin(ctx, userID) {
		t.Fatal("IsSystemAdmin must return false after login-time demotion (binding should be deleted)")
	}
}

// =============================================================================
// handleAuthAdminStatus Tests
// =============================================================================

func TestHandleAuthAdminStatus_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/auth/admin-status", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for unauthenticated request, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthAdminStatus_SuperAdmin(t *testing.T) {
	srv, _ := testServer(t)

	// The dev user is created with Role="admin", so IsUnscopedLocalPlatformAdmin
	// returns true. Use the dev auth token to authenticate as a super-admin.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/admin-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AdminStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.IsAdmin {
		t.Error("expected isAdmin=true for super-admin")
	}
	if !resp.IsSuperAdmin {
		t.Error("expected isSuperAdmin=true for super-admin")
	}

	// Super-admin should receive all permission IDs from the registry.
	allIDs := allPermissionIDs()
	require.ElementsMatch(t, allIDs, resp.Permissions)
}

func TestHandleAuthAdminStatus_HubAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-super-admin user with a hub-admin role binding.
	userID := tid("hub-admin-handler")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "hubadmin-handler@test.com", DisplayName: "HubAdmin", Role: "member", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role definition must exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		userID, "hubadmin-handler@test.com", "HubAdmin", "member", ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/admin-status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AdminStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if !resp.IsAdmin {
		t.Error("expected isAdmin=true for hub-admin user")
	}
	if resp.IsSuperAdmin {
		t.Error("expected isSuperAdmin=false for hub-admin (non-super-admin) user")
	}

	// Hub-admin should have the curated hub-admin permission set.
	expectedPerms := hubAdminPermissionIDs()
	require.ElementsMatch(t, expectedPerms, resp.Permissions)
}

func TestHandleAuthAdminStatus_PlainMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("plain-member-handler")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "member-handler@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		userID, "member-handler@test.com", "Member", "member", ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/admin-status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AdminStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if resp.IsAdmin {
		t.Error("expected isAdmin=false for plain member")
	}
	if resp.IsSuperAdmin {
		t.Error("expected isSuperAdmin=false for plain member")
	}

	// Plain member should have an empty permissions array (not null).
	if resp.Permissions == nil {
		t.Error("expected permissions to be an empty array, got nil")
	}
	if len(resp.Permissions) != 0 {
		t.Errorf("expected 0 permissions for plain member, got %d: %v", len(resp.Permissions), resp.Permissions)
	}
}

func TestHandleAuthAdminStatus_CustomRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a custom role with only template permissions.
	customRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "template-manager",
		Description: "Can manage templates only",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"template.list", "template.read", "template.create", "template.update", "template.delete"},
		System:      false,
	})
	require.NoError(t, err)

	// Create a non-admin user and bind the custom role.
	userID := tid("custom-role-handler")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "customrole@test.com", DisplayName: "CustomRole", Role: "member", Status: "active",
	}))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		userID, "customrole@test.com", "CustomRole", "member", ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/admin-status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	var resp AdminStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Custom role with permissions → isAdmin=true, isSuperAdmin=false.
	if !resp.IsAdmin {
		t.Error("expected isAdmin=true for user with custom role permissions")
	}
	if resp.IsSuperAdmin {
		t.Error("expected isSuperAdmin=false for user with custom role")
	}

	// Permissions should contain exactly the template permission IDs.
	expectedPerms := []string{
		"template.list",
		"template.read",
		"template.create",
		"template.update",
		"template.delete",
	}
	require.ElementsMatch(t, expectedPerms, resp.Permissions)
}

func TestHandleAuthAdminStatus_PermissionsSerializedAsEmptyArray(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A plain member with no role bindings should serialize permissions as []
	// (JSON empty array), NOT null.
	userID := tid("empty-perms-handler")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "emptyperms@test.com", DisplayName: "EmptyPerms", Role: "member", Status: "active",
	}))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		userID, "emptyperms@test.com", "EmptyPerms", "member", ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/admin-status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	// Verify the raw JSON contains "permissions":[] not "permissions":null.
	body := rec.Body.String()
	if !strings.Contains(body, `"permissions":[]`) {
		t.Errorf("expected permissions to serialize as empty array [], got body: %s", body)
	}
}

func TestHandleAuthAdminStatus_WrongMethod(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/admin-status", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for POST, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Login paths: hub role grants follow the role, and invites get the default
// role at join time (design §5.C, §5.D; AC 1, 2, 4).
// ---------------------------------------------------------------------------

// newLoginGrantServer returns a server backed by a real, seeded store and
// configured with the given default role and admin emails. Login-time
// demotion is enabled (as after a successful startup reconcile).
func newLoginGrantServer(t *testing.T, defaultRole string, adminEmails []string) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.config.DefaultUserRole = defaultRole
	srv.config.AdminEmails = adminEmails
	srv.config.UserAccessMode = "open"
	srv.demotionSafe.Store(true)
	return srv, s
}

// createLoginGrantUser creates a user row directly in the store.
func createLoginGrantUser(t *testing.T, s store.Store, id, email, role, status string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(id),
		Email:       email,
		DisplayName: id,
		Role:        role,
		Status:      status,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// createSystemBinding creates a system-scoped role binding for the user.
func createSystemBinding(t *testing.T, s store.Store, userID, roleName, createdBy string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        createdBy,
	})
	require.NoError(t, err)
}

// assertViewerGrants checks the stored role, the grant state and the
// immediate authz effect for a viewer.
func assertViewerGrants(t *testing.T, srv *Server, s store.Store, email string) {
	t.Helper()
	u, err := s.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleViewer, u.Role)
	grants := observeHubRoleGrants(t, s, u.ID)
	assert.False(t, grants.InHubMembers, "viewer must not be in hub-members")
	assert.Equal(t, 1, grants.HubViewerBindings, "viewer must have one hub-viewer binding")
	assert.Equal(t, 0, grants.SuperAdminBinding, "viewer must not have a super-admin binding")
	assertHubRoleAccess(t, srv, s, u.ID, false)
}

// assertMemberGrants is the member counterpart of assertViewerGrants.
func assertMemberGrants(t *testing.T, srv *Server, s store.Store, email string) {
	t.Helper()
	u, err := s.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, u.Role)
	grants := observeHubRoleGrants(t, s, u.ID)
	assert.True(t, grants.InHubMembers, "member must be in hub-members")
	assert.Equal(t, 0, grants.HubViewerBindings, "member must not have a hub-viewer binding")
	assert.Equal(t, 0, grants.SuperAdminBinding, "member must not have a super-admin binding")
	assertHubRoleAccess(t, srv, s, u.ID, true)
}

// assertAdminGrants checks the stored role and super-admin binding for an
// admin, and that no hub-viewer binding is left behind.
func assertAdminGrants(t *testing.T, s store.Store, email string) {
	t.Helper()
	u, err := s.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleAdmin, u.Role)
	grants := observeHubRoleGrants(t, s, u.ID)
	assert.Equal(t, 1, grants.SuperAdminBinding, "admin must have a super-admin binding")
	assert.Equal(t, 0, grants.HubViewerBindings, "admin must not have a hub-viewer binding")
}

// --- provisionUser (hub OAuth / CLI login) ---

func TestProvisionUser_ViewerHasHubReadImmediately(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "newviewer@example.com"})
	require.NoError(t, err)

	// No BackfillRoleBindings: the grant must exist from the first request.
	assertViewerGrants(t, srv, s, "newviewer@example.com")
}

func TestProvisionUser_MemberDefaultHasProjectCreate(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleMember, []string{"boss@example.com"})

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "newmember@example.com"})
	require.NoError(t, err)

	assertMemberGrants(t, srv, s, "newmember@example.com")
}

func TestProvisionUser_AdminDemotedToViewerLosesMemberPerms(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})
	ctx := context.Background()

	// A config-derived admin who is also in hub-members (for example a
	// member who was later added to admin_emails).
	u := createLoginGrantUser(t, s, "demoted-admin", "demoted@example.com", store.UserRoleAdmin, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)
	require.NoError(t, ensureHubMembershipTx(ctx, s, u.ID))

	_, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "demoted@example.com"})
	require.NoError(t, err)

	assertViewerGrants(t, srv, s, "demoted@example.com")
}

func TestProvisionUser_AdminDemotedToMemberGetsHubMembers(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleMember, []string{"boss@example.com"})

	u := createLoginGrantUser(t, s, "demoted-admin-m", "demoted-m@example.com", store.UserRoleAdmin, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "demoted-m@example.com"})
	require.NoError(t, err)

	assertMemberGrants(t, srv, s, "demoted-m@example.com")
}

func TestProvisionUser_MemberLoginRemovesStaleHubViewerBinding(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})

	u := createLoginGrantUser(t, s, "stale-member", "stale-member@example.com", store.UserRoleMember, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleHubViewer, store.SystemBackfillCreatedBy)

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "stale-member@example.com"})
	require.NoError(t, err)

	// An existing member keeps their role when the default is viewer.
	assertMemberGrants(t, srv, s, "stale-member@example.com")
}

// --- provisionUser invited → active (design §5.C) ---

func TestProvisionUser_Invited_DefaultViewer_BecomesViewer(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})
	createLoginGrantUser(t, s, "inv-viewer", "inv-viewer@example.com", store.UserRoleMember, store.UserStatusInvited)

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "inv-viewer@example.com"})
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, user.Status)

	assertViewerGrants(t, srv, s, "inv-viewer@example.com")
}

func TestProvisionUser_Invited_DefaultMember_BecomesMember(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleMember, []string{"boss@example.com"})
	createLoginGrantUser(t, s, "inv-member", "inv-member@example.com", store.UserRoleMember, store.UserStatusInvited)

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "inv-member@example.com"})
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, user.Status)

	assertMemberGrants(t, srv, s, "inv-member@example.com")
}

func TestProvisionUser_Invited_InAdminEmails_BecomesAdmin(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"inv-admin@example.com"})
	createLoginGrantUser(t, s, "inv-admin", "inv-admin@example.com", store.UserRoleMember, store.UserStatusInvited)

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "inv-admin@example.com"})
	require.NoError(t, err)

	assertAdminGrants(t, s, "inv-admin@example.com")
}

func TestProvisionUser_Invited_UIPromotedAdmin_StaysAdmin(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})
	// A pending invite that was promoted through the admin UI before this
	// change: role admin plus an AdminAPICreatedBy super-admin binding.
	u := createLoginGrantUser(t, s, "inv-ui-admin", "inv-ui-admin@example.com", store.UserRoleAdmin, store.UserStatusInvited)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.AdminAPICreatedBy)

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "inv-ui-admin@example.com"})
	require.NoError(t, err)

	assertAdminGrants(t, s, "inv-ui-admin@example.com")
}

func TestProvisionUser_Invited_ConfigAdminPlaceholder_GetsDefault(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})
	// Role admin on an invited row without a UI-promoted binding is a
	// placeholder like any other: the user gets the current default.
	u := createLoginGrantUser(t, s, "inv-cfg-admin", "inv-cfg-admin@example.com", store.UserRoleAdmin, store.UserStatusInvited)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "inv-cfg-admin@example.com"})
	require.NoError(t, err)

	assertViewerGrants(t, srv, s, "inv-cfg-admin@example.com")
}

func TestProvisionUser_Invited_DefaultFlippedBeforeFirstLogin(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleMember, []string{"boss@example.com"})

	// The invite is created through the API while the default is member.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite",
		map[string]string{"email": "flipped@example.com"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	pending, err := s.GetUserByEmail(context.Background(), "flipped@example.com")
	require.NoError(t, err)
	require.Equal(t, store.UserStatusInvited, pending.Status)

	// The default is flipped before the invitee first signs in.
	srv.config.DefaultUserRole = store.UserRoleViewer

	_, err = srv.provisionUser(context.Background(), &ExternalUserInfo{Email: "flipped@example.com"})
	require.NoError(t, err)

	assertViewerGrants(t, srv, s, "flipped@example.com")
}

// --- token refresh ---

// refreshAs mints a refresh token for the stored user and exchanges it.
func refreshAs(t *testing.T, srv *Server, u *store.User, tokenRole string) {
	t.Helper()
	_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
		u.ID, u.Email, u.DisplayName, tokenRole, ClientTypeWeb,
	)
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		AuthRefreshRequest{RefreshToken: refreshToken})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAuthRefresh_AdminDemotedToViewerLosesMemberPerms(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"boss@example.com"})
	ctx := context.Background()

	u := createLoginGrantUser(t, s, "refresh-demoted", "refresh-demoted@example.com", store.UserRoleAdmin, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)
	require.NoError(t, ensureHubMembershipTx(ctx, s, u.ID))

	refreshAs(t, srv, u, store.UserRoleAdmin)

	assertViewerGrants(t, srv, s, "refresh-demoted@example.com")
}

func TestAuthRefresh_PromotedToAdminGetsSuperAdminBinding(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleViewer, []string{"refresh-promoted@example.com"})

	u := createLoginGrantUser(t, s, "refresh-promoted", "refresh-promoted@example.com", store.UserRoleViewer, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleHubViewer, store.SystemReconcileCreatedBy)

	refreshAs(t, srv, u, store.UserRoleViewer)

	assertAdminGrants(t, s, "refresh-promoted@example.com")
}

func TestAuthRefresh_AdminDemotedToMemberGetsHubMembers(t *testing.T) {
	// A config admin removed from admin_emails is demoted to the member
	// default on refresh: super-admin binding removed, hub-members added.
	srv, s := newLoginGrantServer(t, store.UserRoleMember, []string{"boss@example.com"})

	u := createLoginGrantUser(t, s, "refresh-claim", "refresh-claim@example.com", store.UserRoleAdmin, store.UserStatusActive)
	createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)

	refreshAs(t, srv, u, store.UserRoleAdmin)

	assertMemberGrants(t, srv, s, "refresh-claim@example.com")
}

// --- web proxy login and web OAuth callback (real store) ---

// webLoginSettings returns live access settings for the web login tests.
func webLoginSettings(defaultRole string, adminEmails ...string) *staticAccessSettings {
	return &staticAccessSettings{adminEmails: adminEmails, defaultUserRole: defaultRole}
}

// webProxyLogin performs one proxy-auth request for email against a
// WebServer backed by s.
func webProxyLogin(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer)) {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode: "proxy",
		ProxyAuthenticator: &mockProxyAuthenticator{user: &ProxyUserInfo{
			Subject: "sub-" + email,
			Email:   email,
			Domain:  "example.com",
		}},
	})
	ws.SetAccessSettingsProvider(settings)
	ws.SetStore(s)
	var safe atomic.Bool
	safe.Store(true)
	ws.SetDemotionSafe(&safe)
	for _, o := range opts {
		o(ws)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
}

// webOAuthLogin runs the web OAuth callback for email against a WebServer
// backed by s.
func webOAuthLogin(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer)) {
	t.Helper()
	const secret = "test-session-secret-for-login-grant-tests-1234567890"
	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: secret,
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	}, nil)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"id-` + email + `","email":"` + email + `","verified_email":true,"name":"OAuth User"}`,
		},
	}
	ws.SetStore(s)
	ws.SetAccessSettingsProvider(settings)
	var safe atomic.Bool
	safe.Store(true)
	ws.SetDemotionSafe(&safe)
	for _, o := range opts {
		o(ws)
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	const oauthState = "test-state-login-grants"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=test-code&state="+oauthState, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Header().Get("Location"), "error=", "OAuth callback must succeed")
}

// webLoginPaths runs each web login test against both web login paths.
var webLoginPaths = []struct {
	name  string
	login func(t *testing.T, s store.Store, settings *staticAccessSettings, email string, opts ...func(*WebServer))
}{
	{"proxy", webProxyLogin},
	{"oauth", webOAuthLogin},
}

func TestWebLogin_ViewerHasHubReadImmediately(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			p.login(t, s, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-viewer@example.com")
			assertViewerGrants(t, srv, s, "web-viewer@example.com")
		})
	}
}

func TestWebLogin_MemberDefaultHasProjectCreate(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleMember, nil)
			p.login(t, s, webLoginSettings(store.UserRoleMember, "boss@example.com"), "web-member@example.com")
			assertMemberGrants(t, srv, s, "web-member@example.com")
		})
	}
}

func TestWebLogin_AdminDemotedToViewerLosesMemberPerms(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			u := createLoginGrantUser(t, s, "web-demoted", "web-demoted@example.com", store.UserRoleAdmin, store.UserStatusActive)
			createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)
			require.NoError(t, ensureHubMembershipTx(context.Background(), s, u.ID))

			p.login(t, s, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-demoted@example.com")

			assertViewerGrants(t, srv, s, "web-demoted@example.com")
		})
	}
}

func TestWebLogin_Invited_DefaultViewer_BecomesViewer(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			createLoginGrantUser(t, s, "web-inv-viewer", "web-inv-viewer@example.com", store.UserRoleMember, store.UserStatusInvited)

			p.login(t, s, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-inv-viewer@example.com")

			u, err := s.GetUserByEmail(context.Background(), "web-inv-viewer@example.com")
			require.NoError(t, err)
			assert.Equal(t, store.UserStatusActive, u.Status)
			assertViewerGrants(t, srv, s, "web-inv-viewer@example.com")
		})
	}
}

func TestWebLogin_Invited_DefaultMember_BecomesMember(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleMember, nil)
			createLoginGrantUser(t, s, "web-inv-member", "web-inv-member@example.com", store.UserRoleMember, store.UserStatusInvited)

			p.login(t, s, webLoginSettings(store.UserRoleMember, "boss@example.com"), "web-inv-member@example.com")

			assertMemberGrants(t, srv, s, "web-inv-member@example.com")
		})
	}
}

func TestWebLogin_Invited_InAdminEmails_BecomesAdmin(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			_, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			createLoginGrantUser(t, s, "web-inv-admin", "web-inv-admin@example.com", store.UserRoleMember, store.UserStatusInvited)

			p.login(t, s, webLoginSettings(store.UserRoleViewer, "web-inv-admin@example.com"), "web-inv-admin@example.com")

			assertAdminGrants(t, s, "web-inv-admin@example.com")
		})
	}
}

func TestWebLogin_Invited_UIPromotedAdmin_StaysAdmin(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			_, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			u := createLoginGrantUser(t, s, "web-inv-ui-admin", "web-inv-ui-admin@example.com", store.UserRoleAdmin, store.UserStatusInvited)
			createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.AdminAPICreatedBy)

			p.login(t, s, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-inv-ui-admin@example.com")

			assertAdminGrants(t, s, "web-inv-ui-admin@example.com")
		})
	}
}

func TestWebLogin_Invited_ConfigAdminPlaceholder_GetsDefault(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			u := createLoginGrantUser(t, s, "web-inv-cfg-admin", "web-inv-cfg-admin@example.com", store.UserRoleAdmin, store.UserStatusInvited)
			createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)

			p.login(t, s, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-inv-cfg-admin@example.com")

			assertViewerGrants(t, srv, s, "web-inv-cfg-admin@example.com")
		})
	}
}

// --- dev test-login endpoint (real store) ---

// webTestLogin calls the dev-only test-login endpoint for email with role
// against a WebServer backed by s.
func webTestLogin(t *testing.T, s store.Store, email, role string) {
	t.Helper()
	ws := NewWebServer(WebServerConfig{EnableTestLogin: true})
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(s)

	body := `{"email":"` + email + `","role":"` + role + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, tokenSvc))
	rec := httptest.NewRecorder()
	ws.handleTestLogin(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestHandleTestLogin_GrantsFollowRole(t *testing.T) {
	srv, s := newLoginGrantServer(t, store.UserRoleMember, nil)
	const email = "testlogin@example.com"

	webTestLogin(t, s, email, store.UserRoleViewer)
	assertViewerGrants(t, srv, s, email)

	webTestLogin(t, s, email, store.UserRoleMember)
	assertMemberGrants(t, srv, s, email)

	webTestLogin(t, s, email, store.UserRoleViewer)
	assertViewerGrants(t, srv, s, email)
}

// failingUpdateUserStore wraps a real store and fails every UpdateUser call,
// so tests can exercise the login paths' "UpdateUser failed" branches.
type failingUpdateUserStore struct {
	store.Store
}

func (f *failingUpdateUserStore) UpdateUser(context.Context, *store.User) error {
	return errors.New("injected UpdateUser failure")
}

// When UpdateUser fails, the web login paths must skip the grant sync (and
// the super-admin change) so hub grants keep following the persisted role,
// and the login itself must still complete.
func TestWebLogin_UpdateUserFailure_SkipsGrantSync(t *testing.T) {
	for _, p := range webLoginPaths {
		t.Run(p.name, func(t *testing.T) {
			_, s := newLoginGrantServer(t, store.UserRoleViewer, nil)
			u := createLoginGrantUser(t, s, "web-upd-fail", "web-upd-fail@example.com", store.UserRoleAdmin, store.UserStatusActive)
			createSystemBinding(t, s, u.ID, store.SystemRoleSuperAdmin, store.SystemReconcileCreatedBy)
			require.NoError(t, ensureHubMembershipTx(context.Background(), s, u.ID))
			before := observeHubRoleGrants(t, s, u.ID)
			require.Equal(t, hubRoleGrantState{InHubMembers: true, SuperAdminBinding: 1}, before)

			// Admin not in admin_emails with default viewer: the login computes a
			// demotion to viewer, but persisting it fails.
			p.login(t, &failingUpdateUserStore{Store: s}, webLoginSettings(store.UserRoleViewer, "boss@example.com"), "web-upd-fail@example.com")

			stored, err := s.GetUser(context.Background(), u.ID)
			require.NoError(t, err)
			assert.Equal(t, store.UserRoleAdmin, stored.Role, "role update was injected to fail")
			assert.Equal(t, before, observeHubRoleGrants(t, s, u.ID),
				"grants must follow the persisted role when UpdateUser fails")
		})
	}
}
