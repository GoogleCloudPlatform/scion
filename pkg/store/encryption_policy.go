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

package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
)

// ErrEncryptionPolicyMismatch refuses a payload write the database
// encryption policy does not allow (a stale writer key, an older envelope
// or plaintext). It is never followed by an unencrypted fallback write.
var ErrEncryptionPolicyMismatch = errors.New("encryption_policy_mismatch")

// EncryptionPolicyState is the state of the database encryption policy.
type EncryptionPolicyState string

const (
	// EncryptionPolicyPrepared: writers are fenced to the target key;
	// re-encryption has not started.
	EncryptionPolicyPrepared EncryptionPolicyState = "prepared"
	// EncryptionPolicyRewrapping: a migration is re-encrypting rows.
	EncryptionPolicyRewrapping EncryptionPolicyState = "rewrapping"
	// EncryptionPolicyVerified: a full scan verified every payload.
	EncryptionPolicyVerified EncryptionPolicyState = "verified"
)

// EncryptionPolicyMinReaderVersion is the envelope version every reader
// of a policy-enabled database must support.
const EncryptionPolicyMinReaderVersion = 2

// EncryptionPolicy is the singleton database encryption policy (table
// hub_encryption_policy, id 1). It holds key IDs only, never key material.
type EncryptionPolicy struct {
	Generation       int64
	MinReaderVersion int
	AllowedWriteKIDs []string
	RequiredReadKIDs []string
	MigrationID      string // "" when no migration is bound
	State            EncryptionPolicyState
	UpdatedAt        time.Time
}

// Validate checks the policy's shape: a positive generation, the minimum
// reader version, at least one allowed writer, valid unique key IDs, a
// known state.
func (p *EncryptionPolicy) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: nil encryption policy", ErrInvalidInput)
	}
	if p.Generation <= 0 {
		return fmt.Errorf("%w: encryption policy generation must be positive", ErrInvalidInput)
	}
	if p.MinReaderVersion != EncryptionPolicyMinReaderVersion {
		return fmt.Errorf("%w: encryption policy min_reader_version must be %d", ErrInvalidInput, EncryptionPolicyMinReaderVersion)
	}
	if len(p.AllowedWriteKIDs) == 0 {
		return fmt.Errorf("%w: encryption policy needs at least one allowed writer key ID", ErrInvalidInput)
	}
	if err := validKIDList(p.AllowedWriteKIDs, "allowed_write_kids"); err != nil {
		return err
	}
	if err := validKIDList(p.RequiredReadKIDs, "required_read_kids"); err != nil {
		return err
	}
	switch p.State {
	case EncryptionPolicyPrepared, EncryptionPolicyRewrapping, EncryptionPolicyVerified:
	default:
		return fmt.Errorf("%w: unknown encryption policy state", ErrInvalidInput)
	}
	return nil
}

func validKIDList(ids []string, field string) error {
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !secretenvelope.ValidKID(id) {
			return fmt.Errorf("%w: encryption policy %s[%d] is not a valid key ID", ErrInvalidInput, field, i)
		}
		if seen[id] {
			return fmt.Errorf("%w: encryption policy %s[%d] is a duplicate", ErrInvalidInput, field, i)
		}
		seen[id] = true
	}
	return nil
}

// AllowsWrite reports whether a payload write of value (with secretRef)
// passes the policy fence: an enc:v2: value under an allowed writer key
// ID, or an empty reference-only value with a non-empty SecretRef. A nil
// policy (no policy row) allows every write.
func (p *EncryptionPolicy) AllowsWrite(value, secretRef string) bool {
	if p == nil {
		return true
	}
	if value == "" {
		return secretRef != ""
	}
	env, err := secretenvelope.Parse(value)
	if err != nil || env.Format != secretenvelope.FormatV2 {
		return false
	}
	for _, id := range p.AllowedWriteKIDs {
		if id == env.KID {
			return true
		}
	}
	return false
}

// SecretPayload is one row of ScanSecretPayloads: identity, logical
// Version, the stored payload and the backend reference. EncryptedValue is
// ciphertext (or legacy plaintext) and must never be logged or returned.
type SecretPayload struct {
	ID             string
	Key            string
	Scope          string
	ScopeID        string
	Version        int
	EncryptedValue string `json:"-"`
	SecretRef      string
}

// EncryptionMigrationState is the state of an encryption migration job.
type EncryptionMigrationState string

const (
	// EncryptionMigrationRunning: the designated runner holds the job.
	EncryptionMigrationRunning EncryptionMigrationState = "running"
	// EncryptionMigrationInterrupted: the job stopped and can be resumed
	// explicitly by its ID.
	EncryptionMigrationInterrupted EncryptionMigrationState = "interrupted"
	// EncryptionMigrationVerified: the final full scan passed.
	EncryptionMigrationVerified EncryptionMigrationState = "verified"
)

// EncryptionMigration is one encryption migration job (table
// hub_encryption_migrations). Counts and the cursor are progress only;
// they never substitute for a final full scan. It holds no payloads.
type EncryptionMigration struct {
	ID                    string // UUID
	SourceGeneration      int64
	TargetKID             string
	State                 EncryptionMigrationState
	LastScannedID         string
	PlaintextReadAdmitted bool
	Scanned               int64
	Rewritten             int64
	Failures              int64
	Created               time.Time
	Updated               time.Time
}

// Validate checks the record's shape.
func (m *EncryptionMigration) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil encryption migration", ErrInvalidInput)
	}
	if m.ID == "" {
		return fmt.Errorf("%w: encryption migration needs an ID", ErrInvalidInput)
	}
	if !secretenvelope.ValidKID(m.TargetKID) {
		return fmt.Errorf("%w: encryption migration target_kid is not a valid key ID", ErrInvalidInput)
	}
	switch m.State {
	case EncryptionMigrationRunning, EncryptionMigrationInterrupted, EncryptionMigrationVerified:
	default:
		return fmt.Errorf("%w: unknown encryption migration state", ErrInvalidInput)
	}
	if m.Scanned < 0 || m.Rewritten < 0 || m.Failures < 0 {
		return fmt.Errorf("%w: encryption migration counts must not be negative", ErrInvalidInput)
	}
	return nil
}
