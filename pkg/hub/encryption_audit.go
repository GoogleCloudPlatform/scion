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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Startup encryption checks (ptone/scion#1530 §5, §6): before any key
// synchronization or backup write, New checks the local ring against the
// database encryption policy and performs an authenticated read of every
// stored payload, validating the structured infrastructure backups.

// readyReasonBackupUnreadable is the /readyz reason of a managed (keyring
// file) instance whose startup audit found an infrastructure backup it
// cannot read. It names no record, key or ID.
const readyReasonBackupUnreadable = "backup_unreadable"

// auditKindInvalidPayload classifies a record that decrypts but whose
// infrastructure payload does not parse.
const auditKindInvalidPayload = "invalid_payload"

// errExistingRecordUnreadable refuses a backup or synchronization write
// over an existing record this instance cannot read: an unreadable record
// is preserved, never replaced as a startup repair.
var errExistingRecordUnreadable = errors.New("existing stored record is unreadable with the configured encryption keys; refusing to overwrite it")

// AuditRecord identifies one unreadable stored record: identity and a
// stable failure classification, never a payload.
type AuditRecord struct {
	ID      string
	Key     string
	Scope   string
	ScopeID string
	Kind    string
}

// EncryptionAudit is the aggregate result of the startup encryption audit.
// It holds counts, formats, key IDs and record identities only.
type EncryptionAudit struct {
	// Done is false when no audit ran (no store).
	Done bool
	// Scanned counts every row; ReferenceOnly the empty rows with a
	// backend reference, which hold no local payload and are not read.
	Scanned, ReferenceOnly, Readable int
	// ByFormat counts readable and unreadable payloads by envelope format
	// ("plaintext", "v1", "v2", "invalid").
	ByFormat map[string]int
	// ByKID counts v2 payloads by key ID.
	ByKID map[string]int
	// Unreadable lists every record that failed, infrastructure included.
	Unreadable []AuditRecord
	// BackupUnreadable lists the infrastructure backups among them; in
	// managed mode any entry keeps the instance not ready.
	BackupUnreadable []AuditRecord
}

func (a EncryptionAudit) clone() EncryptionAudit {
	c := a
	c.ByFormat = make(map[string]int, len(a.ByFormat))
	for k, v := range a.ByFormat {
		c.ByFormat[k] = v
	}
	c.ByKID = make(map[string]int, len(a.ByKID))
	for k, v := range a.ByKID {
		c.ByKID[k] = v
	}
	c.Unreadable = slices.Clone(a.Unreadable)
	c.BackupUnreadable = slices.Clone(a.BackupUnreadable)
	return c
}

// EncryptionAudit returns a copy of the startup encryption audit (for the
// authenticated key status operation of a later phase).
func (s *Server) EncryptionAudit() EncryptionAudit {
	s.encryptionAuditMu.RLock()
	defer s.encryptionAuditMu.RUnlock()
	return s.encryptionAudit.clone()
}

// infraPayloadValidators validates the decrypted payload of each
// infrastructure backup key (hub scope, any scope ID).
var infraPayloadValidators = map[string]func(string) error{
	SecretKeyAgentSigningKey:       validateSigningKeyPayload,
	SecretKeyUserSigningKey:        validateSigningKeyPayload,
	SecretKeyDownloadSigningKey:    validateSigningKeyPayload,
	SecretKeyArtifactViewKey:       validateSigningKeyPayload,
	SecretKeyAttachmentDeliveryKey: validateSigningKeyPayload,
	SecretKeyListCursorKey:         validateSigningKeyPayload,
	SecretKeyOIDCSigningKey:        validateOIDCKeyPayload,
	SecretKeyOIDCKeyset:            validateOIDCKeysetPayload,
	conduitGrantKeySecretName:      validateGrantRingPayload,
}

func isInfraBackup(key, scope string) bool {
	if scope != store.ScopeHub {
		return false
	}
	_, ok := infraPayloadValidators[key]
	return ok
}

func validateSigningKeyPayload(v string) error {
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(k) == 0 {
		return errors.New("signing key backup is not non-empty base64")
	}
	return nil
}

func validateOIDCKeyPayload(v string) error {
	if _, err := decodePEMPrivateKey([]byte(v)); err != nil {
		return errors.New("OIDC key backup is not an RSA private key PEM")
	}
	return nil
}

func validateOIDCKeysetPayload(v string) error {
	var records []oidcKeyRecord
	if err := json.Unmarshal([]byte(v), &records); err != nil {
		return errors.New("OIDC keyset backup is not keyset JSON")
	}
	for i, rec := range records {
		if _, err := decodePEMPrivateKey([]byte(rec.PrivateKeyPEM)); err != nil {
			return fmt.Errorf("OIDC keyset entry %d is not an RSA private key PEM", i)
		}
	}
	return nil
}

func validateGrantRingPayload(v string) error {
	var ring grant.KeyRing
	if err := json.Unmarshal([]byte(v), &ring); err != nil {
		return errors.New("grant key ring backup is not ring JSON")
	}
	if err := ring.Validate(); err != nil {
		return errors.New("grant key ring backup does not validate")
	}
	return nil
}

// checkEncryptionPolicy compares the local cipher with the database
// encryption policy (§6). Without a policy any instance may start. With
// one, the instance must be ring-aware (a keyring file), write enc:v2:
// under an allowed writer ID, and hold every required reader ID; otherwise
// startup is refused, so an incompatible instance never becomes ready.
// Errors name key IDs and the generation only.
func checkEncryptionPolicy(ctx context.Context, st store.SecretEncryptionStore, c *secret.ValueCipher) error {
	p, err := st.GetEncryptionPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the database encryption policy: %w", err)
	}
	if !c.Managed() {
		return fmt.Errorf("%w: the database encryption policy (generation %d) requires a keyring file; legacy-mode startup is refused",
			store.ErrEncryptionPolicyMismatch, p.Generation)
	}
	if c.WriteFormat() != keyring.WriteV2 || !slices.Contains(p.AllowedWriteKIDs, c.WriterID()) {
		return fmt.Errorf("%w: this instance writes with key %q (%s) but the database encryption policy (generation %d) allows only enc:v2 writers %v",
			store.ErrEncryptionPolicyMismatch, c.WriterID(), c.WriteFormat(), p.Generation, p.AllowedWriteKIDs)
	}
	readers := c.ReaderIDs()
	var missing []string
	for _, id := range p.RequiredReadKIDs {
		if !slices.Contains(readers, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: the database encryption policy (generation %d) requires reader keys %v that this instance's keyring lacks",
			store.ErrEncryptionPolicyMismatch, p.Generation, missing)
	}
	return nil
}

// runEncryptionAudit authenticates every stored payload with c and
// validates the infrastructure backups. Reference-only rows (empty value,
// non-empty ref) hold no local payload and are skipped. A scan failure is
// returned; record failures are reported in the result.
func runEncryptionAudit(ctx context.Context, st store.SecretStore, c *secret.ValueCipher) (EncryptionAudit, error) {
	a := EncryptionAudit{Done: true, ByFormat: map[string]int{}, ByKID: map[string]int{}}
	after := ""
	for {
		page, err := st.ScanSecretPayloads(ctx, after, store.MaxSecretPayloadScan)
		if err != nil {
			return EncryptionAudit{}, fmt.Errorf("scanning stored secret payloads: %w", err)
		}
		for _, p := range page {
			a.Scanned++
			if p.EncryptedValue == "" && p.SecretRef != "" {
				a.ReferenceOnly++
				continue
			}
			format := "invalid"
			if env, err := c.Inspect(p.EncryptedValue); err == nil {
				format = env.Format.String()
				if env.KID != "" {
					a.ByKID[env.KID]++
				}
			}
			a.ByFormat[format]++
			kind := ""
			plain, err := c.Decrypt(p.EncryptedValue)
			if err != nil {
				kind = secret.DecryptFailureKind(err)
			} else if v, ok := infraPayloadValidators[p.Key]; ok && p.Scope == store.ScopeHub {
				if v(plain) != nil {
					kind = auditKindInvalidPayload
				}
			}
			if kind == "" {
				a.Readable++
				continue
			}
			rec := AuditRecord{ID: p.ID, Key: p.Key, Scope: p.Scope, ScopeID: p.ScopeID, Kind: kind}
			a.Unreadable = append(a.Unreadable, rec)
			if isInfraBackup(p.Key, p.Scope) {
				a.BackupUnreadable = append(a.BackupUnreadable, rec)
			}
		}
		if len(page) < store.MaxSecretPayloadScan {
			return a, nil
		}
		after = page[len(page)-1].ID
	}
}

// startupEncryptionChecks runs the policy check and the audit in New,
// before any key synchronization or backup write, and records the audit.
// A managed instance with an unreadable backup stays not ready
// (backupNotReadyReason); a legacy instance logs a conspicuous unhealthy
// diagnostic and keeps serving (maintainer decision 4).
func (s *Server) startupEncryptionChecks(ctx context.Context, st store.Store) error {
	if st == nil {
		return nil
	}
	if err := checkEncryptionPolicy(ctx, st, s.cipher); err != nil {
		return err
	}
	audit, err := runEncryptionAudit(ctx, st, s.cipher)
	if err != nil {
		return err
	}
	s.encryptionAuditMu.Lock()
	s.encryptionAudit = audit
	s.encryptionAuditMu.Unlock()
	if len(audit.Unreadable) == 0 {
		return nil
	}
	refs := auditRecordIDs(audit.Unreadable, auditLogMaxRecords)
	if len(audit.BackupUnreadable) > 0 {
		if s.cipher.Managed() {
			slog.Error("Encryption audit: infrastructure backups are unreadable; this instance is NOT READY (backup_unreadable). "+
				"Restore the original encryption key material to the keyring and restart; the records are preserved, never replaced",
				"backup_unreadable", len(audit.BackupUnreadable), "unreadable_total", len(audit.Unreadable), "records", refs)
		} else {
			slog.Error("Encryption audit: UNHEALTHY: infrastructure backups are unreadable with the configured server secret; "+
				"serving continues in legacy mode, but these records are preserved and never overwritten. Restore the original server secret to recover them",
				"backup_unreadable", len(audit.BackupUnreadable), "unreadable_total", len(audit.Unreadable), "records", refs)
		}
		return nil
	}
	slog.Error("Encryption audit: stored secrets are unreadable with the configured encryption keys; they are preserved, and resolving them fails",
		"unreadable_total", len(audit.Unreadable), "records", refs)
	return nil
}

// auditLogMaxRecords caps the record IDs one audit log line lists.
const auditLogMaxRecords = 20

// auditRecordIDs renders up to max records as "<id> (<kind>)", sorted by
// ID, then "and N more": record IDs and classifications only (A1), never
// payloads, and a bounded line.
func auditRecordIDs(recs []AuditRecord, max int) string {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID+" ("+r.Kind+")")
	}
	sort.Strings(ids)
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(ids[:max], ", "), len(ids)-max)
}

// backupNotReadyReason returns readyReasonBackupUnreadable for a managed
// instance whose startup audit found an unreadable infrastructure backup.
func (s *Server) backupNotReadyReason() string {
	if !s.cipher.Managed() {
		return ""
	}
	s.encryptionAuditMu.RLock()
	defer s.encryptionAuditMu.RUnlock()
	if len(s.encryptionAudit.BackupUnreadable) > 0 {
		return readyReasonBackupUnreadable
	}
	return ""
}

// auditFlagged reports whether the startup audit found the record
// (key, scope, scopeID) unreadable.
func (s *Server) auditFlagged(key, scope, scopeID string) bool {
	s.encryptionAuditMu.RLock()
	defer s.encryptionAuditMu.RUnlock()
	for _, r := range s.encryptionAudit.Unreadable {
		if r.Key == key && r.Scope == scope && r.ScopeID == scopeID {
			return true
		}
	}
	return false
}

// guardOverwrite refuses (errExistingRecordUnreadable) a backup or
// synchronization write over an existing record the startup audit flagged
// or that this instance cannot decrypt now. A missing record, or an empty
// reference-only one, may be written.
func guardOverwrite(ctx context.Context, st store.SecretStore, c *secret.ValueCipher, flagged func(key, scope, scopeID string) bool, key, scope, scopeID string) error {
	if flagged != nil && flagged(key, scope, scopeID) {
		return errExistingRecordUnreadable
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
		return errExistingRecordUnreadable
	}
	return nil
}

// guardBackupOverwrite is guardOverwrite for this server's store, cipher
// and startup audit.
func (s *Server) guardBackupOverwrite(ctx context.Context, key, scope, scopeID string) error {
	return guardOverwrite(ctx, s.store, s.cipher, s.auditFlagged, key, scope, scopeID)
}
