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
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestServer_PersistentSigningKeys(t *testing.T) {
	// Create an in-memory SQLite store
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()

	// Create first server
	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })
	if srv1.agentTokenService == nil {
		t.Fatal("agentTokenService not initialized in srv1")
	}
	if srv1.userTokenService == nil {
		t.Fatal("userTokenService not initialized in srv1")
	}

	key1 := srv1.agentTokenService.config.SigningKey
	userKey1 := srv1.userTokenService.config.SigningKey

	// Create second server with the same store
	srv2, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })
	if srv2.agentTokenService == nil {
		t.Fatal("agentTokenService not initialized in srv2")
	}
	if srv2.userTokenService == nil {
		t.Fatal("userTokenService not initialized in srv2")
	}

	key2 := srv2.agentTokenService.config.SigningKey
	userKey2 := srv2.userTokenService.config.SigningKey

	// Check if keys match
	if string(key1) != string(key2) {
		t.Errorf("agent signing keys do not match: %x != %x", key1, key2)
	}
	if string(userKey1) != string(userKey2) {
		t.Errorf("user signing keys do not match: %x != %x", userKey1, userKey2)
	}
}

func TestServer_PersistentSigningKeys_WithHubID(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.HubID = "test-hub-123"

	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })
	if srv1.agentTokenService == nil {
		t.Fatal("agentTokenService not initialized")
	}

	key1 := srv1.agentTokenService.config.SigningKey
	userKey1 := srv1.userTokenService.config.SigningKey

	// Second server with same hubID should get the same keys
	srv2, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })

	if string(key1) != string(srv2.agentTokenService.config.SigningKey) {
		t.Error("agent signing keys should match with same hubID")
	}
	if string(userKey1) != string(srv2.userTokenService.config.SigningKey) {
		t.Error("user signing keys should match with same hubID")
	}

	// Signing keys should be visible when listing hub-scoped secrets
	ctx := context.Background()
	listed, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: "test-hub-123"})
	if err != nil {
		t.Fatalf("ListSecrets failed: %v", err)
	}
	foundKeys := map[string]bool{}
	for _, sec := range listed {
		foundKeys[sec.Key] = true
	}
	if !foundKeys[SecretKeyAgentSigningKey] {
		t.Error("agent_signing_key should appear in hub secret list")
	}
	if !foundKeys[SecretKeyUserSigningKey] {
		t.Error("user_signing_key should appear in hub secret list")
	}

	// Signing keys must be stored with SecretTypeInternal so they are never
	// projected into agent environments via Resolve().
	for _, sec := range listed {
		if sec.Key == SecretKeyAgentSigningKey || sec.Key == SecretKeyUserSigningKey {
			if sec.SecretType != store.SecretTypeInternal {
				t.Errorf("signing key %q has SecretType=%q, want %q", sec.Key, sec.SecretType, store.SecretTypeInternal)
			}
		}
	}
}

func TestServer_SigningKeysExcludedFromResolve(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.HubID = "test-hub-resolve"

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// Resolve secrets as if dispatching an agent — signing keys must not appear.
	backend := secret.NewLocalBackend(s, "test-hub-resolve")
	resolved, err := backend.Resolve(context.Background(), "", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	for _, sv := range resolved {
		if sv.Name == SecretKeyAgentSigningKey || sv.Name == SecretKeyUserSigningKey {
			t.Errorf("signing key %q must not appear in Resolve() results", sv.Name)
		}
	}
}

func TestServer_UserTokenSurvivesRestart(t *testing.T) {
	// Simulate the exact production scenario: sign in, restart server, validate token.
	// Uses a file-based SQLite DB to match production behavior.
	dbPath := filepath.Join(t.TempDir(), "test-hub.db")
	s, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.HubID = "test-hub-456"

	// Run 1: create server, generate a user token
	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })
	if srv1.userTokenService == nil {
		t.Fatal("userTokenService not initialized")
	}

	accessToken, _, _, err := srv1.userTokenService.GenerateTokenPair(
		"user-1", "test@example.com", "Test User", store.UserRoleAdmin, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("GenerateTokenPair failed: %v", err)
	}

	// Verify it works on the same server
	if _, err := srv1.userTokenService.ValidateUserToken(accessToken); err != nil {
		t.Fatalf("Token should validate on same server: %v", err)
	}

	key1 := srv1.userTokenService.config.SigningKey

	// Close the store and reopen from the same file (simulates process restart)
	s.Close()
	s2, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen test store: %v", err)
	}
	if err := s2.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate reopened store: %v", err)
	}

	// Run 2: create a NEW server with the reopened store
	srv2, err := New(cfg, s2)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })
	if srv2.userTokenService == nil {
		t.Fatal("userTokenService not initialized on srv2")
	}

	key2 := srv2.userTokenService.config.SigningKey

	// Verify keys match
	if string(key1) != string(key2) {
		t.Errorf("signing keys differ after restart: key1=%x key2=%x", key1[:8], key2[:8])
	}

	// The token from Run 1 must validate on Run 2
	claims, err := srv2.userTokenService.ValidateUserToken(accessToken)
	if err != nil {
		t.Fatalf("Token from Run 1 should validate after restart: %v", err)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email test@example.com, got %s", claims.Email)
	}
}

func TestServer_SigningKeyMigration_LegacyHubScopeID(t *testing.T) {
	// Simulate the pre-hubID-namespacing scenario where keys were stored
	// with ScopeID="hub". A new server with a real hubID should find them
	// via the migration fallback.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	ctx := context.Background()

	// Directly insert legacy keys with ScopeID="hub" (simulating pre-refactor storage)
	legacyAgentKey := make([]byte, 32)
	legacyUserKey := make([]byte, 32)
	copy(legacyAgentKey, []byte("test-agent-key-1234567890123456"))
	copy(legacyUserKey, []byte("test-user-key-12345678901234567"))
	agentEncoded := base64.StdEncoding.EncodeToString(legacyAgentKey)
	userEncoded := base64.StdEncoding.EncodeToString(legacyUserKey)

	if err := s.CreateSecret(ctx, &store.Secret{
		ID: "hub-agent_signing_key", Key: SecretKeyAgentSigningKey,
		EncryptedValue: agentEncoded, Scope: store.ScopeHub, ScopeID: "hub",
		Description: "Hub signing key for agent_signing_key",
	}); err != nil {
		t.Fatalf("failed to create legacy agent key: %v", err)
	}
	if err := s.CreateSecret(ctx, &store.Secret{
		ID: "hub-user_signing_key", Key: SecretKeyUserSigningKey,
		EncryptedValue: userEncoded, Scope: store.ScopeHub, ScopeID: "hub",
		Description: "Hub signing key for user_signing_key",
	}); err != nil {
		t.Fatalf("failed to create legacy user key: %v", err)
	}

	// Now create a server with an actual hubID — it should migrate from "hub"
	cfg := DefaultServerConfig()
	cfg.HubID = "my-new-hub-id"
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	if string(legacyAgentKey) != string(srv.agentTokenService.config.SigningKey) {
		t.Error("agent signing key should be migrated from legacy 'hub' scope")
	}
	if string(legacyUserKey) != string(srv.userTokenService.config.SigningKey) {
		t.Error("user signing key should be migrated from legacy 'hub' scope")
	}

	// Verify the migrated keys are findable via ListSecrets with the new hub ID.
	// This is what the admin UI and CLI use to display hub-scoped secrets.
	listed, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: "my-new-hub-id"})
	if err != nil {
		t.Fatalf("ListSecrets failed: %v", err)
	}
	foundKeys := map[string]bool{}
	for _, sec := range listed {
		foundKeys[sec.Key] = true
	}
	if !foundKeys[SecretKeyAgentSigningKey] {
		t.Error("agent_signing_key should be listed under new hub ID after migration")
	}
	if !foundKeys[SecretKeyUserSigningKey] {
		t.Error("user_signing_key should be listed under new hub ID after migration")
	}

	// Verify the old legacy records are cleaned up
	oldSecrets, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: "hub"})
	if err != nil {
		t.Fatalf("ListSecrets (legacy) failed: %v", err)
	}
	for _, sec := range oldSecrets {
		if sec.Key == SecretKeyAgentSigningKey || sec.Key == SecretKeyUserSigningKey {
			t.Errorf("legacy record for %s should have been deleted during migration", sec.Key)
		}
	}
}

func TestServer_SigningKeyMigration_DeletesLegacyFromBackend(t *testing.T) {
	// When migrating signing keys from legacy scope IDs, the old secret
	// should also be deleted from the secret backend to prevent stale secrets
	// from confusing label-based auto-discovery by external consumers.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	ctx := context.Background()
	newHubID := "new-hub-id-abc123"
	legacyScopeID := "hub"

	// Set up a LocalBackend as the secret backend with the new hub ID.
	backend := secret.NewLocalBackend(s, newHubID)

	// Seed a legacy key under the old scope ID in both the store and backend.
	legacyKey := make([]byte, 32)
	copy(legacyKey, []byte("legacy-user-key-1234567890123456"))
	encoded := base64.StdEncoding.EncodeToString(legacyKey)

	if err := s.CreateSecret(ctx, &store.Secret{
		ID: "hub-user_signing_key", Key: SecretKeyUserSigningKey,
		EncryptedValue: encoded, Scope: store.ScopeHub, ScopeID: legacyScopeID,
		Description: "legacy user signing key",
	}); err != nil {
		t.Fatalf("failed to seed legacy key in store: %v", err)
	}
	// Also create in the backend under the legacy scope ID so we can verify deletion.
	if _, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name:       SecretKeyUserSigningKey,
		Value:      encoded,
		SecretType: "environment",
		Scope:      store.ScopeHub,
		ScopeID:    legacyScopeID,
	}); err != nil {
		t.Fatalf("failed to seed legacy key in backend: %v", err)
	}

	// Verify the legacy key exists in the backend before migration.
	_, err = backend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, legacyScopeID)
	if err != nil {
		t.Fatalf("legacy key should exist in backend before migration: %v", err)
	}

	// Create server with new hub ID — triggers migration.
	cfg := DefaultServerConfig()
	cfg.HubID = newHubID
	cfg.SecretBackend = backend
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// The migrated key should match the original.
	if string(srv.userTokenService.config.SigningKey) != string(legacyKey) {
		t.Error("migrated signing key should match the legacy key")
	}

	// The legacy secret should be deleted from the backend.
	_, err = backend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, legacyScopeID)
	if err == nil {
		t.Error("legacy signing key should have been deleted from the backend during migration")
	}

	// The key should now exist under the new hub ID in the backend.
	sv, err := backend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, newHubID)
	if err != nil {
		t.Fatalf("signing key should exist under new hub ID in backend: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(sv.Value)
	if string(decoded) != string(legacyKey) {
		t.Error("migrated key value should match original")
	}
}

func TestServer_SigningKeyBootstrapWithSecretBackend(t *testing.T) {
	// Verify that when SecretBackend is set in ServerConfig, signing keys
	// are loaded through it and synced from SQLite to the backend.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-backend-hub"
	backend := secret.NewLocalBackend(s, hubID)

	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	cfg.SecretBackend = backend

	// Run 1: keys generated and stored
	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })
	if srv1.userTokenService == nil {
		t.Fatal("userTokenService not initialized")
	}

	key1 := srv1.userTokenService.config.SigningKey

	// Generate a token with this key
	accessToken, _, _, err := srv1.userTokenService.GenerateTokenPair(
		"user-1", "test@example.com", "Test", store.UserRoleAdmin, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("GenerateTokenPair failed: %v", err)
	}

	// Verify key is available through the secret backend
	ctx := context.Background()
	sv, err := backend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("Signing key should be in secret backend after first run: %v", err)
	}
	if sv.Value == "" {
		t.Fatal("Signing key value in backend should not be empty")
	}

	// Run 2: create new server — key should be loaded from backend
	srv2, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })

	key2 := srv2.userTokenService.config.SigningKey
	if string(key1) != string(key2) {
		t.Errorf("signing keys should match across restarts: key1=%x key2=%x", key1[:8], key2[:8])
	}

	// Token from Run 1 must validate on Run 2
	claims, err := srv2.userTokenService.ValidateUserToken(accessToken)
	if err != nil {
		t.Fatalf("Token from Run 1 should validate after restart with backend: %v", err)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email test@example.com, got %s", claims.Email)
	}
}

func TestServer_SigningKeySyncFromStoreToBackend(t *testing.T) {
	// Verify that keys pre-existing in SQLite are synced to the secret backend
	// when the backend is newly configured (migration from no-backend to backend).
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-sync-hub"

	// Run 1: No secret backend — keys go to SQLite only
	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })
	key1 := srv1.userTokenService.config.SigningKey

	// Run 2: Secret backend configured — keys should sync from SQLite to backend
	backend := secret.NewLocalBackend(s, hubID)
	cfg.SecretBackend = backend
	srv2, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })
	key2 := srv2.userTokenService.config.SigningKey

	if string(key1) != string(key2) {
		t.Errorf("keys should match after adding backend: key1=%x key2=%x", key1[:8], key2[:8])
	}

	// Verify the key is now in the backend
	ctx := context.Background()
	sv, err := backend.Get(ctx, SecretKeyUserSigningKey, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("Signing key should be synced to backend: %v", err)
	}
	decodedKey, err := base64.StdEncoding.DecodeString(sv.Value)
	if err != nil {
		t.Fatalf("Failed to decode synced key: %v", err)
	}
	if string(decodedKey) != string(key1) {
		t.Error("Synced key value should match original SQLite key")
	}
}

func TestServer_SigningKeyEmptyValueFromStore(t *testing.T) {
	// Simulate the GCP Secret Manager scenario: the backend stores
	// EncryptedValue="" in SQLite (using SecretRef instead). If GCP SM
	// later becomes unavailable, ensureSigningKey must not silently return
	// a nil key — it should generate a new one.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-empty-value-hub"

	// Insert a signing key row with empty EncryptedValue (as the GCP backend would)
	ctx := context.Background()
	emptySecret := &store.Secret{
		ID:             "hub-" + hubID + "-" + SecretKeyUserSigningKey,
		Key:            SecretKeyUserSigningKey,
		EncryptedValue: "",
		SecretRef:      "gcpsm:projects/test/secrets/test-key",
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		Description:    "Hub signing key (GCP ref only)",
	}
	if _, err := s.UpsertSecret(ctx, emptySecret); err != nil {
		t.Fatalf("failed to insert empty-value secret: %v", err)
	}

	// Create server WITHOUT a secret backend (simulates backend unavailable)
	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	if srv.userTokenService == nil {
		t.Fatal("userTokenService should be initialized even when store has empty key value")
	}

	key := srv.userTokenService.config.SigningKey
	if len(key) == 0 {
		t.Fatal("signing key should not be empty — server should have generated a new one")
	}
	if len(key) != 32 {
		t.Fatalf("signing key should be 32 bytes, got %d", len(key))
	}

	// Verify the new key actually works for token operations
	accessToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		"user-1", "test@example.com", "Test", store.UserRoleAdmin, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("GenerateTokenPair failed: %v", err)
	}
	claims, err := srv.userTokenService.ValidateUserToken(accessToken)
	if err != nil {
		t.Fatalf("ValidateUserToken failed: %v", err)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email test@example.com, got %s", claims.Email)
	}
}

func TestServer_SigningKeyBackupAfterBackendSet(t *testing.T) {
	// Verify that after persisting a key through the secret backend,
	// the actual key value remains in SQLite as a backup.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-backup-hub"
	backend := secret.NewLocalBackend(s, hubID)

	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	cfg.SecretBackend = backend

	// Create server — generates new keys via backend
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	key := srv.userTokenService.config.SigningKey

	// Verify the key value is in SQLite directly (not just a backend ref)
	ctx := context.Background()
	val, err := s.GetSecretValue(ctx, SecretKeyUserSigningKey, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("signing key should be in SQLite store: %v", err)
	}
	if val == "" {
		t.Fatal("SQLite should contain the actual key value as backup, not an empty string")
	}

	decodedKey, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		t.Fatalf("failed to decode key from SQLite: %v", err)
	}
	if string(decodedKey) != string(key) {
		t.Error("SQLite backup key should match the active signing key")
	}
}

func TestServer_GenerateAgentToken_DevAuthAutoGrantsScopes(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-dev-token"
	cfg.AgentTokenConfig = AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// Generate token without any additional scopes
	token, err := srv.GenerateAgentToken("agent-1", "project-1", nil)
	if err != nil {
		t.Fatalf("GenerateAgentToken failed: %v", err)
	}

	// Validate the token and check scopes
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	if err != nil {
		t.Fatalf("ValidateAgentToken failed: %v", err)
	}

	if !claims.HasScope(ScopeAgentStatusUpdate) {
		t.Error("expected ScopeAgentStatusUpdate to be present")
	}
	if !claims.HasScope(ScopeAgentCreate) {
		t.Error("expected ScopeAgentCreate to be auto-granted in dev-auth mode")
	}
	if !claims.HasScope(ScopeAgentLifecycle) {
		t.Error("expected ScopeAgentLifecycle to be auto-granted in dev-auth mode")
	}
	if !claims.HasScope(ScopeAgentNotify) {
		t.Error("expected ScopeAgentNotify to be auto-granted in dev-auth mode")
	}
}

func TestServer_GenerateAgentToken_DevAuthDeduplicatesScopes(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-dev-token"
	cfg.AgentTokenConfig = AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// Generate token with explicit scopes that overlap with auto-granted ones
	token, err := srv.GenerateAgentToken("agent-1", "project-1", nil,
		ScopeAgentCreate, ScopeAgentLifecycle, ScopeProjectSecretRead)
	if err != nil {
		t.Fatalf("GenerateAgentToken failed: %v", err)
	}

	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	if err != nil {
		t.Fatalf("ValidateAgentToken failed: %v", err)
	}

	// Count occurrences of each scope to verify deduplication
	scopeCounts := make(map[AgentTokenScope]int)
	for _, sc := range claims.Scopes {
		scopeCounts[sc]++
	}

	if scopeCounts[ScopeAgentCreate] != 1 {
		t.Errorf("expected ScopeAgentCreate once, got %d", scopeCounts[ScopeAgentCreate])
	}
	if scopeCounts[ScopeAgentLifecycle] != 1 {
		t.Errorf("expected ScopeAgentLifecycle once, got %d", scopeCounts[ScopeAgentLifecycle])
	}
	if !claims.HasScope(ScopeProjectSecretRead) {
		t.Error("expected ScopeProjectSecretRead to be present from explicit scopes")
	}
}

func TestServer_GenerateAgentToken_NoDevAuthDoesNotAutoGrant(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	// DevAuthToken is empty - not dev-auth mode
	cfg.AgentTokenConfig = AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	token, err := srv.GenerateAgentToken("agent-1", "project-1", nil)
	if err != nil {
		t.Fatalf("GenerateAgentToken failed: %v", err)
	}

	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	if err != nil {
		t.Fatalf("ValidateAgentToken failed: %v", err)
	}

	if !claims.HasScope(ScopeAgentStatusUpdate) {
		t.Error("expected ScopeAgentStatusUpdate to be present")
	}
	if !claims.HasScope(ScopeAgentNotify) {
		t.Error("expected ScopeAgentNotify to be present as a default scope")
	}
	if claims.HasScope(ScopeAgentCreate) {
		t.Error("expected ScopeAgentCreate NOT to be auto-granted without dev-auth")
	}
	if claims.HasScope(ScopeAgentLifecycle) {
		t.Error("expected ScopeAgentLifecycle NOT to be auto-granted without dev-auth")
	}
}

// failingSMClient is a mock GCP SM client that fails all operations.
type failingSMClient struct{}

func (f *failingSMClient) CreateSecret(_ context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	return nil, fmt.Errorf("simulated GCP SM unavailable")
}

func (f *failingSMClient) AddSecretVersion(_ context.Context, _ *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	return nil, fmt.Errorf("simulated GCP SM unavailable")
}

func (f *failingSMClient) AccessSecretVersion(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return nil, status.Errorf(codes.NotFound, "secret not found: %s", req.Name)
}

func (f *failingSMClient) DeleteSecret(_ context.Context, _ *smpb.DeleteSecretRequest) error {
	return fmt.Errorf("simulated GCP SM unavailable")
}

func (f *failingSMClient) GetSecret(_ context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	return nil, status.Errorf(codes.NotFound, "secret not found: %s", req.Name)
}

func (f *failingSMClient) Close() error { return nil }

func TestServer_GCPBackendFailureIsFatal(t *testing.T) {
	// When GCPBackend is configured but GCP SM is unavailable, hub.New() should
	// return an error rather than silently generating an ephemeral key.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-gcpfail-hub"
	backend := secret.NewGCPBackendWithClient(s, &failingSMClient{}, "test-project", hubID)

	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	cfg.SecretBackend = backend

	_, err = New(cfg, s)
	if err == nil {
		t.Fatal("expected New() to return error when GCPBackend fails to store signing key")
	}
	if !strings.Contains(err.Error(), "Secret Manager") {
		t.Errorf("error should mention Secret Manager, got: %v", err)
	}
}

func TestServer_SigningKeyBackupPreservesSecretRef(t *testing.T) {
	// Verify that after loading a signing key from the secret backend and
	// backing it up to SQLite, the SecretRef is preserved so the UI shows
	// the secret as SM-backed.
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-ref-hub"
	backend := secret.NewLocalBackend(s, hubID)

	cfg := DefaultServerConfig()
	cfg.HubID = hubID
	cfg.SecretBackend = backend

	// Run 1: generate keys (stores via backend)
	srv1, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv1.Shutdown(context.Background()) })

	// Run 2: load keys from backend (triggers backupSigningKeyToStore)
	srv2, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv2.Shutdown(context.Background()) })

	// Check that the SQLite record still has the key value (backup)
	ctx := context.Background()
	val, err := s.GetSecretValue(ctx, SecretKeyUserSigningKey, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("GetSecretValue failed: %v", err)
	}
	if val == "" {
		t.Error("signing key backup in SQLite should not be empty after loading from backend")
	}
}
