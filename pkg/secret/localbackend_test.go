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

package secret

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

func createTestStore(t *testing.T) store.SecretStore {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return s
}

func createTestBackend(t *testing.T) (*LocalBackend, store.SecretStore) {
	t.Helper()
	s := createTestStore(t)
	return NewLocalBackend(s, "test-hub-id"), s
}

// seedSecret inserts a secret directly into the store for testing read operations.
func seedSecret(t *testing.T, s store.SecretStore, sec *store.Secret) {
	t.Helper()
	if err := s.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("failed to seed secret %s: %v", sec.Key, err)
	}
}

func TestLocalBackend_Set(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "sk-test-123",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	created, meta, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if !created {
		t.Error("expected created=true for new secret")
	}
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta.Name != "API_KEY" {
		t.Errorf("expected name %q, got %q", "API_KEY", meta.Name)
	}
	if meta.SecretType != TypeEnvironment {
		t.Errorf("expected type %q, got %q", TypeEnvironment, meta.SecretType)
	}

	// Verify the value was stored by reading it back
	sv, err := backend.Get(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get after Set failed: %v", err)
	}
	if sv.Value != "sk-test-123" {
		t.Errorf("expected value %q, got %q", "sk-test-123", sv.Value)
	}

	// Update the same secret
	input.Value = "sk-updated-456"
	created, meta, err = backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set (update) failed: %v", err)
	}
	if created {
		t.Error("expected created=false for update")
	}

	// Verify updated value
	sv, err = backend.Get(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get after update failed: %v", err)
	}
	if sv.Value != "sk-updated-456" {
		t.Errorf("expected updated value %q, got %q", "sk-updated-456", sv.Value)
	}
}

func TestLocalBackend_SetAndResolveRoundTrip(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	// Set a secret via Set()
	_, _, err := backend.Set(ctx, &SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      "gemini-key-value",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Resolve should find it
	resolved, err := backend.Resolve(ctx, "user-1", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved secret, got %d", len(resolved))
	}
	if resolved[0].Name != "GEMINI_API_KEY" {
		t.Errorf("expected name %q, got %q", "GEMINI_API_KEY", resolved[0].Name)
	}
	if resolved[0].Value != "gemini-key-value" {
		t.Errorf("expected value %q, got %q", "gemini-key-value", resolved[0].Value)
	}
}

func TestLocalBackend_SetUpdateIncrementsVersion(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "VERSION_KEY",
		Value:      "v1",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	_, meta1, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set (create) failed: %v", err)
	}

	input.Value = "v2"
	_, meta2, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set (update) failed: %v", err)
	}

	if meta2.Version <= meta1.Version {
		t.Errorf("expected version to increment: v1=%d, v2=%d", meta1.Version, meta2.Version)
	}
}

func TestLocalBackend_Get(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "API_KEY",
		EncryptedValue: "sk-test-123",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
		Description:    "Test API key",
	})

	sv, err := backend.Get(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sv.Value != "sk-test-123" {
		t.Errorf("expected value %q, got %q", "sk-test-123", sv.Value)
	}
	if sv.SecretType != TypeEnvironment {
		t.Errorf("expected type %q, got %q", TypeEnvironment, sv.SecretType)
	}
}

func TestLocalBackend_Delete(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "TO_DELETE",
		EncryptedValue: "value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "TO_DELETE",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})

	if err := backend.Delete(ctx, "TO_DELETE", ScopeUser, "user-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err := backend.Get(ctx, "TO_DELETE", ScopeUser, "user-1")
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestLocalBackend_DeleteNotFound(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	err := backend.Delete(ctx, "NONEXISTENT", ScopeUser, "user-1")
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestLocalBackend_List(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	for i, name := range []string{"A_KEY", "B_KEY", "C_KEY"} {
		seedSecret(t, s, &store.Secret{
			ID:             "s" + string(rune('1'+i)),
			Key:            name,
			EncryptedValue: "val-" + name,
			SecretType:     store.SecretTypeEnvironment,
			Target:         name,
			Scope:          store.ScopeUser,
			ScopeID:        "user-1",
		})
	}

	metas, err := backend.List(ctx, Filter{Scope: ScopeUser, ScopeID: "user-1"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 secrets, got %d", len(metas))
	}
}

func TestLocalBackend_ListFilterByType(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "ENV_KEY",
		EncryptedValue: "val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "ENV_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})
	seedSecret(t, s, &store.Secret{
		ID:             "s2",
		Key:            "FILE_KEY",
		EncryptedValue: "data",
		SecretType:     store.SecretTypeFile,
		Target:         "/tmp/file",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})

	metas, err := backend.List(ctx, Filter{Scope: ScopeUser, ScopeID: "user-1", Type: TypeFile})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("expected 1 file secret, got %d", len(metas))
	}
	if metas[0].Name != "FILE_KEY" {
		t.Errorf("expected FILE_KEY, got %s", metas[0].Name)
	}
}

func TestLocalBackend_GetMeta(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "META_KEY",
		EncryptedValue: "secret-value",
		SecretType:     store.SecretTypeVariable,
		Target:         "config",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
	})

	meta, err := backend.GetMeta(ctx, "META_KEY", ScopeProject, "grove-1")
	if err != nil {
		t.Fatalf("GetMeta failed: %v", err)
	}
	if meta.Name != "META_KEY" {
		t.Errorf("expected name %q, got %q", "META_KEY", meta.Name)
	}
	if meta.SecretType != TypeVariable {
		t.Errorf("expected type %q, got %q", TypeVariable, meta.SecretType)
	}
}

func TestLocalBackend_Resolve(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User-level secrets
	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "API_KEY",
		EncryptedValue: "user-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})
	seedSecret(t, s, &store.Secret{
		ID:             "s2",
		Key:            "TLS_CERT",
		EncryptedValue: "cert-data",
		SecretType:     store.SecretTypeFile,
		Target:         "/etc/ssl/cert.pem",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})

	// Project-level override
	seedSecret(t, s, &store.Secret{
		ID:             "s3",
		Key:            "API_KEY",
		EncryptedValue: "grove-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
	})
	seedSecret(t, s, &store.Secret{
		ID:             "s4",
		Key:            "DB_PASS",
		EncryptedValue: "grove-db-pass",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DATABASE_PASSWORD",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
	})

	resolved, err := backend.Resolve(ctx, "user-1", "grove-1", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	byName := make(map[string]SecretWithValue)
	for _, sv := range resolved {
		byName[sv.Name] = sv
	}

	// API_KEY overridden by grove
	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	if apiKey.Value != "grove-api-key" {
		t.Errorf("expected grove API_KEY value %q, got %q", "grove-api-key", apiKey.Value)
	}
	if apiKey.Scope != ScopeProject {
		t.Errorf("expected API_KEY scope %q, got %q", ScopeProject, apiKey.Scope)
	}

	// TLS_CERT from user (no override)
	cert, ok := byName["TLS_CERT"]
	if !ok {
		t.Fatal("expected TLS_CERT in resolved secrets")
	}
	if cert.SecretType != TypeFile {
		t.Errorf("expected TLS_CERT type %q, got %q", TypeFile, cert.SecretType)
	}
	if cert.Target != "/etc/ssl/cert.pem" {
		t.Errorf("expected TLS_CERT target %q, got %q", "/etc/ssl/cert.pem", cert.Target)
	}

	// DB_PASS from grove
	dbPass, ok := byName["DB_PASS"]
	if !ok {
		t.Fatal("expected DB_PASS in resolved secrets")
	}
	if dbPass.Target != "DATABASE_PASSWORD" {
		t.Errorf("expected DB_PASS target %q, got %q", "DATABASE_PASSWORD", dbPass.Target)
	}

	if len(resolved) != 3 {
		t.Errorf("expected 3 resolved secrets, got %d", len(resolved))
	}
}

func TestLocalBackend_ResolveNoScopes(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	resolved, err := backend.Resolve(ctx, "", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved secrets, got %d", len(resolved))
	}
}

func TestLocalBackend_ResolveBrokerOverride(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "s1",
		Key:            "API_KEY",
		EncryptedValue: "user-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})
	seedSecret(t, s, &store.Secret{
		ID:             "s2",
		Key:            "API_KEY",
		EncryptedValue: "broker-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeRuntimeBroker,
		ScopeID:        "broker-1",
	})

	resolved, err := backend.Resolve(ctx, "user-1", "", "broker-1", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved secret, got %d", len(resolved))
	}
	if resolved[0].Value != "broker-key" {
		t.Errorf("expected broker override %q, got %q", "broker-key", resolved[0].Value)
	}
	if resolved[0].Scope != ScopeRuntimeBroker {
		t.Errorf("expected scope %q, got %q", ScopeRuntimeBroker, resolved[0].Scope)
	}
}

func TestLocalBackend_ResolveExcludesInternalSecrets(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// Seed an internal signing key at hub scope (simulates hub signing keys)
	seedSecret(t, s, &store.Secret{
		ID:             "signing-1",
		Key:            "agent_signing_key",
		EncryptedValue: "super-secret-key-material",
		SecretType:     store.SecretTypeInternal,
		Target:         "agent_signing_key",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id",
	})

	// Seed a normal hub-scoped environment secret
	seedSecret(t, s, &store.Secret{
		ID:             "hub-env-1",
		Key:            "HUB_API_TOKEN",
		EncryptedValue: "hub-token-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "HUB_API_TOKEN",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id",
	})

	// Seed a user-scoped secret
	seedSecret(t, s, &store.Secret{
		ID:             "user-env-1",
		Key:            "USER_KEY",
		EncryptedValue: "user-key-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "USER_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})

	resolved, err := backend.Resolve(ctx, "user-1", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	byName := make(map[string]SecretWithValue)
	for _, sv := range resolved {
		byName[sv.Name] = sv
	}

	// Internal signing key must NOT be present
	if _, ok := byName["agent_signing_key"]; ok {
		t.Error("internal secret 'agent_signing_key' must not be included in resolved secrets")
	}

	// Normal hub secret should be present
	if _, ok := byName["HUB_API_TOKEN"]; !ok {
		t.Error("expected HUB_API_TOKEN in resolved secrets")
	}

	// User secret should be present
	if _, ok := byName["USER_KEY"]; !ok {
		t.Error("expected USER_KEY in resolved secrets")
	}

	if len(resolved) != 2 {
		t.Errorf("expected 2 resolved secrets, got %d", len(resolved))
	}
}

func TestLocalBackend_ResolveDuplicateTargetAcrossScopes(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User-level file secret targeting /tmp/my-secret.json
	seedSecret(t, s, &store.Secret{
		ID:             "u1",
		Key:            "my-svc-account",
		EncryptedValue: "user-cert-data",
		SecretType:     store.SecretTypeFile,
		Target:         "/tmp/my-secret.json",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
		InjectionMode:  store.InjectionModeAlways,
	})

	// Project-level file secret targeting the SAME path
	seedSecret(t, s, &store.Secret{
		ID:             "g1",
		Key:            "my-key",
		EncryptedValue: "grove-cert-data",
		SecretType:     store.SecretTypeFile,
		Target:         "/tmp/my-secret.json",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
		InjectionMode:  store.InjectionModeAlways,
	})

	resolved, err := backend.Resolve(ctx, "user-1", "grove-1", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	// Only one secret should survive for the target /tmp/my-secret.json
	var fileSecrets []SecretWithValue
	for _, sv := range resolved {
		if sv.SecretType == TypeFile && sv.Target == "/tmp/my-secret.json" {
			fileSecrets = append(fileSecrets, sv)
		}
	}
	if len(fileSecrets) != 1 {
		t.Fatalf("expected 1 file secret for /tmp/my-secret.json, got %d", len(fileSecrets))
	}

	// Project-level (higher scope) should win
	if fileSecrets[0].Name != "my-key" {
		t.Errorf("expected grove-level secret 'my-key' to win, got %q", fileSecrets[0].Name)
	}
	if fileSecrets[0].Value != "grove-cert-data" {
		t.Errorf("expected grove-level value, got %q", fileSecrets[0].Value)
	}
}

func TestLocalBackend_ResolveDuplicateEnvTargetAcrossScopes(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User-level env secret targeting FOO_VAR
	seedSecret(t, s, &store.Secret{
		ID:             "u1",
		Key:            "user-foo",
		EncryptedValue: "user-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "FOO_VAR",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	})

	// Project-level env secret targeting the SAME env var
	seedSecret(t, s, &store.Secret{
		ID:             "g1",
		Key:            "grove-foo",
		EncryptedValue: "grove-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "FOO_VAR",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
	})

	resolved, err := backend.Resolve(ctx, "user-1", "grove-1", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	var envSecrets []SecretWithValue
	for _, sv := range resolved {
		if sv.SecretType == TypeEnvironment && sv.Target == "FOO_VAR" {
			envSecrets = append(envSecrets, sv)
		}
	}
	if len(envSecrets) != 1 {
		t.Fatalf("expected 1 env secret for FOO_VAR, got %d", len(envSecrets))
	}
	if envSecrets[0].Name != "grove-foo" {
		t.Errorf("expected grove-level secret to win, got %q", envSecrets[0].Name)
	}
}

func TestDeduplicateByTarget_DifferentTypes(t *testing.T) {
	// File and env secrets with the same target string should NOT conflict
	// because they are different injection types.
	secrets := []SecretWithValue{
		{
			SecretMeta: SecretMeta{Name: "env-secret", SecretType: TypeEnvironment, Target: "MY_VAR", Scope: ScopeUser},
			Value:      "env-val",
		},
		{
			SecretMeta: SecretMeta{Name: "file-secret", SecretType: TypeFile, Target: "MY_VAR", Scope: ScopeUser},
			Value:      "file-val",
		},
	}

	result := DeduplicateByTarget(secrets)
	if len(result) != 2 {
		t.Fatalf("expected 2 secrets (different types), got %d", len(result))
	}
}

func TestDeduplicateByTarget_NoDuplicates(t *testing.T) {
	secrets := []SecretWithValue{
		{
			SecretMeta: SecretMeta{Name: "a", SecretType: TypeFile, Target: "/path/a", Scope: ScopeUser},
			Value:      "val-a",
		},
		{
			SecretMeta: SecretMeta{Name: "b", SecretType: TypeFile, Target: "/path/b", Scope: ScopeProject},
			Value:      "val-b",
		},
	}

	result := DeduplicateByTarget(secrets)
	if len(result) != 2 {
		t.Fatalf("expected 2 secrets (no duplicates), got %d", len(result))
	}
}

// ============================================================================
// Progeny Secret Access Tests
// ============================================================================

// TestLocalBackend_ResolveProgeny_AllowProgenyGrantsAccess verifies the full
// progeny flow: a user-scoped secret with allowProgeny=true is resolved for
// an agent whose ancestry includes the secret's creator.
func TestLocalBackend_ResolveProgeny_AllowProgenyGrantsAccess(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User "alice" creates a secret with allowProgeny
	seedSecret(t, s, &store.Secret{
		ID:             "sec-prog-1",
		Key:            "ANTHROPIC_API_KEY",
		EncryptedValue: "sk-ant-progeny",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "ANTHROPIC_API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	// Resolve as a sub-agent whose ancestry includes alice
	// ancestry: [alice-123, agent-a] means alice created agent-a, agent-a created this agent
	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    func(_ SecretMeta) bool { return true }, // policy allows
	}

	resolved, err := backend.Resolve(ctx, "", "grove-1", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved secret, got %d", len(resolved))
	}
	if resolved[0].Name != "ANTHROPIC_API_KEY" {
		t.Errorf("expected ANTHROPIC_API_KEY, got %s", resolved[0].Name)
	}
	if resolved[0].Value != "sk-ant-progeny" {
		t.Errorf("expected value %q, got %q", "sk-ant-progeny", resolved[0].Value)
	}
}

// TestLocalBackend_ResolveProgeny_DeepAncestry verifies that deeply nested
// progeny agents (grandchild, great-grandchild) receive the secret.
func TestLocalBackend_ResolveProgeny_DeepAncestry(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "sec-prog-deep",
		Key:            "DEEP_KEY",
		EncryptedValue: "deep-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DEEP_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-root",
		AllowProgeny:   true,
		CreatedBy:      "user-root",
	})

	// Agent C is a great-grandchild: user-root -> agent-a -> agent-b -> agent-c
	opts := &ResolveOpts{
		AgentAncestry: []string{"user-root", "agent-a", "agent-b", "agent-c"},
		AuthzCheck:    func(_ SecretMeta) bool { return true },
	}

	resolved, err := backend.Resolve(ctx, "", "", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved secret, got %d", len(resolved))
	}
	if resolved[0].Name != "DEEP_KEY" {
		t.Errorf("expected DEEP_KEY, got %s", resolved[0].Name)
	}
}

// TestLocalBackend_ResolveProgeny_GroveOverridesProgeny verifies that
// grove-scoped secrets with the same key take precedence over progeny secrets.
func TestLocalBackend_ResolveProgeny_GroveOverridesProgeny(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User-scoped progeny secret
	seedSecret(t, s, &store.Secret{
		ID:             "sec-prog-override-user",
		Key:            "API_KEY",
		EncryptedValue: "user-progeny-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	// Project-scoped secret with same key (higher precedence)
	seedSecret(t, s, &store.Secret{
		ID:             "sec-prog-override-grove",
		Key:            "API_KEY",
		EncryptedValue: "grove-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeProject,
		ScopeID:        "grove-1",
	})

	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    func(_ SecretMeta) bool { return true },
	}

	resolved, err := backend.Resolve(ctx, "", "grove-1", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	byName := make(map[string]SecretWithValue)
	for _, sv := range resolved {
		byName[sv.Name] = sv
	}

	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	// Project should win
	if apiKey.Value != "grove-value" {
		t.Errorf("expected grove override %q, got %q", "grove-value", apiKey.Value)
	}
	if apiKey.Scope != ScopeProject {
		t.Errorf("expected scope %q, got %q", ScopeProject, apiKey.Scope)
	}
}

// TestLocalBackend_ResolveProgeny_DeniedWhenFlagFalse verifies that secrets
// without allowProgeny=true are NOT included in progeny resolution.
func TestLocalBackend_ResolveProgeny_DeniedWhenFlagFalse(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// User-scoped secret WITHOUT allowProgeny
	seedSecret(t, s, &store.Secret{
		ID:             "sec-no-prog",
		Key:            "PRIVATE_KEY",
		EncryptedValue: "private-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "PRIVATE_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   false,
		CreatedBy:      "alice-123",
	})

	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    func(_ SecretMeta) bool { return true },
	}

	resolved, err := backend.Resolve(ctx, "", "", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	for _, sv := range resolved {
		if sv.Name == "PRIVATE_KEY" {
			t.Error("secret without allowProgeny should not be included in progeny resolution")
		}
	}
}

// TestLocalBackend_ResolveProgeny_DeniedWhenAncestryMismatch verifies that
// an agent whose ancestry does NOT include the secret's creator cannot access it.
func TestLocalBackend_ResolveProgeny_DeniedWhenAncestryMismatch(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// Alice's secret with allowProgeny
	seedSecret(t, s, &store.Secret{
		ID:             "sec-ancestry-miss",
		Key:            "ALICE_SECRET",
		EncryptedValue: "alice-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "ALICE_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	// Bob's agent — ancestry does NOT include alice
	opts := &ResolveOpts{
		AgentAncestry: []string{"bob-456", "agent-b"},
		AuthzCheck:    func(_ SecretMeta) bool { return true },
	}

	resolved, err := backend.Resolve(ctx, "", "", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	for _, sv := range resolved {
		if sv.Name == "ALICE_SECRET" {
			t.Error("agent with wrong ancestry should not receive alice's progeny secret")
		}
	}
}

// TestLocalBackend_ResolveProgeny_DeniedByPolicyCheck verifies that even when
// allowProgeny=true and ancestry matches, a deny from the policy engine
// prevents the secret from being included.
func TestLocalBackend_ResolveProgeny_DeniedByPolicyCheck(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "sec-policy-deny",
		Key:            "POLICY_KEY",
		EncryptedValue: "policy-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "POLICY_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    func(_ SecretMeta) bool { return false }, // policy DENIES
	}

	resolved, err := backend.Resolve(ctx, "", "", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	for _, sv := range resolved {
		if sv.Name == "POLICY_KEY" {
			t.Error("secret should be excluded when policy check returns false")
		}
	}
}

// TestLocalBackend_ResolveProgeny_NilAuthzCheckIncludesAll verifies that
// when no AuthzCheck is provided, progeny secrets with matching ancestry
// are included (the policy check is optional).
func TestLocalBackend_ResolveProgeny_NilAuthzCheckIncludesAll(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "sec-no-authz",
		Key:            "NO_AUTHZ_KEY",
		EncryptedValue: "no-authz-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "NO_AUTHZ_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    nil, // no policy checker — secrets are included by default
	}

	resolved, err := backend.Resolve(ctx, "", "", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	found := false
	for _, sv := range resolved {
		if sv.Name == "NO_AUTHZ_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("progeny secret should be included when AuthzCheck is nil (no policy gating)")
	}
}

// TestLocalBackend_ResolveProgeny_NilOptsNoProgeny verifies that passing
// nil opts preserves the original behavior (no progeny resolution).
func TestLocalBackend_ResolveProgeny_NilOptsNoProgeny(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	seedSecret(t, s, &store.Secret{
		ID:             "sec-nil-opts",
		Key:            "NIL_OPTS_KEY",
		EncryptedValue: "nil-opts-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "NIL_OPTS_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	// nil opts — no progeny resolution
	resolved, err := backend.Resolve(ctx, "", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	for _, sv := range resolved {
		if sv.Name == "NIL_OPTS_KEY" {
			t.Error("progeny secrets should not be included when opts is nil")
		}
	}
}

// TestLocalBackend_SetProgeny_RejectsNonUserScope verifies that setting
// allowProgeny=true on non-user-scoped secrets is handled correctly at
// the backend level (the API layer validates this, but the backend should
// store whatever is passed).
func TestLocalBackend_SetProgeny_AllowProgenyPersists(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()

	// Create with allowProgeny=true
	_, meta, err := backend.Set(ctx, &SetSecretInput{
		Name:         "PROG_TEST",
		Value:        "value",
		SecretType:   TypeEnvironment,
		Scope:        ScopeUser,
		ScopeID:      "user-1",
		AllowProgeny: true,
		CreatedBy:    "user-1",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if !meta.AllowProgeny {
		t.Error("expected AllowProgeny=true after Set")
	}

	// Verify via GetMeta
	got, err := backend.GetMeta(ctx, "PROG_TEST", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("GetMeta failed: %v", err)
	}
	if !got.AllowProgeny {
		t.Error("expected AllowProgeny=true from GetMeta")
	}

	// Update to allowProgeny=false
	_, meta2, err := backend.Set(ctx, &SetSecretInput{
		Name:         "PROG_TEST",
		Value:        "value-2",
		SecretType:   TypeEnvironment,
		Scope:        ScopeUser,
		ScopeID:      "user-1",
		AllowProgeny: false,
		CreatedBy:    "user-1",
	})
	if err != nil {
		t.Fatalf("Set (update) failed: %v", err)
	}
	if meta2.AllowProgeny {
		t.Error("expected AllowProgeny=false after update")
	}
}
