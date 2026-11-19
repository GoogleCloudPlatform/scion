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
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// webhookEndpointReservedSecretPrefix is the canonical secret-key prefix
	// reserved for hub-managed webhook endpoint HMAC signing secrets (§4.1, R1).
	webhookEndpointReservedSecretPrefix = "webhook_endpoint."

	// webhookEndpointSanitizedSecretPrefix is the GCP Secret Manager secret-ID
	// sanitized counterpart of webhookEndpointReservedSecretPrefix (where '.'
	// maps to '-' while '_' is preserved, so ordinary user env/secret names
	// like WEBHOOK_ENDPOINT_URL are not matched).
	webhookEndpointSanitizedSecretPrefix = "webhook_endpoint-"
)

func hasWebhookEndpointReservedPrefix(s string) bool {
	sanitized := systemOneKeyNonIDChars.ReplaceAllString(s, "-")
	return strings.HasPrefix(strings.ToLower(sanitized), webhookEndpointSanitizedSecretPrefix)
}

// isWebhookEndpointReservedSecretKey reports whether key falls under the
// reserved "webhook_endpoint." secret-key namespace (including case-folded and
// GCP Secret Manager character-sanitized forms such as "webhook_endpoint-...").
func isWebhookEndpointReservedSecretKey(key string) bool {
	if hasWebhookEndpointReservedPrefix(key) {
		return true
	}
	trimmed := strings.TrimSpace(key)
	return trimmed != key && hasWebhookEndpointReservedPrefix(trimmed)
}

// rejectWebhookEndpointReservedSecretWrite rejects generic secret/env writes to
// the reserved "webhook_endpoint." namespace. Callers run this after
// refuseInternalSecretMutation so mutations of an already-stored
// SecretTypeInternal row continue to answer 409 internal_secret (§4.1, §16.8).
func rejectWebhookEndpointReservedSecretWrite(w http.ResponseWriter, key string) bool {
	if !isWebhookEndpointReservedSecretKey(key) {
		return false
	}
	ValidationError(w, fmt.Sprintf("secret key prefix %q is reserved for webhook endpoint signing secrets", webhookEndpointReservedSecretPrefix), map[string]interface{}{
		"field": "key",
		"value": key,
	})
	return true
}

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
	if s == nil {
		return nil
	}
	return s.cipher
}

// sealWebhookEndpointSecret encrypts plaintext with the Hub's shared
// ValueCipher; a nil cipher fails closed with secret.ErrCipherNotConfigured.
func (s *Server) sealWebhookEndpointSecret(plaintext string) (string, error) {
	return s.webhookSecretCipher().Encrypt(plaintext)
}

// unsealWebhookEndpointSecret decrypts a stored webhook endpoint secret row
// with the Hub's shared ValueCipher; a nil cipher fails closed.
func (s *Server) unsealWebhookEndpointSecret(encrypted string) (string, error) {
	return s.webhookSecretCipher().Decrypt(encrypted)
}

// errWebhookSecretTypeMismatch is returned when a secret row under the
// webhook_endpoint.* key namespace does not carry SecretTypeInternal
// (1b1-rev-2 O3). Ingress fails closed (503) rather than using a non-internal
// secret row for HMAC verification.
var errWebhookSecretTypeMismatch = errors.New("webhook endpoint secret row is not SecretTypeInternal")

// loadWebhookEndpointSecret loads and unseals the hub-scoped SecretTypeInternal
// signing secret row for (projectID, endpointID, version) in a single GetSecret
// query (§4.1, §8.1 step 6).
func (s *Server) loadWebhookEndpointSecret(ctx context.Context, projectID, endpointID string, version int) (string, error) {
	if s == nil || s.store == nil {
		return "", errors.New("hub store is not configured")
	}
	key := store.WebhookEndpointSecretKey(projectID, endpointID, version)
	meta, err := s.store.GetSecret(ctx, key, store.ScopeHub, s.HubID())
	if err != nil {
		return "", err
	}
	if meta == nil || meta.SecretType != store.SecretTypeInternal {
		gotType := ""
		if meta != nil {
			gotType = meta.SecretType
		}
		return "", fmt.Errorf("%w: got %q", errWebhookSecretTypeMismatch, gotType)
	}
	if meta.EncryptedValue == "" {
		return "", errors.New("webhook endpoint secret row has empty value")
	}
	return s.unsealWebhookEndpointSecret(meta.EncryptedValue)
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
