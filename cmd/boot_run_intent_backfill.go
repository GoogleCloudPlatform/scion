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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runRunIntentBackfill sets agents.run_intent for rows that predate the
// column: running for an agent in phase running or starting, stopped for
// every other phase. Only NULL rows are written, so a rerun after a partial
// failure is safe, and the completion marker keeps it from running again
// once a pass has succeeded. Rows created after the upgrade get their intent
// from the lifecycle writers instead.
//
// An agent whose container was already lost before the upgrade (phase
// error, container_status missing) gets stopped: a loss that
// happened before intent existed is never treated as one to recover from.
func runRunIntentBackfill(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationRunIntentBackfill)
	if err != nil {
		slog.Error("Run intent backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Run intent backfill: already complete, skipping")
		return
	}

	written, err := s.BackfillRunIntent(ctx)
	if err != nil {
		slog.Error("Run intent backfill: pass did not complete; will retry next boot",
			"error", err)
		return
	}
	slog.Info("Run intent backfill: pass completed", "written", written)

	if markErr := MarkMigrationComplete(ctx, s, MigrationRunIntentBackfill, 0); markErr != nil {
		slog.Error("Run intent backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}
