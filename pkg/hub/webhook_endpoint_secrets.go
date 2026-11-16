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

package hub

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// webhookEndpointSecretBytes is the entropy of a generated webhook endpoint
// HMAC secret (32 bytes = 256 bits, encoded as unpadded base64url).
const webhookEndpointSecretBytes = 32

// generateWebhookEndpointSecret returns a fresh 32-byte random secret encoded
// with base64.RawURLEncoding.
func generateWebhookEndpointSecret() (string, error) {
	buf := make([]byte, webhookEndpointSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate webhook endpoint secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Server) webhookSecretCipher() *secret.ValueCipher {
	if s != nil && s.cipher != nil {
		return s.cipher
	}
	return secret.NewLegacyValueCipher("")
}

// sealWebhookEndpointSecret encrypts plaintext with the Hub's shared
// ValueCipher, falling back to plaintext only when Hub encryption is unmanaged.
func (s *Server) sealWebhookEndpointSecret(plaintext string) (string, error) {
	return s.webhookSecretCipher().Encrypt(plaintext)
}

// unsealWebhookEndpointSecret decrypts a stored webhook endpoint secret row.
func (s *Server) unsealWebhookEndpointSecret(encrypted string) (string, error) {
	return s.webhookSecretCipher().Decrypt(encrypted)
}

// newWebhookEndpointSecretRow constructs the hub-scoped SecretTypeInternal row
// stored in the secrets table for a webhook endpoint signing secret version.
func (s *Server) newWebhookEndpointSecretRow(projectID, endpointID string, version int, plaintext, createdBy string) (*store.Secret, error) {
	sealed, err := s.sealWebhookEndpointSecret(plaintext)
	if err != nil {
		return nil, err
	}
	return &store.Secret{
		ID:             api.NewUUID(),
		Key:            store.WebhookEndpointSecretKey(projectID, endpointID, version),
		EncryptedValue: sealed,
		Scope:          store.ScopeHub,
		ScopeID:        s.hubID,
		SecretType:     store.SecretTypeInternal,
		Description:    "Webhook endpoint signing secret",
		CreatedBy:      createdBy,
		UpdatedBy:      createdBy,
	}, nil
}
