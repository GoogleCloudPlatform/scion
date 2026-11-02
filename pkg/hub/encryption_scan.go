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
	"fmt"
	"slices"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The authenticating payload scan (ptone/scion#1530 §6, §7): one paginated
// pass over every stored secret row, shared by the startup audit, key
// status and `scion server keys verify`. Every row that holds a local
// payload is decrypted and authenticated, and infrastructure backups are
// structurally validated; a row's format and key ID come from the key
// that actually authenticated it, never from its envelope label.
//
// Row classes:
//   - Reference-only (empty value, non-empty SecretRef): the payload lives
//     in a secret backend. It is skipped before any decrypt.
//   - Empty value AND empty SecretRef: not reference-only, so it is
//     decrypted like every other row. It parses as plaintext, so a managed
//     (keyring file) cipher reports it unreadable (plaintext_not_admitted)
//     and a legacy cipher reads it as plaintext; verify fails it either
//     way (it is not v2). The scan flags it (ScanClass.EmptyNoRef) so the
//     audit can count it. Remedies: re-encrypt the empty value under an
//     allowed v2 key, give the row a backend reference, or delete it.
//   - Every other row: decrypted with the cipher's ring.

// payloadDecrypter is the one cipher capability the scan uses
// (*secret.ValueCipher).
type payloadDecrypter interface {
	DecryptAuthenticated(stored string) (string, secret.Authentication, error)
}

// ScanClass classifies one scanned row. It holds no payload.
type ScanClass struct {
	// ReferenceOnly: an empty value with a backend reference; never
	// decrypted, and Format, KID and Failure are empty.
	ReferenceOnly bool
	// EmptyNoRef: an empty value with no backend reference (decrypted as
	// plaintext like any other row).
	EmptyNoRef bool
	// Format ("plaintext", "v1", "v2") and KID (encrypted formats only)
	// of the key that authenticated the row; both empty when it failed.
	Format string
	KID    string
	// Failure is "" for a readable row, else its classification
	// (secret.DecryptFailureKind, or auditKindInvalidPayload for a
	// readable infrastructure backup that does not validate).
	Failure string
}

// Readable reports whether the row was decrypted, authenticated and,
// for an infrastructure backup, validated.
func (c ScanClass) Readable() bool { return !c.ReferenceOnly && c.Failure == "" }

// ScanRow is one scanned row: identity and classification only.
type ScanRow struct {
	ID      string
	Key     string
	Scope   string
	ScopeID string
	Version int
	Class   ScanClass
}

// classifyPayload decrypts and authenticates p with d, unless it is
// reference-only, and validates an infrastructure backup's payload.
func classifyPayload(d payloadDecrypter, p store.SecretPayload) ScanClass {
	if p.EncryptedValue == "" && p.SecretRef != "" {
		return ScanClass{ReferenceOnly: true}
	}
	c := ScanClass{EmptyNoRef: p.EncryptedValue == ""}
	plain, auth, err := d.DecryptAuthenticated(p.EncryptedValue)
	if err != nil {
		c.Failure = secret.DecryptFailureKind(err)
		return c
	}
	if v, ok := infraPayloadValidators[p.Key]; ok && p.Scope == store.ScopeHub && v(plain) != nil {
		c.Failure = auditKindInvalidPayload
		return c
	}
	c.Format = auth.Format.String()
	if auth.Format != secretenvelope.FormatPlaintext {
		c.KID = auth.KID
	}
	return c
}

// scanSecretPayloads visits every stored secret row, classified with d,
// in ascending ID order, reading ScanSecretPayloads pages of
// store.MaxSecretPayloadScan rows. It returns the number of rows visited.
// A store error, a page out of order, or an error from visit stops the
// scan and is returned.
func scanSecretPayloads(ctx context.Context, st store.SecretStore, d payloadDecrypter, visit func(ScanRow) error) (int, error) {
	n := 0
	after := ""
	for {
		page, err := st.ScanSecretPayloads(ctx, after, store.MaxSecretPayloadScan)
		if err != nil {
			return n, fmt.Errorf("scanning stored secret payloads: %w", err)
		}
		for _, p := range page {
			if p.ID <= after {
				return n, errors.New("scanning stored secret payloads: the store returned rows out of ID order")
			}
			after = p.ID
			n++
			if err := visit(ScanRow{ID: p.ID, Key: p.Key, Scope: p.Scope, ScopeID: p.ScopeID, Version: p.Version, Class: classifyPayload(d, p)}); err != nil {
				return n, err
			}
		}
		if len(page) < store.MaxSecretPayloadScan {
			return n, nil
		}
	}
}

// Verify allowed-set sources: where EncryptionVerifyReport's allowed
// writer key IDs come from.
const (
	// VerifyAllowedFromPolicy: the database encryption policy's
	// allowed_write_kids.
	VerifyAllowedFromPolicy = "policy"
	// VerifyAllowedFromKeyring: no policy; the keyring file's writer key,
	// which writes enc:v2:. Not reachable in this release, whose keyring
	// check refuses write_format v2; kept for when that refusal is lifted.
	VerifyAllowedFromKeyring = "keyring"
	// VerifyAllowedNone: no policy, and no keyring file writing enc:v2:
	// (legacy mode, or write_format v1): no row can pass.
	VerifyAllowedNone = "none"
)

// Verify failure reasons for rows that are readable but do not pass.
// Unreadable rows carry their ScanClass.Failure.
const (
	verifyReasonPlaintext     = "plaintext"
	verifyReasonV1            = "v1"
	verifyReasonKIDNotAllowed = "v2_kid_not_allowed"
)

// EncryptionVerifyReport is the result of VerifyEncryption: aggregate
// counts only. Failures (record identities, never payloads) is excluded
// from JSON; the CLI writes it only to a private report file.
type EncryptionVerifyReport struct {
	// OK is true when every row that holds a local payload authenticates
	// in enc:v2: under an allowed writer key ID.
	OK               bool     `json:"ok"`
	AllowedWriteKIDs []string `json:"allowed_write_kids"`
	// AllowedSource is VerifyAllowedFromPolicy, VerifyAllowedFromKeyring
	// or VerifyAllowedNone.
	AllowedSource string `json:"allowed_source"`
	Scanned       int    `json:"scanned"`
	// ReferenceOnly rows hold no local payload; they pass and are never
	// decrypted.
	ReferenceOnly int `json:"reference_only"`
	// EmptyNoRef counts rows with an empty value and no reference (they
	// fail: an empty value is plaintext, not v2).
	EmptyNoRef int `json:"empty_no_ref"`
	Passed     int `json:"passed"`
	Failed     int `json:"failed"`
	// FailedByReason counts failures by reason: a decrypt failure kind,
	// invalid_payload, plaintext, v1 or v2_kid_not_allowed.
	FailedByReason map[string]int `json:"failed_by_reason"`
	// Failures lists every failing record (identity and reason only).
	Failures []VerifyFailure `json:"-"`
}

// VerifyFailure identifies one record that failed verification: identity
// and reason, never a payload.
type VerifyFailure struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Reason  string `json:"reason"`
}

// VerifyEncryption runs the authenticating scan with c and checks that
// every row holding a local payload authenticates in enc:v2: under an
// allowed writer key ID. The allowed set is the database encryption
// policy's allowed_write_kids when a policy exists; otherwise c's writer
// key when c is a managed (keyring file) cipher writing enc:v2:; otherwise
// empty, so every payload row fails. It is read-only and takes no policy
// lock: the result is a point-in-time check, not a fence against
// concurrent writes. A store error is returned; record failures are in
// the report.
func VerifyEncryption(ctx context.Context, st store.Store, c *secret.ValueCipher) (EncryptionVerifyReport, error) {
	r := EncryptionVerifyReport{FailedByReason: map[string]int{}, AllowedWriteKIDs: []string{}, AllowedSource: VerifyAllowedNone}
	p, err := st.GetEncryptionPolicy(ctx)
	switch {
	case err == nil:
		r.AllowedSource = VerifyAllowedFromPolicy
		r.AllowedWriteKIDs = nonNil(slices.Clone(p.AllowedWriteKIDs))
	case errors.Is(err, store.ErrNotFound):
		if c.Managed() && c.WriteFormat() == keyring.WriteV2 {
			r.AllowedSource = VerifyAllowedFromKeyring
			r.AllowedWriteKIDs = []string{c.WriterID()}
		}
	default:
		return EncryptionVerifyReport{}, fmt.Errorf("reading the database encryption policy: %w", err)
	}
	n, err := scanSecretPayloads(ctx, st, c, func(row ScanRow) error {
		if row.Class.ReferenceOnly {
			r.ReferenceOnly++
			r.Passed++
			return nil
		}
		if row.Class.EmptyNoRef {
			r.EmptyNoRef++
		}
		reason := verifyReason(row.Class, r.AllowedWriteKIDs)
		if reason == "" {
			r.Passed++
			return nil
		}
		r.Failed++
		r.FailedByReason[reason]++
		r.Failures = append(r.Failures, VerifyFailure{ID: row.ID, Key: row.Key, Scope: row.Scope, ScopeID: row.ScopeID, Reason: reason})
		return nil
	})
	if err != nil {
		return EncryptionVerifyReport{}, err
	}
	r.Scanned = n
	sort.Slice(r.Failures, func(i, j int) bool { return r.Failures[i].ID < r.Failures[j].ID })
	r.OK = r.Failed == 0 && r.Passed == r.Scanned
	return r, nil
}

// verifyReason is "" when a non-reference-only row passes verification,
// else why it fails.
func verifyReason(c ScanClass, allowed []string) string {
	switch {
	case c.Failure != "":
		return c.Failure
	case c.Format == secretenvelope.FormatPlaintext.String():
		return verifyReasonPlaintext
	case c.Format == secretenvelope.FormatV1.String():
		return verifyReasonV1
	case c.Format == secretenvelope.FormatV2.String() && c.KID != "" && slices.Contains(allowed, c.KID):
		return ""
	default:
		return verifyReasonKIDNotAllowed
	}
}
