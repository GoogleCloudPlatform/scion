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

// Package keymaterial holds the Hub's shared signing-key bookkeeping that
// both package hub and its key-manager area packages use: the signing-key
// source labels and the overwrite guard for infrastructure backups. It
// imports only leaf packages (pkg/store, pkg/secret), never pkg/hub; package
// hub keeps its historical names for these as aliases.
package keymaterial

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Signing-key source labels (signingKeySource). Labels only: no key
// material is ever recorded or returned.
const (
	SourceConfig        = "config"
	SourceSharedSecret  = "shared_secret"
	SourceSecretBackend = "secret_backend"
	SourceStore         = "store"
	SourceLegacyStore   = "legacy_store"
	SourceGenerated     = "generated"
	SourceEphemeral     = "ephemeral"
	SourceUnresolved    = "unresolved"
	// SourceSharedScope is a key persisted under a fixed scope every node
	// and hub sharing the database reads (not hub-id scoped).
	SourceSharedScope = "shared_scope"
)

// ErrExistingRecordUnreadable refuses a backup or synchronization write
// over an existing record this instance cannot read: an unreadable record
// is preserved, never replaced as a startup repair.
var ErrExistingRecordUnreadable = errors.New("existing stored record is unreadable with the configured encryption keys; refusing to overwrite it")

// GuardOverwrite refuses (ErrExistingRecordUnreadable) a backup or
// synchronization write over an existing record the startup audit flagged
// or that this instance cannot decrypt now. A missing record, or an empty
// reference-only one, may be written.
func GuardOverwrite(ctx context.Context, st store.SecretStore, c *secret.ValueCipher, flagged func(key, scope, scopeID string) bool, key, scope, scopeID string) error {
	if flagged != nil && flagged(key, scope, scopeID) {
		return ErrExistingRecordUnreadable
	}
	if st == nil {
		return nil
	}
	existing, err := st.GetSecret(ctx, key, scope, scopeID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the existing record before overwrite: %w", err)
	}
	if existing.EncryptedValue == "" {
		return nil
	}
	if _, err := c.Decrypt(existing.EncryptedValue); err != nil {
		return ErrExistingRecordUnreadable
	}
	return nil
}
