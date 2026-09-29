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

package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// migrateNamesMockSMClient is a minimal secret.SMClient fake for exercising
// runMigrateNames end-to-end against the real GCPBackend naming/migration
// logic, without any GCP network calls or credentials — per the
// ptone/scion#2152 constraint that tests run only against fakes/mocks.
type migrateNamesMockSMClient struct {
	mu       sync.Mutex
	secrets  map[string]*smpb.Secret
	versions map[string][]byte
}

var _ secret.SMClient = (*migrateNamesMockSMClient)(nil)

func newMigrateNamesMockSMClient() *migrateNamesMockSMClient {
	return &migrateNamesMockSMClient{
		secrets:  make(map[string]*smpb.Secret),
		versions: make(map[string][]byte),
	}
}

func (m *migrateNamesMockSMClient) CreateSecret(_ context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fullName := fmt.Sprintf("%s/secrets/%s", req.Parent, req.SecretId)
	if _, exists := m.secrets[fullName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "secret %s already exists", fullName)
	}
	sec := &smpb.Secret{Name: fullName, Labels: req.Secret.GetLabels()}
	m.secrets[fullName] = sec
	return sec, nil
}

func (m *migrateNamesMockSMClient) AddSecretVersion(_ context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Parent]; !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Parent)
	}
	m.versions[req.Parent] = req.Payload.Data
	return &smpb.SecretVersion{Name: req.Parent + "/versions/1"}, nil
}

func (m *migrateNamesMockSMClient) AccessSecretVersion(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := req.Name
	for _, suffix := range []string{"/versions/latest", "/versions/1"} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	data, exists := m.versions[name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "version not found for %s", req.Name)
	}
	return &smpb.AccessSecretVersionResponse{Name: req.Name, Payload: &smpb.SecretPayload{Data: data}}, nil
}

func (m *migrateNamesMockSMClient) DeleteSecret(_ context.Context, req *smpb.DeleteSecretRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Name]; !exists {
		return status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	delete(m.secrets, req.Name)
	delete(m.versions, req.Name)
	return nil
}

func (m *migrateNamesMockSMClient) GetSecret(_ context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec, exists := m.secrets[req.Name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	return sec, nil
}

func (m *migrateNamesMockSMClient) Close() error { return nil }

// seed creates a secret and an initial version directly, simulating a secret
// that already exists in GCP SM under a specific name (e.g. a legacy
// pre-prefix name) without going through GCPBackend.
func (m *migrateNamesMockSMClient) seed(t *testing.T, projectID, smName, value string) {
	t.Helper()
	ctx := context.Background()
	fullName := fmt.Sprintf("projects/%s/secrets/%s", projectID, smName)
	_, err := m.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", projectID),
		SecretId: smName,
		Secret:   &smpb.Secret{},
	})
	require.NoError(t, err)
	_, err = m.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent:  fullName,
		Payload: &smpb.SecretPayload{Data: []byte(value)},
	})
	require.NoError(t, err)
}

func (m *migrateNamesMockSMClient) has(fullName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.secrets[fullName]
	return ok
}

// migrateNamesTestBackend builds a GCPBackend the same way production code
// does (secret.NewGCPBackendWithClient), backed by a fake SMClient and the
// real in-memory/sqlite SecretStore, so runMigrateNames is exercised through
// its exported production surface.
func migrateNamesTestBackend(t *testing.T, db store.SecretStore, mock secret.SMClient, hubID string) *secret.GCPBackend {
	t.Helper()
	return secret.NewGCPBackendWithClient(db, mock, "test-project", hubID)
}

func TestRunMigrateNames_NoCandidates(t *testing.T) {
	// Even with an empty DB, the known hub-scope signing-key names are always
	// checked (and found absent on a fresh hub), so nothing is migrated.
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	var out bytes.Buffer
	err := runMigrateNames(context.Background(), backend, db, migrateNamesTestHubID, false, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "0 migrated")
	assert.NotContains(t, out.String(), "ERROR")
}

func TestRunMigrateNames_DryRunDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	rec := &store.Secret{ID: tid("dry-run-secret"), Key: "API_KEY", Scope: "user", ScopeID: "user-1"}
	require.NoError(t, db.CreateSecret(ctx, rec))

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	mock.seed(t, "test-project", legacyName, "old-value")

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true /* dryRun */, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "WOULD MIGRATE")
	assert.Contains(t, out.String(), "API_KEY")

	// Dry run must not create the prefixed secret or touch the DB ref.
	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	assert.False(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)), "dry-run must not create the prefixed secret")

	updated, err := db.GetSecret(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	assert.Empty(t, updated.SecretRef, "dry-run must not update the DB SecretRef")
}

func TestRunMigrateNames_MigratesAndUpdatesRef(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	rec := &store.Secret{
		ID:        tid("migrate-secret"),
		Key:       "API_KEY",
		Scope:     "user",
		ScopeID:   "user-1",
		SecretRef: "gcpsm:" + fmt.Sprintf("projects/test-project/secrets/%s", legacyName),
	}
	require.NoError(t, db.CreateSecret(ctx, rec))
	mock.seed(t, "test-project", legacyName, "old-value")

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "MIGRATED")

	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	assert.True(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)), "expected prefixed secret to be created")

	updated, err := db.GetSecret(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	assert.Equal(t, "gcpsm:"+fmt.Sprintf("projects/test-project/secrets/%s", prefixedName), updated.SecretRef)

	// Legacy secret remains until an explicit --delete-legacy.
	assert.True(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", legacyName)))
}

func TestRunMigrateNames_IdempotentReRun(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	rec := &store.Secret{ID: tid("idempotent-secret"), Key: "API_KEY", Scope: "user", ScopeID: "user-1"}
	require.NoError(t, db.CreateSecret(ctx, rec))
	mock.seed(t, "test-project", legacyName, "old-value")

	var out1 bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out1))
	assert.Contains(t, out1.String(), "1 migrated")

	var out2 bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out2))
	assert.Contains(t, out2.String(), "0 migrated")
	assert.NotContains(t, out2.String(), "ERROR")
}

func TestRunMigrateNames_DeleteLegacyOrdering(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	rec := &store.Secret{ID: tid("delete-legacy-secret"), Key: "API_KEY", Scope: "user", ScopeID: "user-1"}
	require.NoError(t, db.CreateSecret(ctx, rec))
	mock.seed(t, "test-project", legacyName, "old-value")

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true /* deleteLegacy */, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "MIGRATED")
	assert.Contains(t, out.String(), "DELETED LEGACY")

	assert.False(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", legacyName)), "expected legacy secret to be deleted")

	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	assert.True(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)), "expected prefixed copy to survive")
}

// TestRunMigrateNames_KnownHubSigningKeyWithoutDBRecord covers the
// ptone/scion#2152 decision to also check known hub-scope signing-key names
// even when no Hub DB record covers them (e.g. after a database reset that
// left the value only in GCP SM under the legacy name).
func TestRunMigrateNames_KnownHubSigningKeyWithoutDBRecord(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest(hub.SecretKeyUserSigningKey, "hub", migrateNamesTestHubID)
	mock.seed(t, "test-project", legacyName, "key-material")

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), hub.SecretKeyUserSigningKey)
	assert.Contains(t, out.String(), "MIGRATED")

	prefixedName := prefixedNameForTest(hub.SecretKeyUserSigningKey, "hub", migrateNamesTestHubID)
	assert.True(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)))
}

// legacyNameForTest and prefixedNameForTest recompute the GCP SM naming
// formulas for test setup/assertions, since this test file (package cmd)
// cannot reach GCPBackend's unexported naming methods (package secret). They
// mirror the formula pinned in
// /scion-volumes/scratchpad/projects/secret-prefix/formula-decision.md and
// independently verified by the golden-vector test in
// pkg/secret/gcpbackend_test.go — a coincidental drift in both places is the
// only way these could mask a real bug.
const migrateNamesTestHubID = "hub-1"

func legacyNameForTest(name, scope, scopeID string) string {
	return sanitizeGCPSecretIDForTest(fmt.Sprintf("scion-%s-%s-%s", scope, hash12ForTest(migrateNamesTestHubID+":"+scopeID), name))
}

func prefixedNameForTest(name, scope, scopeID string) string {
	prefix := "scion-" + hash12ForTest(migrateNamesTestHubID) + "-"
	return sanitizeGCPSecretIDForTest(fmt.Sprintf("%s%s-%s-%s", prefix, scope, hash12ForTest(migrateNamesTestHubID+":"+scopeID), name))
}

func hash12ForTest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

var invalidSecretIDCharsForTest = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeGCPSecretIDForTest(s string) string {
	s = invalidSecretIDCharsForTest.ReplaceAllString(s, "-")
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}
