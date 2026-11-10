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
	// AlreadyRef counts rows that already reference Secret Manager.
	AlreadyRef int `json:"alreadyRef"`
	// Gone counts rows deleted between listing and reading.
	Gone int `json:"gone"`
	// DecodeFailed counts rows whose stored value did not decode; their
	// IDs are in DecodeFailedIDs.
	DecodeFailed    int      `json:"decodeFailed"`
	DecodeFailedIDs []string `json:"decodeFailedIds,omitempty"`
	// Unverified counts rows written to Secret Manager whose read-back
	// failed or did not match.
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
// A row that fails to decode is skipped and reported by row ID only. A row
// that cannot be read, written or verified keeps its local payload. When
// any row was left so, the report is returned with an error wrapping
// ErrMigrationIncomplete, and a later run retries those rows. A dry run
// decodes every row but writes nothing, and reports rather than fails.
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
			logf(logger, "  SKIP  %s (scope: %s/%s) - already references Secret Manager\n", s.Key, s.Scope, s.ScopeID)
			report.AlreadyRef++
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
	logf(logger, "\nMigration %s: %d migrated, %d already migrated, %d gone, %d decode failure(s), %d unverified, %d failed\n",
		outcome, report.Migrated, report.AlreadyRef, report.Gone, report.DecodeFailed, report.Unverified, report.Failed)

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
			logf(logger, "  UNVERIFIED  %s (scope: %s/%s) - the Secret Manager read-back did not confirm the value; the local copy is kept\n", s.Key, s.Scope, s.ScopeID)
			report.Unverified++
			return
		}
		logf(logger, "  ERROR  %s (scope: %s/%s) - writing to Secret Manager failed (%s); the local copy is kept\n", s.Key, s.Scope, s.ScopeID, status.Code(err))
		report.Failed++
		return
	}
	logf(logger, "  MIGRATED  %s (scope: %s/%s, type: %s) - verified\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
	report.Migrated++
}

func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
