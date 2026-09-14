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
