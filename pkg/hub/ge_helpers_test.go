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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeGEExchangeMetrics records every RecordGEExchangeRequest call.
type fakeGEExchangeMetrics struct {
	mu       sync.Mutex
	outcomes []GEExchangeOutcome
}

type countingGoogleValidator struct {
	fakeGoogleValidator
	idTokenCalls     atomic.Int64
	accessTokenCalls atomic.Int64
}

// newRejectingCountingValidator returns a *countingGoogleValidator whose zero
// value would otherwise be a fakeGoogleValidator that returns (nil, nil) — if
// a mutation ever lets a "must not reach the validator" test actually reach
// it, a nil identity dereference panics the whole test binary instead of
// failing the test's own counting.totalCalls() assertion. Giving both
// methods a default error means that mutation fails loudly and locally.
func newRejectingCountingValidator() *countingGoogleValidator {
	return &countingGoogleValidator{fakeGoogleValidator: fakeGoogleValidator{
		idTokenErr:     ErrGoogleInvalidCredential,
		accessTokenErr: ErrGoogleInvalidCredential,
	}}
}

func exchangeRequest(body string) *http.Request {
	return exchangeRequestWithAddr(body, "192.0.2.1:12345")
}

type fakeGoogleValidator struct {
	idTokenResult     *ValidatedGoogleIdentity
	idTokenErr        error
	accessTokenResult *ValidatedGoogleIdentity
	accessTokenErr    error
}

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

func newMemExtIDStore() *memExtIDStore {
	return &memExtIDStore{
		bindings: make(map[string]*store.ExternalIdentityBinding),
		byUser:   make(map[string][]*store.ExternalIdentityBinding),
	}
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

func sqliteDriverName() string {
	for _, driver := range sql.Drivers() {
		if driver == "sqlite" || driver == "sqlite3" {
			return driver
		}
	}
	return ""
}

func (f *fakeGEExchangeMetrics) RecordGEExchangeRequest(outcome GEExchangeOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcome)
}

func (f *fakeGEExchangeMetrics) all() []GEExchangeOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GEExchangeOutcome, len(f.outcomes))
	copy(out, f.outcomes)
	return out
}

func (v *countingGoogleValidator) ValidateIDToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.idTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateIDToken(ctx, token, clientIDs)
}

func (v *countingGoogleValidator) ValidateAccessToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.accessTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateAccessToken(ctx, token, clientIDs)
}

func (v *countingGoogleValidator) totalCalls() int64 {
	return v.idTokenCalls.Load() + v.accessTokenCalls.Load()
}

func exchangeRequestWithAddr(body, remoteAddr string) *http.Request {
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	return req
}

func (f *fakeGoogleValidator) ValidateIDToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return f.idTokenResult, f.idTokenErr
}

func (f *fakeGoogleValidator) ValidateAccessToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return f.accessTokenResult, f.accessTokenErr
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

type memExtIDStore struct {
	mu       sync.Mutex
	bindings map[string]*store.ExternalIdentityBinding // key: provider:issuer:subject
	byUser   map[string][]*store.ExternalIdentityBinding
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

func memExtIDKey(provider, issuer, subject string) string {
	return provider + ":" + issuer + ":" + subject
}
