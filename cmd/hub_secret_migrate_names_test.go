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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
// mirror the formula documented in .design/secret-id-hub-refactor.md §7 and
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

// --- ptone/scion#2152 round-1 review fixes ---

// TestRunMigrateNames_TwoStepDeleteLegacyWorkflow reproduces review finding 1:
// the documented workflow is a plain run followed by a separate
// --delete-legacy run. Before the fix, the second run saw the prefixed copy
// already present, reported "0 migrated, N skipped", and never reached the
// delete step.
func TestRunMigrateNames_TwoStepDeleteLegacyWorkflow(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{ID: tid("two-step"), Key: "API_KEY", Scope: "user", ScopeID: "user-1"}))
	mock.seed(t, "test-project", legacyName, "v")

	var out1 bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out1))
	assert.Contains(t, out1.String(), "MIGRATED")

	var out2 bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true /* deleteLegacy */, &out2))
	t.Logf("second run output:\n%s", out2.String())
	assert.Contains(t, out2.String(), "DELETED LEGACY")

	assert.False(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", legacyName)),
		"legacy secret must be gone after the documented plain-then---delete-legacy workflow")
}

// TestRunMigrateNames_RepairsRefLeftByCopyForward reproduces review finding
// 2/3: a signing key copied forward at hub startup (not via migrate-names)
// gets its DB ref repaired the next time migrate-names runs, even though no
// copy is needed that run.
func TestRunMigrateNames_RepairsRefLeftByCopyForward(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("user_signing_key", "hub", migrateNamesTestHubID)
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("copy-forward-sk"), Key: "user_signing_key", Scope: "hub", ScopeID: migrateNamesTestHubID, SecretRef: legacyRef,
	}))
	mock.seed(t, "test-project", legacyName, "k")

	// Simulate the hub-startup copy-forward: it creates the prefixed copy.
	// (Whether or not CopyHubSecretForward itself also repairs the ref is
	// covered at the pkg/secret level; this test's point is that
	// migrate-names repairs it regardless, since it must not assume the
	// startup path already did.)
	_, err := backend.MigrateNameForward(ctx, "user_signing_key", "hub", migrateNamesTestHubID)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out))
	t.Logf("output:\n%s", out.String())

	rec, err := db.GetSecret(ctx, "user_signing_key", "hub", migrateNamesTestHubID)
	require.NoError(t, err)
	assert.NotEqual(t, legacyRef, rec.SecretRef, "SecretRef must move off the legacy path")
	prefixedName := prefixedNameForTest("user_signing_key", "hub", migrateNamesTestHubID)
	assert.Equal(t, "gcpsm:projects/test-project/secrets/"+prefixedName, rec.SecretRef)
}

// TestRunMigrateNames_DryRunPlansRefRepairAndDelete extends the dry-run
// coverage to the ref-repair and delete-legacy planning branches (not just
// the copy branch), since finding 1's restructure added those as
// independently-triggered actions.
func TestRunMigrateNames_DryRunPlansRefRepairAndDelete(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{ID: tid("dry-run-ref"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: legacyRef}))
	mock.seed(t, "test-project", legacyName, "v")
	// Prefixed copy already exists (as if copied forward already), so a real
	// run would only need to repair the ref and (if asked) delete legacy.
	_, err := backend.MigrateNameForward(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true /* dryRun */, true /* deleteLegacy */, &out))
	assert.Contains(t, out.String(), "REPAIR REF")
	assert.Contains(t, out.String(), "DELETE LEGACY")
	assert.NotContains(t, out.String(), "MIGRATE ") // no copy needed; guard against matching "MIGRATE AND ..."

	// Still must not have written anything.
	rec, err := db.GetSecret(ctx, "user_signing_key", "hub", migrateNamesTestHubID)
	if err == nil {
		t.Fatalf("unexpected record found: %+v", rec)
	}
	rec2, err := db.GetSecret(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	assert.Equal(t, legacyRef, rec2.SecretRef, "dry-run must not repair the ref")
}

func TestOpenMigrateNamesStore_UnsupportedDriver(t *testing.T) {
	cfg := &config.GlobalConfig{Database: config.DatabaseConfig{Driver: "mysql", URL: "unused"}}
	_, err := openMigrateNamesStore(context.Background(), cfg, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mysql")
}

func TestOpenMigrateNamesStore_SQLiteDefaultAndExplicit(t *testing.T) {
	for _, driver := range []string{"", "sqlite"} {
		t.Run(fmt.Sprintf("driver=%q", driver), func(t *testing.T) {
			dbPath := "file:" + t.TempDir() + "/migrate-names-test.db"
			cfg := &config.GlobalConfig{Database: config.DatabaseConfig{Driver: driver, URL: dbPath}}
			cs, err := openMigrateNamesStore(context.Background(), cfg, false)
			require.NoError(t, err)
			defer func() { _ = cs.Close() }()
			// Migration ran (dryRun=false): a real query against the schema
			// must succeed.
			_, err = cs.ListSecrets(context.Background(), store.SecretFilter{})
			assert.NoError(t, err)
		})
	}
}

// TestOpenMigrateNamesStore_DryRunFailsOnMissingFile reproduces review
// finding 5: --dry-run must make no writes at all, including to SQLite. The
// round-1 fix only skipped schema migration, but plain entc.OpenSQLite still
// creates the database file if it's missing and switches its journal to
// WAL — both writes. Opening the same nonexistent path under --dry-run must
// now fail cleanly instead, and critically must not create the file.
func TestOpenMigrateNamesStore_DryRunFailsOnMissingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate-names-dry-run-missing.db")
	cfg := &config.GlobalConfig{Database: config.DatabaseConfig{Driver: "sqlite", URL: "file:" + dbPath}}

	_, err := openMigrateNamesStore(context.Background(), cfg, true /* dryRun */)
	require.Error(t, err, "opening a nonexistent database read-only must fail, not silently create it")

	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("--dry-run must not create the database file; stat error: %v", statErr)
	}
}

// TestOpenMigrateNamesStore_DryRunReadsExistingWithoutWriting covers the
// companion case: --dry-run against an already-migrated database can still
// read from it (that's the whole point of --dry-run), and the connection is
// genuinely read-only (a write attempt through it fails).
func TestOpenMigrateNamesStore_DryRunReadsExistingWithoutWriting(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate-names-dry-run-existing.db")
	cfg := &config.GlobalConfig{Database: config.DatabaseConfig{Driver: "sqlite", URL: "file:" + dbPath}}

	// Create and migrate the schema first, as a normal (non-dry-run) run would.
	seedCS, err := openMigrateNamesStore(context.Background(), cfg, false)
	require.NoError(t, err)
	require.NoError(t, seedCS.CreateSecret(context.Background(), &store.Secret{
		ID: tid("dry-run-existing"), Key: "API_KEY", Scope: "user", ScopeID: "user-1",
	}))
	require.NoError(t, seedCS.Close())

	cs, err := openMigrateNamesStore(context.Background(), cfg, true /* dryRun */)
	require.NoError(t, err, "dry-run must still be able to read an existing, already-migrated database")
	defer func() { _ = cs.Close() }()

	secrets, err := cs.ListSecrets(context.Background(), store.SecretFilter{})
	require.NoError(t, err)
	assert.Len(t, secrets, 1)

	// The connection must be genuinely read-only: an attempted write fails.
	err = cs.CreateSecret(context.Background(), &store.Secret{ID: tid("dry-run-write-attempt"), Key: "OTHER", Scope: "user", ScopeID: "user-1"})
	assert.Error(t, err, "a write through the --dry-run (read-only) connection must fail")
}

// TestRunMigrateNames_ResyncsStaleValueFromMixedVersionRollingDeploy
// reproduces review finding 1: a mixed-version rolling deploy of one hub (or
// a rollback-then-roll-forward window) can leave a prefixed copy stale
// relative to the DB ref's designated (legacy) value. migrate-names must
// resync the prefixed copy to the newer, ref-designated value — not blindly
// repoint the ref at the stale one — and the hub must serve the correct
// value afterward.
func TestRunMigrateNames_ResyncsStaleValueFromMixedVersionRollingDeploy(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{ID: tid("resync-rolling-deploy"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: legacyRef}))
	mock.seed(t, "test-project", legacyName, "v1-old")

	// First migrate-names run: creates the prefixed copy and repairs the ref.
	var out1 bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out1))

	// Mixed-version rolling deploy: an old replica (or a rolled-back binary)
	// still targets the legacy name and rotates the secret through it,
	// resetting the DB ref back to legacy — all without going through this
	// hub's migrate-names.
	_, err := mock.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent:  "projects/test-project/secrets/" + legacyName,
		Payload: &smpb.SecretPayload{Data: []byte("v2-rotated")},
	})
	require.NoError(t, err)
	rec, err := db.GetSecret(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	rec.SecretRef = legacyRef
	require.NoError(t, db.UpdateSecret(ctx, rec))

	sv, err := backend.Get(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	require.Equal(t, "v2-rotated", sv.Value, "precondition: hub serves the rotated value before migrate-names runs again")

	// Second migrate-names run must resync, not silently repoint to the stale copy.
	var out2 bytes.Buffer
	err = runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out2)
	require.NoError(t, err)
	assert.Contains(t, out2.String(), "RESYNCED")

	sv, err = backend.Get(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	assert.Equal(t, "v2-rotated", sv.Value, "BUG: hub serves the stale pre-rotation value after migrate-names")
}

// sprev2DenyLegacyAccess denies AccessSecretVersion for one legacy secret's
// full resource path, simulating an operator running migrate-names after the
// legacy IAM grant has already been dropped (or with credentials that never
// had it).
type sprev2DenyLegacyAccess struct {
	*migrateNamesMockSMClient
	legacyFull string
}

func (d *sprev2DenyLegacyAccess) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if strings.HasPrefix(req.Name, d.legacyFull+"/") {
		return nil, status.Error(codes.PermissionDenied, "denied by IAM condition")
	}
	return d.migrateNamesMockSMClient.AccessSecretVersion(ctx, req)
}

// TestRunMigrateNames_FailsWhenLegacyDeniedForDependentRecord reproduces
// review finding 3: migrate-names must not report success when a DB record's
// ref still depends on a legacy name it can no longer read (e.g. the legacy
// IAM grant was dropped too early). Silently skipping it as "absent" would
// leave the record's ref pointed at an now-unreadable secret while the
// operator believes migration finished.
func TestRunMigrateNames_FailsWhenLegacyDeniedForDependentRecord(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyFull := "projects/test-project/secrets/" + legacyName
	mock.seed(t, "test-project", legacyName, "v")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{ID: tid("legacy-denied"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: "gcpsm:" + legacyFull}))
	backend := migrateNamesTestBackend(t, db, &sprev2DenyLegacyAccess{mock, legacyFull}, migrateNamesTestHubID)

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	t.Logf("output:\n%s", out.String())
	require.Error(t, err, "migrate-names must not exit 0 while API_KEY's DB ref still depends on an unreadable legacy secret")
	assert.Contains(t, out.String(), "1 failed")

	// The record's ref must be left untouched — no silent repointing to
	// somewhere the value was never actually verified.
	rec, err := db.GetSecret(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, err)
	assert.Equal(t, "gcpsm:"+legacyFull, rec.SecretRef)
}

// TestRunMigrateNames_DryRunPlansResyncNotDeleteOnMismatch covers
// non-blocking finding 13: once a stale prefixed copy exists, --dry-run must
// plan RESYNC (what the real run would actually do), not a plain
// DELETE LEGACY that a real run would in fact refuse due to the value
// mismatch DeleteLegacySecretName checks for.
func TestRunMigrateNames_DryRunPlansResyncNotDeleteOnMismatch(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{ID: tid("dry-run-resync"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: legacyRef}))
	mock.seed(t, "test-project", legacyName, "v2-rotated")
	mock.seed(t, "test-project", prefixedName, "v1-old") // stale

	var out bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true /* dryRun */, true /* deleteLegacy */, &out))
	assert.Contains(t, out.String(), "RESYNC")
	assert.NotContains(t, out.String(), "WOULD DELETE LEGACY  API_KEY", "a plain (non-resync) delete-legacy plan would be wrong here: the real run would refuse due to the value mismatch until it resyncs first")

	// Nothing must have been written.
	mock.mu.Lock()
	prefixedValue := string(mock.versions["projects/test-project/secrets/"+prefixedName])
	mock.mu.Unlock()
	assert.Equal(t, "v1-old", prefixedValue, "dry-run must not have written the resync")
}
