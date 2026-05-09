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
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	// UATRandomBytes is the number of random bytes in a UAT.
	UATRandomBytes = 32
	// UATPrefixLength is the length of the visible prefix for identification.
	UATPrefixLength = 12
)

var (
	ErrInvalidUAT        = errors.New("invalid access token")
	ErrUATExpired        = errors.New("access token expired")
	ErrUATRevoked        = errors.New("access token revoked")
	ErrInvalidUATFormat  = errors.New("invalid token format")
	ErrUATLimitExceeded  = errors.New("token limit exceeded")
	ErrInvalidUATScope   = errors.New("invalid token scope")
	ErrUATExpiryTooLong  = errors.New("token expiry exceeds maximum (1 year)")
	ErrUATExpiryRequired = errors.New("token expiry is required")
)

// UserAccessTokenService handles UAT generation, validation, and management.
type UserAccessTokenService struct {
	tokens store.UserAccessTokenStore
	users  store.UserStore
	projects store.ProjectStore
}

// NewUserAccessTokenService creates a new UAT service.
func NewUserAccessTokenService(tokens store.UserAccessTokenStore, users store.UserStore, projects store.ProjectStore) *UserAccessTokenService {
	return &UserAccessTokenService{
		tokens: tokens,
		users:  users,
		projects: projects,
	}
}

// CreateToken generates a new user access token.
// Returns the plaintext token (shown only once) and the stored metadata.
func (s *UserAccessTokenService) CreateToken(ctx context.Context, userID, name, projectID string, scopes []string, expiresAt *time.Time) (string, *store.UserAccessToken, error) {
	if name == "" {
		return "", nil, fmt.Errorf("token name is required")
	}
	if projectID == "" {
		return "", nil, fmt.Errorf("project ID is required")
	}

	// Validate project exists
	if _, err := s.projects.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, fmt.Errorf("project not found: %s", projectID)
		}
		return "", nil, fmt.Errorf("failed to validate project: %w", err)
	}

	// Expand and validate scopes
	expanded := expandScopes(scopes)
	for _, scope := range expanded {
		if !store.UATValidScopes[scope] {
			return "", nil, fmt.Errorf("%w: %s", ErrInvalidUATScope, scope)
		}
	}
	if len(expanded) == 0 {
		return "", nil, fmt.Errorf("at least one scope is required")
	}

	// Validate / default expiry
	now := time.Now()
	if expiresAt == nil {
		defaultExpiry := now.Add(store.UATDefaultExpiry)
		expiresAt = &defaultExpiry
	}
	if expiresAt.Before(now) {
		return "", nil, fmt.Errorf("expiry must be in the future")
	}
	if expiresAt.After(now.Add(store.UATMaxExpiry)) {
		return "", nil, ErrUATExpiryTooLong
	}

	// Check token count limit
	count, err := s.tokens.CountUserAccessTokens(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to check token count: %w", err)
	}
	if count >= store.UATMaxPerUser {
		return "", nil, ErrUATLimitExceeded
	}

	// Generate random token
	randomBytes := make([]byte, UATRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.UATPrefix + keyBody

	// Visible prefix for identification
	prefix := store.UATPrefix + keyBody[:UATPrefixLength]

	// Hash for storage
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	token := &store.UserAccessToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		Prefix:    prefix,
		KeyHash:   hashStr,
		ProjectID:   projectID,
		Scopes:    expanded,
		ExpiresAt: expiresAt,
		Created:   now,
	}

	if err := s.tokens.CreateUserAccessToken(ctx, token); err != nil {
		return "", nil, fmt.Errorf("failed to create token: %w", err)
	}

	return fullKey, token, nil
}

// ValidateToken validates a UAT and returns the scoped user identity.
func (s *UserAccessTokenService) ValidateToken(ctx context.Context, key string) (*ScopedUserIdentity, error) {
	if !strings.HasPrefix(key, store.UATPrefix) {
		return nil, ErrInvalidUATFormat
	}

	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	token, err := s.tokens.GetUserAccessTokenByHash(ctx, hashStr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidUAT
		}
		return nil, fmt.Errorf("failed to look up token: %w", err)
	}

	if token.Revoked {
		return nil, ErrUATRevoked
	}

	if token.ExpiresAt != nil && time.Now().After(*token.ExpiresAt) {
		return nil, ErrUATExpired
	}

	// Update last used (async)
	go func() {
		_ = s.tokens.UpdateUserAccessTokenLastUsed(context.Background(), token.ID)
	}()

	user, err := s.users.GetUser(ctx, token.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("token user not found")
		}
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}

	return NewScopedUserIdentity(
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeAPI)),
		token.ProjectID,
		token.Scopes,
	), nil
}

// ListTokens returns all tokens for a user.
func (s *UserAccessTokenService) ListTokens(ctx context.Context, userID string) ([]store.UserAccessToken, error) {
	return s.tokens.ListUserAccessTokens(ctx, userID)
}

// GetToken retrieves a single token by ID, verifying ownership.
func (s *UserAccessTokenService) GetToken(ctx context.Context, userID, tokenID string) (*store.UserAccessToken, error) {
	token, err := s.tokens.GetUserAccessToken(ctx, tokenID)
	if err != nil {
		return nil, err
	}
	if token.UserID != userID {
		return nil, store.ErrNotFound
	}
	return token, nil
}

// RevokeToken revokes a token, verifying ownership.
func (s *UserAccessTokenService) RevokeToken(ctx context.Context, userID, tokenID string) error {
	token, err := s.tokens.GetUserAccessToken(ctx, tokenID)
	if err != nil {
		return err
	}
	if token.UserID != userID {
		return store.ErrNotFound
	}
	return s.tokens.RevokeUserAccessToken(ctx, tokenID)
}

// DeleteToken permanently deletes a token, verifying ownership.
func (s *UserAccessTokenService) DeleteToken(ctx context.Context, userID, tokenID string) error {
	token, err := s.tokens.GetUserAccessToken(ctx, tokenID)
	if err != nil {
		return err
	}
	if token.UserID != userID {
		return store.ErrNotFound
	}
	return s.tokens.DeleteUserAccessToken(ctx, tokenID)
}

// expandScopes expands convenience aliases like agent:manage.
func expandScopes(scopes []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, scope := range scopes {
		if scope == store.UATScopeAgentManage {
			for _, s := range store.UATManageScopes {
				if !seen[s] {
					seen[s] = true
					result = append(result, s)
				}
			}
		} else if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return result
}

// IsUAT returns true if the token appears to be a user access token.
func IsUAT(token string) bool {
	return strings.HasPrefix(token, store.UATPrefix)
}
