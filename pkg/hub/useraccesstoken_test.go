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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Unit tests for ValidateToken, expandScopes, ScopedUserIdentity, IsUAT.
// These exercise internal helpers that do not require the bounded domain
// service (authorization, transactions, audit). The full integration matrix
// for CreateToken/RevokeToken/DeleteToken lives in rs4_credential_test.go.
// ---------------------------------------------------------------------------

// mockUATStore implements store.UserAccessTokenStore for validate-only tests.
// ValidateToken updates last-used from a background goroutine, so every
// access to tokens goes through mu. Tests that edit a stored row use mutate.
type mockUATStore struct {
	mu     sync.Mutex
	tokens map[string]*store.UserAccessToken
}

// mutate applies fn to the stored token with the given ID while holding mu.
func (m *mockUATStore) mutate(t *testing.T, id string, fn func(*store.UserAccessToken)) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[id]
	if !ok {
		t.Fatalf("token %q not found in mock store", id)
	}
	fn(tok)
}

func newMockUATStore() *mockUATStore {
	return &mockUATStore{tokens: make(map[string]*store.UserAccessToken)}
}

func (m *mockUATStore) CreateUserAccessToken(_ context.Context, token *store.UserAccessToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tokens[token.ID]; exists {
		return store.ErrAlreadyExists
	}
	cp := *token
	m.tokens[token.ID] = &cp
	return nil
}

func (m *mockUATStore) GetUserAccessToken(_ context.Context, id string) (*store.UserAccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *mockUATStore) GetUserAccessTokenByHash(_ context.Context, hash string) (*store.UserAccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.KeyHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockUATStore) UpdateUserAccessTokenLastUsed(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	t.LastUsed = &now
	return nil
}

func (m *mockUATStore) RevokeUserAccessToken(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	t.Revoked = true
	return nil
}

func (m *mockUATStore) DeleteUserAccessToken(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.tokens, id)
	return nil
}

func (m *mockUATStore) ListUserAccessTokens(_ context.Context, userID string) ([]store.UserAccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.UserAccessToken
	for _, t := range m.tokens {
		if t.UserID == userID {
			result = append(result, *t)
		}
	}
	return result, nil
}

func (m *mockUATStore) CountUserAccessTokens(_ context.Context, userID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, t := range m.tokens {
		if t.UserID == userID && !t.Revoked {
			count++
		}
	}
	return count, nil
}

func (m *mockUATStore) DeleteUserAccessTokensByProject(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, t := range m.tokens {
		if t.ProjectID == projectID {
			delete(m.tokens, id)
			count++
		}
	}
	return count, nil
}

func (m *mockUATStore) LockUserForTokens(_ context.Context, _ string) error {
	return nil
}

// mockUserStore implements store.UserStore for testing (minimal).
type mockUserStore struct {
	users map[string]*store.User
}

func (m *mockUserStore) GetUser(_ context.Context, id string) (*store.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}
func (m *mockUserStore) GetUsersByIDs(_ context.Context, ids []string) (map[string]*store.User, error) {
	out := make(map[string]*store.User, len(ids))
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
func (m *mockUserStore) GetUserByEmail(context.Context, string) (*store.User, error) {
	return nil, store.ErrNotFound
}
func (m *mockUserStore) CreateUser(context.Context, *store.User) error { return nil }
func (m *mockUserStore) UpdateUser(context.Context, *store.User) error { return nil }
func (m *mockUserStore) ListUsers(context.Context, store.UserFilter, store.ListOptions) (*store.ListResult[store.User], error) {
	return nil, nil
}
func (m *mockUserStore) DeleteUser(context.Context, string) error                    { return nil }
func (m *mockUserStore) UpdateUserLastSeen(context.Context, string, time.Time) error { return nil }
func (m *mockUserStore) IsUserInvitedOrActive(context.Context, string) (bool, error) {
	return false, nil
}
func (m *mockUserStore) IncrementSessionGeneration(context.Context, string) error { return nil }

// newTestValidateService creates a minimal UAT service for ValidateToken tests.
func newTestValidateService() (*UserAccessTokenService, *mockUATStore, *mockUserStore) {
	tokenStore := newMockUATStore()
	userStore := &mockUserStore{
		users: map[string]*store.User{
			tid("user-1"): {ID: tid("user-1"), Email: "test@example.com", DisplayName: "Test User", Role: "member"},
		},
	}
	svc := &UserAccessTokenService{
		tokens:  tokenStore,
		users:   userStore,
		nowFunc: time.Now,
		logger:  slog.Default(),
	}
	return svc, tokenStore, userStore
}

// seedTestToken creates a real token (with proper hash) directly in the store.
type testTokenPair struct {
	plaintext string
	stored    *store.UserAccessToken
}

func seedTestToken(t *testing.T, tokenStore *mockUATStore, userID, projectID string, scopes []string) testTokenPair {
	t.Helper()

	randomBytes := make([]byte, UATRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatalf("failed to generate random bytes: %v", err)
	}

	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.UATPrefix + keyBody
	prefix := store.UATPrefix + keyBody[:UATPrefixLength]
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	future := time.Now().Add(90 * 24 * time.Hour)
	tok := &store.UserAccessToken{
		ID:           uuid.New().String(),
		UserID:       userID,
		Name:         "test-token",
		Prefix:       prefix,
		KeyHash:      hashStr,
		BoundaryKind: string(permissions.BoundaryKindProject),
		ProjectID:    projectID,
		Scopes:       scopes,
		ExpiresAt:    &future,
		Created:      time.Now(),
	}
	if err := tokenStore.CreateUserAccessToken(context.Background(), tok); err != nil {
		t.Fatalf("failed to seed token: %v", err)
	}
	return testTokenPair{plaintext: fullKey, stored: tok}
}

func TestValidateToken(t *testing.T) {
	svc, tokenStore, _ := newTestValidateService()
	ctx := context.Background()

	token := seedTestToken(t, tokenStore, tid("user-1"), tid("project-1"),
		[]string{"agent:attach", "agent:read"})

	t.Run("valid token", func(t *testing.T) {
		identity, err := svc.ValidateToken(ctx, token.plaintext)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if identity.ID() != tid("user-1") {
			t.Errorf("expected user ID 'user-1', got %q", identity.ID())
		}
		if identity.Boundary().ProjectID != tid("project-1") {
			t.Errorf("expected project 'project-1', got %q", identity.Boundary().ProjectID)
		}
		if identity.CredentialID() != token.stored.ID {
			t.Errorf("expected credential ID %q, got %q", token.stored.ID, identity.CredentialID())
		}
		if !identity.HasScope("agent:attach") {
			t.Error("expected identity to have scope agent:attach")
		}
		if identity.HasScope("agent:delete") {
			t.Error("expected identity NOT to have scope agent:delete")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		_, err := svc.ValidateToken(ctx, "scion_pat_invalid_token_value")
		if !errors.Is(err, ErrInvalidUAT) {
			t.Errorf("expected ErrInvalidUAT, got %v", err)
		}
	})

	t.Run("wrong prefix", func(t *testing.T) {
		_, err := svc.ValidateToken(ctx, "sk_live_something")
		if !errors.Is(err, ErrInvalidUATFormat) {
			t.Errorf("expected ErrInvalidUATFormat, got %v", err)
		}
	})

	t.Run("revoked token", func(t *testing.T) {
		revokedToken := seedTestToken(t, tokenStore, tid("user-1"), tid("project-1"),
			[]string{"agent:read"})
		tokenStore.mutate(t, revokedToken.stored.ID, func(tok *store.UserAccessToken) {
			tok.Revoked = true
		})
		_, err := svc.ValidateToken(ctx, revokedToken.plaintext)
		if !errors.Is(err, ErrUATRevoked) {
			t.Errorf("expected ErrUATRevoked, got %v", err)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expiredToken := seedTestToken(t, tokenStore, tid("user-1"), tid("project-1"),
			[]string{"agent:read"})
		past := time.Now().Add(-1 * time.Hour)
		tokenStore.mutate(t, expiredToken.stored.ID, func(tok *store.UserAccessToken) {
			tok.ExpiresAt = &past
		})
		_, err := svc.ValidateToken(ctx, expiredToken.plaintext)
		if !errors.Is(err, ErrUATExpired) {
			t.Errorf("expected ErrUATExpired, got %v", err)
		}
	})
}

// TestValidateToken_RejectsMalformedStoredBoundary pins that a stored row
// whose boundary_kind/project_id combination is invalid must never
// authenticate. Each case simulates a row a real database could never
// produce through CreateUserAccessToken's own ValidateBoundary call — the
// point is that ValidateToken denies it anyway, as a second, independent
// check at load, not just at write. An empty or malformed project ID is
// never coerced into a hub boundary.
func TestValidateToken_RejectsMalformedStoredBoundary(t *testing.T) {
	cases := []struct {
		name         string
		boundaryKind string
		projectID    string
	}{
		{"hub kind with a project id set", "hub", tid("mismatch-project")},
		{"empty kind", "", tid("mismatch-project")},
		{"unrecognized kind", "org", tid("mismatch-project")},
		{"project kind with an empty project id", "project", ""},
		{"project kind with the nil uuid as project id", "project", "00000000-0000-0000-0000-000000000000"},
		{"project kind with a non-uuid project id", "project", "not-a-uuid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, tokenStore, _ := newTestValidateService()
			token := seedTestToken(t, tokenStore, tid("user-1"), tid("mismatch-project"), []string{"agent:read"})

			// Overwrite the stored row directly: no production write path
			// (CreateUserAccessToken) can produce this shape, but a stored
			// row's own load-time validation must still catch it — belt and
			// suspenders, since a malformed row must never authenticate
			// regardless of how it came to exist.
			tokenStore.mutate(t, token.stored.ID, func(stored *store.UserAccessToken) {
				stored.BoundaryKind = c.boundaryKind
				stored.ProjectID = c.projectID
			})

			_, err := svc.ValidateToken(context.Background(), token.plaintext)
			if !errors.Is(err, ErrInvalidUAT) {
				t.Errorf("expected ErrInvalidUAT, got %v", err)
			}
			var rejection *UATRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("expected a *UATRejection, got %T: %v", err, err)
			}
			if rejection.Reason != "invalid" {
				t.Errorf("expected reason %q, got %q", "invalid", rejection.Reason)
			}
			if !rejection.Found || rejection.TokenID != token.stored.ID {
				t.Errorf("expected Found=true and TokenID=%q (a matched, rejected record), got Found=%v TokenID=%q",
					token.stored.ID, rejection.Found, rejection.TokenID)
			}
		})
	}
}

// TestValidateToken_CarriesStoredBoundary pins that ValidateToken carries
// the stored row's boundary kind and project ID onto the ScopedUserIdentity,
// and that credentialContextForIdentity fills CredentialContext.Boundary
// from that identity, for both boundary kinds.
func TestValidateToken_CarriesStoredBoundary(t *testing.T) {
	projectID := tid("carried-project")
	cases := []struct {
		name           string
		boundaryKind   string
		projectID      string
		wantBoundary   TokenBoundary
		wantProjectID  string
		wantDecoration decorationBoundary
	}{
		{
			name:           "hub row",
			boundaryKind:   string(permissions.BoundaryKindHub),
			projectID:      "",
			wantBoundary:   TokenBoundary{Kind: BoundaryKindHub, ProjectID: ""},
			wantProjectID:  "",
			wantDecoration: decorationBoundary{Kind: "hub", ProjectID: ""},
		},
		{
			name:           "project row",
			boundaryKind:   string(permissions.BoundaryKindProject),
			projectID:      projectID,
			wantBoundary:   TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID},
			wantProjectID:  projectID,
			wantDecoration: decorationBoundary{Kind: "project", ProjectID: projectID},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, tokenStore, _ := newTestValidateService()
			token := seedTestToken(t, tokenStore, tid("user-1"), projectID, []string{"agent:read"})
			tokenStore.mutate(t, token.stored.ID, func(stored *store.UserAccessToken) {
				stored.BoundaryKind = c.boundaryKind
				stored.ProjectID = c.projectID
			})

			identity, err := svc.ValidateToken(context.Background(), token.plaintext)
			if err != nil {
				t.Fatalf("ValidateToken: %v", err)
			}
			if got := identity.Boundary(); got != c.wantBoundary {
				t.Errorf("identity.Boundary() = %+v, want %+v", got, c.wantBoundary)
			}
			if got := identity.Boundary().ProjectID; got != c.wantProjectID {
				t.Errorf("identity.Boundary().ProjectID = %q, want %q", got, c.wantProjectID)
			}

			cc := credentialContextForIdentity(identity)
			if cc.Kind != CredentialKindUAT {
				t.Errorf("CredentialContext.Kind = %q, want %q", cc.Kind, CredentialKindUAT)
			}
			if cc.Boundary == nil {
				t.Fatalf("CredentialContext.Boundary is nil for a UAT identity")
			}
			if *cc.Boundary != c.wantBoundary {
				t.Errorf("CredentialContext.Boundary = %+v, want %+v", *cc.Boundary, c.wantBoundary)
			}
			if cc.ProjectID != c.wantProjectID {
				t.Errorf("CredentialContext.ProjectID = %q, want %q", cc.ProjectID, c.wantProjectID)
			}

			decoration := identity.Decoration()
			if decoration == nil {
				t.Fatalf("identity.Decoration() is nil for a UAT identity")
			}
			if decoration.Boundary != c.wantDecoration {
				t.Errorf("identity.Decoration().Boundary = %+v, want %+v", decoration.Boundary, c.wantDecoration)
			}
		})
	}
}

// TestCredentialContextForIdentity_BoundaryOnlyForUAT pins that
// CredentialContext.Boundary is nil for every identity that is not a
// non-nil UAT identity: each non-UAT arm of credentialContextForIdentity, a
// typed-nil *ScopedUserIdentity, and a nil Identity.
func TestCredentialContextForIdentity_BoundaryOnlyForUAT(t *testing.T) {
	var typedNilUAT *ScopedUserIdentity
	cases := []struct {
		name     string
		identity Identity
	}{
		{"nil identity", nil},
		{"typed-nil UAT identity", typedNilUAT},
		{"interactive user", &AuthenticatedUser{}},
		{"dev user", &DevUser{}},
		{"agent JWT identity", &agentIdentityWrapper{}},
		{"stored agent identity", &storedAgentIdentity{}},
		{"peer agent identity", &peerAgentIdentity{}},
		{"explain agent identity", &explainAgentIdentity{}},
		{"federated user", &FederatedUserIdentity{}},
		{"federated agent", &FederatedAgentIdentity{}},
		{"federated service", &FederatedServiceIdentity{}},
		{"broker", &brokerIdentityImpl{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if cc := credentialContextForIdentity(c.identity); cc.Boundary != nil {
				t.Errorf("CredentialContext.Boundary = %+v, want nil", *cc.Boundary)
			}
		})
	}
}

func TestExpandScopes(t *testing.T) {
	skillManageCount := len(permissions.UATManageScopesFor(permissions.ResourceSkill))
	templateManageCount := len(permissions.UATManageScopesFor(permissions.ResourceTemplate))
	harnessConfigManageCount := len(permissions.UATManageScopesFor(permissions.ResourceHarnessConfig))
	groupManageCount := len(permissions.UATManageScopesFor(permissions.ResourceGroup))

	tests := []struct {
		name     string
		input    []string
		expected int
	}{
		{"single scope", []string{"agent:read"}, 1},
		{"manage alias", []string{"agent:manage"}, len(store.UATManageScopes)},
		{"manage with extra", []string{"agent:manage", "project:read"}, len(store.UATManageScopes) + 1},
		{"dedup", []string{"agent:read", "agent:read"}, 1},
		{"manage dedup with explicit", []string{"agent:manage", "agent:read"}, len(store.UATManageScopes)},
		{"skill:manage alias", []string{"skill:manage"}, skillManageCount},
		{"template:manage alias", []string{"template:manage"}, templateManageCount},
		{"harness_config:manage alias", []string{"harness_config:manage"}, harnessConfigManageCount},
		{"group:manage alias", []string{"group:manage"}, groupManageCount},
		{"skill:manage with extra", []string{"skill:manage", "agent:read"}, skillManageCount + 1},
		{"skill:manage dedup with explicit", []string{"skill:manage", "skill:read"}, skillManageCount},
		{"multiple manage aliases", []string{"agent:manage", "skill:manage"}, len(store.UATManageScopes) + skillManageCount},
		{"group:manage with extra and dedup", []string{"group:manage", "group:read", "project:read"}, groupManageCount + 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := expandScopes(tc.input)
			if len(result) != tc.expected {
				t.Errorf("expected %d scopes, got %d: %v", tc.expected, len(result), result)
			}
		})
	}
}

func TestExpandScopes_ManageAliasesExpandToCorrectResource(t *testing.T) {
	for alias, resource := range permissions.UATManageAliases {
		t.Run(alias, func(t *testing.T) {
			result := expandScopes([]string{alias})
			if len(result) == 0 {
				t.Fatalf("%s expanded to zero scopes", alias)
			}
			prefix := resource + ":"
			for _, scope := range result {
				if !strings.HasPrefix(scope, prefix) {
					t.Errorf("%s expanded to non-%s scope %q", alias, resource, scope)
				}
			}
		})
	}
}

func TestScopedUserIdentity(t *testing.T) {
	base := NewAuthenticatedUser(tid("user-1"), "test@example.com", "Test", "member", "api")
	scoped := NewScopedUserIdentity(base, tid("project-1"), []string{"agent:attach", "agent:read"})

	if scoped.ID() != tid("user-1") {
		t.Errorf("expected ID 'user-1', got %q", scoped.ID())
	}
	if scoped.Email() != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", scoped.Email())
	}
	if scoped.Boundary().ProjectID != tid("project-1") {
		t.Errorf("expected project 'project-1', got %q", scoped.Boundary().ProjectID)
	}
	if !scoped.HasScope("agent:attach") {
		t.Error("expected HasScope('agent:attach') to be true")
	}
	if scoped.HasScope("agent:delete") {
		t.Error("expected HasScope('agent:delete') to be false")
	}
}

func TestIsUAT(t *testing.T) {
	if !IsUAT("scion_pat_abc123") {
		t.Error("expected IsUAT to return true for scion_pat_ prefix")
	}
	if IsUAT("sk_live_abc123") {
		t.Error("expected IsUAT to return false for sk_live_ prefix")
	}
	if IsUAT("Bearer something") {
		t.Error("expected IsUAT to return false for Bearer prefix")
	}
}
