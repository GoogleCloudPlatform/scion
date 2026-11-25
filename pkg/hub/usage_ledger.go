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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// usageLedgerHookStore is the optional Ent-backed store capability used by the
// Hub to enable/disable atomic run_attribution writes on run-ID minting and
// swaps, and to run the one-shot active-run backfill. Store fakes that do not
// implement it leave the hook inactive.
type usageLedgerHookStore interface {
	SetRunAttributionConfig(cfg *store.RunAttributionConfig)
	RunAttributionConfigInstalled() bool
	BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error)
}

// reconcileUsageLedger synchronizes the store's atomic run_attribution writer
// with the tokenomics.usage_ledger experiment and, when enabled, runs the
// one-shot idempotent active-run backfill. Called at startup and on every
// operational-settings snapshot apply (so toggling the experiment takes effect
// immediately with no Hub restart, and zero rows are written while off).
func (s *Server) reconcileUsageLedger(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	uls, ok := s.store.(usageLedgerHookStore)
	if !ok {
		return
	}
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		if uls.RunAttributionConfigInstalled() {
			slog.Info("tokenomics.usage_ledger disabled; clearing run_attribution writer")
		}
		uls.SetRunAttributionConfig(nil)
		return
	}

	hubID := s.HubID()
	wasInstalled := uls.RunAttributionConfigInstalled()
	uls.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: hubID})
	if wasInstalled {
		return
	}
	slog.Info("tokenomics.usage_ledger enabled; atomic run_attribution writer active", "hub_id", hubID)

	// Run idempotent active-run backfill at startup (when enabled) and on
	// every OFF -> ON transition so runs minted while the experiment was off
	// are attributed immediately.
	inserted, err := uls.BackfillActiveRunAttributions(ctx, hubID)
	if err != nil {
		slog.Warn("tokenomics.usage_ledger: active-run attribution backfill failed", "error", err)
		return
	}
	if inserted > 0 {
		slog.Info("tokenomics.usage_ledger: backfilled run_attribution rows for active runs", "inserted", inserted)
	}
}
