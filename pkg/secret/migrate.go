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

package secret

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrMigrationIncomplete reports a secret-backend migration run that left
// at least one secret not verified in Secret Manager: a row whose stored
// value could not be decoded, read, written or read back. Such a row keeps
// its local payload, so it stays readable under the local key.
var ErrMigrationIncomplete = errors.New("secret migration incomplete")

// MigrationReport counts the outcome of MigrateLocalToGCP. It holds counts
// and row IDs only, never a value.
type MigrationReport struct {
	// Migrated counts rows written to Secret Manager and verified by
	// reading them back (in a dry run: rows that would be migrated).
	Migrated int `json:"migrated"`
	// Repaired counts rows that already referenced Secret Manager but whose
	// Secret Manager value was a local at-rest envelope (written by an
	// earlier migration that did not decode): the envelope opened under
	// the local cipher, and its decoded value was written back and
	// verified (in a dry run: rows that would be repaired).
	Repaired int `json:"repaired"`
	// Verified counts rows that already referenced Secret Manager whose
	// Secret Manager value was read back and is not a local envelope.
	Verified int `json:"verified"`
	// Gone counts rows deleted between listing and reading.
	Gone int `json:"gone"`
	// DecodeFailed counts rows whose stored value did not decode; their
	// IDs are in DecodeFailedIDs.
	DecodeFailed    int      `json:"decodeFailed"`
	DecodeFailedIDs []string `json:"decodeFailedIds,omitempty"`
	// Unverified counts rows whose Secret Manager value could not be read
	// back, did not match what was written, or (for a row that already
	// referenced Secret Manager) has no readable Secret Manager reference
	// or holds a value that looks like a local at-rest envelope but does
	// not open under the local key.
	Unverified int `json:"unverified"`
	// Failed counts rows whose stored value could not be read or whose
	// Secret Manager write failed.
	Failed int  `json:"failed"`
	DryRun bool `json:"dryRun"`
}

// incomplete is the number of rows the run left unverified.
func (r MigrationReport) incomplete() int { return r.DecodeFailed + r.Unverified + r.Failed }

// MigrateLocalToGCP moves secrets whose value is stored in the Hub database
// into Secret Manager. Each row without a Secret Manager reference is read
// in its stored at-rest form, decoded through the local backend's decode
// path (DecodeStoredValue) with cipher c, and written with
// GCPBackend.SetVerified, which replaces the row's local payload with the
// reference only after reading the value back from Secret Manager and
// matching it.
//
// A row that already references Secret Manager is checked, so a run made
// before this check existed cannot leave a broken value behind: its Secret
// Manager value is read back, and when it opens as a local at-rest
// envelope under c (authenticated decryption, so a real value that merely
// looks like an envelope is never taken for one), the decoded value is
// written back with SetVerified and counted as repaired. A value that
// parses as an envelope but does not open under c is counted as
// unverified, so the run stays incomplete. Any other readable value is
// counted as verified. This repair needs the local key
// that sealed the envelope; once that key is gone, it cannot be done.
//
// A row that fails to decode is skipped and reported by row ID only. A row
// that cannot be read, written or verified keeps its local payload. When
// any row was left so, the report is returned with an error wrapping
// ErrMigrationIncomplete, and a later run retries those rows. A dry run
// decodes and checks every row but writes nothing, and reports rather than
// fails.
//
// Values never reach logger or the returned error: log lines and errors
// name keys, scopes, row IDs and fixed reasons (with a gRPC code), never a
// value or the text of a Secret Manager error.
func MigrateLocalToGCP(ctx context.Context, st store.SecretStore, c *ValueCipher, gcp *GCPBackend, dryRun bool, logger io.Writer) (MigrationReport, error) {
	report := MigrationReport{DryRun: dryRun}
	if !c.configured() {
		return report, fmt.Errorf("secret migration needs the local cipher to decode stored values: %w", ErrCipherNotConfigured)
	}
	if gcp == nil {
		return report, errors.New("secret migration needs a GCP Secret Manager backend")
	}

	all, err := st.ListSecrets(ctx, store.SecretFilter{})
	if err != nil {
		return report, fmt.Errorf("failed to list secrets: %w", err)
	}
	logf(logger, "Found %d secret(s) to process.\n", len(all))
	if dryRun {
		logf(logger, "DRY RUN: No changes will be made.\n")
	}

	for _, s := range all {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if s.SecretRef != "" {
			checkRefRow(ctx, c, gcp, s, dryRun, logger, &report)
			continue
		}
		migrateRow(ctx, st, c, gcp, s, dryRun, logger, &report)
	}

	outcome := "complete"
	switch {
	case dryRun:
		outcome = "dry run complete"
	case report.incomplete() > 0:
		outcome = "INCOMPLETE"
	}
	logf(logger, "\nMigration %s: %d migrated, %d repaired, %d verified, %d gone, %d decode failure(s), %d unverified, %d failed\n",
		outcome, report.Migrated, report.Repaired, report.Verified, report.Gone, report.DecodeFailed, report.Unverified, report.Failed)

	if !dryRun && report.incomplete() > 0 {
		return report, fmt.Errorf("%w: %d decode failure(s), %d unverified, %d failed; the local key must stay in place until a re-run verifies every secret",
			ErrMigrationIncomplete, report.DecodeFailed, report.Unverified, report.Failed)
	}
	return report, nil
}

// migrateRow migrates one row that has no Secret Manager reference and
// records the outcome in report.
func migrateRow(ctx context.Context, st store.SecretStore, c *ValueCipher, gcp *GCPBackend, s store.Secret, dryRun bool, logger io.Writer, report *MigrationReport) {
	raw, err := st.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
	if errors.Is(err, store.ErrNotFound) {
		logf(logger, "  GONE  %s (scope: %s/%s) - deleted since it was listed\n", s.Key, s.Scope, s.ScopeID)
		report.Gone++
		return
	}
	if err != nil {
		logf(logger, "  ERROR  %s (scope: %s/%s) - reading the stored value failed\n", s.Key, s.Scope, s.ScopeID)
		report.Failed++
		return
	}

	value, err := DecodeStoredValue(c, raw, s.SecretRef)
	if err != nil {
		// Row ID only: the decode error, key and scope stay out of the log.
		logf(logger, "  SKIP-DECODE  row %s - the stored value did not decode\n", s.ID)
		report.DecodeFailed++
		report.DecodeFailedIDs = append(report.DecodeFailedIDs, s.ID)
		return
	}

	if dryRun {
		logf(logger, "  WOULD MIGRATE  %s (scope: %s/%s, type: %s)\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
		report.Migrated++
		return
	}

	if writeVerified(ctx, gcp, s, value, "the row is left as it was", logger, report) {
		logf(logger, "  MIGRATED  %s (scope: %s/%s, type: %s) - verified\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
		report.Migrated++
	}
}

// checkRefRow checks one row that already references Secret Manager and
// repairs it when its Secret Manager value is a local at-rest envelope.
func checkRefRow(ctx context.Context, c *ValueCipher, gcp *GCPBackend, s store.Secret, dryRun bool, logger io.Writer, report *MigrationReport) {
	smPath, ok := extractGCPSMPath(s.SecretRef)
	if !ok {
		logf(logger, "  UNVERIFIED  %s (scope: %s/%s) - the reference is not a Secret Manager reference\n", s.Key, s.Scope, s.ScopeID)
		report.Unverified++
		return
	}
	current, err := gcp.accessLatestVersionByPath(ctx, smPath)
	if err != nil {
		logf(logger, "  UNVERIFIED  %s (scope: %s/%s) - reading the Secret Manager value failed (%s)\n", s.Key, s.Scope, s.ScopeID, status.Code(err))
		report.Unverified++
		return
	}

	// Only an authenticated decryption counts as an envelope to repair: a
	// plaintext value (which a legacy-mode cipher passes through, and a
	// managed cipher refuses as unadmitted plaintext) and a value that does
	// not parse as an envelope are real values and are left alone. A value
	// that parses as an enc:v1: or enc:v2: envelope but does not open (for
	// example after its key left the ring) is not a usable value, so it
	// keeps the run incomplete rather than letting it complete and the
	// local key be dropped.
	decoded, auth, err := c.DecryptAuthenticated(current)
	if err != nil && parsesAsEnvelope(current) {
		logf(logger, "  UNVERIFIED  %s (scope: %s/%s) - the Secret Manager value looks like a local envelope but does not open under the local key\n", s.Key, s.Scope, s.ScopeID)
		report.Unverified++
		return
	}
	if err != nil || auth.Format == secretenvelope.FormatPlaintext {
		logf(logger, "  VERIFIED  %s (scope: %s/%s) - the Secret Manager value is readable\n", s.Key, s.Scope, s.ScopeID)
		report.Verified++
		return
	}

	if dryRun {
		logf(logger, "  WOULD REPAIR  %s (scope: %s/%s) - the Secret Manager value is a local at-rest envelope\n", s.Key, s.Scope, s.ScopeID)
		report.Repaired++
		return
	}
	if writeVerified(ctx, gcp, s, decoded, "the row is left as it was and the previous Secret Manager version is kept one version back", logger, report) {
		logf(logger, "  REPAIRED  %s (scope: %s/%s, type: %s) - the decoded value replaced a local at-rest envelope; verified\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
		report.Repaired++
	}
}

// parsesAsEnvelope reports whether stored is a well-formed enc:v1: or
// enc:v2: envelope.
func parsesAsEnvelope(stored string) bool {
	env, err := secretenvelope.Parse(stored)
	return err == nil && env.Format != secretenvelope.FormatPlaintext
}

// writeVerified writes value for row s with GCPBackend.SetVerified. On
// failure it logs the row with kept (a fixed text saying what was kept,
// never the value or the error text), counts it as unverified or failed,
// and returns false; the row is left as it was.
func writeVerified(ctx context.Context, gcp *GCPBackend, s store.Secret, value, kept string, logger io.Writer, report *MigrationReport) bool {
	input := &SetSecretInput{
		Name:        s.Key,
		Value:       value,
		SecretType:  s.SecretType,
		Target:      s.Target,
		Scope:       s.Scope,
		ScopeID:     s.ScopeID,
		Description: s.Description,
		CreatedBy:   s.CreatedBy,
		UpdatedBy:   s.UpdatedBy,
	}
	if _, _, err := gcp.SetVerified(ctx, input); err != nil {
		if errors.Is(err, ErrSecretVerify) {
			logf(logger, "  UNVERIFIED  %s (scope: %s/%s) - the Secret Manager read-back did not confirm the value; %s\n", s.Key, s.Scope, s.ScopeID, kept)
			report.Unverified++
			return false
		}
		logf(logger, "  ERROR  %s (scope: %s/%s) - writing to Secret Manager failed (%s); %s\n", s.Key, s.Scope, s.ScopeID, status.Code(err), kept)
		report.Failed++
		return false
	}
	return true
}

func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
