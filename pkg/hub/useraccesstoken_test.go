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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockUATStore implements store.UserAccessTokenStore for testing.
type mockUATStore struct {
	mu     sync.Mutex
	tokens map[string]*store.UserAccessToken
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
	for _, t := range m.tokens {
		if t.KeyHash == token.KeyHash {
			return store.ErrAlreadyExists
		}
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

// mockProjectStore implements store.ProjectStore for testing (minimal).
type mockProjectStore struct {
	projects map[string]*store.Project
}

func (m *mockProjectStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	g, ok := m.projects[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return g, nil
}
func (m *mockProjectStore) CreateProject(context.Context, *store.Project) error { return nil }
func (m *mockProjectStore) UpdateProject(context.Context, *store.Project) error { return nil }
func (m *mockProjectStore) DeleteProject(context.Context, string) error       { return nil }
func (m *mockProjectStore) GetProjectBySlug(context.Context, string) (*store.Project, error) {
	return nil, store.ErrNotFound
}
func (m *mockProjectStore) GetProjectBySlugCaseInsensitive(context.Context, string) (*store.Project, error) {
	return nil, store.ErrNotFound
}
func (m *mockProjectStore) GetProjectsByGitRemote(context.Context, string) ([]*store.Project, error) {
	return []*store.Project{}, nil
}
func (m *mockProjectStore) NextAvailableSlug(_ context.Context, baseSlug string) (string, error) {
	return baseSlug, nil
}
func (m *mockProjectStore) ListProjects(context.Context, store.ProjectFilter, store.ListOptions) (*store.ListResult[store.Project], error) {
	return nil, nil
}

func newTestUATService() (*UserAccessTokenService, *mockUATStore, *mockUserStore) {
	tokenStore := newMockUATStore()
	userStore := &mockUserStore{
		users: map[string]*store.User{
			"user-1": {ID: "user-1", Email: "test@example.com", DisplayName: "Test User", Role: "member"},
		},
	}
	projectStore := &mockProjectStore{
		projects: map[string]*store.Project{
			"project-1": {ID: "project-1", Name: "test-project"},
		},
	}
	svc := NewUserAccessTokenService(tokenStore, userStore, projectStore)
	return svc, tokenStore, userStore
}

func TestCreateToken(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	t.Run("basic creation", func(t *testing.T) {
		key, token, err := svc.CreateToken(ctx, "user-1", "ci-token", "project-1",
			[]string{"agent:dispatch", "agent:read"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(key, store.UATPrefix) {
			t.Errorf("expected key to start with %q, got %q", store.UATPrefix, key[:20])
		}
		if token.Name != "ci-token" {
			t.Errorf("expected name 'ci-token', got %q", token.Name)
		}
		if token.ProjectID != "project-1" {
			t.Errorf("expected projectID 'project-1', got %q", token.ProjectID)
		}
		if len(token.Scopes) != 2 {
			t.Errorf("expected 2 scopes, got %d", len(token.Scopes))
		}
		if token.ExpiresAt == nil {
			t.Error("expected default expiry to be set")
		}
	})

	t.Run("expands agent:manage", func(t *testing.T) {
		_, token, err := svc.CreateToken(ctx, "user-1", "manage-token", "project-1",
			[]string{"agent:manage"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(token.Scopes) != len(store.UATManageScopes) {
			t.Errorf("expected %d expanded scopes, got %d", len(store.UATManageScopes), len(token.Scopes))
		}
	})

	t.Run("rejects invalid scope", func(t *testing.T) {
		_, _, err := svc.CreateToken(ctx, "user-1", "bad-token", "project-1",
			[]string{"invalid:scope"}, nil)
		if !errors.Is(err, ErrInvalidUATScope) {
			t.Errorf("expected ErrInvalidUATScope, got %v", err)
		}
	})

	t.Run("rejects missing project", func(t *testing.T) {
		_, _, err := svc.CreateToken(ctx, "user-1", "bad-token", "nonexistent",
			[]string{"agent:read"}, nil)
		if err == nil {
			t.Error("expected error for nonexistent project")
		}
	})

	t.Run("rejects expiry too long", func(t *testing.T) {
		tooFar := time.Now().Add(400 * 24 * time.Hour)
		_, _, err := svc.CreateToken(ctx, "user-1", "bad-token", "project-1",
			[]string{"agent:read"}, &tooFar)
		if !errors.Is(err, ErrUATExpiryTooLong) {
			t.Errorf("expected ErrUATExpiryTooLong, got %v", err)
		}
	})

	t.Run("rejects empty scopes", func(t *testing.T) {
		_, _, err := svc.CreateToken(ctx, "user-1", "bad-token", "project-1",
			[]string{}, nil)
		if err == nil {
			t.Error("expected error for empty scopes")
		}
	})
}

func TestValidateToken(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	key, _, err := svc.CreateToken(ctx, "user-1", "test-token", "project-1",
		[]string{"agent:dispatch", "agent:read"}, nil)
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	t.Run("valid token", func(t *testing.T) {
		identity, err := svc.ValidateToken(ctx, key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if identity.ID() != "user-1" {
			t.Errorf("expected user ID 'user-1', got %q", identity.ID())
		}
		if identity.ScopedProjectID() != "project-1" {
			t.Errorf("expected project 'project-1', got %q", identity.ScopedProjectID())
		}
		if !identity.HasScope("agent:dispatch") {
			t.Error("expected identity to have scope agent:dispatch")
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
}

func TestRevokeToken(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	key, token, err := svc.CreateToken(ctx, "user-1", "test-token", "project-1",
		[]string{"agent:read"}, nil)
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	// Revoke it
	if err := svc.RevokeToken(ctx, "user-1", token.ID); err != nil {
		t.Fatalf("failed to revoke token: %v", err)
	}

	// Validation should fail
	_, err = svc.ValidateToken(ctx, key)
	if !errors.Is(err, ErrUATRevoked) {
		t.Errorf("expected ErrUATRevoked after revocation, got %v", err)
	}

	// Wrong user can't revoke
	if err := svc.RevokeToken(ctx, "other-user", token.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for wrong user, got %v", err)
	}
}

func TestDeleteToken(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	key, token, err := svc.CreateToken(ctx, "user-1", "test-token", "project-1",
		[]string{"agent:read"}, nil)
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	if err := svc.DeleteToken(ctx, "user-1", token.ID); err != nil {
		t.Fatalf("failed to delete token: %v", err)
	}

	// Validation should fail
	_, err = svc.ValidateToken(ctx, key)
	if !errors.Is(err, ErrInvalidUAT) {
		t.Errorf("expected ErrInvalidUAT after deletion, got %v", err)
	}
}

func TestTokenLimit(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	// Create max tokens
	for i := 0; i < store.UATMaxPerUser; i++ {
		_, _, err := svc.CreateToken(ctx, "user-1", "token-"+string(rune('a'+i%26))+string(rune('0'+i/26)), "project-1",
			[]string{"agent:read"}, nil)
		if err != nil {
			t.Fatalf("failed to create token %d: %v", i, err)
		}
	}

	// Next one should fail
	_, _, err := svc.CreateToken(ctx, "user-1", "one-too-many", "project-1",
		[]string{"agent:read"}, nil)
	if !errors.Is(err, ErrUATLimitExceeded) {
		t.Errorf("expected ErrUATLimitExceeded, got %v", err)
	}
}

func TestListTokens(t *testing.T) {
	svc, _, _ := newTestUATService()
	ctx := context.Background()

	// Create 3 tokens
	for i := 0; i < 3; i++ {
		_, _, err := svc.CreateToken(ctx, "user-1", "token-"+string(rune('a'+i)), "project-1",
			[]string{"agent:read"}, nil)
		if err != nil {
			t.Fatalf("failed to create token: %v", err)
		}
	}

	tokens, err := svc.ListTokens(ctx, "user-1")
	if err != nil {
		t.Fatalf("failed to list tokens: %v", err)
	}
	if len(tokens) != 3 {
		t.Errorf("expected 3 tokens, got %d", len(tokens))
	}

	// Different user should see no tokens
	tokens, err = svc.ListTokens(ctx, "user-2")
	if err != nil {
		t.Fatalf("failed to list tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens for other user, got %d", len(tokens))
	}
}

func TestExpandScopes(t *testing.T) {
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

func TestScopedUserIdentity(t *testing.T) {
	base := NewAuthenticatedUser("user-1", "test@example.com", "Test", "member", "api")
	scoped := NewScopedUserIdentity(base, "project-1", []string{"agent:dispatch", "agent:read"})

	if scoped.ID() != "user-1" {
		t.Errorf("expected ID 'user-1', got %q", scoped.ID())
	}
	if scoped.Email() != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", scoped.Email())
	}
	if scoped.ScopedProjectID() != "project-1" {
		t.Errorf("expected project 'project-1', got %q", scoped.ScopedProjectID())
	}
	if !scoped.HasScope("agent:dispatch") {
		t.Error("expected HasScope('agent:dispatch') to be true")
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
