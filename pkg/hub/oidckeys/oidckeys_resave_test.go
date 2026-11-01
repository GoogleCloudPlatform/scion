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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/keymaterial"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The OIDC key backup's startup re-save skip (keymaterial.BackupIsCurrent):
// these tests read the manager's unexported state, so they live with it and
// use this package's copies of the shared test helpers (oidckeys_cas_test.go).

// oidcResaveRow is a backup row's value, Version and ref.
type oidcResaveRow struct {
	value   string
	version int
	ref     string
}

// oidcResaveRowOf returns the OIDC signing-key backup row.
func oidcResaveRowOf(t *testing.T, st store.Store) oidcResaveRow {
	t.Helper()
	r, err := st.GetSecret(context.Background(), SecretKeyOIDCSigningKey, store.ScopeHub, oidcCASTestHubID)
	require.NoError(t, err)
	return oidcResaveRow{r.EncryptedValue, r.Version, r.SecretRef}
}

// oidcResaveManager builds an OIDC key manager whose local secret backend
// shares c, so every restart loads through the backend and reaches the
// backup.
func oidcResaveManager(t *testing.T, st store.Store, c *secret.ValueCipher) *OIDCKeyManager {
	t.Helper()
	m, err := NewOIDCKeyManager(context.Background(), OIDCKeyManagerConfig{Store: st, HubID: oidcCASTestHubID,
		IssuerURL: "https://example.com", ValueCipher: c, Backend: secret.NewLocalBackendWithCipher(st, oidcCASTestHubID, c)})
	require.NoError(t, err)
	return m
}

// OIDC acceptance: restarts with an unchanged ring do not re-save the
// OIDC key backup; a new writer re-saves it once, then stops.
func TestOIDCBackupResave(t *testing.T) {
	t.Run("unchanged ring", func(t *testing.T) {
		st := createOIDCTestStore(t)
		c := oidcCASV2Cipher(t, "k2", "k2")
		kid := oidcResaveManager(t, st, c).activeKey.KeyID
		first := oidcResaveRowOf(t, st)
		for i := range 3 {
			m := oidcResaveManager(t, st, c)
			assert.Equal(t, keymaterial.SourceSecretBackend, m.Source(), "restart %d did not load through the backend", i)
			assert.Equal(t, kid, m.activeKey.KeyID)
			assert.Equal(t, first, oidcResaveRowOf(t, st), "restart %d re-saved the OIDC backup", i)
		}
	})
	t.Run("new writer", func(t *testing.T) {
		st := createOIDCTestStore(t)
		kid := oidcResaveManager(t, st, oidcCASV2Cipher(t, "k1", "k1")).activeKey.KeyID
		before := oidcResaveRowOf(t, st)

		next := oidcCASV2Cipher(t, "k2", "k1", "k2")
		m := oidcResaveManager(t, st, next)
		assert.Equal(t, kid, m.activeKey.KeyID)
		resaved := oidcResaveRowOf(t, st)
		assert.Equal(t, before.version+1, resaved.version, "the first restart on the new writer re-saves once")
		assert.True(t, strings.HasPrefix(resaved.value, "enc:v2:k2:"))
		for i := range 2 {
			oidcResaveManager(t, st, next)
			assert.Equal(t, resaved, oidcResaveRowOf(t, st), "restart %d after the re-save wrote the backup", i)
		}
	})
}

// The OIDC skip compares the key: a writer-sealed backup holding another
// PEM key is overwritten; the same key again writes nothing.
func TestOIDCBackupResave_ComparesKey(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()
	c := oidcCASV2Cipher(t, "k2", "k2")
	m := oidcResaveManager(t, st, c)
	before := oidcResaveRowOf(t, st)
	other, err := GenerateRSAKeyPair()
	require.NoError(t, err)
	otherPEM, err := EncodePEMPrivateKey(other)
	require.NoError(t, err)

	require.NoError(t, m.backupKeyToStore(ctx, SecretKeyOIDCSigningKey, string(otherPEM), oidcCASTestHubID))
	after := oidcResaveRowOf(t, st)
	assert.Equal(t, before.version+1, after.version, "a backup of another key must be overwritten")
	pt, ok := c.DecryptWriter(after.value)
	require.True(t, ok)
	assert.Equal(t, string(otherPEM), pt)

	require.NoError(t, m.backupKeyToStore(ctx, SecretKeyOIDCSigningKey, string(otherPEM), oidcCASTestHubID))
	assert.Equal(t, after, oidcResaveRowOf(t, st), "a current backup of the same key was re-saved")
}

// decodePEMKeyBlock accepts exactly one PEM block.
func TestDecodePEMKeyBlock(t *testing.T) {
	k, err := GenerateRSAKeyPair()
	require.NoError(t, err)
	p, err := EncodePEMPrivateKey(k)
	require.NoError(t, err)
	der, ok := decodePEMKeyBlock(string(p))
	require.True(t, ok)
	assert.NotEmpty(t, der)
	_, ok = decodePEMKeyBlock(string(p) + "\n")
	assert.True(t, ok, "trailing whitespace is accepted")
	for name, v := range map[string]string{"empty": "", "not pem": "fake-not-pem", "two blocks": string(p) + string(p)} {
		_, ok := decodePEMKeyBlock(v)
		assert.False(t, ok, name)
	}
}
