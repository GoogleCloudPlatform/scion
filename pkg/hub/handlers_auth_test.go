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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
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
		ID:          "user_123",
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

func TestAuthValidate(t *testing.T) {
	srv, _ := testServer(t)

	if srv.userTokenService == nil {
		t.Fatal("userTokenService not initialized")
	}

	// Generate a token
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		"user_1", "test@example.com", "Test", "member", ClientTypeWeb,
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
	})

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
