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
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubSecretWritersUseSharedBackendContract(t *testing.T) {
	tests := []struct {
		name      string
		secretKey string
		write     func(*Server) error
	}{
		{
			name:      "chat integration",
			secretKey: "CHAT_TEST_SECRET",
			write: func(s *Server) error {
				return s.SetChatIntegrationSecret(t.Context(), "CHAT_TEST_SECRET", "secret-value", "test description", "test-user")
			},
		},
		{
			name:      "GitHub App",
			secretKey: GitHubAppSecretPrivateKey,
			write: func(s *Server) error {
				return s.setGitHubAppSecret(t.Context(), GitHubAppSecretPrivateKey, "secret-value", "test description", "test-user")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := createTestStore(t)
			backend := secret.NewLocalBackend(st, "test-hub", "test-encryption-key")
			srv := &Server{store: st, hubID: "test-hub", secretBackend: backend}

			require.NoError(t, tt.write(srv))
			stored, err := backend.Get(t.Context(), tt.secretKey, store.ScopeHub, "test-hub")
			require.NoError(t, err)
			assert.Equal(t, "secret-value", stored.Value)
			assert.Equal(t, secret.TypeVariable, stored.SecretType)
			assert.Equal(t, "test description", stored.Description)
			assert.Equal(t, "as_needed", stored.InjectionMode)
			assert.Equal(t, "test-user", stored.CreatedBy)
			assert.Equal(t, "test-user", stored.UpdatedBy)
		})
	}
}

func TestHubSecretWritersUseValidFallbackIDs(t *testing.T) {
	tests := []struct {
		name      string
		secretKey string
		write     func(*Server) error
	}{
		{
			name:      "chat integration",
			secretKey: "CHAT_TEST_SECRET",
			write: func(s *Server) error {
				return s.SetChatIntegrationSecret(t.Context(), "CHAT_TEST_SECRET", "secret-value", "test description", "test-user")
			},
		},
		{
			name:      "GitHub App",
			secretKey: GitHubAppSecretPrivateKey,
			write: func(s *Server) error {
				return s.setGitHubAppSecret(t.Context(), GitHubAppSecretPrivateKey, "secret-value", "test description", "test-user")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := createTestStore(t)
			srv := &Server{store: st, hubID: "test-hub"}

			require.NoError(t, tt.write(srv))
			stored, err := st.GetSecret(t.Context(), tt.secretKey, store.ScopeHub, "test-hub")
			require.NoError(t, err)
			assert.NoError(t, uuid.Validate(stored.ID))
			firstID := stored.ID
			assert.Equal(t, "secret-value", stored.EncryptedValue)
			assert.Equal(t, store.SecretTypeVariable, stored.SecretType)
			assert.Equal(t, "test description", stored.Description)
			assert.Equal(t, "test-user", stored.CreatedBy)
			assert.Equal(t, "test-user", stored.UpdatedBy)

			require.NoError(t, tt.write(srv))
			updated, err := st.GetSecret(t.Context(), tt.secretKey, store.ScopeHub, "test-hub")
			require.NoError(t, err)
			assert.Equal(t, firstID, updated.ID)
		})
	}
}

// noBackendHubSecrets are the hub secret writers and readers that use the
// hub's own row when no secret backend is configured.
var noBackendHubSecrets = []struct {
	name  string
	key   string
	write func(*Server, string) error
	read  func(*Server) (string, error)
}{
	{
		name: "chat integration",
		key:  "CHAT_TEST_SECRET",
		write: func(s *Server, v string) error {
			return s.SetChatIntegrationSecret(context.Background(), "CHAT_TEST_SECRET", v, "test description", "test-user")
		},
		read: func(s *Server) (string, error) {
			return s.LoadChatIntegrationSecret(context.Background(), "CHAT_TEST_SECRET")
		},
	},
	{
		name: "GitHub App",
		key:  GitHubAppSecretPrivateKey,
		write: func(s *Server, v string) error {
			return s.setGitHubAppSecret(context.Background(), GitHubAppSecretPrivateKey, v, "test description", "test-user")
		},
		read: func(s *Server) (string, error) {
			return s.loadGitHubAppSecret(context.Background(), GitHubAppSecretPrivateKey)
		},
	},
}

// legacyServerNoBackend builds a legacy-mode (no keyring file) hub with no
// secret backend.
func legacyServerNoBackend(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.HubID = encTestHubID
	cfg.SharedSigningSecret = "FAKE-legacy-secret-no-backend"
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err)
	require.False(t, srv.cipher.Managed())
	return srv
}

// seedHubSecretRow writes a no-backend hub secret row directly, as an
// earlier release (or a tampering writer) would have stored it.
func seedHubSecretRow(t *testing.T, st store.Store, key, value string) {
	t.Helper()
	_, err := st.UpsertSecret(context.Background(), &store.Secret{
		ID: uuid.NewString(), Key: key, EncryptedValue: value,
		Scope: store.ScopeHub, ScopeID: encTestHubID, SecretType: store.SecretTypeVariable, Version: 1,
	})
	require.NoError(t, err)
}

func storedHubSecret(t *testing.T, st store.Store, key string) string {
	t.Helper()
	v, err := st.GetSecretValue(context.Background(), key, store.ScopeHub, encTestHubID)
	require.NoError(t, err)
	return v
}

// activatePolicy stores a prepared encryption policy admitting writer k2.
func activatePolicy(t *testing.T, st store.Store) {
	t.Helper()
	applied, err := st.UpdateEncryptionPolicy(context.Background(), 0, store.EncryptionPolicy{
		Generation: 1, MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{"k2"}, RequiredReadKIDs: []string{"k2"}, State: store.EncryptionPolicyPrepared,
	})
	require.NoError(t, err)
	require.True(t, applied)
}

// In managed ring mode with no secret backend, a hub secret is stored
// sealed under the active writer and reads back.
func TestHubSecretNoBackend_RingModeEncryptsAndReadsBack(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := encServer(t, st, v2Cipher(t, "k2", "k2"), false)
			require.Nil(t, srv.secretBackend)

			require.NoError(t, tt.write(srv, "FAKE-hub-secret-value"))
			stored := storedHubSecret(t, st, tt.key)
			assert.True(t, strings.HasPrefix(stored, "enc:v2:k2:"), "stored value must be sealed under the writer")
			assert.NotContains(t, stored, "FAKE-hub-secret-value")

			got, err := tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-hub-secret-value", got)
		})
	}
}

// Under an encryption policy the ring-mode write succeeds (enc:v2: under
// an allowed writer) instead of failing the policy fence with plaintext.
func TestHubSecretNoBackend_RingModeWriteSatisfiesPolicy(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			activatePolicy(t, st)
			srv := encServer(t, st, v2Cipher(t, "k2", "k2"), false)

			require.NoError(t, tt.write(srv, "FAKE-hub-secret-value"))
			assert.True(t, strings.HasPrefix(storedHubSecret(t, st, tt.key), "enc:v2:k2:"))
			got, err := tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-hub-secret-value", got)

			// The fence itself still refuses a plaintext write.
			_, err = st.UpsertSecret(context.Background(), &store.Secret{
				ID: uuid.NewString(), Key: tt.key, EncryptedValue: "FAKE-plaintext",
				Scope: store.ScopeHub, ScopeID: encTestHubID, SecretType: store.SecretTypeVariable, Version: 1,
			})
			assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		})
	}
}

// A plaintext row (written before managed mode) reads through in ring mode
// while no encryption policy exists, and is refused once one does.
func TestHubSecretNoBackend_RingModePlaintextRowAdmittedOnlyWithoutPolicy(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := encServer(t, st, v2Cipher(t, "k2", "k2"), false)
			seedHubSecretRow(t, st, tt.key, "FAKE-pre-existing-plaintext")

			got, err := tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-pre-existing-plaintext", got)

			activatePolicy(t, st)
			got, err = tt.read(srv)
			require.ErrorIs(t, err, secret.ErrPlaintextNotAdmitted)
			assert.Empty(t, got)
			assert.NotContains(t, err.Error(), "FAKE-pre-existing-plaintext")
		})
	}
}

// A value that looks sealed but fails to parse or authenticate is an
// error, never returned as plaintext, in either mode.
func TestHubSecretNoBackend_CorruptEnvelopeIsAnError(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := encServer(t, st, v2Cipher(t, "k2", "k2"), false)
			require.NoError(t, tt.write(srv, "FAKE-hub-secret-value"))
			sealed := storedHubSecret(t, st, tt.key)

			// Flip one character of the sealed body (after "enc:v2:k2:").
			body := []byte(sealed)
			i := len("enc:v2:k2:") + 4
			if body[i] == 'A' {
				body[i] = 'B'
			} else {
				body[i] = 'A'
			}
			for name, v := range map[string]string{
				"tampered ciphertext": string(body),
				"malformed envelope":  "enc:v2:no-separator",
				"unknown key ID":      sealForeign(t, "k9", "FAKE-hub-secret-value"),
			} {
				seedHubSecretRow(t, st, tt.key, v)
				got, err := tt.read(srv)
				assert.Error(t, err, name)
				assert.Empty(t, got, name)
			}

			legacy := legacyServerNoBackend(t, encStore(t))
			seedHubSecretRow(t, legacy.store, tt.key, string(body))
			got, err := tt.read(legacy)
			assert.Error(t, err, "legacy mode: tampered envelope")
			assert.Empty(t, got)
		})
	}
}

// Legacy (no keyring file) mode keeps the plaintext write, and plaintext
// rows written before this change read back unchanged with no policy.
func TestHubSecretNoBackend_LegacyModeStaysPlaintext(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := legacyServerNoBackend(t, st)
			require.Nil(t, srv.secretBackend)

			seedHubSecretRow(t, st, tt.key, "FAKE-pre-existing-plaintext")
			got, err := tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-pre-existing-plaintext", got)

			require.NoError(t, tt.write(srv, "FAKE-hub-secret-value"))
			assert.Equal(t, "FAKE-hub-secret-value", storedHubSecret(t, st, tt.key))
			got, err = tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-hub-secret-value", got)
		})
	}
}

// Legacy mode decrypts a no-backend row stored sealed under the legacy
// key (for example by an earlier managed-mode hub, then rolled back to no
// keyring file) instead of handing the ciphertext to the consumer.
func TestHubSecretNoBackend_LegacyModeDecryptsSealedRow(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := legacyServerNoBackend(t, st)
			sealed, err := srv.cipher.Encrypt("FAKE-hub-secret-value")
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(sealed, "enc:"), "fixture must be a sealed envelope")
			seedHubSecretRow(t, st, tt.key, sealed)

			got, err := tt.read(srv)
			require.NoError(t, err)
			assert.Equal(t, "FAKE-hub-secret-value", got)
		})
	}
}

// errPolicyReadFailed is the injected policy-read failure.
var errPolicyReadFailed = errors.New("injected policy read failure")

// policyReadFailingStore fails encryption policy reads with
// errPolicyReadFailed while its fault switch is armed; otherwise, and for
// every other method, it delegates to the embedded store.
type policyReadFailingStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (w *policyReadFailingStore) GetEncryptionPolicy(ctx context.Context) (*store.EncryptionPolicy, error) {
	if !w.fault.Active() {
		return w.Store.GetEncryptionPolicy(ctx)
	}
	return nil, errPolicyReadFailed
}

// When the encryption policy cannot be read (any error but not found), a
// plaintext row is refused rather than read through: the read fails closed.
func TestHubSecretNoBackend_PolicyReadErrorRefusesPlaintext(t *testing.T) {
	for _, tt := range noBackendHubSecrets {
		t.Run(tt.name, func(t *testing.T) {
			st := encStore(t)
			srv := encServer(t, st, v2Cipher(t, "k2", "k2"), false)
			waitUserScopedDataSweep(t, srv)
			_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *policyReadFailingStore {
				return &policyReadFailingStore{Store: inner, fault: f}
			})
			seedHubSecretRow(t, st, tt.key, "FAKE-pre-existing-plaintext")
			got, err := tt.read(srv)
			require.NoError(t, err, "disarmed, the policy read delegates (no policy: plaintext reads through)")
			assert.Equal(t, "FAKE-pre-existing-plaintext", got)

			fault.Arm()
			got, err = tt.read(srv)
			require.ErrorIs(t, err, errPolicyReadFailed)
			assert.Empty(t, got)
		})
	}
}
