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

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/store"
)

const (
	// APIKeyRandomBytes is the number of random bytes in an API key.
	APIKeyRandomBytes = 24
	// APIKeyPrefixLength is the length of the visible prefix for identification.
	APIKeyPrefixLength = 12
)

var (
	// ErrInvalidAPIKey is returned when an API key is invalid.
	ErrInvalidAPIKey = errors.New("invalid API key")
	// ErrAPIKeyExpired is returned when an API key has expired.
	ErrAPIKeyExpired = errors.New("API key expired")
	// ErrAPIKeyRevoked is returned when an API key has been revoked.
	ErrAPIKeyRevoked = errors.New("API key revoked")
	// ErrInvalidKeyFormat is returned when the key format is invalid.
	ErrInvalidKeyFormat = errors.New("invalid key format")
)

// APIKeyService handles API key generation and validation.
type APIKeyService struct {
	store store.APIKeyStore
	users store.UserStore
}

// NewAPIKeyService creates a new API key service.
func NewAPIKeyService(keyStore store.APIKeyStore, userStore store.UserStore) *APIKeyService {
	return &APIKeyService{
		store: keyStore,
		users: userStore,
	}
}

// CreateAPIKey generates a new API key for a user.
// Returns the full key (only shown once) and the stored key metadata.
func (s *APIKeyService) CreateAPIKey(ctx context.Context, userID, name string, scopes []string, expiresAt *time.Time) (string, *store.APIKey, error) {
	// Generate random bytes for the key
	randomBytes := make([]byte, APIKeyRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Create the full key with prefix
	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.APIKeyPrefixLive + keyBody

	// Create the visible prefix for identification
	prefix := store.APIKeyPrefixLive + keyBody[:APIKeyPrefixLength]

	// Hash the full key for storage
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	// Create the API key record
	apiKey := &store.APIKey{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		Prefix:    prefix,
		KeyHash:   hashStr,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
		Created:   time.Now(),
	}

	if err := s.store.CreateAPIKey(ctx, apiKey); err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}

	return fullKey, apiKey, nil
}

// ValidateAPIKey validates an API key and returns the associated user identity.
func (s *APIKeyService) ValidateAPIKey(ctx context.Context, key string) (UserIdentity, error) {
	// Validate key format
	if !strings.HasPrefix(key, store.APIKeyPrefixLive) && !strings.HasPrefix(key, store.APIKeyPrefixTest) {
		return nil, ErrInvalidKeyFormat
	}

	// Hash the provided key
	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	// Look up by hash
	apiKey, err := s.store.GetAPIKeyByHash(ctx, hashStr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidAPIKey
		}
		return nil, fmt.Errorf("failed to look up API key: %w", err)
	}

	// Check if revoked
	if apiKey.Revoked {
		return nil, ErrAPIKeyRevoked
	}

	// Check expiration
	if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
		return nil, ErrAPIKeyExpired
	}

	// Update last used (async to not block validation)
	go func() {
		_ = s.store.UpdateAPIKeyLastUsed(context.Background(), apiKey.ID)
	}()

	// Look up the user
	user, err := s.users.GetUser(ctx, apiKey.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("API key user not found")
		}
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}

	return NewAuthenticatedUser(
		user.ID,
		user.Email,
		user.DisplayName,
		user.Role,
		string(ClientTypeAPI),
	), nil
}

// ListAPIKeys returns all API keys for a user (without the actual key values).
func (s *APIKeyService) ListAPIKeys(ctx context.Context, userID string) ([]store.APIKey, error) {
	return s.store.ListAPIKeys(ctx, userID)
}

// RevokeAPIKey revokes an API key.
func (s *APIKeyService) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	// Get the key
	apiKey, err := s.store.GetAPIKey(ctx, keyID)
	if err != nil {
		return err
	}

	// Verify ownership
	if apiKey.UserID != userID {
		return store.ErrNotFound // Don't reveal that the key exists
	}

	// Mark as revoked
	apiKey.Revoked = true
	return s.store.UpdateAPIKey(ctx, apiKey)
}

// DeleteAPIKey permanently deletes an API key.
func (s *APIKeyService) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	// Get the key
	apiKey, err := s.store.GetAPIKey(ctx, keyID)
	if err != nil {
		return err
	}

	// Verify ownership
	if apiKey.UserID != userID {
		return store.ErrNotFound // Don't reveal that the key exists
	}

	return s.store.DeleteAPIKey(ctx, keyID)
}

// IsAPIKey returns true if the token appears to be an API key.
func IsAPIKey(token string) bool {
	return strings.HasPrefix(token, store.APIKeyPrefixLive) || strings.HasPrefix(token, store.APIKeyPrefixTest)
}
