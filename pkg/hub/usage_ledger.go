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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// defaultUsageLedgerBackfillRetryInterval bounds how long a failed active-run
// backfill waits before retrying automatically while tokenomics.usage_ledger
// stays enabled.
const defaultUsageLedgerBackfillRetryInterval = 2 * time.Minute

// usageLedgerHookStore is the optional Ent-backed store capability used by the
// Hub to enable/disable atomic run_attribution writes on run-ID minting and
// swaps, and to run the idempotent active-run backfill. Store fakes that do not
// implement it leave the hook inactive.
type usageLedgerHookStore interface {
	SetRunAttributionConfig(cfg *store.RunAttributionConfig)
	RunAttributionConfigInstalled() bool
	BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error)
}

// usageLedgerRuntime manages the Hub's tokenomics.usage_ledger backfill state
// with a monotonic generation counter bumped on every OFF/ON transition:
//   - A backfill goroutine captures its generation at start.
//   - If an OFF -> ON transition occurs while a cancelled backfill is still
//     unwinding, the generation advances; when the unwinding goroutine exits
//     (even if it returned nil), it sees rt.enabled && rt.gen != myGen and
//     immediately launches a fresh backfill for the new generation.
//   - If a backfill for the current generation fails, a bounded periodic retry
//     timer re-triggers it without requiring a settings change or Hub restart.
//   - Disabling the experiment or stopping the Hub cancels both any in-flight
//     backfill and any armed retry timer.
type usageLedgerRuntime struct {
	mu                sync.Mutex
	gen               uint64
	enabled           bool
	hubID             string
	backfillSucceeded bool
	backfillRunning   bool
	backfillCancel    context.CancelFunc
	backfillDone      chan struct{}
	retryTimer        *time.Timer
	// retryInterval overrides defaultUsageLedgerBackfillRetryInterval in tests.
	retryInterval time.Duration
	// backfillFn overrides uls.BackfillActiveRunAttributions in tests.
	backfillFn func(ctx context.Context, hubID string) (int, error)
}

func (rt *usageLedgerRuntime) effectiveRetryInterval() time.Duration {
	if rt.retryInterval > 0 {
		return rt.retryInterval
	}
	return defaultUsageLedgerBackfillRetryInterval
}

// reconcileUsageLedger synchronizes the store's atomic run_attribution writer
// with the tokenomics.usage_ledger experiment and, when enabled and not yet
// succeeded for the current generation, runs the idempotent active-run
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
		if rt.enabled {
			rt.gen++
		}
		rt.enabled = false
		rt.backfillSucceeded = false
		if rt.retryTimer != nil {
			rt.retryTimer.Stop()
			rt.retryTimer = nil
		}
		cancel := rt.backfillCancel
		rt.backfillCancel = nil
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
	if !rt.enabled {
		// OFF -> ON transition: bump generation and clear backfillSucceeded so
		// any runs minted while off are attributed.
		rt.gen++
		rt.enabled = true
		rt.backfillSucceeded = false
	}
	rt.hubID = hubID
	wasInstalled := uls.RunAttributionConfigInstalled()
	uls.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: hubID})
	if !wasInstalled {
		slog.Info("tokenomics.usage_ledger enabled; atomic run_attribution writer active", "hub_id", hubID)
	}

	if rt.backfillSucceeded || rt.backfillRunning {
		rt.mu.Unlock()
		return
	}

	runBackfill := s.prepareBackfillLocked(ctx, uls)
	rt.mu.Unlock()

	if async {
		go runBackfill()
		return
	}
	runBackfill()
}

// prepareBackfillLocked initializes state for a single backfill execution of
// the current generation. Caller must hold s.usageLedger.mu.
func (s *Server) prepareBackfillLocked(ctx context.Context, uls usageLedgerHookStore) func() {
	rt := &s.usageLedger
	if rt.retryTimer != nil {
		rt.retryTimer.Stop()
		rt.retryTimer = nil
	}
	myGen := rt.gen
	myHubID := rt.hubID
	backfillCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.backfillRunning = true
	rt.backfillCancel = cancel
	rt.backfillDone = done
	backfillFn := rt.backfillFn
	if backfillFn == nil {
		backfillFn = uls.BackfillActiveRunAttributions
	}

	return func() {
		inserted, err := backfillFn(backfillCtx, myHubID)
		ctxErr := backfillCtx.Err()
		cancel()

		var nextRun func()
		rt.mu.Lock()
		rt.backfillRunning = false
		if rt.backfillDone == done {
			rt.backfillCancel = nil
			rt.backfillDone = nil
		}

		switch {
		case !rt.enabled:
			// Experiment was disabled or Hub is shutting down; stop cleanly.
		case rt.gen != myGen:
			// An OFF -> ON transition happened while this run was unwinding
			// (regardless of whether this run returned nil or context.Canceled).
			// Immediately start a fresh backfill for the new generation.
			nextRun = s.prepareBackfillLocked(context.Background(), uls)
		case err == nil && ctxErr == nil:
			rt.backfillSucceeded = true
			if inserted > 0 {
				slog.Info("tokenomics.usage_ledger: backfilled run_attribution rows for active runs", "inserted", inserted)
			}
		default:
			slog.Warn("tokenomics.usage_ledger: active-run attribution backfill failed; scheduling retry",
				"inserted", inserted, "retry_in", rt.effectiveRetryInterval(), "error", err)
			retryGen := rt.gen
			rt.retryTimer = time.AfterFunc(rt.effectiveRetryInterval(), func() {
				rt.mu.Lock()
				stillRelevant := rt.enabled && rt.gen == retryGen && !rt.backfillSucceeded
				rt.mu.Unlock()
				if stillRelevant {
					s.reconcileUsageLedgerAsync(context.Background())
				}
			})
		}
		rt.mu.Unlock()
		close(done)

		if nextRun != nil {
			go nextRun()
		}
	}
}

// stopUsageLedger cancels any in-flight background active-run backfill and
// stops any armed retry timer, waiting for any active goroutine to exit during
// Hub shutdown.
func (s *Server) stopUsageLedger() {
	if s == nil {
		return
	}
	rt := &s.usageLedger
	for {
		rt.mu.Lock()
		if rt.enabled {
			rt.gen++
			rt.enabled = false
		}
		if rt.retryTimer != nil {
			rt.retryTimer.Stop()
			rt.retryTimer = nil
		}
		cancel := rt.backfillCancel
		done := rt.backfillDone
		rt.backfillCancel = nil
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done == nil {
			return
		}
		<-done
	}
}
