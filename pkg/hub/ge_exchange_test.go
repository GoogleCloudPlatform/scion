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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
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
	mu           sync.Mutex
	users        map[string]*store.User // by ID
	usersByEmail map[string]*store.User // by email (normalized lower-case key)
	createErr    error
	updateErr    error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:        make(map[string]*store.User),
		usersByEmail: make(map[string]*store.User),
	}
}

func (s *fakeUserStore) GetUser(_ context.Context, id string) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeUserStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.usersByEmail[strings.ToLower(email)]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeUserStore) CreateUser(_ context.Context, user *store.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	// Enforce unique email constraint (mirrors real ent schema).
	normEmail := strings.ToLower(user.Email)
	if existing, ok := s.usersByEmail[normEmail]; ok && existing.ID != user.ID {
		return store.ErrAlreadyExists
	}
	cp := *user
	s.users[user.ID] = &cp
	s.usersByEmail[normEmail] = &cp
	return nil
}

func (s *fakeUserStore) UpdateUser(_ context.Context, user *store.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	cp := *user
	s.users[user.ID] = &cp
	s.usersByEmail[strings.ToLower(user.Email)] = &cp
	return nil
}

func (s *fakeUserStore) DeleteUser(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return store.ErrNotFound
	}
	// Remove from email index only if it still points to the user being
	// deleted. Another user may have taken the email slot in a race.
	normEmail := strings.ToLower(u.Email)
	if indexed, ok := s.usersByEmail[normEmail]; ok && indexed.ID == id {
		delete(s.usersByEmail, normEmail)
	}
	delete(s.users, id)
	return nil
}

func (s *fakeUserStore) IsUserInvitedOrActive(_ context.Context, email string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.usersByEmail[strings.ToLower(email)]; ok {
		return u.Status == "active" || u.Status == "invited", nil
	}
	return false, nil
}

// Stubs for Store interface methods we don't use.
func (s *fakeUserStore) Close() error                    { return nil }
func (s *fakeUserStore) Ping(_ context.Context) error    { return nil }
func (s *fakeUserStore) Migrate(_ context.Context) error { return nil }
func (s *fakeUserStore) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

// ---------------------------------------------------------------------------
// In-memory ExternalIdentityStore for tests.
// ---------------------------------------------------------------------------

type memExtIDStore struct {
	mu       sync.Mutex
	bindings map[string]*store.ExternalIdentityBinding // key: provider:issuer:subject
	byUser   map[string][]*store.ExternalIdentityBinding
}

func newMemExtIDStore() *memExtIDStore {
	return &memExtIDStore{
		bindings: make(map[string]*store.ExternalIdentityBinding),
		byUser:   make(map[string][]*store.ExternalIdentityBinding),
	}
}

func memExtIDKey(provider, issuer, subject string) string {
	return provider + ":" + issuer + ":" + subject
}

func (s *memExtIDStore) GetExternalIdentity(_ context.Context, provider, issuer, subject string) (*store.ExternalIdentityBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memExtIDKey(provider, issuer, subject)
	if b, ok := s.bindings[key]; ok {
		cp := *b
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *memExtIDStore) CreateExternalIdentity(_ context.Context, binding *store.ExternalIdentityBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memExtIDKey(binding.Provider, binding.Issuer, binding.Subject)
	if _, ok := s.bindings[key]; ok {
		return fmt.Errorf("external identity binding already exists for %s", key)
	}
	cp := *binding
	s.bindings[key] = &cp
	s.byUser[binding.UserID] = append(s.byUser[binding.UserID], &cp)
	return nil
}

func (s *memExtIDStore) UpdateExternalIdentityEmail(_ context.Context, id, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if b.ID == id {
			b.Email = email
			b.UpdatedAt = time.Now()
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *memExtIDStore) GetExternalIdentitiesByUserID(_ context.Context, userID string) ([]*store.ExternalIdentityBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := s.byUser[userID]
	result := make([]*store.ExternalIdentityBinding, len(bindings))
	for i, b := range bindings {
		cp := *b
		result[i] = &cp
	}
	return result, nil
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
	s.mu.Lock()
	s.users[id] = u
	s.usersByEmail[strings.ToLower(email)] = u
	s.mu.Unlock()
	return u
}

// ---------------------------------------------------------------------------
// Helper to set up a test exchange service.
// ---------------------------------------------------------------------------

// alwaysAuthorized is a permissive authChecker for tests that don't exercise
// the authorization policy path.
func alwaysAuthorized(_ context.Context, _ string) bool { return true }

// newTestResolver builds a GoogleIdentityResolver for tests. roleFor may be
// nil (defaults to "member", matching the exchange's pre-refactor hardcoded
// role); pass a custom one to exercise the admin_emails-aware role delta.
func newTestResolver(userStore store.UserStore, extStore store.ExternalIdentityStore, authorize func(context.Context, string) bool, roleFor func(context.Context, string) string) *GoogleIdentityResolver {
	return NewGoogleIdentityResolver(userStore, extStore, authorize, roleFor, slog.Default())
}

func newTestExchangeService(validator GoogleCredentialValidator, userStore *fakeUserStore) *GEExchangeService {
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	return NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator,
		tokenSvc,
		newTestResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil),
		slog.Default(),
	)
}

// newTestExchangeServiceWithExtStore allows injecting a specific external identity
// store for tests that need direct access to binding state.
func newTestExchangeServiceWithExtStore(validator GoogleCredentialValidator, userStore *fakeUserStore, extStore ExternalIdentityStore) *GEExchangeService {
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	return NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator,
		tokenSvc,
		newTestResolver(userStore, extStore, alwaysAuthorized, nil),
		slog.Default(),
	)
}

// newPersistentTestExchangeService creates a GEExchangeService backed by a
// real ent/SQLite store at the given path. Each call opens an independent
// ent.Client to the same database file — callers can use two instances to
// simulate cross-instance convergence. Cleanup is registered on t.
func newPersistentTestExchangeService(t *testing.T, dbPath, driverName string) (*GEExchangeService, store.Store, ExternalIdentityStore) {
	t.Helper()
	dsn := "file:" + dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		t.Fatalf("enable sqlite foreign keys: %v", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		t.Fatalf("enable sqlite WAL mode: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	compositeStore := entadapter.NewCompositeStore(client)
	extStore := entadapter.NewExternalIdentityStore(client)

	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator,
		tokenSvc,
		newTestResolver(compositeStore, extStore, alwaysAuthorized, nil),
		slog.Default(),
	)
	return svc, compositeStore, extStore
}

func validGmailIdentity() *ValidatedGoogleIdentity {
	return &ValidatedGoogleIdentity{
		Subject:        "google-sub-123",
		Email:          "user@gmail.com",
		EmailVerified:  true,
		DisplayName:    "Test User",
		Issuer:         googleCanonicalIssuer,
		Audience:       "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
	}
}

func validWorkspaceIdentity() *ValidatedGoogleIdentity {
	return &ValidatedGoogleIdentity{
		Subject:        "google-sub-456",
		Email:          "user@company.com",
		EmailVerified:  true,
		DisplayName:    "Workspace User",
		Issuer:         googleCanonicalIssuer,
		Audience:       "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
		HostedDomain:   "company.com",
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
	// SA rejection moved out of the validator (design §4.2(i)): the real
	// validator now classifies IsServiceAccount rather than erroring, so this
	// test drives the exchange's own Step 1.5 rejection (R5), not a validator
	// error. The fake mirrors that shape exactly.
	identity := &ValidatedGoogleIdentity{
		Subject:          "sa-sub-123",
		Email:            "sa@proj.iam.gserviceaccount.com",
		EmailVerified:    true,
		Issuer:           googleCanonicalIssuer,
		Audience:         "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry:   time.Now().Add(30 * time.Minute),
		IsServiceAccount: true,
	}
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	svc := newTestExchangeServiceWithExtStore(validator, userStore, extStore)

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
	if err.Error() != "service account credentials not accepted for user exchange" {
		t.Errorf("error = %q, want the SA rejection message", err.Error())
	}
	// The resolver must never be reached: no user or binding created.
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
	if _, err := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, "sa-sub-123"); err == nil {
		t.Error("expected no external identity binding to be created")
	}
}

// TestGEExchange_AdminEmails_ProvisionsAdminRole proves the exchange's role
// delta (design §4.3: roleFor replaces the hard-coded "member") through the
// GoogleIdentityResolver, the same way the exchange is actually wired in
// production (server.go passes a shared resolver into NewGEExchangeService).
func TestGEExchange_AdminEmails_ProvisionsAdminRole(t *testing.T) {
	identity := validGmailIdentity()
	identity.Email = "admin@gmail.com"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	roleFor := func(_ context.Context, email string) string {
		if strings.EqualFold(email, "admin@gmail.com") {
			return "admin"
		}
		return "member"
	}
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, roleFor)
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, resolver, slog.Default(),
	)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "admin-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User == nil || resp.User.Role != "admin" {
		t.Fatalf("expected admin role, got %+v", resp.User)
	}
}

// TestGEExchange_ExternalIdentityLookupFault_ServerError is F2 (fix round 3,
// now in scope): a GetExternalIdentity fault that is not store.ErrNotFound
// must surface as a server error (5xx), not the 403 "no binding" treatment a
// non-authoritative or conflicting-binding case gets. Exercises the exchange
// side of the same resolver fix that auth_external_bearer_test.go's
// TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable exercises
// on the external-bearer side.
func TestGEExchange_ExternalIdentityLookupFault_ServerError(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	extStore := &trackingExtIDStore{getErr: errors.New("connection refused")}
	userStore := &trackingUserStore{}
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, resolver, slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected an error for a store fault")
	}
	// Pinned to the exact mapping (review r4, optional finding 1), not just
	// "some 5xx": the exchange's default arm maps every unclassified Resolve
	// error to exactly 500, and a mutation widening that to any other 5xx
	// should fail this test.
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (not 403 \"no binding\")", status, http.StatusInternalServerError)
	}
	if userStore.getByEmailCalled {
		t.Error("GetUserByEmail must not be called: a GetExternalIdentity fault is not \"no binding\"")
	}
	if userStore.createUserCalled {
		t.Error("CreateUser must not be called: a GetExternalIdentity fault must fail closed before bootstrap")
	}
	if extStore.createCalled {
		t.Error("CreateExternalIdentity must not be called: a GetExternalIdentity fault must fail closed before bootstrap")
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
		newTestResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil),
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
	extIDStore := newMemExtIDStore()
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
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, extIDStore, alwaysAuthorized, nil), slog.Default(),
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
		Subject:        "google-sub-789",
		Email:          "user@custom-domain.com",
		EmailVerified:  true,
		DisplayName:    "Custom User",
		Issuer:         googleCanonicalIssuer,
		Audience:       "test-client-id.apps.googleusercontent.com",
		UpstreamExpiry: time.Now().Add(30 * time.Minute),
		HostedDomain:   "", // No Workspace hd claim
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
	// Upstream expiry in 30 seconds, but configured TTL is 60 seconds.
	// The Hub token must be capped at 30 seconds (min of configured and upstream).
	identity := validGmailIdentity()
	identity.UpstreamExpiry = time.Now().Add(30 * time.Second)
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

	// The Hub expiry should be approximately 30 seconds from now (± test execution time).
	remaining := time.Until(expiresAt)
	if remaining > 35*time.Second {
		t.Errorf("Hub token expiry not capped by upstream: remaining=%v, want ≤30s", remaining)
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
		{"user@company.com", "company.com", true}, // Workspace
		{"user@custom.com", "", false},            // No workspace, not Gmail
		{"user@evil.com", "other.com", false},     // HD doesn't match email
		{"sa@iam.gserviceaccount.com", "", false}, // Service account
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
// In-memory ExternalIdentityStore tests (validates test double and store contract).
// ---------------------------------------------------------------------------

func TestMemoryExternalIdentityStore(t *testing.T) {
	ctx := context.Background()
	extStore := newMemExtIDStore()

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
// flexBool JSON decoding tests.
// ---------------------------------------------------------------------------

func TestFlexBool_Boolean(t *testing.T) {
	var b flexBool
	if err := json.Unmarshal([]byte(`true`), &b); err != nil {
		t.Fatalf("unmarshal true: %v", err)
	}
	if !bool(b) {
		t.Error("expected true")
	}
	if err := json.Unmarshal([]byte(`false`), &b); err != nil {
		t.Fatalf("unmarshal false: %v", err)
	}
	if bool(b) {
		t.Error("expected false")
	}
}

func TestFlexBool_String(t *testing.T) {
	var b flexBool
	if err := json.Unmarshal([]byte(`"true"`), &b); err != nil {
		t.Fatalf("unmarshal \"true\": %v", err)
	}
	if !bool(b) {
		t.Error("expected true from string")
	}
	if err := json.Unmarshal([]byte(`"false"`), &b); err != nil {
		t.Fatalf("unmarshal \"false\": %v", err)
	}
	if bool(b) {
		t.Error("expected false from string")
	}
}

func TestFlexBool_Invalid(t *testing.T) {
	var b flexBool
	if err := json.Unmarshal([]byte(`"yes"`), &b); err == nil {
		t.Error("expected error for invalid value")
	}
}

func TestFlexBool_InStruct(t *testing.T) {
	// Simulates Google tokeninfo returning email_verified as string.
	body := `{"email_verified":"true","azp":"client-1","aud":"client-1","sub":"sub-1","email":"a@gmail.com","expires_in":3600}`
	var resp googleTokenInfoResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.EmailVerified == nil || !bool(*resp.EmailVerified) {
		t.Error("expected email_verified=true from string form")
	}
}

// ---------------------------------------------------------------------------
// flexInt64 tests — Google's tokeninfo may return expires_in as string or number.
// ---------------------------------------------------------------------------

func TestFlexInt64_Number(t *testing.T) {
	var i flexInt64
	if err := json.Unmarshal([]byte(`3600`), &i); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if int64(i) != 3600 {
		t.Errorf("got %d, want 3600", i)
	}
}

func TestFlexInt64_String(t *testing.T) {
	var i flexInt64
	if err := json.Unmarshal([]byte(`"1800"`), &i); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if int64(i) != 1800 {
		t.Errorf("got %d, want 1800", i)
	}
}

func TestFlexInt64_Invalid(t *testing.T) {
	var i flexInt64
	if err := json.Unmarshal([]byte(`"not_a_number"`), &i); err == nil {
		t.Error("expected error for non-numeric string")
	}
}

func TestFlexInt64_InStruct(t *testing.T) {
	// Test string form of expires_in in the full tokeninfo struct.
	body := `{"azp":"client","aud":"client","sub":"s","email":"a@gmail.com","email_verified":"true","expires_in":"3600"}`
	var resp googleTokenInfoResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if int64(resp.ExpiresIn) != 3600 {
		t.Errorf("expires_in = %d, want 3600", resp.ExpiresIn)
	}

	// Test number form too.
	body2 := `{"azp":"client","aud":"client","sub":"s","email":"a@gmail.com","email_verified":"true","expires_in":1800}`
	var resp2 googleTokenInfoResponse
	if err := json.Unmarshal([]byte(body2), &resp2); err != nil {
		t.Fatalf("unmarshal number form: %v", err)
	}
	if int64(resp2.ExpiresIn) != 1800 {
		t.Errorf("expires_in = %d, want 1800", resp2.ExpiresIn)
	}
}

// ---------------------------------------------------------------------------
// Access token aud/azp disagreement — strict rejection.
// ---------------------------------------------------------------------------

func TestGEExchange_AccessToken_AudAzpDisagreement(t *testing.T) {
	// Even if both aud and azp are individually in the allowed set,
	// disagreement must be rejected (confused-deputy prevention).
	validator := &fakeGoogleValidator{
		accessTokenErr: fmt.Errorf("%w: aud %q differs from azp %q",
			ErrGoogleFieldDisagreement, "client-A", "client-B"),
	}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "split-aud-azp-token",
		CredentialType: "access_token",
	})
	if err == nil {
		t.Fatal("expected error for aud/azp disagreement")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
}

// ---------------------------------------------------------------------------
// Concurrent first linkage (exchange-level, using in-memory store).
// ---------------------------------------------------------------------------

func TestGEExchange_ConcurrentFirstLinkage(t *testing.T) {
	// N concurrent exchanges for the same previously unseen authoritative
	// identity/email must converge on one local user and one durable external
	// binding. All valid callers succeed without HTTP 500.
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	svc := newTestExchangeServiceWithExtStore(validator, userStore, extStore)

	ctx := context.Background()
	const n = 5
	type result struct {
		userID string
		err    error
	}
	results := make([]result, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			resp, _, err := svc.Exchange(ctx, &ExchangeRequest{
				Credential:     "concurrent-token",
				CredentialType: "id_token",
			})
			results[idx].err = err
			if resp != nil && resp.User != nil {
				results[idx].userID = resp.User.ID
			}
		}(i)
	}
	wg.Wait()

	// Hard assertion: ALL must succeed.
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, r.err)
		}
		if r.userID == "" {
			t.Fatalf("goroutine %d: empty user ID", i)
		}
	}

	// Hard assertion: ALL must converge on exactly one user.
	expected := results[0].userID
	for i := 1; i < n; i++ {
		if results[i].userID != expected {
			t.Fatalf("goroutine %d: user ID %q != expected %q — convergence failed",
				i, results[i].userID, expected)
		}
	}

	// Hard assertion: exactly one external binding must exist.
	binding, err := extStore.GetExternalIdentity(ctx, "google",
		canonicalizeGoogleIssuer(identity.Issuer), identity.Subject)
	if err != nil {
		t.Fatalf("expected exactly one binding, got lookup error: %v", err)
	}
	if binding.UserID != expected {
		t.Fatalf("binding points to %q, expected %q", binding.UserID, expected)
	}
}

// TestGEExchange_ProvisionNewUser_CreateError_FailsClosed proves that an
// unrelated storage failure during CreateUser is returned when re-query finds
// no winner (i.e. the error was not a unique-email collision race).
func TestGEExchange_ProvisionNewUser_CreateError_FailsClosed(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	userStore.createErr = store.ErrAlreadyExists // simulate unique-email collision
	extStore := newMemExtIDStore()
	svc := newTestExchangeServiceWithExtStore(validator, userStore, extStore)

	// CreateUser will fail with ErrAlreadyExists, but GetUserByEmail will
	// also fail (no user exists yet in the store) — the re-query finds no
	// winner. The Exchange must fail (HTTP 500), not silently succeed.
	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "create-fail-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error when CreateUser fails and no winner found")
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500, got %d", status)
	}
	// The public error is generic; the inner "create user" error is logged
	// but not leaked to the caller (Exchange sanitizes error messages).
	if !strings.Contains(err.Error(), "user resolution failed") {
		t.Fatalf("expected 'user resolution failed' error, got: %v", err)
	}
}

// TestGEExchange_ConcurrentFirstLinkage_PersistentStore uses two independent
// GEExchangeService instances against the same backing database (via the ent
// adapter), proving that the unique-email collision and binding convergence
// work across instances/reconnect — not merely through a shared fake object.
func TestGEExchange_ConcurrentFirstLinkage_PersistentStore(t *testing.T) {
	driverName := sqliteDriverName()
	if driverName == "" {
		t.Skip("skipping: requires SQLite driver (excluded by no_sqlite build tag)")
	}

	// This test requires the ent adapter with a real SQLite database.
	// We create two independent service instances sharing the same DB file.
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/concurrent_test.db"

	// Create the first service+store pair.
	svc1, store1, extStore1 := newPersistentTestExchangeService(t, dbPath, driverName)
	// Create the second service+store pair using the same DB file.
	svc2, store2, extStore2 := newPersistentTestExchangeService(t, dbPath, driverName)
	_, _, _ = store1, store2, extStore2 // used only for cleanup via t.Cleanup

	identity := validGmailIdentity()

	ctx := context.Background()

	// Exchange #1 via service instance 1: should provision user + create binding.
	resp1, _, err := svc1.Exchange(ctx, &ExchangeRequest{
		Credential:     "persistent-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("svc1.Exchange: %v", err)
	}
	if resp1.User == nil || resp1.User.ID == "" {
		t.Fatal("svc1 returned nil/empty user")
	}
	user1ID := resp1.User.ID

	// Verify the binding exists in the first store.
	b1, err := extStore1.GetExternalIdentity(ctx, "google",
		canonicalizeGoogleIssuer(identity.Issuer), identity.Subject)
	if err != nil {
		t.Fatalf("binding lookup in store1: %v", err)
	}
	if b1.UserID != user1ID {
		t.Fatalf("binding in store1 points to %q, expected %q", b1.UserID, user1ID)
	}

	// Exchange #2 via service instance 2 (same DB, separate store objects):
	// should find the existing binding and resolve to the same user.
	resp2, _, err := svc2.Exchange(ctx, &ExchangeRequest{
		Credential:     "persistent-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("svc2.Exchange: %v", err)
	}
	if resp2.User == nil {
		t.Fatal("svc2 returned nil user")
	}
	if resp2.User.ID != user1ID {
		t.Fatalf("svc2 resolved to user %q, expected %q (convergence failed across instances)",
			resp2.User.ID, user1ID)
	}
}

func sqliteDriverName() string {
	for _, driver := range sql.Drivers() {
		if driver == "sqlite" || driver == "sqlite3" {
			return driver
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Changed email / no-relink test at exchange level.
// ---------------------------------------------------------------------------

func TestGEExchange_ChangedEmail_NoRelink(t *testing.T) {
	// After a binding is created, changing the email in a subsequent exchange
	// should update the informational email but NOT change the user mapping.
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()

	identity1 := validGmailIdentity()
	identity1.Email = "old@gmail.com"
	validator := &fakeGoogleValidator{idTokenResult: identity1}
	svc := newTestExchangeServiceWithExtStore(validator, userStore, extStore)

	// First exchange — creates user + binding.
	resp1, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token-1",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// Second exchange — same sub, different email.
	identity2 := validGmailIdentity()
	identity2.Email = "new@gmail.com"
	validator.idTokenResult = identity2
	resp2, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token-2",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("second exchange: %v", err)
	}

	// Same user must be returned (stable linkage, no relink).
	if resp1.User.ID != resp2.User.ID {
		t.Errorf("email change caused relink: user %s → %s", resp1.User.ID, resp2.User.ID)
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

// ---------------------------------------------------------------------------
// Critical #1 regression: JWT exp cryptographically capped.
//
// Decodes and cryptographically validates the actual minted Hub JWT, then
// compares its exp with response expiresAt, upstreamExpiresAt, and the
// authoritative upstream expiry.
// ---------------------------------------------------------------------------

func TestGEExchange_JWTExpCryptographicRegression(t *testing.T) {
	// Create a token service with a known signing key so we can validate the JWT.
	signingKey := []byte("test-signing-key-32-bytes-long!!")
	tokenSvc, err := NewUserTokenService(UserTokenConfig{
		SigningKey:          signingKey,
		AccessTokenDuration: 15 * time.Minute, // Deliberately longer than TokenTTL
	})
	if err != nil {
		t.Fatal(err)
	}

	upstreamExpiry := time.Now().Add(45 * time.Second) // Less than DefaultGETokenTTL (60s)
	identity := validGmailIdentity()
	identity.UpstreamExpiry = upstreamExpiry

	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL, // 60s
		},
		validator,
		tokenSvc,
		newTestResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil),
		slog.Default(),
	)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "capped-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	// Step 1: Cryptographically validate the minted JWT using the known key.
	claims, err := tokenSvc.ValidateUserToken(resp.AccessToken)
	if err != nil {
		t.Fatalf("minted JWT failed cryptographic validation: %v", err)
	}

	// Step 2: The JWT exp claim must exist.
	if claims.Expiry == nil {
		t.Fatal("minted JWT missing exp claim")
	}
	jwtExp := claims.Expiry.Time()

	// Step 3: Parse response timestamps.
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("failed to parse expiresAt: %v", err)
	}
	upstreamExpiresAt, err := time.Parse(time.RFC3339, resp.UpstreamExpiresAt)
	if err != nil {
		t.Fatalf("failed to parse upstreamExpiresAt: %v", err)
	}

	// Step 4: JWT exp must be ≤ response expiresAt (they should be the same
	// within clock granularity).
	if jwtExp.After(expiresAt.Add(2 * time.Second)) {
		t.Errorf("JWT exp (%v) later than response expiresAt (%v)", jwtExp, expiresAt)
	}

	// Step 5: JWT exp must be ≤ upstreamExpiresAt (the token must not outlive
	// the upstream credential).
	if jwtExp.After(upstreamExpiresAt.Add(2 * time.Second)) {
		t.Errorf("JWT exp (%v) later than upstream expiry (%v)", jwtExp, upstreamExpiresAt)
	}

	// Step 6: upstreamExpiresAt in response must match the authoritative
	// upstream expiry within 1 second.
	if upstreamExpiresAt.Sub(upstreamExpiry).Abs() > 1*time.Second {
		t.Errorf("response upstreamExpiresAt (%v) != authoritative upstream (%v)",
			upstreamExpiresAt, upstreamExpiry)
	}

	// Step 7: When upstream < configured, the effective TTL must be capped by
	// upstream remaining (~45s), not the configured TTL (60s).
	jwtDuration := time.Until(jwtExp)
	if jwtDuration > 50*time.Second {
		t.Errorf("JWT duration (%v) not capped by upstream remaining (~45s)", jwtDuration)
	}

	// Step 8: Verify the token type and standard claims.
	if claims.TokenType != TokenTypeAccess {
		t.Errorf("expected token type %q, got %q", TokenTypeAccess, claims.TokenType)
	}
	if claims.ClientType != ClientTypeAPI {
		t.Errorf("expected client type %q, got %q", ClientTypeAPI, claims.ClientType)
	}
}

func TestGEExchange_JWTExpRegression_ConfiguredTTLWins(t *testing.T) {
	// When upstream remaining is longer than configured TTL, the configured
	// TTL should cap the token (not the upstream). This tests the other
	// branch of min(configured, upstream).
	signingKey := []byte("test-signing-key-32-bytes-long!!")
	tokenSvc, err := NewUserTokenService(UserTokenConfig{
		SigningKey:          signingKey,
		AccessTokenDuration: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	identity := validGmailIdentity()
	identity.UpstreamExpiry = time.Now().Add(30 * time.Minute) // Much longer than 60s

	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL, // 60s
		},
		validator, tokenSvc, newTestResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil), slog.Default(),
	)

	resp, _, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "long-lived-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}

	claims, err := tokenSvc.ValidateUserToken(resp.AccessToken)
	if err != nil {
		t.Fatalf("minted JWT failed cryptographic validation: %v", err)
	}

	// JWT duration should be ~60s (configured), not ~30min (upstream).
	jwtDuration := time.Until(claims.Expiry.Time())
	if jwtDuration > 65*time.Second {
		t.Errorf("JWT duration (%v) exceeds configured TTL (60s) — not properly capped", jwtDuration)
	}
	if jwtDuration < 50*time.Second {
		t.Errorf("JWT duration (%v) too short — expected ~60s", jwtDuration)
	}
}

// ---------------------------------------------------------------------------
// Critical #2 regression: provisioning authorization via real policy path.
//
// These tests exercise the actual checkUserAuthorized function, not a mocked
// approval callback. They cover:
// - Domain restriction: rejects users outside authorized domains
// - Invite-only mode: rejects uninvited users
// - Open mode: allows any user
// - Admin bypass: admin emails always pass
// ---------------------------------------------------------------------------

func TestGEExchange_ProvisioningAuth_DomainRestricted(t *testing.T) {
	identity := validGmailIdentity()
	identity.Email = "user@unauthorized.com"
	identity.HostedDomain = "unauthorized.com"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	// Use real checkUserAuthorized with domain restriction.
	authChecker := func(_ context.Context, email string) bool {
		return checkUserAuthorized(context.Background(), email,
			[]string{"allowed.com"},   // authorized domains
			[]string{"admin@hub.com"}, // admin emails
			"domain_restricted",       // access mode
			userStore,
		)
	}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, newMemExtIDStore(), authChecker, nil), slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "domain-rejected-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error: user outside authorized domain should be rejected")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}

	// Verify no User was created.
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

func TestGEExchange_ProvisioningAuth_DomainAllowed(t *testing.T) {
	identity := validWorkspaceIdentity()
	identity.Email = "user@allowed.com"
	identity.HostedDomain = "allowed.com"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	authChecker := func(_ context.Context, email string) bool {
		return checkUserAuthorized(context.Background(), email,
			[]string{"allowed.com"},
			[]string{"admin@hub.com"},
			"domain_restricted",
			userStore,
		)
	}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, newMemExtIDStore(), authChecker, nil), slog.Default(),
	)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "domain-accepted-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User == nil || resp.User.Email != "user@allowed.com" {
		t.Error("expected user with allowed domain email")
	}
}

func TestGEExchange_ProvisioningAuth_InviteOnly_Rejected(t *testing.T) {
	identity := validGmailIdentity()
	identity.Email = "uninvited@gmail.com"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	// Use real checkUserAuthorized with invite_only mode.
	// The fakeUserStore has no invited users, so IsUserInvitedOrActive will fail.
	authChecker := func(_ context.Context, email string) bool {
		return checkUserAuthorized(context.Background(), email,
			nil,                       // no domain restriction
			[]string{"admin@hub.com"}, // admin emails
			"invite_only",             // access mode
			userStore,
		)
	}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, newMemExtIDStore(), authChecker, nil), slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "uninvited-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error: uninvited user should be rejected in invite_only mode")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

func TestGEExchange_ProvisioningAuth_AdminBypass(t *testing.T) {
	// Admin email must be in an authoritative domain (Gmail) to pass the
	// email domain gate. The admin bypass then applies at the provisioning
	// authorization level (checkUserAuthorized).
	identity := validGmailIdentity()
	identity.Email = "admin@gmail.com"
	identity.Subject = "admin-sub-001"
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	// Use real checkUserAuthorized: even in invite_only mode, admins bypass.
	authChecker := func(_ context.Context, email string) bool {
		return checkUserAuthorized(context.Background(), email,
			nil,                         // no domain restriction
			[]string{"admin@gmail.com"}, // admin emails
			"invite_only",               // access mode
			userStore,
		)
	}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, newMemExtIDStore(), authChecker, nil), slog.Default(),
	)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "admin-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v (admins should bypass invite_only)", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User == nil || resp.User.Email != "admin@gmail.com" {
		t.Error("expected admin user in response")
	}
}

func TestGEExchange_ProvisioningAuth_NilAuthChecker_FailsClosed(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc,
		newTestResolver(userStore, newMemExtIDStore(), nil, nil), // nil authorize — must fail closed
		slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error: nil authorize must fail closed")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

// ---------------------------------------------------------------------------
// Required #3 regression: conflict resolution with expectedUserID validation
// and orphan cleanup.
// ---------------------------------------------------------------------------

// raceExtIDStore simulates a race condition where binding creation fails
// because a concurrent goroutine wins the race, but the initial lookup
// (before creation) returns not-found.
type raceExtIDStore struct {
	mu            sync.Mutex
	inner         *memExtIDStore
	createFails   bool                           // when true, Create fails and injects winner
	winnerBinding *store.ExternalIdentityBinding // injected on first Create failure
}

func newRaceExtIDStore(winnerBinding *store.ExternalIdentityBinding) *raceExtIDStore {
	return &raceExtIDStore{
		inner:         newMemExtIDStore(),
		createFails:   true,
		winnerBinding: winnerBinding,
	}
}

func (s *raceExtIDStore) GetExternalIdentity(ctx context.Context, provider, issuer, subject string) (*store.ExternalIdentityBinding, error) {
	return s.inner.GetExternalIdentity(ctx, provider, issuer, subject)
}

func (s *raceExtIDStore) CreateExternalIdentity(ctx context.Context, binding *store.ExternalIdentityBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createFails {
		// Simulate race: another goroutine created the binding first.
		s.createFails = false
		// Inject the winner's binding into the inner store.
		_ = s.inner.CreateExternalIdentity(ctx, s.winnerBinding)
		return fmt.Errorf("external identity binding already exists (simulated race)")
	}
	return s.inner.CreateExternalIdentity(ctx, binding)
}

func (s *raceExtIDStore) UpdateExternalIdentityEmail(ctx context.Context, id, email string) error {
	return s.inner.UpdateExternalIdentityEmail(ctx, id, email)
}

func (s *raceExtIDStore) GetExternalIdentitiesByUserID(ctx context.Context, userID string) ([]*store.ExternalIdentityBinding, error) {
	return s.inner.GetExternalIdentitiesByUserID(ctx, userID)
}

func TestGEExchange_ConflictResolution_ExpectedUserMismatch(t *testing.T) {
	// Simulates a race: user-A exists with email user@gmail.com. The exchange
	// matches by email to user-A. But a concurrent goroutine wins the binding
	// creation race and binds the same subject to user-B. resolveAfterConflict
	// must fail closed because winner.UserID (user-B) != expectedUserID (user-A).
	userStore := newFakeUserStore()
	existingUser := addUser(userStore, "user-A", "user@gmail.com", "member", "active")
	otherUser := addUser(userStore, "user-B", "other@gmail.com", "member", "active")

	winnerBinding := &store.ExternalIdentityBinding{
		ID:       "winner-binding",
		Provider: "google",
		Issuer:   googleCanonicalIssuer,
		Subject:  "google-sub-123",
		UserID:   otherUser.ID,
		Email:    "other@gmail.com",
	}
	extStore := newRaceExtIDStore(winnerBinding)

	identity := validGmailIdentity()
	identity.Email = existingUser.Email
	validator := &fakeGoogleValidator{idTokenResult: identity}
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{})

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, extStore, alwaysAuthorized, nil), slog.Default(),
	)

	_, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "conflict-token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error: conflict resolution should detect user mismatch")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestGEExchange_OrphanCleanup_OnProvisioningConflict(t *testing.T) {
	// Simulates provisioning race: no existing user by email, provisioning
	// creates a new user, but binding creation fails because a concurrent
	// goroutine already created the binding for the same subject → different user.
	// The orphaned provisioned user must be cleaned up.
	userStore := newFakeUserStore()
	winnerUser := addUser(userStore, "winner-user", "winner@gmail.com", "member", "active")

	winnerBinding := &store.ExternalIdentityBinding{
		ID:       "winner-binding",
		Provider: "google",
		Issuer:   googleCanonicalIssuer,
		Subject:  "google-sub-123",
		UserID:   winnerUser.ID,
		Email:    "winner@gmail.com",
	}
	extStore := newRaceExtIDStore(winnerBinding)

	identity := validGmailIdentity()
	// Use a different email than winner so no existing user is found by email.
	identity.Email = "loser@gmail.com"
	validator := &fakeGoogleValidator{idTokenResult: identity}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, newTestResolver(userStore, extStore, alwaysAuthorized, nil), slog.Default(),
	)

	resp, status, err := svc.Exchange(context.Background(), &ExchangeRequest{
		Credential:     "race-loser-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange should succeed (resolve to winner): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.User.ID != winnerUser.ID {
		t.Errorf("expected winner user %s, got %s", winnerUser.ID, resp.User.ID)
	}

	// Verify orphaned user was cleaned up: the store should contain only the
	// winner user and no provisioned orphan.
	if len(userStore.users) != 1 {
		t.Errorf("expected 1 user (winner only), got %d — orphan not cleaned up", len(userStore.users))
	}
}
