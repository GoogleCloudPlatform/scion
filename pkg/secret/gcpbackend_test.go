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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingHandler is a minimal slog.Handler that captures log records for
// assertions, used to verify the legacy-name-fallback WARN log fires.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) hasWarnContaining(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

// seedMockSecret creates a secret and an initial version directly in the mock
// SM client, bypassing GCPBackend, to simulate a secret that already exists
// under a specific GCP SM name (e.g. a legacy pre-prefix name).
func seedMockSecret(t *testing.T, mock *mockSMClient, projectID, smName, value string) {
	t.Helper()
	seedMockSecretWithLabels(t, mock, projectID, smName, value, nil)
}

func seedMockSecretWithLabels(t *testing.T, mock *mockSMClient, projectID, smName, value string, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	fullName := fmt.Sprintf("projects/%s/secrets/%s", projectID, smName)
	if _, err := mock.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", projectID),
		SecretId: smName,
		Secret:   &smpb.Secret{Labels: labels},
	}); err != nil {
		t.Fatalf("failed to seed mock secret %s: %v", smName, err)
	}
	if _, err := mock.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent:  fullName,
		Payload: &smpb.SecretPayload{Data: []byte(value)},
	}); err != nil {
		t.Fatalf("failed to seed mock secret version %s: %v", smName, err)
	}
}

func testResolveHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

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
	s, err := newTestStore(":memory:")
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
	s1, err := newTestStore(":memory:")
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
	s2, err := newTestStore(":memory:")
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

func TestGCPBackend_Get_DBRecordExists_GCPSMNotFound(t *testing.T) {
	// When a DB record exists but the corresponding GCP Secret Manager secret
	// is missing (gRPC NotFound), Get must return store.ErrNotFound — not a
	// wrapped generic error. This is critical for MigratePluginSecrets which
	// relies on ErrNotFound to know a secret needs migration.
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newMockSMClient() // empty — no secrets in GCP SM
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	ctx := context.Background()

	// Seed a DB record that points to a GCP SM secret which does not exist.
	if _, err := s.UpsertSecret(ctx, &store.Secret{
		ID:        tid("orphan-db-record"),
		Key:       "bot_token",
		Scope:     store.ScopeProject,
		ScopeID:   "proj-1",
		SecretRef: "gcpsm:projects/test-project/secrets/nonexistent",
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	_, err = backend.Get(ctx, "bot_token", store.ScopeProject, "proj-1")
	if err != store.ErrNotFound {
		t.Errorf("expected store.ErrNotFound when GCP SM secret is missing, got: %v", err)
	}
}

func TestGCPBackend_Get_DBRecordExists_GCPSMNotFound_ComputedName(t *testing.T) {
	// Same scenario but without a stored SecretRef — the backend computes the
	// GCP SM name. The gRPC NotFound must still be converted to store.ErrNotFound.
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newMockSMClient() // empty — no secrets in GCP SM
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	ctx := context.Background()

	// Seed a DB record WITHOUT a gcpsm: SecretRef — forces the computed-name path.
	if _, err := s.UpsertSecret(ctx, &store.Secret{
		ID:      tid("orphan-no-ref"),
		Key:     "bot_token",
		Scope:   store.ScopeProject,
		ScopeID: "proj-1",
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	_, err = backend.Get(ctx, "bot_token", store.ScopeProject, "proj-1")
	if err != store.ErrNotFound {
		t.Errorf("expected store.ErrNotFound when GCP SM secret is missing (computed name), got: %v", err)
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
		ScopeID:     "project-1",
		Description: "Test meta",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	meta, err := backend.GetMeta(ctx, "META_KEY", ScopeProject, "project-1")
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

	// Project-level value for the same key (user scope wins)
	_, _, _ = backend.Set(ctx, &SetSecretInput{
		Name:       "API_KEY",
		Value:      "project-api-key",
		SecretType: TypeEnvironment,
		Scope:      ScopeProject,
		ScopeID:    "project-1",
	})

	// Project-only secret
	_, _, _ = backend.Set(ctx, &SetSecretInput{
		Name:       "DB_PASS",
		Value:      "db-password",
		SecretType: TypeEnvironment,
		Target:     "DATABASE_PASSWORD",
		Scope:      ScopeProject,
		ScopeID:    "project-1",
	})

	resolved, err := backend.Resolve(ctx, "user-1", "project-1", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	byName := make(map[string]SecretWithValue)
	for _, sv := range resolved {
		byName[sv.Name] = sv
	}

	// API_KEY: user scope wins over project (runtime_broker < hub < project
	// < user, lowest first — user is the strongest scope).
	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	if apiKey.Value != "user-api-key" {
		t.Errorf("expected user API_KEY value %q, got %q", "user-api-key", apiKey.Value)
	}

	// DB_PASS from project
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

// testHubPrefix computes the expected hub prefix for the given hubID,
// mirroring GCPBackend.secretNamePrefix's formula (ptone/scion#2152):
// "scion-" + first 12 hex chars of sha256(raw hubID) + "-".
func testHubPrefix(hubID string) string {
	h := sha256.Sum256([]byte(hubID))
	return "scion-" + hex.EncodeToString(h[:6]) + "-"
}

func TestGCPBackend_SecretNameSanitization(t *testing.T) {
	backend, _ := createTestGCPBackend(t)

	// Helper to compute expected hash prefix (now includes hubID)
	hashCombined := func(scopeID string) string {
		combined := "test-hub-id:" + scopeID
		h := sha256.Sum256([]byte(combined))
		return hex.EncodeToString(h[:6])
	}
	hubPrefix := testHubPrefix("test-hub-id")

	// Test the hashed naming convention
	name := backend.gcpSecretName("MY_KEY", "user", "user-123")
	expectedHash := hashCombined("user-123")
	expectedPrefix := hubPrefix + "user-" + expectedHash + "-"
	if !strings.HasPrefix(name, expectedPrefix) {
		t.Errorf("expected prefix %q, got name %q", expectedPrefix, name)
	}
	if !strings.HasSuffix(name, "-MY_KEY") {
		t.Errorf("expected suffix %q, got name %q", "-MY_KEY", name)
	}
	// Hash portion should be exactly 12 hex chars
	parts := strings.SplitN(name, "-", 5) // scion, hubhash, user, hash, name
	if len(parts) != 5 {
		t.Fatalf("expected 5 parts in name, got %d: %q", len(parts), name)
	}
	if len(parts[1]) != 12 {
		t.Errorf("expected 12-char hub prefix hash, got %d chars: %q", len(parts[1]), parts[1])
	}
	if len(parts[3]) != 12 {
		t.Errorf("expected 12-char scope hash, got %d chars: %q", len(parts[3]), parts[3])
	}

	// Determinism: same inputs produce same output
	name2 := backend.gcpSecretName("MY_KEY", "user", "user-123")
	if name != name2 {
		t.Errorf("gcpSecretName is not deterministic: %q != %q", name, name2)
	}

	// Test sanitization of special characters in name (scopeID is hashed, not sanitized)
	name = backend.gcpSecretName("my.key/with spaces", "project", "project@id")
	expectedHash = hashCombined("project@id")
	expectedFull := fmt.Sprintf("%sproject-%s-my-key-with-spaces", hubPrefix, expectedHash)
	if name != expectedFull {
		t.Errorf("expected sanitized name %q, got %q", expectedFull, name)
	}
}

// TestGCPBackend_SecretNamePrefix_GoldenVectors pins the prefix formula
// against fixed hubID -> prefix pairs, documented in
// .design/secret-id-hub-refactor.md §7 (ptone/scion#2152), so the
// Terraform-computed IAM condition
// (`"scion-${substr(sha256(var.hub_id), 0, 12)}-"`) and the Go implementation
// can never silently drift apart. These vectors match the live tfha h1/h2 IAM
// grants — do not change them without updating the Terraform side too.
func TestGCPBackend_SecretNamePrefix_GoldenVectors(t *testing.T) {
	vectors := map[string]string{
		"tfha-h1": "scion-a9be7bcccaae-",
		"tfha-h2": "scion-612a0e4e9c04-",
	}
	for hubID, want := range vectors {
		backend := NewGCPBackendWithClient(nil, nil, "test-project", hubID)
		got := backend.secretNamePrefix()
		if got != want {
			t.Errorf("secretNamePrefix() for hubID %q = %q, want %q", hubID, got, want)
		}
	}
}

func TestGCPBackend_SecretNamePrefix_AllScopes(t *testing.T) {
	// The hub prefix must be present and identical across every scope, since it
	// is derived from hubID alone (ptone/scion#2152 scope item 1).
	backend, _ := createTestGCPBackend(t)
	hubPrefix := testHubPrefix("test-hub-id")

	for _, sc := range []string{store.ScopeHub, ScopeUser, ScopeProject, ScopeRuntimeBroker} {
		name := backend.gcpSecretName("KEY", sc, sc+"-scope-id")
		if !strings.HasPrefix(name, hubPrefix) {
			t.Errorf("scope %q: expected name to start with hub prefix %q, got %q", sc, hubPrefix, name)
		}
	}
}

func TestGCPBackend_SecretNamePrefix_TrailingDash(t *testing.T) {
	// The prefix must always end in "-" so an IAM startsWith() condition on it
	// can never accidentally match a different hub's prefix (collision safety).
	for _, hubID := range []string{"", "a", "hub-1", "a-very-long-hub-identifier-used-in-a-shared-project"} {
		backend := NewGCPBackendWithClient(nil, nil, "test-project", hubID)
		prefix := backend.secretNamePrefix()
		if !strings.HasSuffix(prefix, "-") {
			t.Errorf("hubID %q: expected prefix to end with '-', got %q", hubID, prefix)
		}
		if !strings.HasPrefix(prefix, "scion-") {
			t.Errorf("hubID %q: expected prefix to start with 'scion-', got %q", hubID, prefix)
		}
	}

	// Distinct hub IDs must produce distinct prefixes, and since the hash
	// segment is always exactly 12 hex chars, neither prefix can ever be a
	// prefix of the other.
	p1 := NewGCPBackendWithClient(nil, nil, "test-project", "hub-1").secretNamePrefix()
	p2 := NewGCPBackendWithClient(nil, nil, "test-project", "hub-12").secretNamePrefix()
	if p1 == p2 {
		t.Errorf("distinct hub IDs produced the same prefix: %q", p1)
	}
	if len(p1) != len(p2) {
		t.Errorf("expected fixed-length prefixes, got %q (%d) and %q (%d)", p1, len(p1), p2, len(p2))
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
			"scion-scope":    "user",
			"scion-scope-id": "user-1",
			"scion-type":     "environment",
			"scion-name":     "api_key",
			"scion-target":   "anthropic_api_key",
			"scion-userid":   "alice-example-com",
			"scion-hub-name": sanitizeLabel(testResolveHostname()),
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

func TestGCPBackend_Labels_ExplicitHubName(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	backend.SetHubName("prod-hub")
	ctx := context.Background()

	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "sk-test-456",
		SecretType: TypeEnvironment,
		Target:     "ANTHROPIC_API_KEY",
		Scope:      ScopeUser,
		ScopeID:    "user-2",
		UserEmail:  "bob@example.com",
	}

	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.secrets) != 1 {
		t.Fatalf("expected 1 secret in mock, got %d", len(mock.secrets))
	}

	for _, sec := range mock.secrets {
		got, ok := sec.Labels["scion-hub-name"]
		if !ok {
			t.Error("missing scion-hub-name label")
		} else if got != "prod-hub" {
			t.Errorf("scion-hub-name: expected %q, got %q", "prod-hub", got)
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

// permDeniedSMClient returns PermissionDenied for CreateSecret, AddSecretVersion,
// AccessSecretVersion, and DeleteSecret. GetSecret returns NotFound to trigger
// the creation path.
type permDeniedSMClient struct {
	mockSMClient
}

func newPermDeniedSMClient() *permDeniedSMClient {
	return &permDeniedSMClient{
		mockSMClient: mockSMClient{
			secrets:  make(map[string]*smpb.Secret),
			versions: make(map[string][]byte),
		},
	}
}

func (m *permDeniedSMClient) CreateSecret(_ context.Context, _ *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	return nil, status.Errorf(codes.PermissionDenied, "caller does not have permission secretmanager.secrets.create")
}

func (m *permDeniedSMClient) AddSecretVersion(_ context.Context, _ *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	return nil, status.Errorf(codes.PermissionDenied, "caller does not have permission secretmanager.versions.add")
}

func (m *permDeniedSMClient) AccessSecretVersion(_ context.Context, _ *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return nil, status.Errorf(codes.PermissionDenied, "caller does not have permission secretmanager.versions.access")
}

func (m *permDeniedSMClient) DeleteSecret(_ context.Context, _ *smpb.DeleteSecretRequest) error {
	return status.Errorf(codes.PermissionDenied, "caller does not have permission secretmanager.secrets.delete")
}

func TestGCPBackend_Set_PermissionDenied(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newPermDeniedSMClient()
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")

	ctx := context.Background()
	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "sk-test-123",
		SecretType: TypeEnvironment,
		Target:     "API_KEY",
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	_, _, err = backend.Set(ctx, input)
	if err == nil {
		t.Fatal("expected error from Set when SM returns PermissionDenied")
	}

	var permErr *PermissionError
	if !errors.As(err, &permErr) {
		t.Fatalf("expected *PermissionError, got %T: %v", err, err)
	}
	if !strings.Contains(permErr.Error(), "service account lacks the required Secret Manager permission") {
		t.Errorf("error message should mention service account permission, got: %v", permErr.Error())
	}
	if !strings.Contains(permErr.Error(), "roles/secretmanager.admin") {
		t.Errorf("error message should mention the required role, got: %v", permErr.Error())
	}
	// Must NOT contain a granular permission name — the message should be
	// operation-agnostic so it is correct for create, get, delete, etc.
	if strings.Contains(permErr.Error(), "secretmanager.secrets.") {
		t.Errorf("error message should not contain a granular permission name, got: %v", permErr.Error())
	}
	// ptone/scion#2152: the hint should also point at a least-privilege,
	// hub-scoped conditioned grant instead of only the project-wide role.
	if permErr.HubPrefix != backend.secretNamePrefix() {
		t.Errorf("expected PermissionError.HubPrefix %q, got %q", backend.secretNamePrefix(), permErr.HubPrefix)
	}
	if !strings.Contains(permErr.Error(), "resource.name.startsWith") {
		t.Errorf("error message should suggest a conditioned IAM grant, got: %v", permErr.Error())
	}
	if !strings.Contains(permErr.Error(), backend.secretNamePrefix()) {
		t.Errorf("error message should include this hub's secret prefix %q, got: %v", backend.secretNamePrefix(), permErr.Error())
	}
}

func TestGCPBackend_Set_GetSecretPermissionDenied(t *testing.T) {
	// When GetSecret (the existence check) returns PermissionDenied,
	// Set should surface a *PermissionError rather than a generic error.
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	// Use a client that returns PermissionDenied on GetSecret (not just CreateSecret).
	mock := &getPermDeniedSMClient{
		mockSMClient: mockSMClient{
			secrets:  make(map[string]*smpb.Secret),
			versions: make(map[string][]byte),
		},
	}
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")

	ctx := context.Background()
	input := &SetSecretInput{
		Name:       "API_KEY",
		Value:      "sk-test-123",
		SecretType: TypeEnvironment,
		Target:     "API_KEY",
		Scope:      ScopeUser,
		ScopeID:    "user-1",
	}

	_, _, err = backend.Set(ctx, input)
	if err == nil {
		t.Fatal("expected error from Set when GetSecret returns PermissionDenied")
	}

	var permErr *PermissionError
	if !errors.As(err, &permErr) {
		t.Fatalf("expected *PermissionError, got %T: %v", err, err)
	}
	if permErr.Operation != "check secret" {
		t.Errorf("expected operation %q, got %q", "check secret", permErr.Operation)
	}
}

// getPermDeniedSMClient returns PermissionDenied specifically for GetSecret,
// exercising the "check secret" branch in Set that is not covered by
// permDeniedSMClient (which leaves GetSecret as the base NotFound mock).
type getPermDeniedSMClient struct {
	mockSMClient
}

func (m *getPermDeniedSMClient) GetSecret(_ context.Context, _ *smpb.GetSecretRequest) (*smpb.Secret, error) {
	return nil, status.Errorf(codes.PermissionDenied, "caller does not have permission secretmanager.secrets.get")
}

func TestGCPBackend_Get_PermissionDenied(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newPermDeniedSMClient()
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	ctx := context.Background()

	// Store a metadata record so Get tries to access the GCP SM value
	if _, err := s.UpsertSecret(ctx, &store.Secret{
		ID:        tid("test-perm-denied"),
		Key:       "API_KEY",
		Scope:     "user",
		ScopeID:   "user-1",
		SecretRef: "gcpsm:projects/test-project/secrets/test-secret",
	}); err != nil {
		t.Fatalf("failed to seed DB: %v", err)
	}

	_, err = backend.Get(ctx, "API_KEY", "user", "user-1")
	if err == nil {
		t.Fatal("expected error from Get when SM returns PermissionDenied")
	}

	var permErr *PermissionError
	if !errors.As(err, &permErr) {
		t.Fatalf("expected *PermissionError, got %T: %v", err, err)
	}
}

func TestGCPBackend_Delete_PermissionDenied(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newPermDeniedSMClient()
	backend := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	ctx := context.Background()

	err = backend.Delete(ctx, "API_KEY", "user", "user-1")
	if err == nil {
		t.Fatal("expected error from Delete when SM returns PermissionDenied")
	}

	var permErr *PermissionError
	if !errors.As(err, &permErr) {
		t.Fatalf("expected *PermissionError, got %T: %v", err, err)
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
		ScopeID:    "project-1",
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

// --- ptone/scion#2152: hub-prefixed names, legacy fallback, and migration ---

func TestGCPBackend_Get_LegacyFallbackWithWarn(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	// Seed a DB record with no SecretRef (predates SecretRef persistence, or a
	// ref that was cleared) so Get() must fall back to a computed name.
	rec := &store.Secret{
		ID:      tid("legacy-secret"),
		Key:     "LEGACY_KEY",
		Scope:   ScopeUser,
		ScopeID: "user-1",
	}
	if err := backend.store.CreateSecret(ctx, rec); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	// Only the legacy (pre-prefix) name exists in GCP SM.
	legacyName := backend.legacyGCPSecretName("LEGACY_KEY", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "legacy-value")

	orig := slog.Default()
	h := &recordingHandler{}
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(orig)

	sv, err := backend.Get(ctx, "LEGACY_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sv.Value != "legacy-value" {
		t.Errorf("expected value %q, got %q", "legacy-value", sv.Value)
	}
	if !h.hasWarnContaining("legacy") {
		t.Error("expected a WARN log mentioning the legacy fallback")
	}
}

func TestGCPBackend_Resolve_LegacyFallback(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	rec := &store.Secret{
		ID:      tid("resolve-legacy"),
		Key:     "OLD_API_KEY",
		Scope:   ScopeUser,
		ScopeID: "user-1",
	}
	if err := backend.store.CreateSecret(ctx, rec); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}
	legacyName := backend.legacyGCPSecretName("OLD_API_KEY", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "old-value")

	resolved, err := backend.Resolve(ctx, "user-1", "", "", nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Value != "old-value" {
		t.Fatalf("expected resolved legacy secret with value %q, got %+v", "old-value", resolved)
	}
}

func TestGCPBackend_Delete_RemovesBothPrefixedAndLegacyNames(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	if _, _, err := backend.Set(ctx, &SetSecretInput{
		Name: "DUAL", Value: "v1", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "user-1",
	}); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Simulate a leftover legacy-named secret from before the prefix existed.
	legacyName := backend.legacyGCPSecretName("DUAL", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "legacy-v1")

	if err := backend.Delete(ctx, "DUAL", ScopeUser, "user-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.secrets) != 0 {
		t.Errorf("expected both prefixed and legacy secrets removed, got %d remaining: %v", len(mock.secrets), mock.secrets)
	}
}

func TestGCPBackend_Delete_NotFoundOnEitherIsFine(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()

	// A DB record exists, but neither the prefixed nor legacy GCP SM secret
	// does (e.g. a record whose GCP SM write never completed). Delete must
	// still succeed, ignoring NotFound from both GCP SM lookups.
	rec := &store.Secret{ID: tid("never-created"), Key: "NEVER_CREATED", Scope: ScopeUser, ScopeID: "user-1"}
	if err := backend.store.CreateSecret(ctx, rec); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if err := backend.Delete(ctx, "NEVER_CREATED", ScopeUser, "user-1"); err != nil {
		t.Errorf("expected no error when neither name exists in GCP SM, got: %v", err)
	}
}

func TestGCPBackend_MigrateNameForward(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("MIGRATE_ME", ScopeUser, "user-1")
	seedMockSecretWithLabels(t, mock, backend.projectID, legacyName, "legacy-value", map[string]string{
		"scion-name":  "migrate_me",
		"scion-scope": "user",
	})

	migrated, err := backend.MigrateNameForward(ctx, "MIGRATE_ME", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("MigrateNameForward failed: %v", err)
	}
	if !migrated {
		t.Error("expected migrated=true on first run")
	}

	prefixedName := backend.gcpSecretName("MIGRATE_ME", ScopeUser, "user-1")
	fullName := fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	mock.mu.Lock()
	val, ok := mock.versions[fullName]
	labels := mock.secrets[fullName].Labels
	mock.mu.Unlock()
	if !ok {
		t.Fatal("expected prefixed secret to exist after migration")
	}
	if string(val) != "legacy-value" {
		t.Errorf("expected copied value %q, got %q", "legacy-value", val)
	}
	if labels["scion-name"] != "migrate_me" {
		t.Errorf("expected labels preserved from legacy secret, got %v", labels)
	}

	// Legacy secret must remain untouched by a forward migration.
	legacyFullName := fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	mock.mu.Lock()
	_, legacyStillThere := mock.secrets[legacyFullName]
	mock.mu.Unlock()
	if !legacyStillThere {
		t.Error("expected legacy secret to remain until an explicit --delete-legacy")
	}

	// Idempotent re-run: second call is a no-op.
	migrated2, err := backend.MigrateNameForward(ctx, "MIGRATE_ME", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("second MigrateNameForward failed: %v", err)
	}
	if migrated2 {
		t.Error("expected migrated=false on idempotent re-run")
	}
}

func TestGCPBackend_MigrateNameForward_NothingToMigrate(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	if _, err := backend.MigrateNameForward(ctx, "NOWHERE", ScopeUser, "user-1"); err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestGCPBackend_NeedsNameMigration_DryRunDoesNotWrite(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("PLAN_ME", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "legacy-value")

	needs, err := backend.NeedsNameMigration(ctx, "PLAN_ME", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("NeedsNameMigration failed: %v", err)
	}
	if !needs {
		t.Error("expected needs=true")
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.secrets) != 1 {
		t.Errorf("dry-run check must not create the prefixed secret, got %d secrets: %v", len(mock.secrets), mock.secrets)
	}
}

func TestGCPBackend_NeedsNameMigration_NeitherExists(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	if _, err := backend.NeedsNameMigration(ctx, "NOWHERE", ScopeUser, "user-1"); err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestGCPBackend_DeleteLegacySecretName_RefusesWithoutPrefixedCopy(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("ORPHAN", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "only-copy")

	if err := backend.DeleteLegacySecretName(ctx, "ORPHAN", ScopeUser, "user-1"); err == nil {
		t.Error("expected error refusing to delete legacy secret with no prefixed copy")
	}

	mock.mu.Lock()
	_, stillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)]
	mock.mu.Unlock()
	if !stillThere {
		t.Error("legacy secret should not have been deleted")
	}
}

func TestGCPBackend_DeleteLegacySecretName_SucceedsAfterMigration(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("READY", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "value")

	if _, err := backend.MigrateNameForward(ctx, "READY", ScopeUser, "user-1"); err != nil {
		t.Fatalf("MigrateNameForward failed: %v", err)
	}

	if err := backend.DeleteLegacySecretName(ctx, "READY", ScopeUser, "user-1"); err != nil {
		t.Fatalf("DeleteLegacySecretName failed: %v", err)
	}

	mock.mu.Lock()
	_, stillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)]
	mock.mu.Unlock()
	if stillThere {
		t.Error("expected legacy secret to be deleted")
	}
}

func TestGCPBackend_DeleteLegacySecretName_NotFoundIsSuccess(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	if err := backend.DeleteLegacySecretName(ctx, "GONE", ScopeUser, "user-1"); err != nil {
		t.Errorf("expected nil error when legacy secret already gone, got: %v", err)
	}
}

func TestGCPBackend_RepairRefToPrefixed(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("REF_ME", ScopeUser, "user-1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "value")
	rec := &store.Secret{
		ID:        tid("ref-me"),
		Key:       "REF_ME",
		Scope:     ScopeUser,
		ScopeID:   "user-1",
		SecretRef: "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName),
	}
	if err := backend.store.CreateSecret(ctx, rec); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if _, err := backend.RepairRefToPrefixed(ctx, "REF_ME", ScopeUser, "user-1"); err != nil {
		t.Fatalf("RepairRefToPrefixed failed: %v", err)
	}

	updated, err := backend.store.GetSecret(ctx, "REF_ME", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedName := backend.gcpSecretName("REF_ME", ScopeUser, "user-1")
	expectedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	if updated.SecretRef != expectedRef {
		t.Errorf("expected SecretRef %q, got %q", expectedRef, updated.SecretRef)
	}
}

func TestGCPBackend_RepairRefToPrefixed_NoDBRecordIsNoOp(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	if _, err := backend.RepairRefToPrefixed(ctx, "NOT_IN_DB", ScopeUser, "user-1"); err != nil {
		t.Errorf("expected nil for a secret identity with no DB row, got: %v", err)
	}
}

func TestGCPBackend_CopyHubSecretForward(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("user_signing_key", store.ScopeHub, backend.hubID)
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	seedMockSecret(t, mock, backend.projectID, legacyName, "key-material")

	// Seed a DB record with a legacy SecretRef, as ensureSigningKey's
	// pre-existing backup path would have created before ptone/scion#2152.
	if err := backend.store.CreateSecret(ctx, &store.Secret{
		ID:        tid("copy-forward-signing-key"),
		Key:       "user_signing_key",
		Scope:     store.ScopeHub,
		ScopeID:   backend.hubID,
		SecretRef: legacyRef,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if err := backend.CopyHubSecretForward(ctx, "user_signing_key"); err != nil {
		t.Fatalf("CopyHubSecretForward failed: %v", err)
	}

	prefixedName := backend.gcpSecretName("user_signing_key", store.ScopeHub, backend.hubID)
	value, err := backend.accessLatestVersion(ctx, prefixedName)
	if err != nil {
		t.Fatalf("expected prefixed signing key to be readable: %v", err)
	}
	if value != "key-material" {
		t.Errorf("expected copied key material %q, got %q", "key-material", value)
	}

	// ptone/scion#2152 review finding 3: the DB ref must move to the
	// prefixed name too, or a subsequent Get() (which trusts the stored ref
	// verbatim, with no fallback) keeps reading the legacy copy forever.
	rec, err := backend.store.GetSecret(ctx, "user_signing_key", store.ScopeHub, backend.hubID)
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	if rec.SecretRef != prefixedRef {
		t.Errorf("expected SecretRef repaired to %q, got %q", prefixedRef, rec.SecretRef)
	}

	// Legacy secret is untouched — deletion is a separate, explicit step.
	mock.mu.Lock()
	_, legacyStillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)]
	mock.mu.Unlock()
	if !legacyStillThere {
		t.Error("expected legacy signing key secret to remain (deleted only via --delete-legacy)")
	}

	// Idempotent: a second call is a no-op and does not error.
	if err := backend.CopyHubSecretForward(ctx, "user_signing_key"); err != nil {
		t.Fatalf("second CopyHubSecretForward call failed: %v", err)
	}
}

// TestGCPBackend_CopyHubSecretForward_RepairsRefWhenAlreadyCopied covers the
// case where the prefixed copy was already created on an earlier boot (or by
// an operator's migrate-names run) but the DB ref update failed or never
// happened — CopyHubSecretForward must still repair it, not just on the call
// that performs the copy (ptone/scion#2152 review finding 3).
func TestGCPBackend_CopyHubSecretForward_RepairsRefWhenAlreadyCopied(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("user_signing_key", store.ScopeHub, backend.hubID)
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	seedMockSecret(t, mock, backend.projectID, legacyName, "key-material")
	prefixedName := backend.gcpSecretName("user_signing_key", store.ScopeHub, backend.hubID)
	seedMockSecret(t, mock, backend.projectID, prefixedName, "key-material")

	if err := backend.store.CreateSecret(ctx, &store.Secret{
		ID:        tid("already-copied-signing-key"),
		Key:       "user_signing_key",
		Scope:     store.ScopeHub,
		ScopeID:   backend.hubID,
		SecretRef: legacyRef, // stale: still legacy even though the prefixed copy exists
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if err := backend.CopyHubSecretForward(ctx, "user_signing_key"); err != nil {
		t.Fatalf("CopyHubSecretForward failed: %v", err)
	}

	rec, err := backend.store.GetSecret(ctx, "user_signing_key", store.ScopeHub, backend.hubID)
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	if rec.SecretRef != prefixedRef {
		t.Errorf("expected stale ref to be repaired to %q, got %q", prefixedRef, rec.SecretRef)
	}
}

func TestGCPBackend_CopyHubSecretForward_NothingToCopy(t *testing.T) {
	// First boot: neither the prefixed nor legacy signing key exists yet.
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	if err := backend.CopyHubSecretForward(ctx, "agent_signing_key"); err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound when nothing to copy, got: %v", err)
	}
}

// --- ptone/scion#2152 round-1 review fixes ---

// denyDeleteSMClient denies DeleteSecret for exactly one full GCP SM
// resource name (simulating an IAM condition that no longer covers it),
// while every other call passes through to the wrapped mock.
type denyDeleteSMClient struct {
	*mockSMClient
	denyFullName string
}

func (m *denyDeleteSMClient) DeleteSecret(ctx context.Context, req *smpb.DeleteSecretRequest) error {
	if req.Name == m.denyFullName {
		return status.Errorf(codes.PermissionDenied, "denied by IAM condition")
	}
	return m.mockSMClient.DeleteSecret(ctx, req)
}

// TestGCPBackend_Delete_SucceedsWhenLegacyGrantRemoved reproduces review
// finding 4: once an operator narrows IAM to only the hub-prefixed name, the
// Hub's service account can no longer even query the legacy name (GCP
// evaluates IAM before existence), so Delete must not fail just because the
// best-effort legacy cleanup got PermissionDenied.
func TestGCPBackend_Delete_SucceedsWhenLegacyGrantRemoved(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	if _, _, err := backend.Set(ctx, &SetSecretInput{
		Name: "K", Value: "v", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "u1",
	}); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyFull := fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	denyBackend := NewGCPBackendWithClient(backend.store, &denyDeleteSMClient{mock, legacyFull}, "test-project", "test-hub-id")

	if err := denyBackend.Delete(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Errorf("expected Delete to succeed despite the legacy delete being denied, got: %v", err)
	}
	if _, err := backend.store.GetSecret(ctx, "K", ScopeUser, "u1"); err != store.ErrNotFound {
		t.Errorf("expected DB record removed, got err=%v", err)
	}

	// The prefixed (actually-used) copy must be gone too.
	prefixedName := backend.gcpSecretName("K", ScopeUser, "u1")
	mock.mu.Lock()
	_, prefixedStillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)]
	mock.mu.Unlock()
	if prefixedStillThere {
		t.Error("expected the prefixed secret to be deleted")
	}
}

// TestGCPBackend_Delete_FailsWhenPrefixedDeleteDenied ensures Delete still
// treats a denial on the *prefixed* (currently-used) name as fatal — only the
// legacy cleanup is best-effort.
func TestGCPBackend_Delete_FailsWhenPrefixedDeleteDenied(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	if _, _, err := backend.Set(ctx, &SetSecretInput{
		Name: "K", Value: "v", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "u1",
	}); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	prefixedName := backend.gcpSecretName("K", ScopeUser, "u1")
	prefixedFull := fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	denyBackend := NewGCPBackendWithClient(backend.store, &denyDeleteSMClient{mock, prefixedFull}, "test-project", "test-hub-id")

	if err := denyBackend.Delete(ctx, "K", ScopeUser, "u1"); err == nil {
		t.Error("expected Delete to fail when the prefixed (in-use) name can't be deleted")
	}
}

// TestGCPBackend_DeleteLegacySecretName_RefusesWhenDBRefNotPrefixed
// reproduces review finding 2: deleting the legacy secret while a DB record
// still points at it would make the record unreadable, since Get() trusts a
// stored SecretRef verbatim with no fallback.
func TestGCPBackend_DeleteLegacySecretName_RefusesWhenDBRefNotPrefixed(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	seedMockSecret(t, mock, backend.projectID, legacyName, "v")
	if _, err := backend.MigrateNameForward(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("MigrateNameForward failed: %v", err)
	}
	if err := backend.store.CreateSecret(ctx, &store.Secret{
		ID: tid("ref-not-repaired"), Key: "K", Scope: ScopeUser, ScopeID: "u1", SecretRef: legacyRef,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if err := backend.DeleteLegacySecretName(ctx, "K", ScopeUser, "u1"); err == nil {
		t.Error("expected DeleteLegacySecretName to refuse while the DB ref is still legacy")
	}
	mock.mu.Lock()
	_, stillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)]
	mock.mu.Unlock()
	if !stillThere {
		t.Error("legacy secret should not have been deleted while the DB ref was unrepaired")
	}

	// After repairing the ref, the same call must succeed.
	if _, err := backend.RepairRefToPrefixed(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("RepairRefToPrefixed failed: %v", err)
	}
	if err := backend.DeleteLegacySecretName(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Errorf("expected DeleteLegacySecretName to succeed once the ref is repaired, got: %v", err)
	}
}

// TestGCPBackend_DeleteLegacySecretName_NoDBRecordIsFine covers the
// defensive case (no DB row at all, e.g. a signing key recovered directly
// from GCP SM) — RefPointsAtPrefixed's hasRecord=false must not block delete.
func TestGCPBackend_DeleteLegacySecretName_NoDBRecordIsFine(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "v")
	if _, err := backend.MigrateNameForward(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("MigrateNameForward failed: %v", err)
	}
	if err := backend.DeleteLegacySecretName(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Errorf("expected success with no DB record, got: %v", err)
	}
}

func TestGCPBackend_RefPointsAtPrefixed(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	if hasRecord, _, err := backend.RefPointsAtPrefixed(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if hasRecord {
		t.Error("expected hasRecord=false with no DB row")
	}

	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "v")
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	if err := backend.store.CreateSecret(ctx, &store.Secret{ID: tid("rpap"), Key: "K", Scope: ScopeUser, ScopeID: "u1", SecretRef: legacyRef}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}
	if hasRecord, refIsPrefixed, err := backend.RefPointsAtPrefixed(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if !hasRecord || refIsPrefixed {
		t.Errorf("expected hasRecord=true, refIsPrefixed=false, got hasRecord=%v refIsPrefixed=%v", hasRecord, refIsPrefixed)
	}

	if _, err := backend.RepairRefToPrefixed(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("RepairRefToPrefixed failed: %v", err)
	}
	if hasRecord, refIsPrefixed, err := backend.RefPointsAtPrefixed(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if !hasRecord || !refIsPrefixed {
		t.Errorf("expected hasRecord=true, refIsPrefixed=true after repair, got hasRecord=%v refIsPrefixed=%v", hasRecord, refIsPrefixed)
	}
}

// raceCreateSMClient simulates two replicas of one hub racing to create the
// same GCP SM secret container: GetSecret reports NotFound (as if this
// replica hasn't seen it yet), but the underlying CreateSecret call hits a
// container another replica already created, returning AlreadyExists.
type raceCreateSMClient struct {
	*mockSMClient
}

func (m *raceCreateSMClient) GetSecret(_ context.Context, _ *smpb.GetSecretRequest) (*smpb.Secret, error) {
	return nil, status.Errorf(codes.NotFound, "not found (this replica hasn't seen it yet)")
}

// TestGCPBackend_Set_ConcurrentCreateRace reproduces review finding 11: two
// replicas of the same hub both see NotFound on GetSecret and race to
// CreateSecret; the loser must not fail, since the container now exists
// either way and both replicas write identical key material.
func TestGCPBackend_Set_ConcurrentCreateRace(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	mock := newMockSMClient()
	winner := NewGCPBackendWithClient(s, mock, "test-project", "test-hub-id")
	smName := winner.gcpSecretName("API_KEY", ScopeUser, "user-1")

	ctx := context.Background()
	if _, err := mock.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent: "projects/test-project", SecretId: smName, Secret: &smpb.Secret{},
	}); err != nil {
		t.Fatalf("failed to seed the winning replica's container: %v", err)
	}

	loser := NewGCPBackendWithClient(s, &raceCreateSMClient{mock}, "test-project", "test-hub-id")
	if _, _, err := loser.Set(ctx, &SetSecretInput{Name: "API_KEY", Value: "v", Scope: ScopeUser, ScopeID: "user-1"}); err != nil {
		t.Errorf("expected Set to succeed despite the AlreadyExists race, got: %v", err)
	}

	fullName := fmt.Sprintf("projects/test-project/secrets/%s", smName)
	mock.mu.Lock()
	val, ok := mock.versions[fullName]
	mock.mu.Unlock()
	if !ok || string(val) != "v" {
		t.Errorf("expected version %q to be added despite the race, got %q (present=%v)", "v", val, ok)
	}
}

// TestGCPBackend_NeedsNameMigration_FailedPreconditionOnPrefixedIsAbsent
// reproduces review finding 12: a prefixed container whose latest version is
// disabled/destroyed returns FailedPrecondition, not NotFound, and must
// still be treated as "needs copying", not surfaced as an error.
func TestGCPBackend_NeedsNameMigration_FailedPreconditionOnPrefixedIsAbsent(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	seedMockSecret(t, mock, backend.projectID, legacyName, "v")

	// Create the prefixed container but give it no accessible version: the
	// mock's AccessSecretVersion returns NotFound for a container with no
	// version, so wrap it to simulate the disabled/destroyed-version case
	// (FailedPrecondition) instead.
	prefixedName := backend.gcpSecretName("K", ScopeUser, "u1")
	if _, err := mock.CreateSecret(ctx, &smpb.CreateSecretRequest{Parent: "projects/test-project", SecretId: prefixedName, Secret: &smpb.Secret{}}); err != nil {
		t.Fatalf("failed to seed prefixed container: %v", err)
	}
	prefixedFull := fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)
	fpBackend := NewGCPBackendWithClient(backend.store, &failedPreconditionSMClient{mock, prefixedFull}, "test-project", "test-hub-id")

	needs, err := fpBackend.NeedsNameMigration(ctx, "K", ScopeUser, "u1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !needs {
		t.Error("expected needs=true when the prefixed name has no enabled version")
	}
}

// failedPreconditionSMClient returns FailedPrecondition from
// AccessSecretVersion for exactly one full GCP SM resource name.
type failedPreconditionSMClient struct {
	*mockSMClient
	failName string
}

func (m *failedPreconditionSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if strings.HasPrefix(req.Name, m.failName) {
		return nil, status.Errorf(codes.FailedPrecondition, "secret version is disabled")
	}
	return m.mockSMClient.AccessSecretVersion(ctx, req)
}

// denyAccessSMClient returns PermissionDenied from AccessSecretVersion for
// exactly one full GCP SM resource name.
type denyAccessSMClient struct {
	*mockSMClient
	denyName string
}

func (m *denyAccessSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if strings.HasPrefix(req.Name, m.denyName) {
		return nil, status.Errorf(codes.PermissionDenied, "denied by IAM condition")
	}
	return m.mockSMClient.AccessSecretVersion(ctx, req)
}

// TestGCPBackend_NeedsNameMigration_PermissionDeniedOnLegacyIsAbsent
// reproduces review finding 13: on a hub whose service account only holds
// the new-prefix conditioned grant, checking the legacy name returns
// PermissionDenied (GCP evaluates IAM before existence), not NotFound. This
// must be treated as "nothing to migrate" (store.ErrNotFound), not a hard
// error — otherwise CopyHubSecretForward logs a spurious WARN on every boot.
func TestGCPBackend_NeedsNameMigration_PermissionDeniedOnLegacyIsAbsent(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	denyBackend := NewGCPBackendWithClient(backend.store, &denyAccessSMClient{mock, legacyFull}, "test-project", "test-hub-id")

	_, err := denyBackend.NeedsNameMigration(ctx, "K", ScopeUser, "u1")
	if err != store.ErrNotFound {
		t.Errorf("expected store.ErrNotFound, got: %v", err)
	}

	// CopyHubSecretForward must therefore also return ErrNotFound (not a
	// hard error), which is what tells server.go's WARN-on-failure not to fire.
	if err := denyBackend.CopyHubSecretForward(ctx, "K"); err != store.ErrNotFound {
		t.Errorf("expected CopyHubSecretForward to propagate ErrNotFound, got: %v", err)
	}
}

// TestGCPBackend_SecretName_LongNamesDoNotSilentlyCollide reproduces review
// finding 16: the longer hub prefix moves the 255-char truncation boundary
// earlier than before ptone/scion#2152, so plain truncation risks two
// distinct, very long secret names colliding once cut to the same 255 bytes.
func TestGCPBackend_SecretName_LongNamesDoNotSilentlyCollide(t *testing.T) {
	backend, _ := createTestGCPBackend(t)

	// Two names that are identical for the first 240 characters and differ
	// only after that — long enough that the sanitized ID exceeds 255 bytes
	// and gets truncated.
	base := strings.Repeat("A", 240)
	name1 := base + "-one"
	name2 := base + "-two"

	got1 := backend.gcpSecretName(name1, ScopeUser, "user-1")
	got2 := backend.gcpSecretName(name2, ScopeUser, "user-1")

	if len(got1) > 255 || len(got2) > 255 {
		t.Fatalf("expected both names within the 255-char GCP SM limit, got %d and %d", len(got1), len(got2))
	}
	if got1 == got2 {
		t.Errorf("two distinct long names collided after truncation: %q", got1)
	}

	// Determinism: recomputing the same long name gives the same result.
	if again := backend.gcpSecretName(name1, ScopeUser, "user-1"); again != got1 {
		t.Errorf("gcpSecretName is not deterministic for a truncated name: %q != %q", again, got1)
	}
}

// --- ptone/scion#2152 round-2 review fixes ---

// TestGCPBackend_SecretName_TruncationContract pins the full contract behind
// TestGCPBackend_SecretName_LongNamesDoNotSilentlyCollide (review finding 14):
// the prefix is always preserved and the result never exceeds 255 chars, a
// name exactly at the cap is untouched, one byte over the cap is
// hash-suffixed, and legacyGCPSecretName's plain-truncation behavior (which
// must stay byte-identical to what pre-ptone/scion#2152 deployments already
// produced) is unaffected by any of this.
func TestGCPBackend_SecretName_TruncationContract(t *testing.T) {
	b, _ := createTestGCPBackend(t)
	prefix := b.secretNamePrefix()
	short := b.gcpSecretName("X", ScopeUser, "u1")
	fixed := len(short) - 1 // length of everything but the 1-char name "X"

	for _, n := range []int{255 - fixed, 256 - fixed, 400} {
		name := strings.Repeat("a", n)
		got := b.gcpSecretName(name, ScopeUser, "u1")
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("n=%d: expected prefix %q preserved, got %q", n, prefix, got)
		}
		if len(got) > 255 {
			t.Errorf("n=%d: expected len<=255, got %d", n, len(got))
		}
		untruncated := short[:len(short)-1] + name
		switch {
		case len(untruncated) <= 255 && got != untruncated:
			t.Errorf("n=%d: name under the cap should be unchanged, got %q want %q", n, got, untruncated)
		case len(untruncated) > 255 && got == untruncated[:255]:
			t.Errorf("n=%d: name over the cap should be hash-suffixed, not plain-truncated: %q", n, got)
		}
	}

	// legacyGCPSecretName must remain plain-truncated, byte-identical to the
	// pre-ptone/scion#2152 formula, regardless of the above.
	longName := strings.Repeat("b", 400)
	if got := b.legacyGCPSecretName(longName, ScopeUser, "u1"); len(got) != 255 || !strings.HasPrefix(got, "scion-user-") {
		t.Errorf("legacy truncation formula changed: %q", got)
	}
}

// TestGCPBackend_RepairRefToPrefixed_ResyncsStaleValue reproduces review
// finding 1: a prefixed copy created earlier can go stale relative to the
// DB ref's designated (legacy) value — e.g. an old binary in a mixed-version
// rolling deploy, or a rollback, rotates the secret through the legacy name
// again. Repairing the ref must overwrite the stale prefixed value with the
// ref-designated one *before* repointing, not just repoint blindly.
func TestGCPBackend_RepairRefToPrefixed_ResyncsStaleValue(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("API_KEY", ScopeUser, "user-1")
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	prefixedName := backend.gcpSecretName("API_KEY", ScopeUser, "user-1")

	// The prefixed copy already exists with an OLD value (from an earlier
	// migration or copy-forward)...
	seedMockSecret(t, mock, backend.projectID, prefixedName, "v1-old")
	// ...but the DB ref still designates the legacy name, which now holds a
	// NEWER, rotated value (an old binary wrote it after the prefixed copy
	// was created).
	seedMockSecret(t, mock, backend.projectID, legacyName, "v2-rotated")
	if err := backend.store.CreateSecret(ctx, &store.Secret{
		ID: tid("resync-stale"), Key: "API_KEY", Scope: ScopeUser, ScopeID: "user-1", SecretRef: legacyRef,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	action, err := backend.RepairRefToPrefixed(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("RepairRefToPrefixed failed: %v", err)
	}
	if action != "resynced" {
		t.Errorf("expected action %q, got %q", "resynced", action)
	}

	value, err := backend.accessLatestVersion(ctx, prefixedName)
	if err != nil {
		t.Fatalf("failed to read prefixed value: %v", err)
	}
	if value != "v2-rotated" {
		t.Errorf("BUG: prefixed copy still serves %q instead of the ref-designated %q", value, "v2-rotated")
	}

	rec, err := backend.store.GetSecret(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, prefixedName)
	if rec.SecretRef != prefixedRef {
		t.Errorf("expected ref repointed to %q, got %q", prefixedRef, rec.SecretRef)
	}

	// Get() must now also serve the correct (post-resync) value.
	sv, err := backend.Get(ctx, "API_KEY", ScopeUser, "user-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sv.Value != "v2-rotated" {
		t.Errorf("Get() returned %q, want %q", sv.Value, "v2-rotated")
	}
}

// TestGCPBackend_CopyHubSecretForward_ResyncsStaleKey is the startup
// copy-forward's counterpart to TestGCPBackend_RepairRefToPrefixed_ResyncsStaleValue.
func TestGCPBackend_CopyHubSecretForward_ResyncsStaleKey(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	legacyName := backend.legacyGCPSecretName("user_signing_key", store.ScopeHub, backend.hubID)
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, legacyName)
	prefixedName := backend.gcpSecretName("user_signing_key", store.ScopeHub, backend.hubID)

	seedMockSecret(t, mock, backend.projectID, prefixedName, "k-old")
	seedMockSecret(t, mock, backend.projectID, legacyName, "k-new")
	if err := backend.store.CreateSecret(ctx, &store.Secret{
		ID: tid("copy-forward-resync"), Key: "user_signing_key", Scope: store.ScopeHub, ScopeID: backend.hubID, SecretRef: legacyRef,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	if err := backend.CopyHubSecretForward(ctx, "user_signing_key"); err != nil {
		t.Fatalf("CopyHubSecretForward failed: %v", err)
	}

	sv, err := backend.Get(ctx, "user_signing_key", store.ScopeHub, backend.hubID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sv.Value != "k-new" {
		t.Errorf("BUG: copy-forward repointed ref at stale prefixed copy; hub now reads %q instead of %q", sv.Value, "k-new")
	}
}

// TestGCPBackend_RepairRefToPrefixed_PermissionDeniedOnLegacyIsFatal
// reproduces review finding 3's ref-repair half: unlike the no-DB-record
// path (migrationCheck), a DB record whose ref depends on a now-unreadable
// legacy secret must surface that as a real error, not silently be treated
// as "nothing to migrate".
func TestGCPBackend_RepairRefToPrefixed_PermissionDeniedOnLegacyIsFatal(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	legacyRef := "gcpsm:" + legacyFull
	if err := backend.store.CreateSecret(ctx, &store.Secret{ID: tid("ref-denied"), Key: "K", Scope: ScopeUser, ScopeID: "u1", SecretRef: legacyRef}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}
	denyBackend := NewGCPBackendWithClient(backend.store, &denyAccessSMClient{mock, legacyFull}, "test-project", "test-hub-id")

	if _, err := denyBackend.RepairRefToPrefixed(ctx, "K", ScopeUser, "u1"); err == nil {
		t.Error("expected an error when the ref-designated legacy secret is permission-denied")
	}
}

// TestGCPBackend_CopyHubSecretForward_EmptyPrefixedContainer reproduces
// review finding 6: a prefixed container that exists but has never had a
// version added (as opposed to a disabled/destroyed version, covered by
// TestGCPBackend_NeedsNameMigration_FailedPreconditionOnPrefixedIsAbsent)
// must still be treated as "not accessible", so copy-forward proceeds to
// populate it from the legacy value.
func TestGCPBackend_CopyHubSecretForward_EmptyPrefixedContainer(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	prefixedName := backend.gcpSecretName("agent_signing_key", store.ScopeHub, backend.hubID)
	if _, err := mock.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent: "projects/test-project", SecretId: prefixedName, Secret: &smpb.Secret{},
	}); err != nil {
		t.Fatalf("failed to seed empty prefixed container: %v", err)
	}
	legacyName := backend.legacyGCPSecretName("agent_signing_key", store.ScopeHub, backend.hubID)
	seedMockSecret(t, mock, backend.projectID, legacyName, "key-material")

	if err := backend.CopyHubSecretForward(ctx, "agent_signing_key"); err != nil {
		t.Fatalf("CopyHubSecretForward failed: %v", err)
	}
	value, err := backend.accessLatestVersion(ctx, prefixedName)
	if err != nil {
		t.Fatalf("expected prefixed secret to now have a version: %v", err)
	}
	if value != "key-material" {
		t.Errorf("expected copied value %q, got %q", "key-material", value)
	}
}

// TestNewGCPBackend_EmptyHubIDWarns reproduces review finding 7: the WARN in
// NewGCPBackend fires before any network call (newGCPSMClient), so it can be
// tested without a real GCP client by supplying credentials JSON that fails
// to parse locally.
func TestNewGCPBackend_EmptyHubIDWarns(t *testing.T) {
	orig := slog.Default()
	h := &recordingHandler{}
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(orig)

	_, err := NewGCPBackend(context.Background(), nil, GCPBackendConfig{
		ProjectID:       "test-project",
		CredentialsJSON: "{not valid json",
	}, "" /* empty hubID */)
	if err == nil {
		t.Fatal("expected NewGCPBackend to fail on invalid credentials JSON (no network needed to observe this)")
	}
	if !h.hasWarnContaining("empty hub ID") {
		t.Error("expected a WARN log about the empty hub ID")
	}
}

func TestNewGCPBackend_NonEmptyHubIDDoesNotWarn(t *testing.T) {
	orig := slog.Default()
	h := &recordingHandler{}
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(orig)

	_, _ = NewGCPBackend(context.Background(), nil, GCPBackendConfig{
		ProjectID:       "test-project",
		CredentialsJSON: "{not valid json",
	}, "real-hub-id")
	if h.hasWarnContaining("empty hub ID") {
		t.Error("did not expect a WARN about the empty hub ID when one was provided")
	}
}

// TestGCPBackend_Delete_UnmigratedSecretFailsClosedOnTransientError
// reproduces review finding 2: deleting an UNMIGRATED secret (its only copy
// is the legacy name; the DB ref points there) while the legacy delete fails
// with anything other than NotFound/PermissionDenied-when-safe must fail the
// whole Delete, leaving both the DB record and the legacy GCP SM secret
// intact — never report success while the secret's only copy survives.
type flakyLegacyDeleteSMClient struct {
	*mockSMClient
	legacyFull string
}

func (m *flakyLegacyDeleteSMClient) DeleteSecret(ctx context.Context, req *smpb.DeleteSecretRequest) error {
	if req.Name == m.legacyFull {
		return status.Error(codes.Unavailable, "transient backend error")
	}
	return m.mockSMClient.DeleteSecret(ctx, req)
}

func TestGCPBackend_Delete_UnmigratedSecretFailsClosedOnTransientError(t *testing.T) {
	b, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := b.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	seedMockSecret(t, mock, "test-project", legacyName, "secret-value")
	legacyRef := "gcpsm:" + legacyFull
	if err := b.store.CreateSecret(ctx, &store.Secret{ID: tid("unmigrated-flaky-delete"), Key: "K", Scope: ScopeUser, ScopeID: "u1", SecretRef: legacyRef}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	nb := NewGCPBackendWithClient(b.store, &flakyLegacyDeleteSMClient{mock, legacyFull}, "test-project", "test-hub-id")
	if err := nb.Delete(ctx, "K", ScopeUser, "u1"); err == nil {
		t.Fatal("expected Delete to fail: the only copy of the secret (legacy name) was not actually deleted")
	}

	// Nothing must have been touched: the DB record survives with its
	// original ref, and the legacy GCP SM secret is still there.
	rec, err := b.store.GetSecret(ctx, "K", ScopeUser, "u1")
	if err != nil {
		t.Fatalf("expected the DB record to survive a failed Delete, got: %v", err)
	}
	if rec.SecretRef != legacyRef {
		t.Errorf("expected SecretRef unchanged at %q, got %q", legacyRef, rec.SecretRef)
	}
	mock.mu.Lock()
	_, legacyStillThere := mock.secrets[legacyFull]
	mock.mu.Unlock()
	if !legacyStillThere {
		t.Error("expected the legacy GCP SM secret to survive a failed Delete")
	}

	// Because nothing was actually deleted, a subsequent Get() correctly
	// resolves through the still-intact DB record — this is not the
	// "resurrection via DB-less fallback" the original bug produced (there
	// the DB record itself was gone), it is simply proof that Delete()
	// failing left the secret exactly as it was.
	if sv, err := nb.Get(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Errorf("expected Get to still resolve the untouched secret, got err: %v", err)
	} else if sv.Value != "secret-value" {
		t.Errorf("expected unchanged value %q, got %q", "secret-value", sv.Value)
	}
}

// TestGCPBackend_Delete_PermissionDeniedOnLegacyFatalWhenUnmigrated extends
// finding 2 to the specific PermissionDenied case: round 1 only tested
// PermissionDenied on the legacy name for an already-migrated secret (where
// it must be non-fatal); for an unmigrated one it must still be fatal, since
// the legacy copy is the only one and PermissionDenied is not proof it's
// merely a superseded copy.
func TestGCPBackend_Delete_PermissionDeniedOnLegacyFatalWhenUnmigrated(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	legacyName := backend.legacyGCPSecretName("K", ScopeUser, "u1")
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	seedMockSecret(t, mock, "test-project", legacyName, "v")
	legacyRef := "gcpsm:" + legacyFull
	if err := backend.store.CreateSecret(ctx, &store.Secret{ID: tid("unmigrated-denied-delete"), Key: "K", Scope: ScopeUser, ScopeID: "u1", SecretRef: legacyRef}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}
	denyBackend := NewGCPBackendWithClient(backend.store, &denyDeleteSMClient{mock, legacyFull}, "test-project", "test-hub-id")

	if err := denyBackend.Delete(ctx, "K", ScopeUser, "u1"); err == nil {
		t.Error("expected Delete to fail: legacy is the only copy, so PermissionDenied on it must be fatal")
	}
	if _, err := backend.store.GetSecret(ctx, "K", ScopeUser, "u1"); err != nil {
		t.Errorf("expected the DB record to survive a failed Delete, got: %v", err)
	}
}
