package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

func TestAuthLogin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// 1. Successful login (new user)
	body := AuthLoginRequest{
		Provider:      "google",
		ProviderToken: "dummy-token",
		Email:         "new@example.com",
		Name:          "New User",
		Avatar:        "https://example.com/avatar.png",
	}

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AuthLoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.User.Email != "new@example.com" {
		t.Errorf("expected email 'new@example.com', got %q", resp.User.Email)
	}

	if resp.AccessToken == "" {
		t.Error("expected access token to be set")
	}

	// Verify user was created in store
	user, err := s.GetUserByEmail(ctx, "new@example.com")
	if err != nil {
		t.Fatalf("failed to get user from store: %v", err)
	}
	if user.DisplayName != "New User" {
		t.Errorf("expected display name 'New User', got %q", user.DisplayName)
	}

	// 2. Successful login (existing user) - DisplayName should NOT be updated if already set
	body2 := AuthLoginRequest{
		Provider:      "google",
		ProviderToken: "dummy-token-2",
		Email:         "new@example.com",
		Name:          "Updated Name",
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec2.Code)
	}

	// Verify user was NOT updated (per implementation)
	user2, _ := s.GetUserByEmail(ctx, "new@example.com")
	if user2.DisplayName != "New User" {
		t.Errorf("expected display name 'New User', got %q", user2.DisplayName)
	}

	// 3. Missing fields
	body3 := AuthLoginRequest{
		Provider: "google",
		// Missing Email
	}
	rec3 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/login", body3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing fields, got %d", rec3.Code)
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
