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

// nonAgentDispatchStateExpiredReason is the exact reason string
// brokerMessageSweepHandler writes when ExpireStuckPendingMessages flips a
// stuck-pending row to "failed" (pkg/hub/sweep.go). Must match verbatim — a
// different reason means a genuine, unrelated delivery failure.
const nonAgentDispatchStateExpiredReason = "expired: stuck in pending state beyond TTL"

// runNonAgentDispatchStateBackfill repairs non-agent-recipient message rows
// left "pending" or TTL-expired-"failed" by the pre-fix nc-promote-busy bug
// (see BackfillNonAgentDispatchState and the investigation note for the
// full rationale). M-1' semantics: an empty pass still writes the
// completion marker; only a run-level store failure leaves it unwritten for
// retry on the next boot.
func runNonAgentDispatchStateBackfill(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationNonAgentDispatchStateBackfill)
	if err != nil {
		slog.Error("Non-agent dispatch_state backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Non-agent dispatch_state backfill: already complete, skipping")
		return
	}

	slog.Info("Non-agent dispatch_state backfill: starting")

	repaired, err := s.BackfillNonAgentDispatchState(ctx, nonAgentDispatchStateExpiredReason)
	if err != nil {
		slog.Error("Non-agent dispatch_state backfill: pass did not complete; will retry next boot",
			"error", err)
		return
	}

	slog.Info("Non-agent dispatch_state backfill: pass completed",
		"repaired", repaired)

	if markErr := MarkMigrationComplete(ctx, s, MigrationNonAgentDispatchStateBackfill, 0); markErr != nil {
		slog.Error("Non-agent dispatch_state backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}
