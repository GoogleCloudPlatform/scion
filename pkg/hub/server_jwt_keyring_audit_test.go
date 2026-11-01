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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The startup encryption audit (encryption_audit.go) over the retained JWT
// signing-key backups in phase 3's managed ring mode (ptone/scion#1530,
// phase 3 item D1): real hub servers over one real SQLite store and local
// backend (newP3Fixture). The audit authenticates the agent_signing_key and
// user_signing_key backups on every start and never rewrites them; an
// unreadable one keeps a managed instance not ready (backup_unreadable) and
// draws a conspicuous diagnostic in legacy mode. The harness attributes the
// audit's reads separately (aliasCounter.audits), so they never count as
// alias fallback reads.

// p3bForeignSecret seals a backup under encryption material no fixture
// server holds.
const p3bForeignSecret = "test-only-p3b-foreign-secret"

// p3bE2Key is a second encryption key (ID "e2"), 32 bytes.
var p3bE2Key = []byte("test-only-p3b-e2-encryption-key!")

// p3bRing builds and parses a keyring file whose signing ring is the
// fixture's legacy generation and whose encryption ring holds the legacy
// key and "e2" (p3bE2Key). With active "legacy" and write_format "v1" it is
// a ring this release accepts, which still reads enc:v2: values under e2;
// with active "e2" and "v2" it only seals test values (never given to a
// server).
func p3bRing(t *testing.T, encActive, writeFormat string) *keyring.Keyring {
	t.Helper()
	doc := map[string]any{
		"schema":   1,
		"revision": "test-p3b-rev",
		"signing": map[string]any{"active": "legacy", "emit": "legacy", "legacy_read_ids": []any{"legacy"},
			"keys": []any{map[string]any{"id": "legacy", "secret": jkLegacySecret, "sign_until": nil, "verify_until": nil}}},
		"encryption": map[string]any{
			"active": encActive, "write_format": writeFormat, "legacy_v1_read_ids": []any{"legacy"},
			"keys": []any{
				map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(jkLegacySecret))},
				map[string]any{"id": "e2", "key_b64": base64.StdEncoding.EncodeToString(p3bE2Key)},
			},
		},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(data)
	require.NoError(t, err)
	return k
}

// tamperV2 flips the last byte (in the authentication tag) of a version 2
// envelope's sealed bytes: the envelope and its key ID still parse,
// decryption fails.
func tamperV2(t *testing.T, v, kid string) string {
	t.Helper()
	prefix := "enc:v2:" + kid + ":"
	require.True(t, strings.HasPrefix(v, prefix), "a version 2 envelope under %s", kid)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	raw[len(raw)-1] ^= 0x01
	return prefix + base64.StdEncoding.EncodeToString(raw)
}

// setAliasRow replaces the stored payload and reference of a retained alias
// through the raw store (uncounted), before the test's snapshot.
func setAliasRow(t *testing.T, f *p3Fixture, name, value, ref string) {
	t.Helper()
	ctx := context.Background()
	row, err := f.raw.GetSecret(ctx, name, store.ScopeHub, p3HubID)
	require.NoError(t, err)
	row.EncryptedValue = value
	row.SecretRef = ref
	require.NoError(t, f.raw.UpdateSecret(ctx, row))
}

// aliasCiphertext is a retained alias's stored payload.
func aliasCiphertext(t *testing.T, f *p3Fixture, name string) string {
	t.Helper()
	row, err := f.raw.GetSecret(context.Background(), name, store.ScopeHub, p3HubID)
	require.NoError(t, err)
	return row.EncryptedValue
}

// tamperV1 flips the last byte (in the authentication tag) of a version 1
// envelope's sealed bytes: the envelope still parses, decryption fails.
func tamperV1(t *testing.T, v string) string {
	t.Helper()
	const prefix = "enc:v1:"
	require.True(t, strings.HasPrefix(v, prefix), "fixture alias is a version 1 envelope")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	raw[len(raw)-1] ^= 0x01
	return prefix + base64.StdEncoding.EncodeToString(raw)
}

// requireRingSignsAndVerifies checks that the ring still issues and
// validates a user JWT.
func requireRingSignsAndVerifies(t *testing.T, srv *Server) {
	t.Helper()
	tok, _, _, err := srv.userTokenService.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
	require.NoError(t, err)
	_, err = srv.userTokenService.ValidateUserToken(tok)
	require.NoError(t, err)
}

// TestP3b_ManagedAuditAuthenticatesSigningBackups: on every managed start
// (two startup generations alternating A, B, A, B, for each emit mode) the
// audit reads and authenticates both retained aliases (one attributed audit
// read each per start), finds them readable, and leaves them byte-, Version-,
// Updated- and SecretRef-equal; there is no alias read or write and readiness
// is not held by backup_unreadable.
func TestP3b_ManagedAuditAuthenticatesSigningBackups(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		name := "emit legacy"
		if versioned {
			name = "emit versioned"
		}
		t.Run(name, func(t *testing.T) {
			if versioned {
				allowVersionedJWTRings(t)
			}
			f := newP3Fixture(t)
			a, b := f.p3Rings(t, versioned)
			before := f.snapshot(t)
			for i, k := range []*keyring.Keyring{a, b, a, b} {
				srv, err := f.start(t, f.config(k, true))
				require.NoError(t, err, "start %d", i)
				audit := srv.EncryptionAudit()
				require.True(t, audit.Done, "start %d", i)
				assert.Empty(t, audit.Unreadable, "start %d", i)
				assert.Empty(t, audit.BackupUnreadable, "start %d", i)
				assert.GreaterOrEqual(t, audit.Readable, len(p3AliasNames), "start %d", i)
				for _, n := range p3AliasNames {
					assert.Equal(t, i+1, f.c.auditsOf(n), "start %d: %s audit reads", i, n)
				}
				_, reason := readyz(t, srv)
				assert.NotEqual(t, readyReasonBackupUnreadable, reason, "start %d", i)
				requireRingSignsAndVerifies(t, srv)
				require.NoError(t, srv.Shutdown(context.Background()))
				requireNoAliasAccess(t, f)
				assert.Equal(t, before, f.snapshot(t), "start %d", i)
			}
		})
	}
}

// TestP3b_ManagedAuditSecretRefRows: a retained alias that also carries a
// backend reference keeps its local payload authenticated (decrypted and
// validated), and a reference-only alias (no local payload) is counted as
// such without being decrypted; neither is rewritten across a restart.
func TestP3b_ManagedAuditSecretRefRows(t *testing.T) {
	f := newP3Fixture(t)
	k := jkRing(t, "legacy", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	// agent: local payload plus a reference (the external-backend local copy).
	setAliasRow(t, f, SecretKeyAgentSigningKey, aliasCiphertext(t, f, SecretKeyAgentSigningKey), "test-only-ref/agent_signing_key")
	// user: reference only.
	setAliasRow(t, f, SecretKeyUserSigningKey, "", "test-only-ref/user_signing_key")
	before := f.snapshot(t)
	for i := 0; i < 2; i++ {
		srv, err := f.start(t, f.config(k, true))
		require.NoError(t, err, "start %d", i)
		audit := srv.EncryptionAudit()
		require.True(t, audit.Done, "start %d", i)
		assert.Empty(t, audit.Unreadable, "start %d", i)
		assert.Empty(t, audit.BackupUnreadable, "start %d", i)
		assert.Equal(t, 1, audit.ReferenceOnly, "start %d: the reference-only alias", i)
		assert.GreaterOrEqual(t, audit.Readable, 1, "start %d: the referenced alias's local payload", i)
		for _, n := range p3AliasNames {
			assert.Equal(t, i+1, f.c.auditsOf(n), "start %d: %s audit reads", i, n)
		}
		_, reason := readyz(t, srv)
		assert.NotEqual(t, readyReasonBackupUnreadable, reason, "start %d", i)
		require.NoError(t, srv.Shutdown(context.Background()))
		requireNoAliasAccess(t, f)
		assert.Equal(t, before, f.snapshot(t), "start %d", i)
	}
}

// TestP3b_ManagedUnreadableSigningBackupNotReady: an agent_signing_key
// backup the managed ring cannot authenticate (sealed under other material,
// a well-formed v1 envelope with tampered sealed bytes, or a well-formed v2
// envelope naming a key ID the ring holds, with a tampered tag) keeps the
// instance not ready with reason backup_unreadable, is classified as a
// decryption failure (not trusted from its envelope or key ID), and is
// preserved byte- and Version-equal across restarts; the ring still signs
// and verifies, nothing reads or writes the aliases, and no log names key
// material.
func TestP3b_ManagedUnreadableSigningBackupNotReady(t *testing.T) {
	cases := map[string]func(t *testing.T, f *p3Fixture) string{
		"sealed under other encryption material": func(t *testing.T, f *p3Fixture) string {
			v, err := secret.NewLegacyValueCipher(p3bForeignSecret).Encrypt(
				base64.StdEncoding.EncodeToString(p3StaleKey(SecretKeyAgentSigningKey)))
			require.NoError(t, err)
			return v
		},
		"well-formed envelope, tampered sealed bytes": func(t *testing.T, f *p3Fixture) string {
			return tamperV1(t, aliasCiphertext(t, f, SecretKeyAgentSigningKey))
		},
		"well-formed v2 envelope with a known key ID, tampered tag": func(t *testing.T, f *p3Fixture) string {
			sealer, err := secret.NewValueCipher(p3bRing(t, "e2", "v2"))
			require.NoError(t, err)
			v, err := sealer.Encrypt(base64.StdEncoding.EncodeToString(p3StaleKey(SecretKeyAgentSigningKey)))
			require.NoError(t, err)
			return tamperV2(t, v, "e2")
		},
	}
	for name, unreadable := range cases {
		t.Run(name, func(t *testing.T) {
			f := newP3Fixture(t)
			// The server's ring holds e2, so a v2 value under e2 is decrypted,
			// not rejected as an unknown key ID.
			k := p3bRing(t, "legacy", "v1")
			setAliasRow(t, f, SecretKeyAgentSigningKey, unreadable(t, f), "")
			before := f.snapshot(t)
			logs := captureLogs(t)
			for i := 0; i < 2; i++ {
				srv, err := f.start(t, f.config(k, true))
				require.NoError(t, err, "start %d: an unreadable backup holds readiness, it does not refuse startup", i)
				code, reason := readyz(t, srv)
				assert.Equal(t, http.StatusServiceUnavailable, code, "start %d", i)
				assert.Equal(t, readyReasonBackupUnreadable, reason, "start %d", i)
				audit := srv.EncryptionAudit()
				require.Len(t, audit.BackupUnreadable, 1, "start %d", i)
				rec := audit.BackupUnreadable[0]
				assert.Equal(t, SecretKeyAgentSigningKey, rec.Key, "start %d", i)
				assert.NotEmpty(t, rec.Kind, "start %d", i)
				assert.NotEqual(t, auditKindInvalidPayload, rec.Kind, "start %d: the verdict comes from decryption, not the envelope", i)
				assert.Equal(t, i+1, f.c.auditsOf(SecretKeyAgentSigningKey), "start %d", i)
				requireRingSignsAndVerifies(t, srv)
				require.NoError(t, srv.Shutdown(context.Background()))
				requireNoAliasAccess(t, f)
				assert.Equal(t, before, f.snapshot(t), "start %d: the unreadable backup is preserved", i)
			}
			out := logs.String()
			assert.Contains(t, out, readyReasonBackupUnreadable)
			assertNoJWTKeyMaterial(t, out)
			for _, n := range p3AliasNames {
				assert.NotContains(t, out, base64.StdEncoding.EncodeToString(p3StaleKey(n)))
			}
		})
	}
}

// TestP3b_LegacyUnreadableSigningBackupDiagnosticThroughP3Harness: the same
// unreadable agent_signing_key in legacy mode (no keyring file, shared
// signing secret) through phase 3's harness. Beyond what phase 2's legacy
// case asserts (preserved row, readiness not held), it pins the conspicuous
// diagnostic naming the record, and that the audit's read of both aliases
// is attributed apart from legacy mode's own alias accesses: the unreadable
// backup gets no write.
func TestP3b_LegacyUnreadableSigningBackupDiagnosticThroughP3Harness(t *testing.T) {
	f := newP3Fixture(t)
	v, err := secret.NewLegacyValueCipher(p3bForeignSecret).Encrypt(
		base64.StdEncoding.EncodeToString(p3StaleKey(SecretKeyAgentSigningKey)))
	require.NoError(t, err)
	setAliasRow(t, f, SecretKeyAgentSigningKey, v, "")
	before := f.snapshot(t)[SecretKeyAgentSigningKey]
	logs := captureLogs(t)

	cfg := testServerConfig()
	cfg.HubID = p3HubID
	cfg.SecretBackend = f.backend
	cfg.SharedSigningSecret = jkLegacySecret
	srv, err := f.start(t, cfg)
	require.NoError(t, err, "legacy mode keeps serving")

	_, reason := readyz(t, srv)
	assert.NotEqual(t, readyReasonBackupUnreadable, reason, "legacy mode reports, it does not hold readiness")
	audit := srv.EncryptionAudit()
	require.Len(t, audit.BackupUnreadable, 1)
	rec := audit.BackupUnreadable[0]
	assert.Equal(t, SecretKeyAgentSigningKey, rec.Key)
	out := logs.String()
	assert.Contains(t, out, "Encryption audit: UNHEALTHY")
	assert.Contains(t, out, rec.ID)
	assertNoJWTKeyMaterial(t, out)
	assert.NotContains(t, out, base64.StdEncoding.EncodeToString(p3StaleKey(SecretKeyAgentSigningKey)))

	for _, n := range p3AliasNames {
		assert.Equal(t, 1, f.c.auditsOf(n), "%s audit reads", n)
	}
	assert.Zero(t, f.c.writesOf(SecretKeyAgentSigningKey), "the unreadable backup is never written")
	assert.Equal(t, before, f.snapshot(t)[SecretKeyAgentSigningKey], "the unreadable backup is preserved")
}
