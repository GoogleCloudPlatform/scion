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

package oidckeys

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The key manager's tests under the shared value cipher: the CAS create and
// the restart round-trip (moved from encryption_ring_test.go with copies of
// their helpers, since they read the manager's unexported state).

// oidcCASTestHubID is the hub ID of these tests (encTestHubID's value).
const oidcCASTestHubID = "enc-test-hub"

// oidcCASV2Cipher is a managed cipher writing enc:v2: under active, holding
// ids (a copy of v2Cipher, kept with the OIDC key manager's tests).
func oidcCASV2Cipher(t *testing.T, active string, ids ...string) *secret.ValueCipher {
	t.Helper()
	return ciphertest.Cipher(t, ciphertest.Ring{Active: active, IDs: ids, WriteFormat: keyring.WriteV2,
		V1Readers: oidcCASKeepIf(ids, keyring.LegacyID)})
}

func oidcCASKeepIf(ids []string, want string) []string {
	out := []string{}
	for _, id := range ids {
		if id == want {
			out = append(out, id)
		}
	}
	return out
}

// oidcCASSealForeign seals plaintext with a cipher whose key the code under
// test does not hold (same key ID, different bytes); a copy of sealForeign.
func oidcCASSealForeign(t *testing.T, kid, plaintext string) string {
	t.Helper()
	foreign := ciphertest.Cipher(t, ciphertest.Ring{Active: kid, IDs: []string{kid},
		Keys: map[string][]byte{kid: []byte("FAKE-FOREIGN-KEY-0000000000000000")[:32]}})
	v, err := foreign.Encrypt(plaintext)
	require.NoError(t, err)
	return v
}

// D2: a CAS loser that cannot decrypt the winner's key fails instead of
// continuing with (and later persisting) its own key.
func TestOIDCCASLoserUndecryptableFails(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	winner, err := GenerateRSAKeyPair()
	require.NoError(t, err)
	winnerPEM, err := EncodePEMPrivateKey(winner)
	require.NoError(t, err)
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{ID: oidcSigningKeySecretID(oidcCASTestHubID), Key: SecretKeyOIDCSigningKey,
		EncryptedValue: oidcCASSealForeign(t, "k2", string(winnerPEM)), Scope: store.ScopeHub, ScopeID: oidcCASTestHubID,
		SecretType: store.SecretTypeInternal}))
	before, err := st.GetSecret(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, oidcCASTestHubID)
	require.NoError(t, err)

	mine, err := GenerateRSAKeyPair()
	require.NoError(t, err)
	minePEM, err := EncodePEMPrivateKey(mine)
	require.NoError(t, err)
	m := &OIDCKeyManager{store: st, hubID: oidcCASTestHubID, log: slog.Default(), cipher: oidcCASV2Cipher(t, "k2", "k2")}
	key, err := m.casCreateKeyInStore(ctx, SecretKeyOIDCSigningKey, string(minePEM), oidcCASTestHubID)
	require.Error(t, err)
	assert.Nil(t, key)
	assert.ErrorIs(t, err, secret.ErrDecrypt)
	assert.NotContains(t, err.Error(), "PRIVATE KEY")
	after, err := st.GetSecret(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, oidcCASTestHubID)
	require.NoError(t, err)
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue)
}

// OIDC acceptance: the RSA key and the keyset round-trip under an
// independent v2 key across a restart, with identical KIDs and JWKS.
func TestOIDCKeyManager_IndependentV2RestartKeepsKIDAndJWKS(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	c := oidcCASV2Cipher(t, "k2", "k2")
	newMgr := func() *OIDCKeyManager {
		m, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{Store: st, HubID: oidcCASTestHubID,
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
		v, err := st.GetSecretValue(ctx, key, store.ScopeHub, oidcCASTestHubID)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(v, "enc:v2:k2:"), "%s is not enc:v2:k2", key)
	}
	m2 := newMgr()
	assert.Equal(t, wantKIDs, kids(m2))
	assert.Equal(t, wantActive, m2.activeKey.KeyID)
}
