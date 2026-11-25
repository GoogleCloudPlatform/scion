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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// usageLedgerHookStore is the optional Ent-backed store capability used by the
// Hub to enable/disable atomic run_attribution writes on run-ID minting and
// swaps, and to run the idempotent active-run backfill. Store fakes that do not
// implement it leave the hook inactive.
type usageLedgerHookStore interface {
	SetRunAttributionConfig(cfg *store.RunAttributionConfig)
	RunAttributionConfigInstalled() bool
	BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error)
}

// usageLedgerRuntime tracks the Hub's tokenomics.usage_ledger backfill state
// independently of whether the atomic run_attribution writer is installed, so
// a failed backfill is retried on the next reconcile without needing a restart
// or OFF -> ON toggle, and toggle-time backfills can run off the admin-request
// path and be cancelled cleanly on disable or shutdown.
type usageLedgerRuntime struct {
	mu                sync.Mutex
	backfillSucceeded bool
	backfillRunning   bool
	backfillCancel    context.CancelFunc
	backfillDone      chan struct{}
	// backfillFn overrides uls.BackfillActiveRunAttributions in tests.
	backfillFn func(ctx context.Context, hubID string) (int, error)
}

// reconcileUsageLedger synchronizes the store's atomic run_attribution writer
// with the tokenomics.usage_ledger experiment and, when enabled and not yet
// succeeded since the last OFF -> ON transition, runs the idempotent active-run
// backfill synchronously. Used at Hub startup so active runs are attributed
// immediately.
func (s *Server) reconcileUsageLedger(ctx context.Context) {
	s.reconcileUsageLedgerMode(ctx, false)
}

// reconcileUsageLedgerAsync synchronizes the store's atomic run_attribution
// writer synchronously and runs any needed idempotent active-run backfill in a
// background goroutine off the caller's path. Used from ApplySnapshot so
// toggling tokenomics.usage_ledger never blocks admin settings HTTP requests.
func (s *Server) reconcileUsageLedgerAsync(ctx context.Context) {
	s.reconcileUsageLedgerMode(ctx, true)
}

func (s *Server) reconcileUsageLedgerMode(ctx context.Context, async bool) {
	if s == nil || s.store == nil {
		return
	}
	uls, ok := s.store.(usageLedgerHookStore)
	if !ok {
		return
	}

	rt := &s.usageLedger
	rt.mu.Lock()
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		cancel := rt.backfillCancel
		rt.backfillCancel = nil
		rt.backfillSucceeded = false
		if uls.RunAttributionConfigInstalled() {
			slog.Info("tokenomics.usage_ledger disabled; clearing run_attribution writer")
		}
		uls.SetRunAttributionConfig(nil)
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}

	hubID := s.HubID()
	wasInstalled := uls.RunAttributionConfigInstalled()
	uls.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: hubID})
	if !wasInstalled {
		slog.Info("tokenomics.usage_ledger enabled; atomic run_attribution writer active", "hub_id", hubID)
	}

	if rt.backfillSucceeded || rt.backfillRunning {
		rt.mu.Unlock()
		return
	}

	backfillCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.backfillRunning = true
	rt.backfillCancel = cancel
	rt.backfillDone = done
	backfillFn := rt.backfillFn
	if backfillFn == nil {
		backfillFn = uls.BackfillActiveRunAttributions
	}
	rt.mu.Unlock()

	runBackfill := func() {
		defer func() {
			cancel()
			rt.mu.Lock()
			rt.backfillRunning = false
			if rt.backfillDone == done {
				rt.backfillCancel = nil
				rt.backfillDone = nil
			}
			rt.mu.Unlock()
			close(done)
		}()

		inserted, err := backfillFn(backfillCtx, hubID)
		if err != nil {
			slog.Warn("tokenomics.usage_ledger: active-run attribution backfill failed; will retry on next reconcile",
				"inserted", inserted, "error", err)
			return
		}
		rt.mu.Lock()
		// Mark succeeded only if the experiment is still active (not disabled
		// while the backfill was in flight).
		if uls.RunAttributionConfigInstalled() && backfillCtx.Err() == nil {
			rt.backfillSucceeded = true
		}
		rt.mu.Unlock()
		if inserted > 0 {
			slog.Info("tokenomics.usage_ledger: backfilled run_attribution rows for active runs", "inserted", inserted)
		}
	}

	if async {
		go runBackfill()
		return
	}
	runBackfill()
}

// stopUsageLedger cancels any in-flight background active-run backfill and
// waits for it to exit during Hub shutdown.
func (s *Server) stopUsageLedger() {
	if s == nil {
		return
	}
	rt := &s.usageLedger
	rt.mu.Lock()
	cancel := rt.backfillCancel
	done := rt.backfillDone
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
