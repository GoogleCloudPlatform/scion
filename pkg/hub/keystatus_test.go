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
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Key status (ptone/scion#1530 Phase 6 T0): the admin endpoint and the
// local builder over real SQLite stores, real hub servers and synthetic
// keyring files loaded with the real loader. Every secret here is a
// synthetic fixture.

const keyStatusPath = "/api/v1/admin/keys/status"

// Seeded secret record IDs (secret IDs are UUIDs).
const (
	ksRecordLegacy     = "6b0f2f1e-5a4b-4c3d-8e2f-00000000a001"
	ksRecordReadable   = "6b0f2f1e-5a4b-4c3d-8e2f-00000000a002"
	ksRecordUnreadable = "6b0f2f1e-5a4b-4c3d-8e2f-00000000a003"
)

// ksFile writes doc as a keyring file with mode 0400 and loads it with the
// real loader.
func ksFile(t *testing.T, doc map[string]any) *keyring.Keyring {
	t.Helper()
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "keyring.json")
	require.NoError(t, os.WriteFile(path, data, 0o400))
	k, err := keyring.LoadFile(path)
	require.NoError(t, err)
	return k
}

func ksEntry(id, secret string, signUntil, verifyUntil *time.Time) map[string]any {
	m := map[string]any{"id": id, "secret": secret, "sign_until": nil, "verify_until": nil}
	if signUntil != nil {
		m["sign_until"] = signUntil.Format(time.RFC3339)
		m["verify_until"] = verifyUntil.Format(time.RFC3339)
	}
	return m
}

// ksPilotDoc is a keyring file this release accepts (CheckSessionPilot):
// the legacy entry, emit legacy, the imported legacy encryption key.
func ksPilotDoc(revision, active string, keys []any, legacyReads []any) map[string]any {
	return map[string]any{
		"schema":   1,
		"revision": revision,
		"signing": map[string]any{
			"active": active, "emit": "legacy", "keys": keys, "legacy_read_ids": legacyReads,
		},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(krLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
}

// ksKeyringA: replica A, active legacy, gen1 staged without deadlines.
func ksKeyringA(t *testing.T) *keyring.Keyring {
	t.Helper()
	k := ksFile(t, ksPilotDoc("rev-a", "legacy",
		[]any{ksEntry("legacy", krLegacySecret, nil, nil), ksEntry("gen1", krGen1Secret, nil, nil)},
		[]any{"legacy", "gen1"}))
	require.NoError(t, CheckKeyringFile(k))
	return k
}

// ksKeyringB: replica B, active gen1, legacy outgoing with deadlines.
func ksKeyringB(t *testing.T) *keyring.Keyring {
	t.Helper()
	k := ksFile(t, ksPilotDoc("rev-b", "gen1",
		[]any{ksEntry("legacy", krLegacySecret, krTime(time.Hour), krTime(48*time.Hour)), ksEntry("gen1", krGen1Secret, nil, nil)},
		[]any{"gen1", "legacy"}))
	require.NoError(t, CheckKeyringFile(k))
	return k
}

// ksSetExperiment turns server.secret_rotation on or off for srv.
func ksSetExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.SecretRotation {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.mu.Lock()
	srv.experiments = reg
	srv.mu.Unlock()
	require.Equal(t, on, srv.experimentEnabled(experiments.SecretRotation))
}

// ksServer builds a real hub over st with keyring k (nil: legacy mode over
// krLegacySecret), the clock clk and server.secret_rotation on.
func ksServer(t *testing.T, st store.Store, k *keyring.Keyring, clk *krClock) *Server {
	t.Helper()
	cfg := testServerConfig()
	cfg.HubID = encTestHubID
	cfg.Keyring = k
	cfg.KeyringConfigured = k != nil
	cfg.SharedSigningSecret = krLegacySecret
	cfg.KeyringNow = clk.now
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err)
	srv.SetHubID(encTestHubID)
	waitUserScopedDataSweep(t, srv)
	ksSetExperiment(t, srv, true)
	return srv
}

// ksRestart shuts srv down and starts a fresh instance with the same
// configuration. The endpoint reports the startup audit, which runs before
// New writes its backups; after a restart it covers every row a first
// start wrote, so it can be compared with a later local audit.
func ksRestart(t *testing.T, srv *Server, st store.Store, k *keyring.Keyring, clk *krClock) *Server {
	t.Helper()
	require.NoError(t, srv.Shutdown(context.Background()))
	return ksServer(t, st, k, clk)
}

// ksGet fetches the report as the dev (unscoped platform admin) identity.
func ksGet(t *testing.T, srv *Server) (KeyStatusReport, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, keyStatusPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var r KeyStatusReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
	return r, rec.Body.String()
}

// withoutInstance clears what legitimately differs between the endpoint
// and the CLI for the same file and store: the instance identity and which
// audit ran when (the endpoint's startup audit, the CLI's fresh one).
func withoutInstance(r KeyStatusReport) KeyStatusReport {
	r.Instance = KeyStatusInstance{}
	r.Audit.Scope = ""
	r.Audit.CompletedAt = nil
	return r
}

func ksJSON(t *testing.T, r KeyStatusReport) string {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	return string(b)
}

// localFor builds the CLI's report for k over st, with the cipher a Hub
// with that configuration builds.
func localFor(t *testing.T, st store.Store, k *keyring.Keyring, now time.Time) KeyStatusReport {
	t.Helper()
	var c *secret.ValueCipher
	if k != nil {
		var err error
		c, err = secret.NewValueCipher(k)
		require.NoError(t, err)
	} else {
		c = secret.NewLegacyValueCipher(krLegacySecret)
	}
	r, err := LocalKeyStatus(context.Background(), k, c, st, now)
	require.NoError(t, err)
	return r
}

// The golden report: the full field set and values of a legacy-mode report
// with no store, pinned byte for byte as JSON.
func TestKeyStatus_GoldenLegacyReport(t *testing.T) {
	r, err := buildKeyStatus(context.Background(), keyStatusInputs{
		instance:   KeyStatusInstance{ID: "inst-a", Hostname: "host-a", Source: KeyStatusSourceEndpoint},
		auditScope: KeyStatusAuditStartup,
		now:        krT0,
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{
	  "report_version": 1,
	  "instance": {"id": "inst-a", "hostname": "host-a", "source": "endpoint"},
	  "keyring": {"source": "legacy", "revision": null},
	  "signing": {
	    "active_id": "legacy", "accepted_ids": ["legacy"], "emit": "legacy",
	    "legacy_read_ids": ["legacy"],
	    "generations": [{"id": "legacy", "sign_until": null, "verify_until": null}]
	  },
	  "encryption": {
	    "active_id": "legacy", "accepted_ids": ["legacy"], "write_format": "v1",
	    "legacy_v1_read_ids": ["legacy"]
	  },
	  "reader_formats": {"signing": ["legacy", "versioned"], "encryption": ["v1", "v2"]},
	  "policy": null,
	  "migration": null,
	  "audit": {
	    "ran": false, "scope": "startup", "completed_at": null, "scanned": 0, "reference_only": 0, "readable": 0, "unreadable": 0,
	    "by_format": {}, "by_kid": {}, "unreadable_by_kind": {}, "backup_unreadable": 0
	  },
	  "readiness": {"reasons": []},
	  "material_digest": null,
	  "material_digest_omitted_reason": "pending_legacy_independence"
	}`, ksJSON(t, r))
}

// jsonPaths returns every object key path in v ("a.b", arrays as "a[]").
func jsonPaths(prefix string, v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			jsonPaths(p, c, out)
		}
	case []any:
		for _, c := range x {
			jsonPaths(prefix+"[]", c, out)
		}
	}
}

// The field set of a fully populated report (file keyring, policy,
// migration, audit) is pinned: a field added, renamed or removed fails
// here, and must bump report_version when it changes meaning.
func TestKeyStatus_GoldenFieldSet(t *testing.T) {
	ctx := context.Background()
	st := encStore(t)
	k := ksVersionedKeyring(t, "k1")
	c, err := secret.NewValueCipher(k)
	require.NoError(t, err)
	ksSeedPolicy(t, st, "k1")
	r, err := LocalKeyStatus(ctx, k, c, st, krT0)
	require.NoError(t, err)

	var doc any
	require.NoError(t, json.Unmarshal([]byte(ksJSON(t, r)), &doc))
	got := map[string]bool{}
	jsonPaths("", doc, got)
	// The audit maps are keyed by data (formats, key IDs), not schema.
	var paths []string
	for p := range got {
		if strings.HasPrefix(p, "audit.by_format.") || strings.HasPrefix(p, "audit.by_kid.") || strings.HasPrefix(p, "audit.unreadable_by_kind.") {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	assert.Equal(t, []string{
		"audit", "audit.backup_unreadable", "audit.by_format", "audit.by_kid", "audit.completed_at", "audit.ran",
		"audit.readable", "audit.reference_only", "audit.scanned", "audit.scope", "audit.unreadable", "audit.unreadable_by_kind",
		"encryption", "encryption.accepted_ids", "encryption.active_id", "encryption.legacy_v1_read_ids", "encryption.write_format",
		"instance", "instance.hostname", "instance.id", "instance.source",
		"keyring", "keyring.revision", "keyring.source",
		"material_digest", "material_digest_omitted_reason",
		"migration", "migration.id", "migration.state", "migration.target_kid",
		"policy", "policy.allowed_write_kids", "policy.generation", "policy.min_reader_version", "policy.required_read_kids", "policy.state",
		"reader_formats", "reader_formats.encryption", "reader_formats.signing",
		"readiness", "readiness.reasons",
		"report_version",
		"signing", "signing.accepted_ids", "signing.active_id", "signing.emit",
		"signing.generations", "signing.generations[].id", "signing.generations[].sign_until", "signing.generations[].verify_until",
		"signing.legacy_read_ids",
	}, paths)
}

// ksVersionedKeyring is a parsed keyring with a random (not derived)
// encryption key written in v2 under kid. This release refuses it at
// startup; the local builder and the policy check accept it.
func ksVersionedKeyring(t *testing.T, kid string) *keyring.Keyring {
	t.Helper()
	b64, _ := randomB64Key(t)
	return ksFile(t, map[string]any{
		"schema":   1,
		"revision": "rev-v2",
		"signing": map[string]any{
			"active": "gen1", "emit": "versioned",
			"keys":            []any{ksEntry("legacy", krLegacySecret, nil, nil), ksEntry("gen1", krGen1Secret, nil, nil)},
			"legacy_read_ids": []any{"legacy"},
		},
		"encryption": map[string]any{
			"active": kid, "write_format": "v2",
			"keys":               []any{map[string]any{"id": kid, "key_b64": b64}},
			"legacy_v1_read_ids": []any{},
		},
	})
}

// ksSeedPolicy stores a rewrapping policy writing kid, bound to a running
// migration.
func ksSeedPolicy(t *testing.T, st store.Store, kid string) {
	t.Helper()
	ctx := context.Background()
	m := &store.EncryptionMigration{ID: "11111111-2222-3333-4444-555555555555", SourceGeneration: 1, TargetKID: kid,
		State: store.EncryptionMigrationRunning, Created: krT0, Updated: krT0}
	require.NoError(t, st.CreateEncryptionMigration(ctx, m))
	applied, err := st.UpdateEncryptionPolicy(ctx, 0, store.EncryptionPolicy{
		Generation: 1, MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{kid}, RequiredReadKIDs: []string{kid},
		MigrationID: m.ID, State: store.EncryptionPolicyRewrapping,
	})
	require.NoError(t, err)
	require.True(t, applied)
}

// Policy and migration state come from the store; a cipher the policy
// refuses is a readiness reason, not an error.
func TestKeyStatus_PolicyAndMigration(t *testing.T) {
	ctx := context.Background()
	st := encStore(t)
	ksSeedPolicy(t, st, "k1")

	k := ksVersionedKeyring(t, "k1")
	c, err := secret.NewValueCipher(k)
	require.NoError(t, err)
	r, err := LocalKeyStatus(ctx, k, c, st, krT0)
	require.NoError(t, err)
	require.NotNil(t, r.Policy)
	assert.Equal(t, KeyStatusPolicy{Generation: 1, State: "rewrapping", MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{"k1"}, RequiredReadKIDs: []string{"k1"}}, *r.Policy)
	require.NotNil(t, r.Migration)
	assert.Equal(t, KeyStatusMigration{ID: "11111111-2222-3333-4444-555555555555", State: "running", TargetKID: "k1"}, *r.Migration)
	assert.Empty(t, r.Readiness.Reasons)

	// A keyring writing under another key: the policy refuses it.
	other := ksVersionedKeyring(t, "k2")
	oc, err := secret.NewValueCipher(other)
	require.NoError(t, err)
	r, err = LocalKeyStatus(ctx, other, oc, st, krT0)
	require.NoError(t, err)
	assert.Equal(t, []string{readyReasonEncryptionPolicyMismatch}, r.Readiness.Reasons)
}

// Legacy mode (no keyring file) reports the legacy generation with null
// deadlines, from the endpoint and the local builder alike.
func TestKeyStatus_LegacyModeEndpoint(t *testing.T) {
	st := encStore(t)
	clk := &krClock{t: krT0}
	// A legacy-mode Hub derives its signing keys from the shared secret and
	// stores no backups, so seed one user secret for the audit to read.
	srv := ksServer(t, st, nil, clk)
	sealed, err := secret.NewLegacyValueCipher(krLegacySecret).Encrypt("ks-synthetic-legacy-payload")
	require.NoError(t, err)
	_, err = st.UpsertSecret(context.Background(), &store.Secret{ID: ksRecordLegacy, Key: "KS_RECORD_KEY_LEGACY", EncryptedValue: sealed,
		Scope: store.ScopeUser, ScopeID: "ks-user", SecretType: store.SecretTypeEnvironment})
	require.NoError(t, err)
	srv = ksRestart(t, srv, st, nil, clk)

	r, _ := ksGet(t, srv)
	assert.Equal(t, KeyStatusReportVersion, r.ReportVersion)
	assert.Equal(t, KeyStatusInstance{ID: srv.InstanceID(), Hostname: r.Instance.Hostname, Source: KeyStatusSourceEndpoint}, r.Instance)
	assert.Equal(t, KeyStatusKeyring{Source: "legacy"}, r.Keyring)
	assert.Equal(t, []KeyStatusGeneration{{ID: "legacy"}}, r.Signing.Generations)
	assert.Equal(t, "legacy", r.Signing.ActiveID)
	assert.Equal(t, []string{"legacy"}, r.Signing.LegacyReadIDs)
	assert.Equal(t, "v1", r.Encryption.WriteFormat)
	assert.True(t, r.Audit.Ran)
	assert.Equal(t, KeyStatusAuditStartup, r.Audit.Scope)
	require.NotNil(t, r.Audit.CompletedAt)
	assert.Equal(t, 1, r.Audit.Scanned, "the startup audit saw the seeded row")
	assert.Equal(t, 1, r.Audit.Readable)
	assert.Equal(t, map[string]int{"v1": 1}, r.Audit.ByFormat)
	assert.Nil(t, r.MaterialDigest)
	assert.Equal(t, MaterialDigestOmittedPendingLegacyIndependence, r.MaterialDigestOmittedReason)

	// The CLI's report for the same configuration and store matches field
	// for field.
	assert.Equal(t, ksJSON(t, withoutInstance(r)), ksJSON(t, withoutInstance(localFor(t, st, nil, krT0))))
}

// Two real instances on one store with different keyring files each
// report their own file; the CLI's report for each file matches that
// instance's endpoint field for field.
func TestKeyStatus_TwoInstancesReportTheirOwnFiles(t *testing.T) {
	st := encStore(t)
	clk := &krClock{t: krT0}
	ka, kb := ksKeyringA(t), ksKeyringB(t)
	a := ksServer(t, st, ka, clk)
	b := ksServer(t, st, kb, clk)
	a = ksRestart(t, a, st, ka, clk)
	b = ksRestart(t, b, st, kb, clk)

	ra, _ := ksGet(t, a)
	rb, _ := ksGet(t, b)
	assert.NotEqual(t, ra.Instance.ID, rb.Instance.ID)
	assert.NotEqual(t, ksJSON(t, withoutInstance(ra)), ksJSON(t, withoutInstance(rb)), "different files give different reports")

	revA, revB := "rev-a", "rev-b"
	assert.Equal(t, KeyStatusKeyring{Source: "file", Revision: &revA}, ra.Keyring)
	assert.Equal(t, KeyStatusKeyring{Source: "file", Revision: &revB}, rb.Keyring)
	assert.Equal(t, KeyStatusSigning{
		ActiveID: "legacy", AcceptedIDs: []string{"legacy", "gen1"}, Emit: "legacy",
		LegacyReadIDs: []string{"legacy", "gen1"},
		Generations:   []KeyStatusGeneration{{ID: "legacy"}, {ID: "gen1"}},
	}, ra.Signing)
	assert.Equal(t, KeyStatusSigning{
		ActiveID: "gen1", AcceptedIDs: []string{"gen1", "legacy"}, Emit: "legacy",
		LegacyReadIDs: []string{"gen1", "legacy"},
		Generations:   []KeyStatusGeneration{{ID: "gen1"}, {ID: "legacy", SignUntil: krTime(time.Hour), VerifyUntil: krTime(48 * time.Hour)}},
	}, rb.Signing)
	assert.Equal(t, KeyStatusEncryption{ActiveID: "legacy", AcceptedIDs: []string{"legacy"}, WriteFormat: "v1", LegacyV1ReadIDs: []string{"legacy"}}, ra.Encryption)
	assert.Empty(t, ra.Readiness.Reasons)
	assert.Empty(t, rb.Readiness.Reasons)

	assert.Equal(t, ksJSON(t, withoutInstance(ra)), ksJSON(t, withoutInstance(localFor(t, st, ka, krT0))))
	assert.Equal(t, ksJSON(t, withoutInstance(rb)), ksJSON(t, withoutInstance(localFor(t, st, kb, krT0))))
	local := localFor(t, st, ka, krT0)
	assert.Equal(t, KeyStatusSourceLocal, local.Instance.Source)
	assert.Empty(t, local.Instance.ID)
	assert.Equal(t, KeyStatusAuditFresh, local.Audit.Scope)
	require.NotNil(t, local.Audit.CompletedAt)
	assert.Equal(t, KeyStatusAuditStartup, ra.Audit.Scope)
}

// The readiness reasons match /readyz: past the active generation's
// sign_until both report signing_issuance_closed.
func TestKeyStatus_ReadinessMatchesReadyz(t *testing.T) {
	st := encStore(t)
	clk := &krClock{t: krT0}
	k := ksFile(t, ksPilotDoc("rev-c", "legacy",
		[]any{ksEntry("legacy", krLegacySecret, krTime(time.Hour), krTime(48*time.Hour))},
		[]any{"legacy"}))
	srv := ksServer(t, st, k, clk)

	r, _ := ksGet(t, srv)
	assert.Empty(t, r.Readiness.Reasons)

	clk.t = krT0.Add(2 * time.Hour)
	code, reason := readyz(t, srv)
	require.Equal(t, http.StatusServiceUnavailable, code)
	r, _ = ksGet(t, srv)
	assert.Equal(t, []string{reason}, r.Readiness.Reasons)
	assert.Equal(t, readyReasonSigningIssuanceClosed, reason)
}

// Only an unscoped platform admin reads the report; with the experiment
// off an admin gets 404 and everyone else is still refused first.
func TestKeyStatus_Authorization(t *testing.T) {
	st := encStore(t)
	clk := &krClock{t: krT0}
	srv := ksServer(t, st, ksKeyringA(t), clk)
	ctx := context.Background()

	member := &store.User{ID: tid("user-ks-member"), Email: "ks-member@test.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, st.CreateUser(ctx, member))
	ensureHubMembership(ctx, st, member.ID)
	hubAdmin := &store.User{ID: tid("user-ks-hubadmin"), Email: "ks-hubadmin@test.com", DisplayName: "Hub Admin",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, st.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, st, hubAdmin.ID)
	def, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: def.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: hubAdmin.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "system"})
	require.NoError(t, err)

	for _, on := range []bool{true, false} {
		ksSetExperiment(t, srv, on)
		assert.Equal(t, http.StatusUnauthorized, doRequestNoAuth(t, srv, http.MethodGet, keyStatusPath, nil).Code, "experiment %v", on)
		assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodGet, keyStatusPath, nil).Code, "experiment %v", on)
		assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, hubAdmin, http.MethodGet, keyStatusPath, nil).Code, "experiment %v", on)
	}
	ksSetExperiment(t, srv, false)
	assert.Equal(t, http.StatusNotFound, doRequest(t, srv, http.MethodGet, keyStatusPath, nil).Code)
	ksSetExperiment(t, srv, true)
	assert.Equal(t, http.StatusOK, doRequest(t, srv, http.MethodGet, keyStatusPath, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequest(t, srv, http.MethodPost, keyStatusPath, nil).Code)
}

// No key byte, derived key, ciphertext or record identifier reaches the
// response; no key byte, derived key or ciphertext reaches the logs.
func TestKeyStatus_NoLeak(t *testing.T) {
	logs := captureLogs(t)
	st := encStore(t)
	clk := &krClock{t: krT0}
	ka := ksKeyringA(t)
	srv := ksServer(t, st, ka, clk)
	ctx := context.Background()

	// A user secret the audit reads, and one it cannot (a foreign key).
	c, err := secret.NewValueCipher(ka)
	require.NoError(t, err)
	readable, err := c.Encrypt("ks-synthetic-payload-readable")
	require.NoError(t, err)
	for _, row := range []*store.Secret{
		{ID: ksRecordReadable, Key: "KS_RECORD_KEY_READABLE", EncryptedValue: readable},
		{ID: ksRecordUnreadable, Key: "KS_RECORD_KEY_UNREADABLE", EncryptedValue: sealForeign(t, "legacy", "ks-synthetic-payload-foreign")},
	} {
		row.Scope, row.ScopeID, row.SecretType = store.ScopeUser, "ks-user", store.SecretTypeEnvironment
		_, err := st.UpsertSecret(ctx, row)
		require.NoError(t, err)
	}
	// Restart over the seeded store so the startup audit sees both rows.
	srv = ksRestart(t, srv, st, ka, clk)

	r, body := ksGet(t, srv)
	assert.Equal(t, 1, r.Audit.Unreadable)
	assert.Equal(t, map[string]int{secret.FailureDecrypt: 1}, r.Audit.UnreadableByKind)

	var forbidden []string
	for _, s := range []string{krLegacySecret, krGen1Secret} {
		forbidden = append(forbidden, s, base64.StdEncoding.EncodeToString([]byte(s)))
		for _, name := range append(append([]string{}, encSigningKeys...), SecretKeyOIDCSigningKey) {
			d := deriveSharedSigningKey(s, name)
			forbidden = append(forbidden, base64.StdEncoding.EncodeToString(d), hex.EncodeToString(d))
		}
	}
	enc := keyring.DeriveLegacyEncryptionKey(krLegacySecret)
	forbidden = append(forbidden, base64.StdEncoding.EncodeToString(enc), hex.EncodeToString(enc))
	rows, err := st.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		if v := row.EncryptedValue; len(v) > 32 {
			// The ciphertext body after the envelope prefix, and its tail.
			forbidden = append(forbidden, v[len(v)-24:], v[8:32])
		}
	}
	for _, f := range forbidden {
		assert.NotContains(t, body, f)
		assert.NotContains(t, logs.String(), f)
	}
	for _, id := range []string{ksRecordReadable, ksRecordUnreadable, "KS_RECORD_KEY_READABLE", "KS_RECORD_KEY_UNREADABLE", "ks-synthetic-payload"} {
		assert.NotContains(t, body, id)
	}
	for _, row := range rows {
		assert.NotContains(t, body, row.ID)
	}
}

// The report never carries a material digest today, in every keyring
// state: legacy mode, an imported-legacy (pilot) file, and even a file with
// a random encryption key.
func TestKeyStatus_MaterialDigestNeverReported(t *testing.T) {
	ctx := context.Background()
	for name, k := range map[string]*keyring.Keyring{
		"legacy":    nil,
		"pilot":     ksKeyringA(t),
		"versioned": ksVersionedKeyring(t, "k1"),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := buildKeyStatus(ctx, keyStatusInputs{keyring: k, now: krT0})
			require.NoError(t, err)
			assert.Nil(t, r.MaterialDigest)
			assert.Equal(t, MaterialDigestOmittedPendingLegacyIndependence, r.MaterialDigestOmittedReason)
			assert.Contains(t, ksJSON(t, r), `"material_digest":null`)
			if k != nil {
				assert.NotContains(t, ksJSON(t, r), keyringMaterialDigest(k))
			}
		})
	}
}

// Why the digest is omitted (sec-review-3's Q3 condition): in legacy mode
// and in every keyring file this release accepts, the only guaranteed
// encryption key is derived from the legacy signing secret, so it adds no
// entropy a weak legacy secret lacks.
func TestKeyStatus_MaterialDigestConditionDoesNotHold(t *testing.T) {
	derived := keyring.DeriveLegacyEncryptionKey(krLegacySecret)
	legacy := keyring.Legacy(krLegacySecret)
	assert.Equal(t, derived, legacy.Encryption().Active().Bytes())
	assert.Equal(t, []string{keyring.LegacyID}, legacy.Encryption().IDs())

	pilot := ksKeyringA(t)
	require.NoError(t, pilot.CheckSessionPilot())
	assert.Equal(t, derived, pilot.Encryption().Active().Bytes(), "the release check requires the derived legacy key as the active key")
}

// The digest construction (not reported yet): deterministic, domain
// separated, and changed by any single change to the keyring.
func TestKeyringMaterialDigest(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"schema":   1,
			"revision": "rev-d",
			"signing": map[string]any{
				"active": "legacy", "emit": "legacy",
				"keys":            []any{ksEntry("legacy", krLegacySecret, nil, nil), ksEntry("gen1", krGen1Secret, krTime(time.Hour), krTime(2*time.Hour))},
				"legacy_read_ids": []any{"legacy"},
			},
			"encryption": map[string]any{
				"active": "e1", "write_format": "v1",
				"keys": []any{
					map[string]any{"id": "e1", "key_b64": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))},
					map[string]any{"id": "e2", "key_b64": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))},
				},
				"legacy_v1_read_ids": []any{"e1"},
			},
		}
	}
	d0 := keyringMaterialDigest(ksFile(t, base()))
	assert.Len(t, d0, 64)
	assert.Equal(t, d0, keyringMaterialDigest(ksFile(t, base())), "deterministic")
	assert.NotEqual(t, d0, materialDigestWithDomain("another-domain", ksFile(t, base())), "domain separated")
	for _, raw := range []string{krLegacySecret, krGen1Secret, strings.Repeat("a", 32)} {
		sum := sha256.Sum256([]byte(raw))
		assert.NotEqual(t, hex.EncodeToString(sum[:]), d0)
	}

	signing := func(d map[string]any) map[string]any { return d["signing"].(map[string]any) }
	encryption := func(d map[string]any) map[string]any { return d["encryption"].(map[string]any) }
	for name, mutate := range map[string]func(map[string]any){
		"revision": func(d map[string]any) { d["revision"] = "rev-e" },
		"signing active": func(d map[string]any) {
			signing(d)["active"] = "gen1"
			signing(d)["legacy_read_ids"] = []any{"legacy", "gen1"}
		},
		"emit": func(d map[string]any) { signing(d)["emit"] = "versioned" },
		"secret byte": func(d map[string]any) {
			signing(d)["keys"].([]any)[1] = ksEntry("gen1", krGen1Secret[:len(krGen1Secret)-1]+"X", krTime(time.Hour), krTime(2*time.Hour))
		},
		"deadline": func(d map[string]any) {
			signing(d)["keys"].([]any)[1] = ksEntry("gen1", krGen1Secret, krTime(time.Hour), krTime(3*time.Hour))
		},
		"legacy readers":    func(d map[string]any) { signing(d)["legacy_read_ids"] = []any{"legacy", "gen1"} },
		"encryption active": func(d map[string]any) { encryption(d)["active"] = "e2" },
		"write format":      func(d map[string]any) { encryption(d)["write_format"] = "v2" },
		"key byte": func(d map[string]any) {
			encryption(d)["keys"].([]any)[1] = map[string]any{"id": "e2", "key_b64": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 31) + "c"))}
		},
		"v1 readers": func(d map[string]any) { encryption(d)["legacy_v1_read_ids"] = []any{"e1", "e2"} },
	} {
		t.Run(name, func(t *testing.T) {
			d := base()
			mutate(d)
			assert.NotEqual(t, d0, keyringMaterialDigest(ksFile(t, d)))
		})
	}
}

// CheckKeyringFile applies the starting Hub's file checks.
func TestCheckKeyringFile(t *testing.T) {
	require.NoError(t, CheckKeyringFile(ksKeyringA(t)))
	assert.Error(t, CheckKeyringFile(nil))
	assert.Error(t, CheckKeyringFile(keyring.Legacy(krLegacySecret)))
	assert.ErrorContains(t, CheckKeyringFile(ksVersionedKeyring(t, "k1")), "not supported in this release")

	noLegacy := ksFile(t, ksPilotDoc("rev-n", "gen1", []any{ksEntry("gen1", krGen1Secret, nil, nil)}, []any{"gen1"}))
	assert.ErrorContains(t, CheckKeyringFile(noLegacy), `no "legacy" entry`)

	notReader := ksFile(t, ksPilotDoc("rev-r", "gen1",
		[]any{ksEntry("legacy", krLegacySecret, nil, nil), ksEntry("gen1", krGen1Secret, nil, nil)}, []any{"legacy"}))
	assert.ErrorContains(t, CheckKeyringFile(notReader), "legacy_read_ids")
}

// Review item 1: `scion server keys check` (CheckKeyringFile) and startup
// (validateKeyringConfig) accept and refuse the same keyring files. The
// shared refactor is deferred to T7 (pkg/hub/keyring.go is T6/T7's file);
// until then this table pins the two together.
func TestCheckKeyringFile_ParityWithValidateKeyringConfig(t *testing.T) {
	for name, k := range map[string]*keyring.Keyring{
		"pilot active legacy": ksKeyringA(t),
		"pilot active gen1":   ksKeyringB(t),
		"versioned refused":   ksVersionedKeyring(t, "k1"),
		"no legacy entry": ksFile(t, ksPilotDoc("rev-n", "gen1",
			[]any{ksEntry("gen1", krGen1Secret, nil, nil)}, []any{"gen1"})),
		"active not a legacy reader": ksFile(t, ksPilotDoc("rev-r", "gen1",
			[]any{ksEntry("legacy", krLegacySecret, nil, nil), ksEntry("gen1", krGen1Secret, nil, nil)}, []any{"legacy"})),
		"legacy adapter":  keyring.Legacy(krLegacySecret),
		"no keyring":      nil,
		"zero keyring":    {},
		"issuance closed": ksFile(t, ksPilotDoc("rev-c", "legacy", []any{ksEntry("legacy", krLegacySecret, krTime(-2*time.Hour), krTime(-time.Hour))}, []any{"legacy"})),
		"active with future deadlines": ksFile(t, ksPilotDoc("rev-f", "legacy",
			[]any{ksEntry("legacy", krLegacySecret, krTime(time.Hour), krTime(48*time.Hour)), ksEntry("gen1", krGen1Secret, nil, nil)},
			[]any{"legacy", "gen1"})),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.KeyringConfigured = true
			cfg.Keyring = k
			cfg.SharedSigningSecret = krLegacySecret
			startupErr := validateKeyringConfig(cfg)
			checkErr := CheckKeyringFile(k)
			assert.Equal(t, startupErr == nil, checkErr == nil, "startup: %v; check: %v", startupErr, checkErr)
		})
	}
}

// Review item 2: the report's signing readiness reason equals /readyz's
// (signingNotReadyReason) for every keyring state and clock. The shared
// refactor is deferred to T7, like item 1.
func TestKeyStatus_SigningReasonParityWithReadyz(t *testing.T) {
	closing := ksFile(t, ksPilotDoc("rev-s", "legacy",
		[]any{ksEntry("legacy", krLegacySecret, krTime(time.Hour), krTime(48*time.Hour))}, []any{"legacy"}))
	for name, k := range map[string]*keyring.Keyring{
		"no keyring":     nil,
		"legacy adapter": keyring.Legacy(krLegacySecret),
		"zero keyring":   {},
		"no deadlines":   ksKeyringA(t),
		"closing":        closing,
	} {
		for _, at := range []time.Duration{0, 2 * time.Hour} {
			now := krT0.Add(at)
			srv := &Server{config: ServerConfig{Keyring: k, KeyringNow: func() time.Time { return now }}}
			want := srv.signingNotReadyReason()
			r, err := buildKeyStatus(context.Background(), keyStatusInputs{keyring: k, now: now})
			require.NoError(t, err, name)
			var got string
			for _, reason := range r.Readiness.Reasons {
				if reason == readyReasonSigningIssuanceClosed || reason == readyReasonSigningNoActiveGeneration {
					got = reason
				}
			}
			assert.Equal(t, want, got, "%s at +%s", name, at)
		}
	}
	// The table reaches both non-empty reasons.
	srv := &Server{config: ServerConfig{Keyring: &keyring.Keyring{}}}
	assert.Equal(t, readyReasonSigningNoActiveGeneration, srv.signingNotReadyReason())
	srv = &Server{config: ServerConfig{Keyring: closing, KeyringNow: func() time.Time { return krT0.Add(2 * time.Hour) }}}
	assert.Equal(t, readyReasonSigningIssuanceClosed, srv.signingNotReadyReason())
}

// Review item 6 / N9: in legacy mode without a session secret, the local
// report audits like a Hub started without one, so the endpoint and the CLI
// agree (instance identity and audit timing aside).
func TestKeyStatus_SecretlessLegacyParity(t *testing.T) {
	st := encStore(t)
	cfg := testServerConfig()
	cfg.HubID = encTestHubID
	cfg.KeyringNow = (&krClock{t: krT0}).now
	start := func() *Server {
		srv, err := newTestHubServer(t, cfg, st)
		require.NoError(t, err)
		srv.SetHubID(encTestHubID)
		waitUserScopedDataSweep(t, srv)
		ksSetExperiment(t, srv, true)
		return srv
	}
	srv := start()
	// One row a secretless Hub cannot read (sealed under a real secret), so
	// the counts are not trivially empty.
	sealed, err := secret.NewLegacyValueCipher(krLegacySecret).Encrypt("ks-synthetic-legacy-payload")
	require.NoError(t, err)
	_, err = st.UpsertSecret(context.Background(), &store.Secret{ID: ksRecordLegacy, Key: "KS_RECORD_KEY_LEGACY", EncryptedValue: sealed,
		Scope: store.ScopeUser, ScopeID: "ks-user", SecretType: store.SecretTypeEnvironment})
	require.NoError(t, err)
	require.NoError(t, srv.Shutdown(context.Background()))
	srv = start()

	endpoint, _ := ksGet(t, srv)
	require.True(t, endpoint.Audit.Ran)
	assert.Positive(t, endpoint.Audit.Scanned)

	local, err := LocalKeyStatus(context.Background(), nil, secret.NewLegacyValueCipher(""), st, krT0)
	require.NoError(t, err)
	assert.True(t, local.Audit.Ran)
	assert.Equal(t, ksJSON(t, withoutInstance(endpoint)), ksJSON(t, withoutInstance(local)))
}

// ksFailingPolicyStore is the real store whose encryption policy read
// fails while fail is set (after startup).
type ksFailingPolicyStore struct {
	store.Store
	fail atomic.Bool
}

func (s *ksFailingPolicyStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *ksFailingPolicyStore) Dialect() string {
	if p, ok := s.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (s *ksFailingPolicyStore) GetEncryptionPolicy(ctx context.Context) (*store.EncryptionPolicy, error) {
	if s.fail.Load() {
		return nil, errors.New("ks-synthetic-store-failure postgres://ks-user:ks-password@db/hub")
	}
	return s.Store.GetEncryptionPolicy(ctx)
}

// Review item 8: a store failure answers 503 with a generic message, and
// the store error reaches only the server log.
func TestKeyStatus_StoreFailureIs503(t *testing.T) {
	logs := captureLogs(t)
	fs := &ksFailingPolicyStore{Store: encStore(t)}
	srv := ksServer(t, fs, ksKeyringA(t), &krClock{t: krT0})
	fs.fail.Store(true)

	rec := doRequest(t, srv, http.MethodGet, keyStatusPath, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "key status unavailable")
	assert.NotContains(t, rec.Body.String(), "ks-synthetic-store-failure")
	assert.NotContains(t, rec.Body.String(), "ks-password")
	assert.Contains(t, logs.String(), "Key status report failed")
	assert.Contains(t, logs.String(), "ks-synthetic-store-failure")

	fs.fail.Store(false)
	assert.Equal(t, http.StatusOK, doRequest(t, srv, http.MethodGet, keyStatusPath, nil).Code)
}
