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

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxBootLogErrors is the maximum number of per-row error messages logged
// during a boot-time migration. result.Errors holds one entry per refused
// row and is unbounded; logging it whole turns a bad migration into a
// disk-space incident (design §4.3). We log the first N plus the total.
const maxBootLogErrors = 10

// runBootDataMigrations runs the conversation-model data migrations that an
// upgrading hub needs. It is called once during store setup, before the hub
// serves any request, so a completed run is observable to every later reader
// without a gate (design §4.1, F3).
//
// It never returns an error: a failed data migration degrades history or
// leaves a repair pending, neither of which justifies refusing to boot
// (design A2). Failures are logged at ERROR and the completion marker is
// left unwritten so the next boot retries.
//
// After the data migrations, the existing backfill warning is re-checked
// so that operators still see unattributed-message counts. Re-pointing
// that warning is M6, not this commit.
func runBootDataMigrations(ctx context.Context, s store.Store) {
	runWithAdvisoryLock(ctx, s, store.LockDataMigrations, "conversation data migrations", func() {
		runDMKeyMigration(ctx, s) // §4.4 — repair, unbudgeted
	})

	// The backfill warning must still fire. Re-pointing it to a split
	// reachable/unreachable report is M6; for now, preserve the existing
	// behaviour exactly.
	maybeWarnUnbackfilledMessages(ctx, s)
}

// runDMKeyMigration runs the DM key migration with M-1' marker semantics.
//
// M-1' (design §4.3): a completion marker records that a full pass
// completed without a run-level failure. Row-level refusals are counted,
// persisted alongside the marker, and reported — they do NOT block the
// marker. A marker must never be written for a pass that did not finish.
//
// The distinction is load-bearing: on production data the migration
// produces thousands of deterministic row-level refusals that no retry
// can change. Blocking the marker on those refusals would create a
// permanent livelock — the migration re-runs every boot, making no
// progress, adding tens of seconds to every startup indefinitely.
func runDMKeyMigration(ctx context.Context, s store.Store) {
	// Fast path: already complete. O(1) with respect to data volume.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	if err != nil {
		slog.Error("DM key migration: failed to check completion marker; will attempt migration",
			"error", err)
		// Fall through: attempting the migration is safer than skipping it
		// when we cannot read the marker. The migration is idempotent.
	} else if done {
		slog.Debug("DM key migration: already complete, skipping")
		return
	}

	slog.Info("DM key migration: starting")

	svc := messaging.NewDMMigrationService(s)
	result, err := svc.Run(ctx, messaging.DMMigrationConfig{
		DryRun: false,
	})

	// --- M-1' decision point ---

	if err != nil {
		// RUN-LEVEL failure: the pass did not complete. Do NOT write the
		// marker. The next boot will retry.
		slog.Error("DM key migration did not complete; will retry next boot",
			"error", err)
		return
	}

	// The pass completed. Row-level refusals are a terminal, correct,
	// permanent outcome — not an error that blocks the marker.
	residuals := len(result.Errors)

	// Log the result.
	slog.Info("DM key migration: pass completed",
		"scanned", result.TotalScanned,
		"rekeyed", result.OldFormatRekeyed,
		"participants_added", result.ParticipantsAdded,
		"empty_ref_skipped", result.EmptyRefSkipped,
		"unparseable", result.Unparseable,
		"ambiguous", result.Ambiguous,
		"row_errors", residuals,
	)

	// Log a bounded sample of per-row errors.
	if residuals > 0 {
		logBoundedErrors("DM key migration", result.Errors, maxBootLogErrors)
	}

	// Write the completion marker. Row-level refusals do not block this.
	if markErr := MarkMigrationComplete(ctx, s, MigrationDMKey, residuals); markErr != nil {
		slog.Error("DM key migration: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// logBoundedErrors logs a sample of per-row errors, capped at limit entries,
// with the total count. This prevents an unbounded error list from turning a
// bad migration into a disk-space incident (design §4.3).
func logBoundedErrors(prefix string, errors []string, limit int) {
	total := len(errors)
	shown := total
	if shown > limit {
		shown = limit
	}

	for i := 0; i < shown; i++ {
		slog.Warn(fmt.Sprintf("%s: row error", prefix),
			"index", i+1,
			"total", total,
			"error", errors[i],
		)
	}

	if total > limit {
		slog.Warn(fmt.Sprintf("%s: %d more row errors not shown", prefix, total-limit),
			"shown", limit,
			"total", total,
		)
	}
}
