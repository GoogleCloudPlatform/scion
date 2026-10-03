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
	"errors"
	"log/slog"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// migrationUpdatedBy is the updatedBy attribution written on every broker
// setting this migration creates or fills in, so that a later reader (or a
// support engineer) can tell a migrated value apart from one an admin set
// through the API.
const migrationUpdatedBy = "migration:ptone/scion#2061"

// runBrokerQuotaBindingsToSettingsMigration is the one-shot boot data
// migration described in design.md §5.5 (P2-D4) and ptone/scion#2063 items 2
// and 3: it copies every existing max_agents_per_broker entitlement binding
// scoped to a specific broker (scopeType=broker) into that broker's
// broker_settings row, so the P2 settings API and effectiveBrokerLimit
// precedence (broker setting beats bindings) reflect what the entitlement
// engine used to enforce.
//
// This migration takes every scopeType=broker binding on this limit for B,
// regardless of subject — it does NOT limit itself to what the entitlement
// engine actually enforced. That is a deliberate, wider net than the
// engine's own resolution: effectiveBrokerLimit resolves the broker scope
// with subjectID=brokerID (broker_capacity.go), so Reserve's matching logic
// (matchesScope / resolveEffectiveLimitWithSource, pkg/hub/quota.go) only
// ever picked up two shapes — a user binding whose subjectId equals B (the
// real "user-subject hack") and a system_default binding with an EMPTY
// subject. A system_default row with a non-empty subject, or a user binding
// for some other user scoped to B, was never enforced by the engine at all;
// that silent no-op is exactly the ptone/scion#2063 item-3 bug. Design §5.5
// asks this migration to sweep up every broker-scoped row anyway — not just
// the ones that happened to work — so an operator's evident intent (they
// scoped a binding to this broker for a reason) is honoured even where the
// old bug ate it. That is why upgrading can newly impose or tighten a cap
// for a broker that previously had no effective per-broker limit at all.
//
// M-1' semantics (same as the other boot migrations in this file): a
// completion marker means a full pass finished without a run-level failure.
// A broker whose bindings reference a runtime broker that no longer exists
// is a deterministic, permanent, non-retryable outcome — it is skipped,
// logged, and counted in the marker's residual field, but does not block
// completion. A run-level failure (store unreachable, a write that fails
// for a reason other than "no such broker") aborts the whole pass without
// writing the marker, so the next boot retries from scratch. The migration
// is idempotent: rows already carrying a non-nil maxAgents setting are left
// untouched, so a retried pass only fills in what an earlier pass missed.
//
// Bindings are never deleted or modified: they are left in place, now
// shadowed by the broker setting (design.md §5.2 precedence — the broker
// setting wins over bindings), and their IDs are logged so an operator can
// find and clean them up later if desired.
func runBrokerQuotaBindingsToSettingsMigration(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings)
	if err != nil {
		slog.Error("Broker quota bindings to settings migration: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Broker quota bindings to settings migration: already complete, skipping")
		return
	}

	slog.Info("Broker quota bindings to settings migration: starting")

	limitDef, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No max_agents_per_broker limit definition exists (a hub that
			// has never seeded quotas). There is nothing to migrate, and
			// nothing that will ever appear for this limit name to change
			// that, so the pass is complete.
			slog.Info("Broker quota bindings to settings migration: max_agents_per_broker limit not found; nothing to migrate")
			if markErr := MarkMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings, 0); markErr != nil {
				slog.Error("Broker quota bindings to settings migration: failed to write completion marker; will retry next boot",
					"error", markErr)
			}
			return
		}
		slog.Error("Broker quota bindings to settings migration: failed to load limit definition; will retry next boot",
			"error", err)
		return
	}

	bindings, err := s.ListEntitlementBindings(ctx, limitDef.ID)
	if err != nil {
		slog.Error("Broker quota bindings to settings migration: failed to list entitlement bindings; will retry next boot",
			"error", err)
		return
	}

	byBroker := make(map[string][]*store.EntitlementBinding)
	for _, b := range bindings {
		if b.ScopeType != store.QuotaScopeBroker || b.ScopeID == "" {
			continue
		}
		byBroker[b.ScopeID] = append(byBroker[b.ScopeID], b)
	}

	// Deterministic order for logging and for tests.
	brokerIDs := make([]string, 0, len(byBroker))
	for id := range byBroker {
		brokerIDs = append(brokerIDs, id)
	}
	sort.Strings(brokerIDs)

	var migrated, alreadySet, missingBroker, shadowedBindings int

	for _, brokerID := range brokerIDs {
		brokerBindings := byBroker[brokerID]
		bindingIDs := entitlementBindingIDs(brokerBindings)

		if _, err := s.GetRuntimeBroker(ctx, brokerID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				missingBroker++
				slog.Info("Broker quota bindings to settings migration: broker no longer exists; skipping its bindings",
					"broker_id", brokerID,
					"binding_ids", bindingIDs,
				)
				continue
			}
			slog.Error("Broker quota bindings to settings migration: failed to look up broker; will retry next boot",
				"broker_id", brokerID,
				"error", err,
			)
			return
		}

		existing, err := s.GetBrokerSettings(ctx, brokerID)
		var (
			newSettings      store.BrokerSettings
			expectedRevision int64
		)
		switch {
		case err == nil:
			newSettings = existing.Settings
			expectedRevision = existing.Revision
			if existing.Settings.MaxAgents != nil {
				alreadySet++
				slog.Info("Broker quota bindings to settings migration: broker already has a maxAgents setting; leaving untouched",
					"broker_id", brokerID,
					"binding_ids", bindingIDs,
				)
				continue
			}
		case errors.Is(err, store.ErrNotFound):
			expectedRevision = 0
		default:
			slog.Error("Broker quota bindings to settings migration: failed to read broker settings; will retry next boot",
				"broker_id", brokerID,
				"error", err,
			)
			return
		}

		value := maxAgentsFromBindings(brokerBindings)
		newSettings.MaxAgents = &value

		if _, err := s.PutBrokerSettings(ctx, brokerID, newSettings, expectedRevision, migrationUpdatedBy); err != nil {
			slog.Error("Broker quota bindings to settings migration: failed to write broker settings; will retry next boot",
				"broker_id", brokerID,
				"value", value,
				"error", err,
			)
			return
		}

		migrated++
		shadowedBindings += len(bindingIDs)
		slog.Info("Broker quota bindings to settings migration: migrated bindings to a broker setting",
			"broker_id", brokerID,
			"max_agents", value,
			"shadowed_binding_ids", bindingIDs,
		)
	}

	slog.Info("Broker quota bindings to settings migration: pass completed",
		"brokers_scanned", len(byBroker),
		"migrated", migrated,
		"already_set", alreadySet,
		"missing_broker", missingBroker,
		"shadowed_bindings", shadowedBindings,
	)

	// missingBroker bindings reference a broker that will never come back —
	// a deterministic, permanent non-participant, exactly what the
	// migrationMarker.Residuals field is for (M-1').
	if markErr := MarkMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings, missingBroker); markErr != nil {
		slog.Error("Broker quota bindings to settings migration: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// maxAgentsFromBindings computes the value the entitlement engine's "most
// generous wins" merge rule (pkg/hub/quota.go ResolveEffectiveLimit) would
// have produced for this set of same-scope bindings: 0 (unlimited) if any
// binding grants unlimited (Value <= 0), otherwise the maximum positive
// value. Callers must not pass an empty slice.
func maxAgentsFromBindings(bindings []*store.EntitlementBinding) int64 {
	for _, b := range bindings {
		if b.Value <= 0 {
			return 0
		}
	}
	var max int64
	for _, b := range bindings {
		if b.Value > max {
			max = b.Value
		}
	}
	return max
}

// entitlementBindingIDs extracts the IDs of a slice of entitlement bindings,
// for logging which bindings are now shadowed by a broker setting or were
// skipped because their broker no longer exists.
func entitlementBindingIDs(bindings []*store.EntitlementBinding) []string {
	ids := make([]string, len(bindings))
	for i, b := range bindings {
		ids[i] = b.ID
	}
	return ids
}
