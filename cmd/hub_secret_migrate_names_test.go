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
	"time"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
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

// migrateNamesCountingSMClient wraps migrateNamesMockSMClient and counts
// AccessSecretVersion calls per (suffix-trimmed) secret name, to prove
// GoogleCloudPlatform/scion#2123 review discussion_r4144099464 /
// discussion_r4144099474's fix: migrateOneCandidate's non-dry-run
// --delete-legacy path must run canDeleteLegacyName's safety check exactly
// once per candidate, not twice. Counts are kept per secret name (rather
// than as one global total) so the assertion is unaffected by the other,
// unrelated known-hub-scope-key candidates runMigrateNames always also
// checks.
type migrateNamesCountingSMClient struct {
	*migrateNamesMockSMClient
	mu          sync.Mutex
	accessCalls map[string]int
}

func (c *migrateNamesCountingSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	name := req.Name
	for _, suffix := range []string{"/versions/latest", "/versions/1"} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	c.mu.Lock()
	if c.accessCalls == nil {
		c.accessCalls = make(map[string]int)
	}
	c.accessCalls[name]++
	c.mu.Unlock()
	return c.migrateNamesMockSMClient.AccessSecretVersion(ctx, req)
}

func (c *migrateNamesCountingSMClient) callsFor(fullName string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessCalls[fullName]
}

// TestRunMigrateNames_DeleteLegacySingleSafetyCheckPerCandidate reproduces
// GoogleCloudPlatform/scion#2123 review discussion_r4144099464 /
// discussion_r4144099474: for a candidate whose DB ref already designates
// the prefixed name (nothing left to copy or repair, so migrateOneCandidate's
// delete-legacy step is the only GCP SM work this run does for it),
// canDeleteLegacyName's two reads (the legacy name, then the prefixed name,
// to confirm it's readable) must each happen exactly once. Before the fix,
// migrateOneCandidate ran the identical check twice -- once via
// PlanLegacyDeletion, again via DeleteLegacySecretName -- doubling each to
// two reads (four total).
func TestRunMigrateNames_DeleteLegacySingleSafetyCheckPerCandidate(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := &migrateNamesCountingSMClient{migrateNamesMockSMClient: newMigrateNamesMockSMClient()}
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	prefixedFull := fmt.Sprintf("projects/test-project/secrets/%s", prefixedName)
	mock.seed(t, "test-project", legacyName, "v")
	mock.seed(t, "test-project", prefixedName, "v")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("single-safety-check"), Key: "API_KEY", Scope: "user", ScopeID: "user-1",
		SecretRef: "gcpsm:" + prefixedFull,
	}))

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true /* deleteLegacy */, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "DELETED LEGACY")
	assert.NotContains(t, out.String(), "MIGRATED", "the ref already designated the prefixed name; nothing to copy")

	assert.Equal(t, 1, mock.callsFor(legacyFull), "canDeleteLegacyName must read the legacy name exactly once per candidate; a doubled safety check would read it twice")
	assert.Equal(t, 1, mock.callsFor(prefixedFull), "canDeleteLegacyName must read the prefixed name exactly once per candidate; a doubled safety check would read it twice")

	assert.False(t, mock.has(legacyFull), "expected legacy secret to be deleted")
	assert.True(t, mock.has(prefixedFull), "expected prefixed copy to survive")
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

// TestHubSecretMigrateNamesCmd_HasConfigFlag reproduces round-4 review
// Consider 3: migrate-names must accept --config (mirroring `server start`),
// so an operator whose hub runs with a non-default settings file can make
// this command resolve the identical hub ID the server does.
func TestHubSecretMigrateNamesCmd_HasConfigFlag(t *testing.T) {
	f := hubSecretMigrateNamesCmd.Flags().Lookup("config")
	require.NotNil(t, f, "migrate-names must have a --config flag")
	assert.Equal(t, "c", f.Shorthand)
	assert.Equal(t, "", f.DefValue)
}

// --- GoogleCloudPlatform/scion#2123 review fixes (gemini-code-assist) ---

// TestMigrateNamesTimeoutContext_DerivesFromCommandContext reproduces
// discussion_r4144099499: the command's timeout context must be derived from
// cmd.Context(), not context.Background(), so cancelling the command (an
// operator's Ctrl+C in production, or a test/caller cancellation here) also
// cancels the derived context instead of running until --timeout fires on
// its own. Exercised directly against migrateNamesTimeoutContext rather than
// the full RunE, since runSecretMigrateNames itself builds a real GCP SM
// client and reads global settings, neither of which this constraint's own
// "no real systems" rule allows a test to touch.
func TestMigrateNamesTimeoutContext_DerivesFromCommandContext(t *testing.T) {
	cmd := &cobra.Command{}
	parentCtx, cancelParent := context.WithCancel(context.Background())
	cmd.SetContext(parentCtx)

	ctx, cancel := migrateNamesTimeoutContext(cmd, time.Minute)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("expected the derived context to still be open before the parent is canceled")
	default:
	}

	cancelParent()

	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("expected canceling cmd.Context() to cancel the derived timeout context; context.Background() would never do this")
	}
}

// TestPrintMigrateNamesHubID_WritesToCommandOutNotStdout reproduces
// discussion_r4144099512: the "Using hub ID" line must go through
// cmd.OutOrStdout(), not a direct fmt.Printf/os.Stdout write, so the
// command's output can be captured (e.g. via cmd.SetOut in a test, or when
// cobra redirects it for a subcommand/completion context) instead of always
// landing on the process's real stdout regardless of how the command is
// invoked.
func TestPrintMigrateNamesHubID_WritesToCommandOutNotStdout(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	printMigrateNamesHubID(cmd, "hub-1", "scion-abc123def456-")

	assert.Equal(t, "Using hub ID: hub-1 (prefix: scion-abc123def456-)\n", buf.String())
}

// TestStripDSNQueryParam reproduces round-4 review Consider 4: a table test
// for the previously-untested stripDSNQueryParam.
func TestStripDSNQueryParam(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		key  string
		want string
	}{
		{"no query at all", "file:/tmp/x.db", "mode", "file:/tmp/x.db"},
		{"key alone", "file:/tmp/x.db?mode", "mode", "file:/tmp/x.db"},
		{"key with value among others", "file:/tmp/x.db?mode=rwc&cache=shared", "mode", "file:/tmp/x.db?cache=shared"},
		{"key with value, only param", "file:/tmp/x.db?mode=rwc", "mode", "file:/tmp/x.db"},
		{"similarly-prefixed key is kept", "file:/tmp/x.db?modex=1", "mode", "file:/tmp/x.db?modex=1"},
		{"key not present", "file:/tmp/x.db?cache=shared", "mode", "file:/tmp/x.db?cache=shared"},
		{"key appears twice", "file:/tmp/x.db?mode=ro&cache=shared&mode=rwc", "mode", "file:/tmp/x.db?cache=shared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, stripDSNQueryParam(c.dsn, c.key))
		})
	}
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

// TestRunMigrateNames_DryRunPlansResyncThenDelete pins the plan for a
// candidate that needs both a resync (the prefixed copy is stale relative to
// the ref-designated value) and a delete-legacy in the same pass. Since
// round-3, the correct plan is a combined "WOULD RESYNC AND DELETE LEGACY"
// line, not a bare RESYNC with no delete: a real run performs the resync
// first (making the ref-designated value and the prefixed copy match) and
// then deletes the now-safely-superseded legacy copy in the same invocation
// — see planMigrateNamesCandidate's actionPlanned handling. An earlier
// version of this test only asserted the *absence* of a differently-worded
// string ("WOULD DELETE LEGACY  API_KEY", which the real output never
// contains verbatim either way since it's always combined with "AND"), which
// never actually distinguished a correct plan from a wrong one (round-4
// review nit 10).
func TestRunMigrateNames_DryRunPlansResyncThenDelete(t *testing.T) {
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
	assert.Contains(t, out.String(), "WOULD RESYNC AND DELETE LEGACY  API_KEY", "a real run resyncs the stale prefixed copy first, then safely deletes the now-superseded legacy copy in the same invocation")

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

// =============================================================================
// Round 4 review regression tests (ptone/scion#2152 PR 2171, sp-rev-4.md)
// =============================================================================

// sprev4DenyNonPrefixed denies AccessSecretVersion on every name outside
// this hub's prefix: a hub whose SA holds only the new-prefix conditioned
// grant (fresh least-privilege hub, or after the legacy grant was removed
// per ptone/scion#2180).
type sprev4DenyNonPrefixed struct {
	*migrateNamesMockSMClient
	prefix string
}

func (d *sprev4DenyNonPrefixed) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if !strings.Contains(req.Name, "/secrets/"+d.prefix) {
		return nil, status.Error(codes.PermissionDenied, "denied by IAM condition")
	}
	return d.migrateNamesMockSMClient.AccessSecretVersion(ctx, req)
}

// TestSPREV4_DeleteLegacyDryRunRealRunParity_NoRecordLegacyDenied reproduces
// round-4 review finding 2: for the known hub-scope keys with no DB record,
// --dry-run --delete-legacy used to skip the delete check when the legacy
// name was permission-denied, but the real --delete-legacy run had no such
// guard and failed on the same signal. Both now route through
// canDeleteLegacyName's shared legacyReadErrIsFatal classification and must
// agree.
func TestSPREV4_DeleteLegacyDryRunRealRunParity_NoRecordLegacyDenied(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	deny := &sprev4DenyNonPrefixed{mock, secret.SecretNamePrefixForHubID(migrateNamesTestHubID)}
	backend := migrateNamesTestBackend(t, db, deny, migrateNamesTestHubID)

	var dry bytes.Buffer
	dryErr := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true, true, &dry)
	t.Logf("dry-run (err=%v):\n%s", dryErr, dry.String())
	assert.NoError(t, dryErr)
	assert.Contains(t, dry.String(), "0 failed")

	var real bytes.Buffer
	realErr := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true, &real)
	t.Logf("real run (err=%v):\n%s", realErr, real.String())
	assert.NoError(t, realErr)
	assert.Contains(t, real.String(), "0 failed")

	assert.Equal(t, dryErr == nil, realErr == nil,
		"BUG: --dry-run --delete-legacy and --delete-legacy disagree on success for the same state")
}

// sprev4FailedPreconditionOnLegacy simulates a legacy secret whose container
// exists but whose latest version has been disabled/destroyed: GCP SM
// returns FailedPrecondition, not NotFound, for that state.
type sprev4FailedPreconditionOnLegacy struct {
	*migrateNamesMockSMClient
	legacyFullName string
}

func (d *sprev4FailedPreconditionOnLegacy) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if strings.HasPrefix(req.Name, d.legacyFullName+"/versions/") {
		return nil, status.Error(codes.FailedPrecondition, "version disabled")
	}
	return d.migrateNamesMockSMClient.AccessSecretVersion(ctx, req)
}

// TestSPREV4_DeleteLegacyDryRunRealRunParity_NoRecordLegacyFailedPrecondition
// is the FailedPrecondition variant of the finding-2 repro above: a legacy
// secret whose latest version is disabled must also get the same outcome in
// both --dry-run and the real run.
func TestSPREV4_DeleteLegacyDryRunRealRunParity_NoRecordLegacyFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()

	legacyName := legacyNameForTest(hub.SecretKeyUserSigningKey, "hub", migrateNamesTestHubID)
	legacyFull := fmt.Sprintf("projects/test-project/secrets/%s", legacyName)
	mock.seed(t, "test-project", legacyName, "key-material")

	fp := &sprev4FailedPreconditionOnLegacy{mock, legacyFull}
	backend := migrateNamesTestBackend(t, db, fp, migrateNamesTestHubID)

	var dry bytes.Buffer
	dryErr := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true, true, &dry)
	t.Logf("dry-run (err=%v):\n%s", dryErr, dry.String())
	assert.NoError(t, dryErr)

	var real bytes.Buffer
	realErr := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true, &real)
	t.Logf("real run (err=%v):\n%s", realErr, real.String())
	assert.NoError(t, realErr)

	assert.Equal(t, dryErr == nil, realErr == nil,
		"BUG: --dry-run --delete-legacy and --delete-legacy disagree on success for a FailedPrecondition legacy secret")
}

// =============================================================================
// Round 5 review regression tests (ptone/scion#2152 PR 2171, sp-rev-5.md)
// =============================================================================

// TestSPREV5_DeleteLegacyFailsForeverOnOrphanedRef reproduces round-5 review
// finding 1, a round-4 regression: canDeleteLegacyName refused as soon as
// hasRecord && !refIsPrefixed held, before ever reading the legacy name --
// so an ORPHAN record (a stored ref whose target is gone) failed
// --delete-legacy on every run, in both modes, contradicting the help text,
// design doc, and PR body, which all say ORPHAN is a skip, not a failure.
func TestSPREV5_DeleteLegacyFailsForeverOnOrphanedRef(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	orphanedRef := "gcpsm:projects/test-project/secrets/scion-deadbeefcafe-user-aaaaaaaaaaaa-GONE"
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("sprev5-orphan-dl"), Key: "GONE", Scope: "user", ScopeID: "user-1", SecretRef: orphanedRef,
	}))

	for _, dry := range []bool{true, false} {
		var out bytes.Buffer
		err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, dry, true, &out)
		t.Logf("dryRun=%v err=%v\n%s", dry, err, out.String())
		assert.NoError(t, err, "BUG: ORPHAN record makes --delete-legacy fail (dryRun=%v)", dry)
		assert.Contains(t, out.String(), "0 failed")
	}
}

// TestSPREV5_DeleteLegacyFailsForeverOnNoRefNoValueRecord is the no-ref/
// no-legacy-value counterpart: "truly nothing to migrate" (a silent skip
// without --delete-legacy) must also not become a permanent failure with it.
func TestSPREV5_DeleteLegacyFailsForeverOnNoRefNoValueRecord(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)

	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("sprev5-noref-dl"), Key: "GHOST", Scope: "user", ScopeID: "user-1",
	}))

	for _, dry := range []bool{true, false} {
		var out bytes.Buffer
		err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, dry, true, &out)
		t.Logf("dryRun=%v err=%v\n%s", dry, err, out.String())
		assert.NoError(t, err, "BUG: no-ref/no-value record makes --delete-legacy fail (dryRun=%v)", dry)
		assert.Contains(t, out.String(), "0 failed")
	}
}

// sprev5OnceAtCallAccessSMClient fires hook exactly once, immediately before
// the call-th call to AccessSecretVersion is delegated to the underlying
// mock. Mirrors pkg/secret's onceAtCallAccessSMClient (unexported there, so
// duplicated minimally here for the cmd-level end-to-end reproduction).
type sprev5OnceAtCallAccessSMClient struct {
	*migrateNamesMockSMClient
	mu    sync.Mutex
	calls int
	call  int
	hook  func()
	fired bool
}

func (c *sprev5OnceAtCallAccessSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	c.mu.Lock()
	c.calls++
	fire := !c.fired && c.calls == c.call
	if fire {
		c.fired = true
	}
	c.mu.Unlock()
	if fire {
		c.hook()
	}
	return c.migrateNamesMockSMClient.AccessSecretVersion(ctx, req)
}

// sprev5AddHookSMClient fires hook once, immediately after the first
// AddSecretVersion whose parent contains match has been applied. Mirrors
// pkg/secret's addHookSMClient.
type sprev5AddHookSMClient struct {
	*migrateNamesMockSMClient
	match string
	once  sync.Once
	hook  func()
}

func (c *sprev5AddHookSMClient) AddSecretVersion(ctx context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	v, err := c.migrateNamesMockSMClient.AddSecretVersion(ctx, req)
	if strings.Contains(req.Parent, c.match) {
		c.once.Do(c.hook)
	}
	return v, err
}

// sprev5CompositeSMClient composes independent access/add hooks so a single
// interleaving can inject a concurrent event at two different points in
// planOrRepairRefAttempt's read/write sequence.
type sprev5CompositeSMClient struct {
	*migrateNamesMockSMClient
	access *sprev5OnceAtCallAccessSMClient
	add    *sprev5AddHookSMClient
}

func (c *sprev5CompositeSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return c.access.AccessSecretVersion(ctx, req)
}

func (c *sprev5CompositeSMClient) AddSecretVersion(ctx context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	return c.add.AddSecretVersion(ctx, req)
}

// TestSPREV5_MigrateNames_ReportsConflictNotSilence reproduces round-5 review
// finding 2's "make it not silent" requirement end to end through
// migrate-names: a concurrent new-binary Set() racing with this run's own
// write to the prefixed name must be reported as a distinct CONFLICT outcome
// with a non-zero exit, not silently accepted.
func TestSPREV5_MigrateNames_ReportsConflictNotSilence(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()

	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	prefixedRef := "gcpsm:projects/test-project/secrets/" + prefixedName
	mock.seed(t, "test-project", legacyName, "v1")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("sprev5-conflict-cli"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: legacyRef,
	}))

	race := &sprev5OnceAtCallAccessSMClient{migrateNamesMockSMClient: mock, call: 2}
	race.hook = func() {
		mock.seed(t, "test-project", prefixedName, "v2-new-binary-set")
	}
	hooked := &sprev5AddHookSMClient{migrateNamesMockSMClient: mock, match: prefixedName}
	hooked.hook = func() {
		_, err := db.UpsertSecret(ctx, &store.Secret{
			Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: prefixedRef,
		})
		require.NoError(t, err)
	}
	composite := &sprev5CompositeSMClient{migrateNamesMockSMClient: mock, access: race, add: hooked}
	backend := migrateNamesTestBackend(t, db, composite, migrateNamesTestHubID)

	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	t.Logf("err=%v\n%s", err, out.String())
	assert.Error(t, err, "a CONFLICT must produce a non-zero exit")
	assert.Contains(t, out.String(), "CONFLICT")
	assert.Contains(t, out.String(), "API_KEY")
}

// sprev8TrueConflict sets up a TRUE conflict (a concurrent new-binary Set
// repoints the ref to the prefixed name while this run is copying) exactly as
// TestSPREV5_MigrateNames_ReportsConflictNotSilence does, and runs the first
// pass with the given deleteLegacy flag. It returns the plain backend for the
// re-run.
func sprev8TrueConflict(t *testing.T, firstDeleteLegacy bool) (context.Context, store.Store, *migrateNamesMockSMClient) {
	ctx := context.Background()
	db := newTestStore(t)
	mock := newMigrateNamesMockSMClient()
	legacyName := legacyNameForTest("API_KEY", "user", "user-1")
	legacyRef := "gcpsm:projects/test-project/secrets/" + legacyName
	prefixedName := prefixedNameForTest("API_KEY", "user", "user-1")
	prefixedRef := "gcpsm:projects/test-project/secrets/" + prefixedName
	mock.seed(t, "test-project", legacyName, "v1")
	require.NoError(t, db.CreateSecret(ctx, &store.Secret{
		ID: tid("sprev8-true-conflict"), Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: legacyRef,
	}))
	race := &sprev5OnceAtCallAccessSMClient{migrateNamesMockSMClient: mock, call: 2}
	race.hook = func() { mock.seed(t, "test-project", prefixedName, "v2-new-binary-set") }
	hooked := &sprev5AddHookSMClient{migrateNamesMockSMClient: mock, match: prefixedName}
	hooked.hook = func() {
		_, err := db.UpsertSecret(ctx, &store.Secret{Key: "API_KEY", Scope: "user", ScopeID: "user-1", SecretRef: prefixedRef})
		require.NoError(t, err)
	}
	composite := &sprev5CompositeSMClient{migrateNamesMockSMClient: mock, access: race, add: hooked}
	var out bytes.Buffer
	err := runMigrateNames(ctx, migrateNamesTestBackend(t, db, composite, migrateNamesTestHubID), db, migrateNamesTestHubID, false, firstDeleteLegacy, &out)
	t.Logf("first run err=%v\n%s", err, out.String())
	require.Error(t, err)
	require.Contains(t, out.String(), "CONFLICT")
	return ctx, db, mock
}

// TestSPREV8_TrueConflictRerunWithDeleteLegacyReportsAction pins round-8
// review Required 1: the CONFLICT guidance's diagnostic re-run must exclude
// --delete-legacy. If an operator instead re-runs the exact CONFLICT-producing
// command line -- which routinely includes --delete-legacy, since that's the
// documented final pass -- for a TRUE conflict the ref already designates the
// prefixed name, so --delete-legacy deletes the legacy copy and reports
// DELETED LEGACY regardless of whether the conflict was ever resolved. That
// is why DELETED LEGACY/WOULD DELETE LEGACY is explicitly carved out as NOT
// the false-positive signal (MIGRATED/RESYNCED/REPAIRED REF is) in the
// CONFLICT line, --help, .design section 7, and the PR body. This test
// asserts the current, now-documented behavior -- not a defect to fix here --
// so the text and the behavior stay pinned together.
func TestSPREV8_TrueConflictRerunWithDeleteLegacyReportsAction(t *testing.T) {
	ctx, db, mock := sprev8TrueConflict(t, true)
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)
	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, true, &out)
	t.Logf("re-run err=%v\n%s", err, out.String())
	require.NoError(t, err, "a re-run with --delete-legacy reports the delete as an action, not a failure")
	assert.Contains(t, out.String(), "DELETED LEGACY", "a re-run with --delete-legacy reports an action for a TRUE conflict too")
	sv, gerr := backend.Get(ctx, "API_KEY", "user", "user-1")
	require.NoError(t, gerr)
	assert.Equal(t, "v1", sv.Value, "the hub keeps serving the stale value; the reported action did not resolve the conflict")
}

// TestSPREV8_TrueConflictDryRunRerunWithDeleteLegacyReportsAction is the
// --dry-run counterpart: the plan for a TRUE conflict re-run with
// --delete-legacy also reports WOULD DELETE LEGACY, for the same reason.
func TestSPREV8_TrueConflictDryRunRerunWithDeleteLegacyReportsAction(t *testing.T) {
	ctx, db, mock := sprev8TrueConflict(t, true)
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)
	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, true, true, &out)
	t.Logf("dry re-run err=%v\n%s", err, out.String())
	require.NoError(t, err)
	assert.Contains(t, out.String(), "WOULD DELETE LEGACY", "a --dry-run --delete-legacy re-run reports the planned action for a TRUE conflict too")
}

// TestSPREV8_TrueConflictPlainRerunReportsNothing is the control: a plain
// re-run (without --delete-legacy) of a TRUE conflict reports nothing for the
// secret, matching the CONFLICT guidance's diagnostic-re-run signal.
func TestSPREV8_TrueConflictPlainRerunReportsNothing(t *testing.T) {
	ctx, db, mock := sprev8TrueConflict(t, false)
	backend := migrateNamesTestBackend(t, db, mock, migrateNamesTestHubID)
	var out bytes.Buffer
	err := runMigrateNames(ctx, backend, db, migrateNamesTestHubID, false, false, &out)
	t.Logf("re-run err=%v\n%s", err, out.String())
	require.NoError(t, err)
	for _, l := range []string{"MIGRATED", "RESYNCED", "REPAIRED REF", "DELETED LEGACY"} {
		require.NotContains(t, out.String(), l)
	}
}
