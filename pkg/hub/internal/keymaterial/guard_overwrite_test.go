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

package keymaterial

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	guardTestKey     = "guard_test_key"
	guardTestScope   = "hub"
	guardTestScopeID = "guard-test-hub"
	guardTestKID     = "k1"
)

// guardSecretStore answers GetSecret from one canned result and counts the
// reads. GuardOverwrite only reads; any other method panics through the nil
// embedded interface.
type guardSecretStore struct {
	store.SecretStore
	secret *store.Secret
	err    error
	reads  int
}

func (s *guardSecretStore) GetSecret(_ context.Context, key, scope, scopeID string) (*store.Secret, error) {
	s.reads++
	if key != guardTestKey || scope != guardTestScope || scopeID != guardTestScopeID {
		return nil, errors.New("unexpected secret coordinates")
	}
	return s.secret, s.err
}

// guardCipher is a managed cipher holding guardTestKID.
func guardCipher(t *testing.T) *secret.ValueCipher {
	t.Helper()
	return ciphertest.Cipher(t, ciphertest.Ring{Active: guardTestKID, IDs: []string{guardTestKID}, WriteFormat: keyring.WriteV2})
}

// guardSealForeign seals plaintext under the same key ID with different key
// bytes, so guardCipher sees a well-formed record it cannot decrypt.
func guardSealForeign(t *testing.T, plaintext string) string {
	t.Helper()
	foreign := ciphertest.Cipher(t, ciphertest.Ring{Active: guardTestKID, IDs: []string{guardTestKID}, WriteFormat: keyring.WriteV2,
		Keys: map[string][]byte{guardTestKID: ciphertest.FakeKey("foreign")}})
	v, err := foreign.Encrypt(plaintext)
	require.NoError(t, err)
	return v
}

func TestGuardOverwrite(t *testing.T) {
	ctx := context.Background()
	c := guardCipher(t)
	readable, err := c.Encrypt("existing key material")
	require.NoError(t, err)
	unreadable := guardSealForeign(t, "existing key material")
	// Input assertion: the foreign record is well formed but this cipher
	// cannot decrypt it, so the case below exercises the decrypt-failure
	// branch rather than a parse failure of a malformed value.
	_, err = c.Decrypt(unreadable)
	require.Error(t, err, "the foreign record must not decrypt with the test cipher")
	_, err = c.Decrypt(readable)
	require.NoError(t, err, "control: the readable record decrypts with the test cipher")

	readErr := errors.New("store unavailable")
	never := func(string, string, string) bool { return false }

	tests := []struct {
		name      string
		store     *guardSecretStore
		flagged   func(key, scope, scopeID string) bool
		wantErr   error // nil: the write is allowed
		wantOther bool  // an error that is not ErrExistingRecordUnreadable
		wantReads int
	}{
		{name: "missing record may be written", store: &guardSecretStore{err: store.ErrNotFound}, flagged: never, wantReads: 1},
		{name: "reference-only record may be written", store: &guardSecretStore{secret: &store.Secret{Key: guardTestKey}}, flagged: never, wantReads: 1},
		{name: "readable record may be overwritten", store: &guardSecretStore{secret: &store.Secret{Key: guardTestKey, EncryptedValue: readable}}, flagged: never, wantReads: 1},
		{name: "undecryptable record is refused", store: &guardSecretStore{secret: &store.Secret{Key: guardTestKey, EncryptedValue: unreadable}}, flagged: never,
			wantErr: ErrExistingRecordUnreadable, wantReads: 1},
		{name: "undecryptable record is refused with no audit callback", store: &guardSecretStore{secret: &store.Secret{Key: guardTestKey, EncryptedValue: unreadable}},
			wantErr: ErrExistingRecordUnreadable, wantReads: 1},
		{name: "audit-flagged record is refused without a read", store: &guardSecretStore{secret: &store.Secret{Key: guardTestKey, EncryptedValue: readable}},
			flagged: func(key, scope, scopeID string) bool {
				return key == guardTestKey && scope == guardTestScope && scopeID == guardTestScopeID
			}, wantErr: ErrExistingRecordUnreadable, wantReads: 0},
		{name: "store read error is returned, not treated as unreadable", store: &guardSecretStore{err: readErr}, flagged: never, wantOther: true, wantReads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := GuardOverwrite(ctx, tt.store, c, tt.flagged, guardTestKey, guardTestScope, guardTestScopeID)
			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantOther:
				require.Error(t, err)
				assert.ErrorIs(t, err, readErr)
				assert.NotErrorIs(t, err, ErrExistingRecordUnreadable)
			default:
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantReads, tt.store.reads, "store reads")
		})
	}

	t.Run("no store allows the write", func(t *testing.T) {
		assert.NoError(t, GuardOverwrite(ctx, nil, c, never, guardTestKey, guardTestScope, guardTestScopeID))
	})
}
