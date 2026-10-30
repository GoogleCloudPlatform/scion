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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Phase 2 (ptone/scion#1530 §5, §6) integration of the shared ValueCipher
// with real stores: signing-key backups, OIDC key and keyset, the conduit
// grant ring, the startup audit and the encryption policy check. Every key
// here is a synthetic ciphertest fixture.

const encTestHubID = "enc-test-hub"

// encSigningKeys are the six ensureSigningKey backups New creates.
var encSigningKeys = []string{
	SecretKeyAgentSigningKey, SecretKeyUserSigningKey, SecretKeyDownloadSigningKey,
	SecretKeyArtifactViewKey, SecretKeyAttachmentDeliveryKey, SecretKeyListCursorKey,
}

// v2Cipher is a managed cipher writing enc:v2: under active, holding ids.
func v2Cipher(t *testing.T, active string, ids ...string) *secret.ValueCipher {
	t.Helper()
	return ciphertest.Cipher(t, ciphertest.Ring{Active: active, IDs: ids, WriteFormat: keyring.WriteV2,
		V1Readers: keepIf(ids, keyring.LegacyID)})
}

func keepIf(ids []string, want string) []string {
	out := []string{}
	for _, id := range ids {
		if id == want {
			out = append(out, id)
		}
	}
	return out
}

// encServer builds a hub on st with cipher c and, when withBackend, a
// local secret backend sharing c.
func encServer(t *testing.T, st store.Store, c *secret.ValueCipher, withBackend bool) *Server {
	t.Helper()
	srv, err := encServerErr(t, st, c, withBackend)
	require.NoError(t, err)
	return srv
}

func encServerErr(t *testing.T, st store.Store, c *secret.ValueCipher, withBackend bool) (*Server, error) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.HubID = encTestHubID
	cfg.ValueCipher = c
	if withBackend {
		cfg.SecretBackend = secret.NewLocalBackendWithCipher(st, encTestHubID, c)
	}
	return newTestHubServer(t, cfg, st)
}

// legacyServer builds a legacy-mode hub over sharedSecret (no ValueCipher
// injected: New builds it).
func legacyServer(t *testing.T, st store.Store, sharedSecret string) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.HubID = encTestHubID
	cfg.SharedSigningSecret = sharedSecret
	cfg.SecretBackend = secret.NewLocalBackend(st, encTestHubID, sharedSecret)
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err)
	return srv
}

func encStore(t *testing.T) store.Store {
	t.Helper()
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	return st
}

type rowSnap struct {
	value   string
	version int
	ref     string
}

func snapRows(t *testing.T, st store.Store, keys ...string) map[string]rowSnap {
	t.Helper()
	out := map[string]rowSnap{}
	for _, k := range keys {
		r, err := st.GetSecret(context.Background(), k, store.ScopeHub, encTestHubID)
		require.NoError(t, err, k)
		out[k] = rowSnap{r.EncryptedValue, r.Version, r.SecretRef}
	}
	return out
}

func signingKeyBytes(t *testing.T, srv *Server) map[string][]byte {
	t.Helper()
	// The download key is held by srv.downloadKeys (capability_keys.go).
	// These test servers run without a keyring file, so it is the fixed
	// legacy key resolved from the signing-key backup, and the active key
	// of that one-key ring is exactly those bytes.
	var download []byte
	if srv.downloadKeys.ring != nil {
		if k, err := srv.downloadKeys.ring.Active(srv.downloadKeys.purpose, time.Now()); err == nil {
			download = append([]byte{}, k.Bytes()...)
		}
	}
	keys := map[string][]byte{
		SecretKeyAgentSigningKey:    append([]byte{}, srv.agentTokenService.config.SigningKey...),
		SecretKeyUserSigningKey:     append([]byte{}, srv.userTokenService.config.SigningKey...),
		SecretKeyDownloadSigningKey: download,
	}
	// Every signing key is 32 bytes. A missing key must fail here rather than
	// compare equal to another missing key in the callers' round-trip checks.
	for name, key := range keys {
		require.Len(t, key, 32, "%s", name)
	}
	return keys
}

func readyz(t *testing.T, srv *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body["reason"]
}

// sealForeign seals plaintext with a cipher whose key the servers under
// test do not hold (same key ID, different bytes).
func sealForeign(t *testing.T, kid, plaintext string) string {
	t.Helper()
	foreign := ciphertest.Cipher(t, ciphertest.Ring{Active: kid, IDs: []string{kid},
		Keys: map[string][]byte{kid: []byte("FAKE-FOREIGN-KEY-0000000000000000")[:32]}})
	v, err := foreign.Encrypt(plaintext)
	require.NoError(t, err)
	return v
}

func seedInfraRow(t *testing.T, st store.Store, key, value, ref string) {
	t.Helper()
	_, err := st.UpsertSecret(context.Background(), &store.Secret{
		ID: signingKeySecretID(key, encTestHubID), Key: key, EncryptedValue: value, SecretRef: ref,
		Scope: store.ScopeHub, ScopeID: encTestHubID, SecretType: store.SecretTypeInternal,
		Description: "seeded " + key,
	})
	require.NoError(t, err)
}

func randomB64Key(t *testing.T) (string, []byte) {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k), k
}

// Acceptance: full real-store roundtrip of the signing backups under an
// independent v2 encryption key, across a restart.
func TestEncryptionRing_SigningBackupRoundTrip(t *testing.T) {
	st := encStore(t)
	c := v2Cipher(t, "k2", "k2")
	srv1 := encServer(t, st, c, true)
	before := signingKeyBytes(t, srv1)
	rows := snapRows(t, st, encSigningKeys...)
	for k, r := range rows {
		assert.True(t, strings.HasPrefix(r.value, "enc:v2:k2:"), "%s backup is not enc:v2:k2", k)
	}
	assert.Empty(t, srv1.EncryptionAudit().Unreadable)

	srv2 := encServer(t, st, c, true)
	after := signingKeyBytes(t, srv2)
	for k, b := range before {
		assert.True(t, bytes.Equal(b, after[k]), "%s changed across restart", k)
	}
	code, reason := readyz(t, srv2)
	assert.NotEqual(t, readyReasonBackupUnreadable, reason)
	_ = code
}

// Acceptance: a wrong or missing decrypt key never overwrites an existing
// record, in managed and legacy mode; managed mode is not ready
// (backup_unreadable), legacy mode keeps serving with the diagnostic.
func TestWrongKey_NoOverwrite_SigningBackup(t *testing.T) {
	t.Run("managed", func(t *testing.T) {
		st := encStore(t)
		encServer(t, st, v2Cipher(t, "k2", "k2"), true)
		before := snapRows(t, st, encSigningKeys...)

		// The k2 key is missing from the new ring.
		srv := encServer(t, st, v2Cipher(t, "k3", "k3"), true)
		assert.Equal(t, before, snapRows(t, st, encSigningKeys...), "an unreadable backup was overwritten")
		audit := srv.EncryptionAudit()
		assert.Len(t, audit.BackupUnreadable, len(encSigningKeys))
		for _, r := range audit.BackupUnreadable {
			assert.Equal(t, secret.FailureUnknownKeyID, r.Kind)
		}
		code, reason := readyz(t, srv)
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, readyReasonBackupUnreadable, reason)

		// A second restart with the wrong key still changes nothing.
		encServer(t, st, v2Cipher(t, "k3", "k3"), true)
		assert.Equal(t, before, snapRows(t, st, encSigningKeys...))

		// Restoring the original key recovers every record unchanged.
		ok := encServer(t, st, v2Cipher(t, "k2", "k2"), true)
		assert.Empty(t, ok.EncryptionAudit().BackupUnreadable)
	})
	t.Run("legacy", func(t *testing.T) {
		st := encStore(t)
		// Generated (not derived) keys, encrypted under secret A.
		cfg := DefaultServerConfig()
		cfg.HubID = encTestHubID
		cfg.ValueCipher = secret.NewLegacyValueCipher("FAKE-legacy-secret-A")
		cfg.SecretBackend = secret.NewLocalBackendWithCipher(st, encTestHubID, cfg.ValueCipher)
		_, err := newTestHubServer(t, cfg, st)
		require.NoError(t, err)
		before := snapRows(t, st, encSigningKeys...)

		// The server secret changed: derived keys are synced, but every
		// unreadable backup is preserved.
		srv := legacyServer(t, st, "FAKE-legacy-secret-B")
		assert.Equal(t, before, snapRows(t, st, encSigningKeys...), "an unreadable backup was overwritten")
		audit := srv.EncryptionAudit()
		assert.Len(t, audit.BackupUnreadable, len(encSigningKeys))
		_, reason := readyz(t, srv)
		assert.NotEqual(t, readyReasonBackupUnreadable, reason, "legacy mode reports, it does not block readiness")
	})
}

// externalBackend stands in for an authoritative external secret backend
// that resolves the agent signing key itself.
type externalBackend struct {
	secret.SecretBackend
	values map[string]string
}

func (b *externalBackend) Get(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	if v, ok := b.values[name]; ok {
		return &secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name, Scope: scope, ScopeID: scopeID}, Value: v}, nil
	}
	return nil, store.ErrNotFound
}

func (b *externalBackend) Set(_ context.Context, in *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return true, &secret.SecretMeta{Name: in.Name, Scope: in.Scope, ScopeID: in.ScopeID}, nil
}

func (b *externalBackend) Delete(context.Context, string, string, string) error { return nil }

// Acceptance: a pre-existing unreadable backup is reported even when the
// external backend loading succeeds, and it is not overwritten.
func TestAudit_UnreadableBackupReportedWithExternalBackend(t *testing.T) {
	for _, mode := range []string{"managed", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			st := encStore(t)
			encoded, raw := randomB64Key(t)
			seedInfraRow(t, st, SecretKeyAgentSigningKey, sealForeign(t, "k2", encoded), "")
			before := snapRows(t, st, SecretKeyAgentSigningKey)

			cfg := DefaultServerConfig()
			cfg.HubID = encTestHubID
			cfg.SecretBackend = &externalBackend{values: map[string]string{SecretKeyAgentSigningKey: encoded}}
			if mode == "managed" {
				cfg.ValueCipher = v2Cipher(t, "k2", "k2")
			} else {
				cfg.ValueCipher = secret.NewLegacyValueCipher("FAKE-legacy-secret-A")
			}
			srv, err := newTestHubServer(t, cfg, st)
			require.NoError(t, err)

			// The external backend supplied the key...
			assert.True(t, bytes.Equal(raw, srv.agentTokenService.config.SigningKey))
			// ...but the unreadable local backup is reported and preserved.
			audit := srv.EncryptionAudit()
			require.Len(t, audit.BackupUnreadable, 1)
			assert.Equal(t, SecretKeyAgentSigningKey, audit.BackupUnreadable[0].Key)
			assert.Equal(t, before, snapRows(t, st, SecretKeyAgentSigningKey))
			code, reason := readyz(t, srv)
			if mode == "managed" {
				assert.Equal(t, http.StatusServiceUnavailable, code)
				assert.Equal(t, readyReasonBackupUnreadable, reason)
			} else {
				assert.NotEqual(t, readyReasonBackupUnreadable, reason)
			}
		})
	}
}

// Restart with a partially migrated, mixed-format database: v1 (legacy
// key), v2 under two keys and a plaintext (non-infrastructure) secret. The managed ring
// holding every key reads all infrastructure rows unchanged across two
// restarts; the plaintext row is reported, not admitted, and does not gate
// readiness (it is not an infrastructure backup).
func TestRestartMixedFormatDatabase(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	legacyKeyring := ciphertest.Cipher(t, ciphertest.Ring{Active: keyring.LegacyID, IDs: []string{keyring.LegacyID},
		WriteFormat: keyring.WriteV1, V1Readers: []string{keyring.LegacyID}})
	writers := map[string]*secret.ValueCipher{
		SecretKeyAgentSigningKey:    legacyKeyring,
		SecretKeyUserSigningKey:     v2Cipher(t, "k1", "k1"),
		SecretKeyDownloadSigningKey: v2Cipher(t, "k2", "k2"),
	}
	want := map[string][]byte{}
	for key, w := range writers {
		encoded, raw := randomB64Key(t)
		v, err := w.Encrypt(encoded)
		require.NoError(t, err)
		seedInfraRow(t, st, key, v, "")
		want[key] = raw
	}
	// A hub-scope (not infrastructure) plaintext row: user-scope rows of
	// users that do not exist are removed by the asynchronous startup
	// sweep, which would race the second restart's audit.
	_, err := st.UpsertSecret(ctx, &store.Secret{ID: "00000000-0000-4000-8000-0000000000aa", Key: "HUB_PLAIN",
		EncryptedValue: "fake-plaintext", Scope: store.ScopeHub, ScopeID: encTestHubID, SecretType: store.SecretTypeEnvironment})
	require.NoError(t, err)

	ring := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{keyring.LegacyID, "k1", "k2"},
		WriteFormat: keyring.WriteV2, V1Readers: []string{keyring.LegacyID}})
	var seeded map[string]rowSnap
	for i := range 2 {
		srv := encServer(t, st, ring, false)
		got := signingKeyBytes(t, srv)
		for key, raw := range want {
			assert.True(t, bytes.Equal(raw, got[key]), "restart %d: %s differs", i, key)
		}
		rows := snapRows(t, st, SecretKeyAgentSigningKey, SecretKeyUserSigningKey, SecretKeyDownloadSigningKey)
		if seeded == nil {
			seeded = rows
		}
		assert.Equal(t, seeded, rows, "restart %d rewrote a stored backup", i)
		audit := srv.EncryptionAudit()
		assert.Empty(t, audit.BackupUnreadable)
		require.Len(t, audit.Unreadable, 1)
		assert.Equal(t, "HUB_PLAIN", audit.Unreadable[0].Key)
		assert.Equal(t, secret.FailurePlaintextNotAdmitted, audit.Unreadable[0].Kind)
		assert.Equal(t, 1, audit.ByFormat["v1"])
		assert.Equal(t, 1, audit.ByFormat["plaintext"])
		assert.GreaterOrEqual(t, audit.ByKID["k1"], 1)
		_, reason := readyz(t, srv)
		assert.NotEqual(t, readyReasonBackupUnreadable, reason)
	}
}

// Acceptance: Version, refs and signing identities survive ciphertext
// replacement. Every infrastructure row written under the legacy v1 key is
// re-encrypted to v2 with ReplaceSecretCiphertext; after a restart on the
// new ring every key, Version and ref is unchanged.
func TestCiphertextReplacementKeepsIdentities(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	oldRing := ciphertest.Cipher(t, ciphertest.Ring{Active: keyring.LegacyID, IDs: []string{keyring.LegacyID},
		WriteFormat: keyring.WriteV1, V1Readers: []string{keyring.LegacyID}})
	srv1 := encServer(t, st, oldRing, true)
	keysBefore := signingKeyBytes(t, srv1)
	ringBefore, _, err := srv1.conduitGrantKeySet().store.Load(ctx)
	if errors.Is(err, store.ErrNotFound) {
		_, err = srv1.conduitGrantKeySet().signer(ctx)
		require.NoError(t, err)
		ringBefore, _, err = srv1.conduitGrantKeySet().store.Load(ctx)
	}
	require.NoError(t, err)
	// Give one backup a ref, as the GCP backend would.
	ref := "gcpsm:projects/fake/secrets/agent"
	agent, err := st.GetSecret(ctx, SecretKeyAgentSigningKey, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	applied, err := st.UpdateSecretRefIfMatches(ctx, agent.Key, agent.Scope, agent.ScopeID, "", agent.Version, ref)
	require.NoError(t, err)
	require.True(t, applied)

	newRing := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{keyring.LegacyID, "k2"},
		WriteFormat: keyring.WriteV2, V1Readers: []string{keyring.LegacyID}})
	type ident struct {
		version int
		ref     string
	}
	before := map[string]ident{}
	rewritten := 0
	page, err := st.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	for _, p := range page {
		if p.EncryptedValue == "" {
			continue
		}
		require.True(t, strings.HasPrefix(p.EncryptedValue, "enc:v1:"), "%s is not v1 before the rewrap", p.Key)
		plain, err := newRing.Decrypt(p.EncryptedValue)
		require.NoError(t, err)
		repl, err := newRing.Encrypt(plain)
		require.NoError(t, err)
		ok, err := st.ReplaceSecretCiphertext(ctx, p.ID, p.Version, p.EncryptedValue, repl)
		require.NoError(t, err)
		require.True(t, ok)
		before[p.ID] = ident{p.Version, p.SecretRef}
		rewritten++
	}
	require.GreaterOrEqual(t, rewritten, len(encSigningKeys)+1, "the signing backups and the grant ring were rewrapped")
	// ReplaceSecretCiphertext itself preserves Version and refs (§6).
	page, err = st.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	for _, p := range page {
		if b, ok := before[p.ID]; ok {
			assert.Equal(t, b.version, p.Version, "%s version changed by the replacement", p.Key)
			assert.Equal(t, b.ref, p.SecretRef, "%s ref changed by the replacement", p.Key)
			assert.True(t, strings.HasPrefix(p.EncryptedValue, "enc:v2:k2:"), "%s is not v2 after the replacement", p.Key)
		}
	}

	srv2 := encServer(t, st, newRing, true)
	keysAfter := signingKeyBytes(t, srv2)
	for k, b := range keysBefore {
		assert.True(t, bytes.Equal(b, keysAfter[k]), "%s changed across rewrap", k)
	}
	ringAfter, _, err := srv2.conduitGrantKeySet().store.Load(ctx)
	require.NoError(t, err)
	assertSameGrantRing(t, ringBefore, ringAfter)
	page, err = st.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	for _, p := range page {
		b, ok := before[p.ID]
		if !ok {
			continue
		}
		// The restart's backend path re-saves each signing-key backup
		// (backupSigningKeyToStore after a backend Get, an UpdateSecret that
		// bumps Version), so only the grant ring keeps its Version here;
		// refs and the payload format hold for every row.
		if p.Key == conduitGrantKeySecretName {
			assert.Equal(t, b.version, p.Version, "%s version changed", p.Key)
		}
		assert.Equal(t, b.ref, p.SecretRef, "%s ref changed", p.Key)
		assert.True(t, strings.HasPrefix(p.EncryptedValue, "enc:v2:k2:"), "%s is not v2 after the restart", p.Key)
	}
	assert.Empty(t, srv2.EncryptionAudit().Unreadable)
}

func assertSameGrantRing(t *testing.T, a, b interface{ Validate() error }) {
	t.Helper()
	ja, err := json.Marshal(a)
	require.NoError(t, err)
	jb, err := json.Marshal(b)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(ja, jb), "grant ring keys, activation or retention changed")
}

// Startup refuses an instance the database encryption policy does not
// admit (§6): legacy mode, a stale writer, a missing required reader.
func TestStartupRefusesIncompatiblePolicy(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	applied, err := st.UpdateEncryptionPolicy(ctx, 0, store.EncryptionPolicy{
		Generation: 1, MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{"k2"}, RequiredReadKIDs: []string{"k1", "k2"}, State: store.EncryptionPolicyPrepared,
	})
	require.NoError(t, err)
	require.True(t, applied)

	for name, c := range map[string]*secret.ValueCipher{
		"legacy mode":             secret.NewLegacyValueCipher("FAKE-legacy-secret-A"),
		"disabled mode":           secret.NewLegacyValueCipher(""),
		"stale writer":            v2Cipher(t, "k1", "k1", "k2"),
		"v1 writer":               ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k1", "k2"}, WriteFormat: keyring.WriteV1, V1Readers: []string{"k2"}}),
		"missing required reader": v2Cipher(t, "k2", "k2"),
	} {
		_, err := encServerErr(t, st, c, true)
		assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch, name)
		if err != nil {
			for _, leak := range []string{"FAKE-KEY", "FAKE-legacy"} {
				assert.NotContains(t, err.Error(), leak, name)
			}
		}
	}
	srv, err := encServerErr(t, st, v2Cipher(t, "k2", "k1", "k2"), true)
	require.NoError(t, err)
	for _, k := range encSigningKeys {
		r, err := st.GetSecret(ctx, k, store.ScopeHub, encTestHubID)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(r.EncryptedValue, "enc:v2:k2:"), "%s was not written under the allowed writer", k)
	}
	_ = srv
}

// A configured ValueCipher must be the one the local backend holds, and
// must wrap the keyring file's encryption ring.
func TestServerValueCipher_SharedObject(t *testing.T) {
	st := encStore(t)
	c := v2Cipher(t, "k2", "k2")
	cfg := DefaultServerConfig()
	cfg.HubID = encTestHubID
	cfg.ValueCipher = c
	cfg.SecretBackend = secret.NewLocalBackendWithCipher(st, encTestHubID, v2Cipher(t, "k2", "k2"))
	_, err := newTestHubServer(t, cfg, st)
	require.Error(t, err, "a backend with a different cipher object must be refused")

	k := ciphertest.Keyring(t, ciphertest.Ring{Active: keyring.LegacyID, IDs: []string{keyring.LegacyID},
		WriteFormat: keyring.WriteV1, V1Readers: []string{keyring.LegacyID}})
	_, err = serverValueCipher(ServerConfig{Keyring: k, ValueCipher: c})
	require.Error(t, err, "a cipher over another ring must be refused in keyring mode")
	got, err := serverValueCipher(ServerConfig{Keyring: k})
	require.NoError(t, err)
	assert.True(t, got.Managed() && got.UsesRing(k.Encryption()))
	legacy, err := serverValueCipher(ServerConfig{SharedSigningSecret: "FAKE-legacy-secret-A"})
	require.NoError(t, err)
	assert.True(t, legacy.Enabled() && !legacy.Managed())
}

// OIDC acceptance: the RSA key and the keyset round-trip under an
// independent v2 key across a restart, with identical KIDs and JWKS.
func TestOIDCKeyManager_IndependentV2RestartKeepsKIDAndJWKS(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	c := v2Cipher(t, "k2", "k2")
	newMgr := func() *OIDCKeyManager {
		m, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: encTestHubID,
			IssuerURL: "https://example.com", ValueCipher: c})
		require.NoError(t, err)
		return m
	}
	m1 := newMgr()
	require.NoError(t, m1.RotateKey(ctx))
	kids := func(m *OIDCKeyManager) []string {
		var out []string
		for _, k := range m.JWKS().Keys {
			out = append(out, k.KeyID)
		}
		sort.Strings(out)
		return out
	}
	wantKIDs, wantActive := kids(m1), m1.activeKey.KeyID
	require.Len(t, wantKIDs, 2)
	for _, key := range []string{SecretKeyOIDCSigningKey, SecretKeyOIDCKeyset} {
		v, err := st.GetSecretValue(ctx, key, store.ScopeHub, encTestHubID)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(v, "enc:v2:k2:"), "%s is not enc:v2:k2", key)
	}
	m2 := newMgr()
	assert.Equal(t, wantKIDs, kids(m2))
	assert.Equal(t, wantActive, m2.activeKey.KeyID)
}

// D1: an unreadable stored keyset is not replaced at startup (it used to
// be saved over after a failed restore).
func TestOIDCRestoreFailureDoesNotSaveKeyset(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	c := v2Cipher(t, "k2", "k2")
	_, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: encTestHubID,
		IssuerURL: "https://example.com", ValueCipher: c})
	require.NoError(t, err)
	// Replace the keyset with one sealed under a key this ring lacks.
	ks, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	ks.EncryptedValue = sealForeign(t, "k2", "[]")
	require.NoError(t, st.UpdateSecret(ctx, ks))
	before, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)

	_, err = NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: encTestHubID,
		IssuerURL: "https://example.com", ValueCipher: c})
	require.NoError(t, err)
	after, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue, "the unreadable keyset was overwritten")
	assert.Equal(t, before.Version, after.Version)
}

// D2: a CAS loser that cannot decrypt the winner's key fails instead of
// continuing with (and later persisting) its own key.
func TestOIDCCASLoserUndecryptableFails(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	winner, err := generateRSAKeyPair()
	require.NoError(t, err)
	winnerPEM, err := encodePEMPrivateKey(winner)
	require.NoError(t, err)
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{ID: oidcSigningKeySecretID(encTestHubID), Key: SecretKeyOIDCSigningKey,
		EncryptedValue: sealForeign(t, "k2", string(winnerPEM)), Scope: store.ScopeHub, ScopeID: encTestHubID,
		SecretType: store.SecretTypeInternal}))
	before, err := st.GetSecret(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, encTestHubID)
	require.NoError(t, err)

	mine, err := generateRSAKeyPair()
	require.NoError(t, err)
	minePEM, err := encodePEMPrivateKey(mine)
	require.NoError(t, err)
	m := &OIDCKeyManager{store: st, hubID: encTestHubID, log: slog.Default(), cipher: v2Cipher(t, "k2", "k2")}
	key, err := m.casCreateKeyInStore(ctx, SecretKeyOIDCSigningKey, string(minePEM), encTestHubID)
	require.Error(t, err)
	assert.Nil(t, key)
	assert.ErrorIs(t, err, secret.ErrDecrypt)
	assert.NotContains(t, err.Error(), "PRIVATE KEY")
	after, err := st.GetSecret(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue)
}

// Grant ring: v2 roundtrip and CAS through the shared cipher; a node
// without the key cannot read, mint or overwrite the stored ring.
func TestConduitGrantRing_IndependentV2RoundTripAndCAS(t *testing.T) {
	_, st := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	c := v2Cipher(t, "k2", "k2")
	db, err := newDBConduitGrantKeyStore(st, c)
	require.NoError(t, err)
	k := newConduitGrantKeys(db, clock.Now)
	_, err = k.signer(ctx)
	require.NoError(t, err)

	row, err := st.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(row.EncryptedValue, "enc:v2:k2:"))

	other, err := newDBConduitGrantKeyStore(st, c)
	require.NoError(t, err)
	ring, rev, err := other.Load(ctx)
	require.NoError(t, err)
	ring2, _, err := db.Load(ctx)
	require.NoError(t, err)
	assertSameGrantRing(t, ring, ring2)

	applied, err := other.CompareAndSwap(ctx, ring, rev)
	require.NoError(t, err)
	assert.True(t, applied)
	applied, err = other.CompareAndSwap(ctx, ring, rev)
	require.NoError(t, err)
	assert.False(t, applied, "a stale revision must not apply")

	before, err := st.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	require.NoError(t, err)
	wrong, err := newDBConduitGrantKeyStore(st, v2Cipher(t, "k3", "k3"))
	require.NoError(t, err)
	_, _, err = wrong.Load(ctx)
	assert.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.ErrorIs(t, err, secret.ErrUnknownKeyID)
	_, err = newConduitGrantKeys(wrong, clock.Now).signer(ctx)
	assert.Error(t, err)
	after, err := st.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	require.NoError(t, err)
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue, "the unreadable ring was overwritten")
	assert.Equal(t, before.Version, after.Version)
}

// D1, the case only the restore-failure branch protects: the stored keyset
// decrypts with the manager's own cipher (so the overwrite guard's decrypt
// check passes) but does not parse. The failed restore must leave it in
// place instead of saving the in-memory keyset over it.
func TestOIDCRestoreFailureDoesNotSaveUnparseableKeyset(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	c := v2Cipher(t, "k2", "k2")
	_, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: encTestHubID,
		IssuerURL: "https://example.com", ValueCipher: c})
	require.NoError(t, err)
	notJSON, err := c.Encrypt("not-json")
	require.NoError(t, err)
	ks, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	ks.EncryptedValue = notJSON
	require.NoError(t, st.UpdateSecret(ctx, ks))
	before, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)

	_, err = NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: encTestHubID,
		IssuerURL: "https://example.com", ValueCipher: c})
	require.NoError(t, err)
	after, err := st.GetSecret(ctx, SecretKeyOIDCKeyset, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue, "the unparseable keyset was saved over")
	assert.Equal(t, before.Version, after.Version)
}

// The audit log lists record IDs and classifications only, capped.
func TestAuditRecordIDs_Capped(t *testing.T) {
	var recs []AuditRecord
	for i := 0; i < 23; i++ {
		recs = append(recs, AuditRecord{ID: fmt.Sprintf("id-%02d", i), Key: "SECRET_KEY_NAME", Scope: "user", ScopeID: "u", Kind: secret.FailureDecrypt})
	}
	got := auditRecordIDs(recs, 20)
	assert.True(t, strings.HasPrefix(got, "id-00 (decrypt_failed), id-01 (decrypt_failed)"))
	assert.True(t, strings.HasSuffix(got, "id-19 (decrypt_failed), and 3 more"))
	assert.NotContains(t, got, "SECRET_KEY_NAME")
	assert.Equal(t, "id-00 (decrypt_failed)", auditRecordIDs(recs[:1], 20))
}
