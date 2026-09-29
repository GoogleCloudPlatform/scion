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

// =============================================================================
// Round 3 review regression tests (ptone/scion#2152 PR 2171, sp-rev-3.md)
// =============================================================================

// TestResolveMigrateNamesHubID_ExplicitFlagWins pins --hub-id as the
// top of the precedence order, matching the flag's documented override
// semantics.
func TestResolveMigrateNamesHubID_ExplicitFlagWins(t *testing.T) {
	orig := migrateNamesHubID
	defer func() { migrateNamesHubID = orig }()
	migrateNamesHubID = "explicit-hub"

	cfg := &config.GlobalConfig{Hub: config.HubServerConfig{HubID: "settings-hub"}}
	id, err := resolveMigrateNamesHubID(cfg, false)
	require.NoError(t, err)
	assert.Equal(t, "explicit-hub", id)
}

// TestResolveMigrateNamesHubID_SettingsHubIDPreferredOverEnvAndHostname
// reproduces round-3 review finding 1 (critical): migrate-names must resolve
// the hub ID exactly the way the running hub server does --
// HubServerConfig.ResolveHubID() checks the settings hub_id field before
// ever consulting the environment or hostname fallback. This pins that a
// settings hub_id that differs from both the env var and (implicitly) the
// hostname-derived fallback wins, exactly as it would on the server.
func TestResolveMigrateNamesHubID_SettingsHubIDPreferredOverEnvAndHostname(t *testing.T) {
	orig := migrateNamesHubID
	defer func() { migrateNamesHubID = orig }()
	migrateNamesHubID = ""

	t.Setenv("SCION_SERVER_HUB_HUBID", "env-hub-id")
	t.Setenv("HOME", t.TempDir()) // would otherwise derive/persist a third, hostname-based value

	cfg := &config.GlobalConfig{Hub: config.HubServerConfig{HubID: "settings-hub-id"}}

	id, err := resolveMigrateNamesHubID(cfg, false)
	require.NoError(t, err)
	assert.Equal(t, "settings-hub-id", id, "settings hub_id must win over both the env var and the hostname fallback, matching HubServerConfig.ResolveHubID()'s own precedence")

	// --dry-run must reach the identical answer via the identical settings
	// check (it never even needs the read-only env fallback here).
	idDryRun, err := resolveMigrateNamesHubID(cfg, true)
	require.NoError(t, err)
	assert.Equal(t, "settings-hub-id", idDryRun)
}

// TestResolveMigrateNamesHubID_EnvFallbackUsedWhenNoSettingsHubID covers the
// non-dry-run environment/hostname fallback path (HubServerConfig.ResolveHubID
// delegates to config.ResolveHubIDFromEnv when settings hub_id is unset).
func TestResolveMigrateNamesHubID_EnvFallbackUsedWhenNoSettingsHubID(t *testing.T) {
	orig := migrateNamesHubID
	defer func() { migrateNamesHubID = orig }()
	migrateNamesHubID = ""

	t.Setenv("SCION_SERVER_HUB_HUBID", "env-hub-id")
	cfg := &config.GlobalConfig{}

	id, err := resolveMigrateNamesHubID(cfg, false)
	require.NoError(t, err)
	assert.Equal(t, "env-hub-id", id)
}

// TestResolveMigrateNamesHubID_DryRunFailsWhenResolutionWouldWriteToDisk
// reproduces round-3 review finding 1's --dry-run requirement: on a
// workstation with no settings hub_id, no SCION_SERVER_HUB_HUBID/K_SERVICE,
// and no persisted ~/.scion/hub-id yet, the real (non-dry-run) resolution
// path would create and persist a hostname-derived ID as a side effect
// (PersistentHubID) -- a write. --dry-run must refuse rather than derive an
// unpersisted value that might not match what a real run ends up writing,
// and critically must not create the file itself.
func TestResolveMigrateNamesHubID_DryRunFailsWhenResolutionWouldWriteToDisk(t *testing.T) {
	orig := migrateNamesHubID
	defer func() { migrateNamesHubID = orig }()
	migrateNamesHubID = ""

	t.Setenv("SCION_SERVER_HUB_HUBID", "")
	t.Setenv("K_SERVICE", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cfg := &config.GlobalConfig{}
	_, err := resolveMigrateNamesHubID(cfg, true)
	require.Error(t, err, "--dry-run must refuse to resolve a hub ID that would require persisting ~/.scion/hub-id for the first time")
	assert.Contains(t, err.Error(), "--hub-id")

	if _, statErr := os.Stat(filepath.Join(tmpHome, ".scion", "hub-id")); !os.IsNotExist(statErr) {
		t.Errorf("--dry-run must not create ~/.scion/hub-id; stat error: %v", statErr)
	}
}

// TestResolveMigrateNamesHubID_DryRunUsesPersistedFileWithoutRewritingIt
// covers the companion case: if ~/.scion/hub-id already exists from an
// earlier server boot, --dry-run may read it (that's a read, not a write)
// without needing --hub-id.
func TestResolveMigrateNamesHubID_DryRunUsesPersistedFileWithoutRewritingIt(t *testing.T) {
	orig := migrateNamesHubID
	defer func() { migrateNamesHubID = orig }()
	migrateNamesHubID = ""

	t.Setenv("SCION_SERVER_HUB_HUBID", "")
	t.Setenv("K_SERVICE", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0700))
	hubIDPath := filepath.Join(scionDir, "hub-id")
	require.NoError(t, os.WriteFile(hubIDPath, []byte("persisted-hub-id\n"), 0600))
	before, err := os.Stat(hubIDPath)
	require.NoError(t, err)

	cfg := &config.GlobalConfig{}
	id, err := resolveMigrateNamesHubID(cfg, true)
	require.NoError(t, err)
	assert.Equal(t, "persisted-hub-id", id)

	after, err := os.Stat(hubIDPath)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "--dry-run must not rewrite the persisted hub-id file")
}

// TestCheckMigrateNamesHubIDAgainstExistingRecords_MismatchRefused
// reproduces round-3 review finding 1's cross-check: a resolved hub ID that
// disagrees with an existing hub-scope secret record's ScopeID must be
// refused rather than silently treating that hub's existing secrets as
// belonging to a different (freshly resolved) hub ID.
func TestCheckMigrateNamesHubIDAgainstExistingRecords_MismatchRefused(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("hub-scope-existing"), Key: hub.SecretKeyAgentSigningKey, Scope: store.ScopeHub, ScopeID: "old-hub-id",
	}))

	err := checkMigrateNamesHubIDAgainstExistingRecords(ctx, db, "new-hub-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old-hub-id")
}

func TestCheckMigrateNamesHubIDAgainstExistingRecords_MatchAllowed(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("hub-scope-existing-match"), Key: hub.SecretKeyAgentSigningKey, Scope: store.ScopeHub, ScopeID: "hub-1",
	}))

	assert.NoError(t, checkMigrateNamesHubIDAgainstExistingRecords(ctx, db, "hub-1"))
}

func TestCheckMigrateNamesHubIDAgainstExistingRecords_NoRecordsAllowed(t *testing.T) {
	db := newTestStore(t)
	assert.NoError(t, checkMigrateNamesHubIDAgainstExistingRecords(context.Background(), db, "any-hub-id"))
}

// TestRunMigrateNames_NoRefNoGCPCopyIsSkippedNotFailed reproduces round-3
// review finding 4: a DB record with no stored ref and no value under
// either GCP SM name is nothing to migrate, not a failure.
func TestRunMigrateNames_NoRefNoGCPCopyIsSkippedNotFailed(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("no-ref-no-copy"), Key: "GHOST", Scope: "user", ScopeID: "user-1",
	}))

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "0 failed")
	assert.NotContains(t, out.String(), "ERROR")
}

// TestRunMigrateNames_DryRun_NoRefNoGCPCopyIsSkippedNotFailed is the
// --dry-run counterpart.
func TestRunMigrateNames_DryRun_NoRefNoGCPCopyIsSkippedNotFailed(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("no-ref-no-copy-dry"), Key: "GHOST", Scope: "user", ScopeID: "user-1",
	}))

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "0 failed")
	assert.NotContains(t, out.String(), "ERROR")
}

// TestRunMigrateNames_OrphanedRefReportedNotFailed and its --dry-run
// counterpart reproduce round-3 review finding 4's other half: a DB record
// with a *stored* ref whose designated value is gone is reported as a
// visible ORPHAN, distinct from a plain skip, but still not a failure.
func TestRunMigrateNames_OrphanedRefReportedNotFailed(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	orphanedRef := "gcpsm:projects/test-project/secrets/scion-deadbeefcafe-user-aaaaaaaaaaaa-GONE"
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("orphan-cli"), Key: "GONE", Scope: "user", ScopeID: "user-1", SecretRef: orphanedRef,
	}))

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "ORPHAN")
	assert.Contains(t, out.String(), "0 failed")
}

func TestRunMigrateNames_DryRun_OrphanedRefReportedNotFailed(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	orphanedRef := "gcpsm:projects/test-project/secrets/scion-deadbeefcafe-user-aaaaaaaaaaaa-GONE"
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("orphan-cli-dry"), Key: "GONE", Scope: "user", ScopeID: "user-1", SecretRef: orphanedRef,
	}))

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true, false, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "ORPHAN")
	assert.Contains(t, out.String(), "0 failed")
}

// TestRunMigrateNames_DeleteLegacyDryRunMatchesRealRunAfterRotation
// reproduces round-3 review finding 2's dry-run/real-run parity requirement:
// once the ref designates the prefixed name, a rotation on the prefixed
// copy performed after migration must not block --delete-legacy in either
// mode -- dry-run and the real run must reach the identical conclusion from
// the identical underlying check.
func TestRunMigrateNames_DeleteLegacyDryRunMatchesRealRunAfterRotation(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("ROTATED", "user", "user-1")
	prefixedName := prefixedNameForTest("ROTATED", "user", "user-1")
	mock.seed(t, "test-project", legacyName, "v1-original")
	mock.seed(t, "test-project", prefixedName, "v1-original")
	prefixedRef := fmt.Sprintf("gcpsm:projects/test-project/secrets/%s", prefixedName)
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("delete-legacy-rotation-cli"), Key: "ROTATED", Scope: "user", ScopeID: "user-1", SecretRef: prefixedRef,
	}))
	// Rotate the prefixed copy post-migration: the legacy value is now
	// stale by design, not by mistake.
	_, err := mock.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent:  fmt.Sprintf("projects/test-project/secrets/%s", prefixedName),
		Payload: &smpb.SecretPayload{Data: []byte("v2-rotated")},
	})
	require.NoError(t, err)

	var dryOut bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true /* dryRun */, true /* deleteLegacy */, &dryOut))
	assert.Contains(t, dryOut.String(), "DELETE LEGACY", "dry-run must agree the rotated-but-authoritative-by-ref secret is safe to delete")

	var out bytes.Buffer
	require.NoError(t, runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true /* deleteLegacy */, &out))
	assert.Contains(t, out.String(), "DELETED LEGACY")
	assert.False(t, mock.has(fmt.Sprintf("projects/test-project/secrets/%s", legacyName)))
}
