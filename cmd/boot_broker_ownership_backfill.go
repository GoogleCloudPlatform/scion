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

	"github.com/GoogleCloudPlatform/scion/pkg/brokerownership"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxBrokerOwnershipLogDetails caps how many per-broker decisions, and how
// many delta observations, are logged individually. Both Result.Decisions
// and Result.Deltas are unbounded (one entry per scanned broker, and per
// timing candidate/near-miss); logging either whole on a large deployment
// turns a routine migration into a log-volume incident, mirroring
// maxBootLogErrors' rationale.
const maxBrokerOwnershipLogDetails = 20

// runBrokerOwnershipBackfill attributes CreatedBy on runtime brokers left
// ownerless (CreatedBy == "") by legacy registration flows, to the project
// that originally created them, when that can be established with
// confidence. Brokers where attribution is ambiguous are left ownerless for
// admin review — this migration never guesses. See
// pkg/brokerownership for the full rule and its rationale.
//
// M-1' semantics (same as runWorkspaceModeBackfill): a completion marker
// records that a full pass completed without a run-level failure. Rows left
// ownerless are an expected, deterministic outcome of the rule — not a
// row-level refusal to retry — so they are counted in the marker's residual
// field for visibility but do not block completion. Only a run-level
// failure (store unreachable, etc.) leaves the marker unwritten for retry
// on the next boot.
//
// This always runs with Config.DryRun=false: gating writes behind an
// operator flag would leave true owners locked out of their brokers by the
// very ownership gate this backfill exists to unblock, on every hub, until
// an operator acts, which is the worse failure mode. DryRun remains an
// out-of-band function mode in pkg/brokerownership for manual review
// against a copy of the data, exercised by that package's own tests — it is
// not wired as a second boot phase here. Every call re-evaluates the rule
// against current data; there is no persisted intermediate decision list
// carried between boots.
func runBrokerOwnershipBackfill(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationBrokerOwnershipBackfill)
	if err != nil {
		slog.Error("Broker ownership backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Broker ownership backfill: already complete, skipping")
		return
	}

	slog.Info("Broker ownership backfill: starting")

	result, err := brokerownership.Backfill(ctx, s, brokerownership.Config{})
	if err != nil {
		slog.Error("Broker ownership backfill: pass did not complete; will retry next boot",
			"error", err)
		return
	}

	attributed := result.Reasons[brokerownership.ReasonAttributed]
	// leftOwnerless is derived by subtraction, not by summing the five
	// "left ownerless" reason codes by name: every scanned broker gets
	// exactly one Decision with exactly one Reason (TotalScanned ==
	// len(Decisions) by construction), so Reasons is a strict partition of
	// TotalScanned. Subtracting the two reasons that are NOT "left
	// ownerless" (attributed, already-set) is exact regardless of how many
	// distinct ownerless reason codes exist, and can't silently drift out
	// of sync if pkg/brokerownership ever adds, removes, or renames one —
	// unlike a hand-written sum of the current five, which a future edit
	// could update in the switch/case above without also updating here.
	// ExcludedEmbedded is not in this residual at all: it is an expected,
	// benign class, not an admin-review item.
	leftOwnerless := result.TotalScanned - attributed -
		result.Reasons[brokerownership.ReasonAlreadySet] -
		result.Reasons[brokerownership.ReasonExcludedEmbedded]

	slog.Info("Broker ownership backfill: pass completed",
		"scanned", result.TotalScanned,
		"attributed", attributed,
		"already_set", result.Reasons[brokerownership.ReasonAlreadySet],
		"excluded_embedded", result.Reasons[brokerownership.ReasonExcludedEmbedded],
		"left_ownerless", leftOwnerless,
		"no_timing_match", result.Reasons[brokerownership.ReasonNoTimingMatch],
		"ambiguous_timing", result.Reasons[brokerownership.ReasonAmbiguousTiming],
		"link_mismatch", result.Reasons[brokerownership.ReasonLinkMismatch],
		"multi_linked", result.Reasons[brokerownership.ReasonMultiLinked],
		"linked_by_mismatch", result.Reasons[brokerownership.ReasonLinkedByMismatch],
	)

	logged := 0
	for _, d := range result.Decisions {
		if d.Reason == brokerownership.ReasonExcludedEmbedded {
			continue // expected, benign class; not worth a line per broker
		}
		if logged >= maxBrokerOwnershipLogDetails {
			slog.Info("Broker ownership backfill: further per-broker decisions suppressed",
				"remaining", len(result.Decisions)-logged)
			break
		}
		switch d.Reason {
		case brokerownership.ReasonAttributed:
			slog.Info("Broker ownership backfill: attributed",
				"broker_id", d.BrokerID,
				"owner_id", d.OwnerID,
				"delta_seconds", d.DeltaSeconds,
			)
		case brokerownership.ReasonAlreadySet:
			// Not a failure and not this pass's doing: another writer set
			// CreatedBy on this row between our read and our write.
			slog.Info("Broker ownership backfill: skipped, already set by a concurrent pass",
				"broker_id", d.BrokerID,
			)
		default:
			slog.Info("Broker ownership backfill: left ownerless for admin review",
				"broker_id", d.BrokerID,
				"reason", d.Reason,
			)
		}
		logged++
	}

	logDeltaSummary(result.Deltas)

	if markErr := MarkMigrationComplete(ctx, s, MigrationBrokerOwnershipBackfill, leftOwnerless); markErr != nil {
		slog.Error("Broker ownership backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// logDeltaSummary reports the observed signed timing deltas
// (RuntimeBroker.Created - Project.Created) for post-hoc audit of
// brokerownership.DefaultWindowSeconds: the run's maximum in-window delta
// (how close a genuine pair came to the window edge), and near-miss deltas
// just outside the window — flagging negative ones, which are always an
// anomaly regardless of window width (see the package doc on why a negative
// delta can never be a genuine pair). Per-observation lines are bounded by
// maxBrokerOwnershipLogDetails, same as the per-broker decision log.
func logDeltaSummary(deltas []brokerownership.DeltaObservation) {
	hasInWindow := false
	maxInWindowDelta := 0.0
	nearMisses := 0
	negativeNearMisses := 0
	for _, d := range deltas {
		if d.InWindow {
			if !hasInWindow || d.DeltaSeconds > maxInWindowDelta {
				maxInWindowDelta = d.DeltaSeconds
			}
			hasInWindow = true
			continue
		}
		nearMisses++
		if d.DeltaSeconds < 0 {
			negativeNearMisses++
		}
	}

	if !hasInWindow {
		slog.Info("Broker ownership backfill: no in-window timing candidates observed this pass",
			"near_misses", nearMisses,
			"negative_near_misses", negativeNearMisses,
		)
		return
	}

	slog.Info("Broker ownership backfill: timing delta summary",
		"max_in_window_delta_seconds", maxInWindowDelta,
		"window_seconds", brokerownership.DefaultWindowSeconds,
		"near_misses", nearMisses,
		"negative_near_misses", negativeNearMisses,
	)

	logged := 0
	for _, d := range deltas {
		if d.InWindow || d.DeltaSeconds >= 0 {
			continue // only negative near-misses get an individual line: they are always an anomaly
		}
		if logged >= maxBrokerOwnershipLogDetails {
			slog.Info("Broker ownership backfill: further negative near-misses suppressed",
				"remaining", negativeNearMisses-logged)
			break
		}
		slog.Info("Broker ownership backfill: negative near-miss delta observed",
			"broker_id", d.BrokerID,
			"project_id", d.ProjectID,
			"delta_seconds", d.DeltaSeconds,
		)
		logged++
	}
}
