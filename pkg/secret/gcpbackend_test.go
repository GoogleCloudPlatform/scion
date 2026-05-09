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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockSMClient implements SMClient for testing.
type mockSMClient struct {
	mu       sync.Mutex
	secrets  map[string]*smpb.Secret // keyed by full name
	versions map[string][]byte       // keyed by full name, latest value
	closed   bool
}

func newMockSMClient() *mockSMClient {
	return &mockSMClient{
		secrets:  make(map[string]*smpb.Secret),
		versions: make(map[string][]byte),
	}
}

func (m *mockSMClient) CreateSecret(_ context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fullName := fmt.Sprintf("%s/secrets/%s", req.Parent, req.SecretId)
	if _, exists := m.secrets[fullName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "secret %s already exists", fullName)
	}

	sec := &smpb.Secret{
		Name:        fullName,
		Replication: req.Secret.Replication,
		Labels:      req.Secret.Labels,
	}
	m.secrets[fullName] = sec
	return sec, nil
}

func (m *mockSMClient) AddSecretVersion(_ context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.secrets[req.Parent]; !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Parent)
	}

	m.versions[req.Parent] = req.Payload.Data
	return &smpb.SecretVersion{
		Name: req.Parent + "/versions/1",
	}, nil
}

func (m *mockSMClient) AccessSecretVersion(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Strip /versions/latest suffix to find the parent secret
	name := req.Name
	for _, suffix := range []string{"/versions/latest", "/versions/1"} {
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			name = name[:len(name)-len(suffix)]
			break
		}
	}

	data, exists := m.versions[name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "secret version not found for %s", req.Name)
	}

	return &smpb.AccessSecretVersionResponse{
		Name: req.Name,
		Payload: &smpb.SecretPayload{
			Data: data,
		},
	}, nil
}

func (m *mockSMClient) DeleteSecret(_ context.Context, req *smpb.DeleteSecretRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.secrets[req.Name]; !exists {
		return status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}

	delete(m.secrets, req.Name)
	delete(m.versions, req.Name)
	return nil
}

func (m *mockSMClient) GetSecret(_ context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sec, exists := m.secrets[req.Name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	return sec, nil
}

func (m *mockSMClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func createTestGCPBackend(t *testing.T) (*GCPBackend, *mockSMClient) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newMockSMClient()
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	return backend, mock
}

func TestGCPBackend_GetRecoverFromGCPSM_NoDBRecord(t *testing.T) {
	// Simulate a database reset: secret exists in GCP SM but not in SQLite.
	// GCPBackend.Get should fall back to GCP SM by computed name.
	ctx := context.Background()

	// Create first backend, store a secret via Set (populates both GCP SM and DB)
	s1, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s1.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newMockSMClient()
	backend1 := NewGCPBackendWithClient(s1, mock, "test-project", "hub-1")

	input := &SetSecretInput{
		Name:       "user_signing_key",
		Value:      "c2VjcmV0LWtleS12YWx1ZQ==",
		SecretType: store.SecretTypeInternal,
		Scope:      store.ScopeHub,
		ScopeID:    "hub-1",
	}
	_, _, err = backend1.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Create a second backend with a FRESH database (simulating DB reset)
	// but sharing the same mock GCP SM client (secrets still exist there)
	s2, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store 2: %v", err)
	}
	if err := s2.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate test store 2: %v", err)
	}
	backend2 := NewGCPBackendWithClient(s2, mock, "test-project", "hub-1")

	// Get should succeed by falling back to GCP SM by computed name
	sv, err := backend2.Get(ctx, "user_signing_key", store.ScopeHub, "hub-1")
	if err != nil {
		t.Fatalf("Get with no DB record should recover from GCP SM, got: %v", err)
	}
	if sv.Value != "c2VjcmV0LWtleS12YWx1ZQ==" {
		t.Errorf("expected value %q, got %q", "c2VjcmV0LWtleS12YWx1ZQ==", sv.Value)
	}
	if sv.Name != "user_signing_key" {
		t.Errorf("expected name %q, got %q", "user_signing_key", sv.Name)
	}
}

func TestGCPBackend_GetNotFoundAnywhere(t *testing.T) {
	// When a secret exists in neither DB nor GCP SM, Get should return ErrNotFound.
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	_, err := backend.Get(ctx, "nonexistent", store.ScopeHub, "hub-1")
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestGCPBackend_SetAndGet(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:        "API_KEY",
		Value:       "sk-test-123",
		SecretType:  TypeEnvironment,
		Target:      "API_KEY",
		Scope:       ScopeUser,
		ScopeID:     "user-1",
		Description: "Test API key",
	}

	created, meta, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if !created {
		t.Error("expected created=true for new secret")
	}
	if meta.Name != "API_KEY" {
		t.Errorf("expected name %q, got %q", "API_KEY", meta.Name)
	}

	// Verify value was stored in mock SM, not in DB
	mock.mu.Lock()
	if len(mock.versions) != 1 {
		t.Errorf("expected 1 version in mock SM, got %d", len(mock.versions))
	}
	mock.mu.Unlock()

	// Get it back
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

func TestGCPBackend_SetUpdate(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "old-value",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Update
	input.Value = "new-value"
	created, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set update failed: %v", err)
	}
	if created {
		t.Error("expected created=false for update")
	}

	sv, err := backend.Get(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sv.Value != "new-value" {
		t.Errorf("expected value %q, got %q", "new-value", sv.Value)
	}
}

func TestGCPBackend_Delete(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "TO_DELETE",
		Value:      "value",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if err := backend.Delete(ctx, "TO_DELETE", ScopeUser, "user-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify removed from both mock SM and DB
	mock.mu.Lock()
	if len(mock.secrets) != 0 {
		t.Errorf("expected 0 secrets in mock SM, got %d", len(mock.secrets))
	}
	mock.mu.Unlock()

	_, err = backend.Get(ctx, "TO_DELETE", ScopeUser, "user-1")
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestGCPBackend_List(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	for _, name := range []string{"A_KEY", "B_KEY", "C_KEY"} {
		_, _, err := backend.Set(ctx, &SetSecretInput{
			Name:       name,
			Value:      "val-" + name,
			SecretType: TypeEnvironment,
			Scope:      ScopeUser,
			ScopeID:    "user-1",
		})
		if err != nil {
			t.Fatalf("Set %s failed: %v", name, err)
		}
	}

	metas, err := backend.List(ctx, Filter{Scope: ScopeUser, ScopeID: "user-1"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 secrets, got %d", len(metas))
	}
}

func TestGCPBackend_GetMeta(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	_, _, err := backend.Set(ctx, &SetSecretInput{
		Name:        "META_KEY",
		Value:       "secret-value",
		SecretType:  TypeVariable,
		Target:      "config",
		Scope:       ScopeProject,
		ScopeID:     "grove-1",
		Description: "Test meta",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

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

func TestGCPBackend_Resolve(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	// User-level secret
	_, _, _ = backend.Set(ctx, &SetSecretInput{
		Name:       "API_KEY",
		Value:      "user-api-key",
		SecretType: TypeEnvironment,
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	})

	// Project-level override
	_, _, _ = backend.Set(ctx, &SetSecretInput{
		Name:       "API_KEY",
		Value:      "grove-api-key",
		SecretType: TypeEnvironment,
		Scope:      ScopeProject,
		ScopeID:    "grove-1",
	})

	// Project-only secret
	_, _, _ = backend.Set(ctx, &SetSecretInput{
		Name:       "DB_PASS",
		Value:      "db-password",
		SecretType: TypeEnvironment,
		Target:     "DATABASE_PASSWORD",
		Scope:      ScopeProject,
		ScopeID:    "grove-1",
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

	// DB_PASS from grove
	dbPass, ok := byName["DB_PASS"]
	if !ok {
		t.Fatal("expected DB_PASS in resolved secrets")
	}
	if dbPass.Value != "db-password" {
		t.Errorf("expected DB_PASS value %q, got %q", "db-password", dbPass.Value)
	}

	if len(resolved) != 2 {
		t.Errorf("expected 2 resolved secrets, got %d", len(resolved))
	}
}

func TestGCPBackend_SecretNameSanitization(t *testing.T) {
	backend, _ := createTestGCPBackend(t)

	// Helper to compute expected hash prefix (now includes hubID)
	hashCombined := func(scopeID string) string {
		combined := "test-hub-id:" + scopeID
		h := sha256.Sum256([]byte(combined))
		return hex.EncodeToString(h[:6])
	}

	// Test the hashed naming convention
	name := backend.gcpSecretName("MY_KEY", "user", "user-123")
	expectedHash := hashCombined("user-123")
	expectedPrefix := "scion-user-" + expectedHash + "-"
	if !strings.HasPrefix(name, expectedPrefix) {
		t.Errorf("expected prefix %q, got name %q", expectedPrefix, name)
	}
	if !strings.HasSuffix(name, "-MY_KEY") {
		t.Errorf("expected suffix %q, got name %q", "-MY_KEY", name)
	}
	// Hash portion should be exactly 12 hex chars
	parts := strings.SplitN(name, "-", 4) // scion, user, hash, name
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts in name, got %d: %q", len(parts), name)
	}
	if len(parts[2]) != 12 {
		t.Errorf("expected 12-char hash, got %d chars: %q", len(parts[2]), parts[2])
	}

	// Determinism: same inputs produce same output
	name2 := backend.gcpSecretName("MY_KEY", "user", "user-123")
	if name != name2 {
		t.Errorf("gcpSecretName is not deterministic: %q != %q", name, name2)
	}

	// Test sanitization of special characters in name (scopeID is hashed, not sanitized)
	name = backend.gcpSecretName("my.key/with spaces", "grove", "grove@id")
	expectedHash = hashCombined("grove@id")
	expectedFull := fmt.Sprintf("scion-grove-%s-my-key-with-spaces", expectedHash)
	if name != expectedFull {
		t.Errorf("expected sanitized name %q, got %q", expectedFull, name)
	}
}

func TestGCPBackend_SecretNameCollisionResistance(t *testing.T) {
	backend, _ := createTestGCPBackend(t)

	// Different UUIDs must produce different GCP SM names
	name1 := backend.gcpSecretName("API_KEY", "user", "550e8400-e29b-41d4-a716-446655440000")
	name2 := backend.gcpSecretName("API_KEY", "user", "550e8400-e29b-41d4-a716-446655440001")
	if name1 == name2 {
		t.Errorf("different scopeIDs produced same name: %q", name1)
	}

	// "default" scopeID must differ from UUID-based scopeIDs
	nameDefault := backend.gcpSecretName("GITHUB_TOKEN", "user", "default")
	nameUUID := backend.gcpSecretName("GITHUB_TOKEN", "user", "550e8400-e29b-41d4-a716-446655440000")
	if nameDefault == nameUUID {
		t.Errorf("default and UUID scopeIDs produced same name: %q", nameDefault)
	}

	// Two different "default-like" scopeIDs that would collide without hashing
	nameA := backend.gcpSecretName("KEY", "user", "abc-def")
	nameB := backend.gcpSecretName("KEY", "user", "abc@def")
	if nameA == nameB {
		t.Errorf("scopeIDs that differ only in special chars produced same name: %q", nameA)
	}
}

func TestGCPBackend_Labels(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "sk-test-123",
		SecretType: TypeEnvironment,
		Target:     "ANTHROPIC_API_KEY",
		Scope:      ScopeUser,
		ScopeID:    "user-1",
		UserEmail:  "alice@example.com",
	}

	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Find the created secret in the mock and verify labels
	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.secrets) != 1 {
		t.Fatalf("expected 1 secret in mock, got %d", len(mock.secrets))
	}

	for _, sec := range mock.secrets {
		labels := sec.Labels
		expectedLabels := map[string]string{
			"scion-scope":        "user",
			"scion-scope-id":     "user-1",
			"scion-type":         "environment",
			"scion-name":         "api_key",
			"scion-target":       "anthropic_api_key",
			"scion-userid":       "alice-example-com",
			"scion-hub-hostname": sanitizeLabel(hostname()),
		}
		for k, expected := range expectedLabels {
			got, ok := labels[k]
			if !ok {
				t.Errorf("missing label %q", k)
			} else if got != expected {
				t.Errorf("label %q: expected %q, got %q", k, expected, got)
			}
		}
		if len(labels) != len(expectedLabels) {
			t.Errorf("expected %d labels, got %d: %v", len(expectedLabels), len(labels), labels)
		}
	}
}

func TestGCPBackend_Labels_NoUserIDForNonUserScope(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	for _, scope := range []string{ScopeProject, ScopeRuntimeBroker} {
		t.Run(scope, func(t *testing.T) {
			input := &SetSecretInput{
				Name:       "KEY_" + scope,
				Value:      "value",
				SecretType: TypeEnvironment,
				Scope:      scope,
				ScopeID:    scope + "-1",
				UserEmail:  "should-be-ignored@example.com",
			}

			_, _, err := backend.Set(ctx, input)
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}

			mock.mu.Lock()
			smName := backend.gcpSecretName(input.Name, scope, scope+"-1")
			fullName := fmt.Sprintf("projects/test-project/secrets/%s", smName)
			sec, ok := mock.secrets[fullName]
			mock.mu.Unlock()

			if !ok {
				t.Fatalf("secret not found in mock: %s", fullName)
			}
			if _, exists := sec.Labels["scion-userid"]; exists {
				t.Errorf("scion-userid label should not be present for scope %q", scope)
			}
			if len(sec.Labels) != 6 {
				t.Errorf("expected 6 labels for scope %q, got %d: %v", scope, len(sec.Labels), sec.Labels)
			}
		})
	}
}

func TestGCPBackend_Labels_DefaultTarget(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	// When Target is empty, it should default to Name
	input := &SetSecretInput{
		Name:       "MY_SECRET",
		Value:      "value",
		SecretType: TypeEnvironment,
		Scope:      ScopeProject,
		ScopeID:    "grove-1",
	}

	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	for _, sec := range mock.secrets {
		if got := sec.Labels["scion-target"]; got != "my_secret" {
			t.Errorf("expected default target label %q, got %q", "my_secret", got)
		}
		if got := sec.Labels["scion-name"]; got != "my_secret" {
			t.Errorf("expected name label %q, got %q", "my_secret", got)
		}
	}
}
