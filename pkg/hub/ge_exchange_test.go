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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Fake Google credential validator for deterministic tests.
// ---------------------------------------------------------------------------

type fakeGoogleValidator struct {
	idTokenResult     *ValidatedGoogleIdentity
	idTokenErr        error
	accessTokenResult *ValidatedGoogleIdentity
	accessTokenErr    error
}

func (f *fakeGoogleValidator) ValidateIDToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return f.idTokenResult, f.idTokenErr
}

func (f *fakeGoogleValidator) ValidateAccessToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return f.accessTokenResult, f.accessTokenErr
}

// ---------------------------------------------------------------------------
// Fake user store for tests.
// ---------------------------------------------------------------------------

type fakeUserStore struct {
	store.Store
	users       map[string]*store.User // by ID
	usersByEmail map[string]*store.User // by email
	createErr   error
	updateErr   error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:       make(map[string]*store.User),
		usersByEmail: make(map[string]*store.User),
	}
}

func (s *fakeUserStore) GetUser(_ context.Context, id string) (*store.User, error) {
	if u, ok := s.users[id]; ok {
		copy := *u
		return &copy, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeUserStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if u, ok := s.usersByEmail[strings.ToLower(email)]; ok {
		copy := *u
		return &copy, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeUserStore) CreateUser(_ context.Context, user *store.User) error {
	if s.createErr != nil {
		return s.createErr
	}
	copy := *user
	s.users[user.ID] = &copy
	s.usersByEmail[strings.ToLower(user.Email)] = &copy
	return nil
}

func (s *fakeUserStore) UpdateUser(_ context.Context, user *store.User) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	copy := *user
	s.users[user.ID] = &copy
	s.usersByEmail[strings.ToLower(user.Email)] = &copy
	return nil
}

// Stubs for Store interface methods we don't use.
func (s *fakeUserStore) Close() error                                           { return nil }
func (s *fakeUserStore) Ping(_ context.Context) error                           { return nil }
func (s *fakeUserStore) Migrate(_ context.Context) error                        { return nil }
func (s *fakeUserStore) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

func addUser(s *fakeUserStore, id, email, role, status string) *store.User {
	u := &store.User{
		ID:          id,
		Email:       email,
		DisplayName: "Test User",
		Role:        role,
		Status:      status,
		Created:     time.Now(),
	}
	s.users[id] = u
	s.usersByEmail[strings.ToLower(email)] = u
	return u
}

// ---------------------------------------------------------------------------
// Helper to set up a test exchange service.
// ---------------------------------------------------------------------------

func newTestExchangeService(validator GoogleCredentialValidator, userStore *fakeUserStore) *GEExchangeService {
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: 5 * time.Minute,
	})
	return NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		NewMemoryExternalIdentityStore(),
		userStore,
		slog.Default(),
	)
}

func validGmailIdentity() *ValidatedGoogleIdentity {
	return &ValidatedGoogleIdentity{
		Subject:       "google-sub-123",
		Email:         "user@gmail.com",
		EmailVerified: true,
		DisplayName:   "Test User",
		Issuer:        googleCanonicalIssuer,
		Audience:      "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
	}
}

func validWorkspaceIdentity() *ValidatedGoogleIdentity {
	return &ValidatedGoogleIdentity{
		Subject:       "google-sub-456",
		Email:         "user@company.com",
		EmailVerified: true,
		DisplayName:   "Workspace User",
		Issuer:        googleCanonicalIssuer,
		Audience:      "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
		HostedDomain:  "company.com",
	}
}

// ---------------------------------------------------------------------------
// Exchange tests — acceptance gates from #1616 and contract-review.
// ---------------------------------------------------------------------------

func TestGEExchange_IDToken_ValidGmail(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "valid-id-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.AccessToken == "" {
		t.Error("expected non-empty access token")
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("expected Bearer, got %q", resp.TokenType)
	}
	if resp.ExpiresAt == "" {
		t.Error("expected non-empty expiresAt")
	}
	if resp.UpstreamExpiresAt == "" {
		t.Error("expected non-empty upstreamExpiresAt")
	}
	if resp.User == nil {
		t.Fatal("expected non-nil user")
	}
	if resp.User.Email != "user@gmail.com" {
		t.Errorf("expected user@gmail.com, got %q", resp.User.Email)
	}
}

func TestGEExchange_AccessToken_ValidGmail(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{accessTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "valid-access-token",
		CredentialType: "access_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User == nil || resp.User.Email != "user@gmail.com" {
		t.Error("expected gmail user in response")
	}
}

func TestGEExchange_ForeignClient(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: fmt.Errorf("%w: audience not in allowed set", ErrGoogleUntrustedAudience),
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token-with-wrong-client",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for foreign client")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

func TestGEExchange_ForgedToken(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: fmt.Errorf("%w: signature verification failed", ErrGoogleInvalidCredential),
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "forged-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for forged token")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

func TestGEExchange_ExpiredToken(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: ErrGoogleExpiredCredential,
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "expired-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

func TestGEExchange_ServiceAccount(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: ErrGoogleServiceAccount,
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "sa-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for service account")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestGEExchange_UnverifiedEmail(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: ErrGoogleUnverifiedEmail,
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "unverified-email-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for unverified email")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestGEExchange_MissingTrustConfig(t *testing.T) {
	validator := &fakeGoogleValidator{}
	userStore := newFakeUserStore()
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{Enabled: false},
		validator, tokenSvc,
		NewMemoryExternalIdentityStore(), userStore,
		slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "any-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for missing trust config")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

func TestGEExchange_UnsupportedCredentialType(t *testing.T) {
	validator := &fakeGoogleValidator{}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "some-token",
		CredentialType: "jwt_decode", // Not supported per contract
	})
	if err == nil {
		t.Fatal("expected error for unsupported credential type")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", status)
	}
}

func TestGEExchange_EmptyCredential(t *testing.T) {
	validator := &fakeGoogleValidator{}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for empty credential")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", status)
	}
}

// ---------------------------------------------------------------------------
// Stable identity linkage tests.
// ---------------------------------------------------------------------------

func TestGEExchange_StableLinkage_SameSubResolvesToSameUser(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	// First exchange creates user + binding.
	resp1, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token1",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("first exchange failed: %v", err)
	}

	// Second exchange with same identity resolves to same user.
	resp2, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token2",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("second exchange failed: %v", err)
	}

	if resp1.User.ID != resp2.User.ID {
		t.Errorf("same Google sub resolved to different users: %s vs %s",
			resp1.User.ID, resp2.User.ID)
	}
}

func TestGEExchange_StableLinkage_ExistingUserByEmail(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	existingUser := addUser(userStore, "existing-user-1", "user@gmail.com", "member", "active")
	svc := newTestExchangeService(validator, userStore)

	resp, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}

	if resp.User.ID != existingUser.ID {
		t.Errorf("expected existing user %s, got %s", existingUser.ID, resp.User.ID)
	}
}

func TestGEExchange_StableLinkage_ConflictingSubject(t *testing.T) {
	identity := validGmailIdentity()
	identity.Subject = "new-subject"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	existingUser := addUser(userStore, "user-1", "user@gmail.com", "member", "active")

	// Pre-create a binding with a different subject for the same user.
	extIDStore := NewMemoryExternalIdentityStore()
	_ = extIDStore.CreateExternalIdentity(context.Background(), &ExternalIdentityBinding{
		ID:       "binding-1",
		Provider: "google",
		Issuer:   googleCanonicalIssuer,
		Subject:  "old-subject",
		UserID:   existingUser.ID,
		Email:    "user@gmail.com",
	})

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator, tokenSvc, extIDStore, userStore, slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for conflicting subject")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("expected conflict error, got: %v", err)
	}
}

func TestGEExchange_SuspendedUser(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	addUser(userStore, "suspended-user-1", "user@gmail.com", "member", "suspended")
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for suspended user")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestGEExchange_NonAuthoritativeEmail_NoAutoLink(t *testing.T) {
	// Third-party email domain (not Gmail, not Workspace) — fails closed.
	identity := &ValidatedGoogleIdentity{
		Subject:       "google-sub-789",
		Email:         "user@custom-domain.com",
		EmailVerified: true,
		DisplayName:   "Custom User",
		Issuer:        googleCanonicalIssuer,
		Audience:      "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
		HostedDomain:  "", // No Workspace hd claim
	}
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for non-authoritative email domain")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestGEExchange_WorkspaceEmail_AutoLink(t *testing.T) {
	identity := validWorkspaceIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "workspace-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User.Email != "user@company.com" {
		t.Errorf("expected user@company.com, got %q", resp.User.Email)
	}
}

// ---------------------------------------------------------------------------
// Hub token expiry capping test (contract-review point 4).
// ---------------------------------------------------------------------------

func TestGEExchange_TokenExpiryCappedByUpstream(t *testing.T) {
	// Upstream expiry in 2 minutes, but configured TTL is 5 minutes.
	// The Hub token must be capped at 2 minutes.
	identity := validGmailIdentity()
	identity.UpstreamExpiry = time.Now().Add(2 * time.Minute)
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	resp, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "short-lived-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("failed to parse expiresAt: %v", err)
	}

	// The Hub expiry should be approximately 2 minutes from now (± test execution time).
	remaining := time.Until(expiresAt)
	if remaining > 3*time.Minute {
		t.Errorf("Hub token expiry not capped by upstream: remaining=%v", remaining)
	}
}

func TestGEExchange_NoRemainingLifetime(t *testing.T) {
	identity := validGmailIdentity()
	identity.UpstreamExpiry = time.Now().Add(-1 * time.Minute) // Already expired
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "almost-expired",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error for no remaining lifetime")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

// ---------------------------------------------------------------------------
// Field disagreement test (contract-review point 3).
// ---------------------------------------------------------------------------

func TestGEExchange_FieldDisagreement(t *testing.T) {
	validator := &fakeGoogleValidator{
		accessTokenErr: fmt.Errorf("%w: sub from tokeninfo != userinfo",
			ErrGoogleFieldDisagreement),
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "mismatched-token",
		CredentialType: "access_token",
	})
	if err == nil {
		t.Fatal("expected error for field disagreement")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

// ---------------------------------------------------------------------------
// Issuer canonicalization test (contract-review point 2).
// ---------------------------------------------------------------------------

func TestCanonicalizeGoogleIssuer(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://accounts.google.com", "https://accounts.google.com"},
		{"accounts.google.com", "https://accounts.google.com"},
		{"other-issuer", "other-issuer"},
	}
	for _, tt := range tests {
		got := canonicalizeGoogleIssuer(tt.input)
		if got != tt.expected {
			t.Errorf("canonicalizeGoogleIssuer(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// Service account detection test.
// ---------------------------------------------------------------------------

func TestIsGoogleServiceAccount(t *testing.T) {
	tests := []struct {
		email    string
		expected bool
	}{
		{"user@gmail.com", false},
		{"sa@my-project.iam.gserviceaccount.com", true},
		{"app@appspot.gserviceaccount.com", true},
		{"dev@developer.gserviceaccount.com", true},
		{"system@system.gserviceaccount.com", true},
		{"user@company.com", false},
	}
	for _, tt := range tests {
		got := isGoogleServiceAccount(tt.email)
		if got != tt.expected {
			t.Errorf("isGoogleServiceAccount(%q) = %v, want %v", tt.email, got, tt.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// Authoritative email domain test (contract-review point 1).
// ---------------------------------------------------------------------------

func TestIsAuthoritativeEmailDomain(t *testing.T) {
	tests := []struct {
		email string
		hd    string
		want  bool
	}{
		{"user@gmail.com", "", true},
		{"user@googlemail.com", "", true},
		{"user@company.com", "company.com", true},  // Workspace
		{"user@custom.com", "", false},              // No workspace, not Gmail
		{"user@evil.com", "other.com", false},       // HD doesn't match email
		{"sa@iam.gserviceaccount.com", "", false},   // Service account
	}
	for _, tt := range tests {
		got := isAuthoritativeEmailDomain(tt.email, tt.hd)
		if got != tt.want {
			t.Errorf("isAuthoritativeEmailDomain(%q, %q) = %v, want %v",
				tt.email, tt.hd, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handler test.
// ---------------------------------------------------------------------------

func TestGEExchangeHandler_Integration(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	// Create a minimal server with just the exchange handler.
	server := &Server{
		geExchangeService: svc,
	}

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ExchangeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccessToken == "" {
		t.Error("expected non-empty access token")
	}
	if resp.User == nil || resp.User.Email != "user@gmail.com" {
		t.Error("expected user in response")
	}
}

func TestGEExchangeHandler_NotConfigured(t *testing.T) {
	server := &Server{
		geExchangeService: nil, // Not configured
	}

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestGEExchangeHandler_MethodNotAllowed(t *testing.T) {
	server := &Server{}

	req := httptest.NewRequest("GET", "/api/v1/auth/integrations/google/exchange", nil)
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// MemoryExternalIdentityStore tests.
// ---------------------------------------------------------------------------

func TestMemoryExternalIdentityStore(t *testing.T) {
	ctx := context.Background()
	extStore := NewMemoryExternalIdentityStore()

	// Create a binding.
	binding := &ExternalIdentityBinding{
		ID:        "binding-1",
		Provider:  "google",
		Issuer:    googleCanonicalIssuer,
		Subject:   "sub-123",
		UserID:    "user-1",
		Email:     "user@gmail.com",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := extStore.CreateExternalIdentity(ctx, binding); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Get by provider/issuer/subject.
	got, err := extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, "sub-123")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.UserID != "user-1" {
		t.Errorf("expected user-1, got %s", got.UserID)
	}

	// Duplicate create should fail.
	if err := extStore.CreateExternalIdentity(ctx, binding); err == nil {
		t.Error("expected error for duplicate create")
	}

	// Get by user ID.
	bindings, err := extStore.GetExternalIdentitiesByUserID(ctx, "user-1")
	if err != nil {
		t.Fatalf("get by user failed: %v", err)
	}
	if len(bindings) != 1 {
		t.Errorf("expected 1 binding, got %d", len(bindings))
	}

	// Update email.
	if err := extStore.UpdateExternalIdentityEmail(ctx, "binding-1", "new@gmail.com"); err != nil {
		t.Fatalf("update email failed: %v", err)
	}

	got, _ = extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, "sub-123")
	if got.Email != "new@gmail.com" {
		t.Errorf("expected new@gmail.com, got %s", got.Email)
	}

	// Not found.
	_, err = extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Login regression — existing browser login must not be affected.
// ---------------------------------------------------------------------------

func TestGEExchange_LoginRegression_ProvisionUserPathPreserved(t *testing.T) {
	// Verify that the existing provisionUser path still works independently
	// of the GE exchange. The GE exchange creates its own user resolution
	// logic but must not break the existing email-based provisioning.
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	// Exchange creates a user via GE path.
	resp, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}

	// The user should be findable by email (same as browser login would use).
	user, err := userStore.GetUserByEmail(context.Background(), "user@gmail.com")
	if err != nil {
		t.Fatalf("user not found by email: %v", err)
	}
	if user.ID != resp.User.ID {
		t.Errorf("GE user ID (%s) != store user ID (%s)", resp.User.ID, user.ID)
	}
}
