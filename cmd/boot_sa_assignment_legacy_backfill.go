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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runSAAssignmentLegacyBackfill records the missing service-account
// assignment row for live assign-mode agents created before assignment rows
// existed. Without the row the GCP token mint refuses them with
// ceiling_provenance_missing. See hub.BackfillLegacySAAssignments for the
// row it writes.
//
// M-1' semantics: per-agent skips are logged at WARN, counted as residuals
// and do not block the marker; a store error aborts the pass and leaves the
// marker unwritten so the next boot retries. Only agents without an active
// row are touched, so a rerun is safe.
func runSAAssignmentLegacyBackfill(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationSAAssignmentLegacyBackfill)
	if err != nil {
		slog.Error("SA assignment legacy backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("SA assignment legacy backfill: already complete, skipping")
		return
	}

	res, err := hub.BackfillLegacySAAssignments(ctx, s)
	for _, agentID := range res.Written {
		slog.Info("SA assignment legacy backfill: recorded assignment", "agent_id", agentID)
	}
	for _, skip := range res.Skipped {
		slog.Warn("SA assignment legacy backfill: agent left without an assignment; its GCP token mint stays refused",
			"agent_id", skip.AgentID, "reason", skip.Reason)
	}
	if err != nil {
		slog.Error("SA assignment legacy backfill: pass did not complete; will retry next boot",
			"written", len(res.Written), "error", err)
		return
	}
	slog.Info("SA assignment legacy backfill: pass completed",
		"examined", res.Examined, "written", len(res.Written), "skipped", len(res.Skipped))

	if markErr := MarkMigrationComplete(ctx, s, MigrationSAAssignmentLegacyBackfill, len(res.Skipped)); markErr != nil {
		slog.Error("SA assignment legacy backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}
