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
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

func (s *Server) setHubSecret(ctx context.Context, name, value, description, userID string) error {
	if s.secretBackend != nil {
		_, _, err := s.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name:          name,
			Value:         value,
			SecretType:    secret.TypeVariable,
			Scope:         store.ScopeHub,
			ScopeID:       s.hubID,
			Description:   description,
			InjectionMode: "as_needed",
			CreatedBy:     userID,
			UpdatedBy:     userID,
		})
		return err
	}

	// With no secret backend the value is stored in the hub's own row. In
	// managed ring mode (a keyring file) it is sealed with the shared
	// ValueCipher, so it satisfies the database encryption policy. Without
	// a keyring file it stays plaintext, as earlier releases wrote it, so a
	// rollback to such a release still reads it; a rewrap converts it.
	stored := value
	if s.cipher.Managed() {
		sealed, err := s.cipher.Encrypt(value)
		if err != nil {
			return err
		}
		stored = sealed
	}
	_, err := s.store.UpsertSecret(ctx, &store.Secret{
		ID:             uuid.NewString(),
		Key:            name,
		EncryptedValue: stored,
		Scope:          store.ScopeHub,
		ScopeID:        s.hubID,
		SecretType:     store.SecretTypeVariable,
		Description:    description,
		Version:        1,
		CreatedBy:      userID,
		UpdatedBy:      userID,
	})
	return err
}

// openHubSecret decodes a hub-scoped secret value that setHubSecret stored
// with no secret backend (the caller reads it with GetSecretValue, so each
// read stays classified at its own call site).
//
//   - A sealed value (enc:v1: or enc:v2:<kid>:, classified by the cipher)
//     is decrypted with the shared ValueCipher. A value that fails to parse
//     or authenticate is an error, never returned as plaintext.
//   - A plaintext value reads through unchanged while the database has no
//     encryption policy, in either mode, so rows written before managed
//     mode, or by a hub without a keyring file, keep working.
//   - Once an encryption policy exists, a plaintext value is refused with
//     secret.ErrPlaintextNotAdmitted; a rewrap under migration admission
//     converts it.
//
// A server without a cipher (bare test servers) returns the stored value.
func (s *Server) openHubSecret(ctx context.Context, stored string) (string, error) {
	if stored == "" || s.cipher == nil {
		return stored, nil
	}
	env, err := s.cipher.Inspect(stored)
	if err != nil {
		return "", err
	}
	if env.Format != secretenvelope.FormatPlaintext {
		return s.cipher.Decrypt(stored)
	}
	if _, err := s.store.GetEncryptionPolicy(ctx); err == nil {
		return "", secret.ErrPlaintextNotAdmitted
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return stored, nil
}
