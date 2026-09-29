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

// agentToUserDispatchStateExpiredReason is the exact reason string written by
// pkg/hub/sweep.go's brokerMessageSweepHandler when ExpireStuckPendingMessages
// flips a stuck-pending row to "failed". It must match verbatim: any other
// failure reason (e.g. an unreachable-agent dispatch failure or a buffered
// delivery-flush failure) records a genuine, unrelated delivery failure and
// must stay "failed".
const agentToUserDispatchStateExpiredReason = "expired: stuck in pending state beyond TTL"

// runAgentToUserDispatchStateBackfill repairs "user:"-recipient message rows
// left by the pre-fix nc-promote-busy bug: deliverToUser, the
// deliveryUserDirect fallback, createInboxMessage, and the group-set-to-user
// path all once persisted a storeMsg without stamping DispatchState, which
// the Ent schema silently defaults to "pending" (pkg/ent/schema/message.go).
// Left unrepaired, existing rows are in one of three states depending on age:
//
//   - Created < 24h ago: still "pending". A DM containing one blocks
//     "Promote to thread" with IN_FLIGHT_MESSAGES until the sweep runs.
//   - 24h–7d old: "failed" with dispatch_failure_reason set to the exact
//     TTL-expiry string above — the stuck-message sweep "healed" the writer
//     bug into this state, indistinguishable at that point from a genuinely
//     stuck agent dispatch.
//   - Older than 7d, on a hub running the failed-message-retention purge
//     (#1892): already hard-deleted. Those rows are unrecoverable; this
//     migration only repairs what still exists.
//
// This backfill — not narrowing the promote guard or the sweep's predicates
// — is the fix of record: narrowing would stop new data loss but leave
// already-mislabeled history “failed” forever, still on the purge's clock at
// the next TTL window if the exact-reason match in
// BackfillUserRecipientDispatchState ever changed. See R1's
// scoping of ExpireStuckPendingMessages/CountStuckPendingMessages/
// PurgeFailedMessages to agent recipients for the sibling defense-in-depth
// fix that stops NEW damage; this migration repairs EXISTING damage.
//
// M-1' semantics (same as the other boot migrations in this file): a
// completion marker records that a full pass completed without a run-level
// failure. There are no row-level refusals in this migration — every
// eligible row is repaired the same way — so residuals is always 0. Only a
// run-level failure (store unreachable, etc.) leaves the marker unwritten
// for retry on the next boot.
func runAgentToUserDispatchStateBackfill(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationAgentToUserDispatchStateBackfill)
	if err != nil {
		slog.Error("Agent-to-user dispatch_state backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Agent-to-user dispatch_state backfill: already complete, skipping")
		return
	}

	slog.Info("Agent-to-user dispatch_state backfill: starting")

	repaired, err := s.BackfillUserRecipientDispatchState(ctx, agentToUserDispatchStateExpiredReason)
	if err != nil {
		slog.Error("Agent-to-user dispatch_state backfill: pass did not complete; will retry next boot",
			"error", err)
		return
	}

	slog.Info("Agent-to-user dispatch_state backfill: pass completed",
		"repaired", repaired)

	if markErr := MarkMigrationComplete(ctx, s, MigrationAgentToUserDispatchStateBackfill, 0); markErr != nil {
		slog.Error("Agent-to-user dispatch_state backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}
