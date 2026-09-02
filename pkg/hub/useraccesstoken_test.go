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
	"strings"
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
type mockUATStore struct {
	tokens map[string]*store.UserAccessToken
}

func newMockUATStore() *mockUATStore {
	return &mockUATStore{tokens: make(map[string]*store.UserAccessToken)}
}

func (m *mockUATStore) CreateUserAccessToken(_ context.Context, token *store.UserAccessToken) error {
	if _, exists := m.tokens[token.ID]; exists {
		return store.ErrAlreadyExists
	}
	cp := *token
	m.tokens[token.ID] = &cp
	return nil
}

func (m *mockUATStore) GetUserAccessToken(_ context.Context, id string) (*store.UserAccessToken, error) {
	t, ok := m.tokens[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *mockUATStore) GetUserAccessTokenByHash(_ context.Context, hash string) (*store.UserAccessToken, error) {
	for _, t := range m.tokens {
		if t.KeyHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockUATStore) UpdateUserAccessTokenLastUsed(_ context.Context, id string) error {
	t, ok := m.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	t.LastUsed = &now
	return nil
}

func (m *mockUATStore) RevokeUserAccessToken(_ context.Context, id string) error {
	t, ok := m.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	t.Revoked = true
	return nil
}

func (m *mockUATStore) DeleteUserAccessToken(_ context.Context, id string) error {
	if _, ok := m.tokens[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.tokens, id)
	return nil
}

func (m *mockUATStore) ListUserAccessTokens(_ context.Context, userID string) ([]store.UserAccessToken, error) {
	var result []store.UserAccessToken
	for _, t := range m.tokens {
		if t.UserID == userID {
			result = append(result, *t)
		}
	}
	return result, nil
}

func (m *mockUATStore) CountUserAccessTokens(_ context.Context, userID string) (int, error) {
	count := 0
	for _, t := range m.tokens {
		if t.UserID == userID && !t.Revoked {
			count++
		}
	}
	return count, nil
}

func (m *mockUATStore) DeleteUserAccessTokensByProject(_ context.Context, projectID string) (int, error) {
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
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      "test-token",
		Prefix:    prefix,
		KeyHash:   hashStr,
		ProjectID: projectID,
		Scopes:    scopes,
		ExpiresAt: &future,
		Created:   time.Now(),
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
		if identity.ScopedProjectID() != tid("project-1") {
			t.Errorf("expected project 'project-1', got %q", identity.ScopedProjectID())
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
		tokenStore.tokens[revokedToken.stored.ID].Revoked = true
		_, err := svc.ValidateToken(ctx, revokedToken.plaintext)
		if !errors.Is(err, ErrUATRevoked) {
			t.Errorf("expected ErrUATRevoked, got %v", err)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expiredToken := seedTestToken(t, tokenStore, tid("user-1"), tid("project-1"),
			[]string{"agent:read"})
		past := time.Now().Add(-1 * time.Hour)
		tokenStore.tokens[expiredToken.stored.ID].ExpiresAt = &past
		_, err := svc.ValidateToken(ctx, expiredToken.plaintext)
		if !errors.Is(err, ErrUATExpired) {
			t.Errorf("expected ErrUATExpired, got %v", err)
		}
	})
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
	if scoped.ScopedProjectID() != tid("project-1") {
		t.Errorf("expected project 'project-1', got %q", scoped.ScopedProjectID())
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
