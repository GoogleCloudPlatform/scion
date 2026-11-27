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

// Backfill retry backoff bounds, periodic legacy catch-up cadence, and bounded
// live-conversion worker queue settings.
const (
	defaultUsageLedgerBackfillRetryInterval = 2 * time.Minute
	defaultUsageLedgerBackfillRetryMax      = 30 * time.Minute
	defaultLegacyCatchupPeriodicInterval    = 1 * time.Hour
	defaultLiveLegacyQueueSize              = 64
	defaultLiveLegacyConvertTimeout         = 10 * time.Second
)

// usageLedgerHookStore is the optional Ent-backed store capability used by the
// Hub to enable/disable atomic run_attribution writes on run-ID minting and
// swaps, and to run the idempotent active-run and legacy session-metrics
// backfills. Store fakes that do not implement it leave the hook inactive.
type usageLedgerHookStore interface {
	SetRunAttributionConfig(cfg *store.RunAttributionConfig)
	RunAttributionConfigInstalled() bool
	BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error)
	BackfillLegacySessionMetrics(ctx context.Context, hubID string) (store.LegacySessionMetricsBackfillResult, error)
}

type liveLegacyConvertJob struct {
	hubID          string
	metrics        *store.AgentSessionMetrics
	preferredRunID string
}

// usageLedgerRuntime manages the Hub's tokenomics.usage_ledger background
// lifecycles with a monotonic generation counter bumped on every OFF/ON
// transition:
//   - Active-run attribution backfill (synchronous on startup, async on toggle).
//   - Legacy agent_session_metrics catch-up backfill: ALWAYS runs asynchronously
//     in its own goroutine (never blocks Hub startup, R2), retries with bounded
//     backoff on failure, and re-runs periodically while enabled so failed or
//     dropped live conversions don't wait for a restart (R3).
//   - Bounded live-conversion worker queue: decouples live POST /metrics from
//     ledger write latency and request-context cancellation (R3); drops to
//     periodic catch-up if full.
//   - Disabling the experiment or stopping the Hub cancels in-flight work and
//     stops timers cleanly.
type usageLedgerRuntime struct {
	mu                sync.Mutex
	gen               uint64
	enabled           bool
	hubID             string
	stopped           bool
	backfillSucceeded bool
	backfillRunning   bool
	backfillCancel    context.CancelFunc
	backfillDone      chan struct{}
	retryTimer        *time.Timer
	retryAttempts     int

	// Independent async lifecycle for legacy session_metrics catch-up (R2/R3).
	legacyRunning       bool
	legacyCancel        context.CancelFunc
	legacyDone          chan struct{}
	legacyTimer         *time.Timer
	legacyRetryAttempts int

	// Bounded per-server worker queue for async live session_metrics conversion (R3).
	liveQueue     chan liveLegacyConvertJob
	liveCancel    context.CancelFunc
	liveDone      chan struct{}
	liveInFlight  sync.WaitGroup
	liveQueueCap  int
	liveTimeout   time.Duration
	liveConvertFn func(ctx context.Context, hubID string, row *store.AgentSessionMetrics, preferredRunID string) (bool, error)

	// Test overrides for intervals/functions.
	retryInterval          time.Duration
	retryMax               time.Duration
	legacyPeriodicInterval time.Duration
	backfillFn             func(ctx context.Context, hubID string) (int, error)
	legacyBackfillFn       func(ctx context.Context, hubID string) (store.LegacySessionMetricsBackfillResult, error)
}

func (rt *usageLedgerRuntime) computeBackoffDelay(attempts int) time.Duration {
	base := rt.retryInterval
	if base <= 0 {
		base = defaultUsageLedgerBackfillRetryInterval
	}
	maxDelay := rt.retryMax
	if maxDelay <= 0 {
		maxDelay = defaultUsageLedgerBackfillRetryMax
	}
	if maxDelay < base {
		maxDelay = base
	}
	delay := base
	for i := 0; i < attempts; i++ {
		if delay >= maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func (rt *usageLedgerRuntime) nextRetryDelay() time.Duration {
	return rt.computeBackoffDelay(rt.retryAttempts)
}

func (rt *usageLedgerRuntime) nextLegacyRetryDelay() time.Duration {
	return rt.computeBackoffDelay(rt.legacyRetryAttempts)
}

func (rt *usageLedgerRuntime) periodicCatchupInterval() time.Duration {
	if rt.legacyPeriodicInterval > 0 {
		return rt.legacyPeriodicInterval
	}
	return defaultLegacyCatchupPeriodicInterval
}

// usageLedgerHubID returns the canonical hub_id stamped on usage ledger rows
// (run_attribution, usage_events) and advertised by usage capabilities.
// Falls back to "hub-local" when the server has no configured HubID.
func (s *Server) usageLedgerHubID() string {
	if id := s.HubID(); id != "" {
		return id
	}
	return "hub-local"
}

// reconcileUsageLedger synchronizes the store's atomic run_attribution writer
// with the tokenomics.usage_ledger experiment and, when enabled and not yet
// succeeded for the current generation, runs the idempotent active-run
// backfill synchronously while launching the legacy session_metrics catch-up
// asynchronously in its own goroutine (R2). Used at Hub startup.
func (s *Server) reconcileUsageLedger(ctx context.Context) {
	s.reconcileUsageLedgerMode(ctx, false)
}

// reconcileUsageLedgerAsync synchronizes the store's atomic run_attribution
// writer synchronously and runs any needed backfills in background goroutines
// off the caller's path. Used from ApplySnapshot so toggling
// tokenomics.usage_ledger never blocks admin settings HTTP requests.
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
		rt.retryAttempts = 0
		rt.legacyRetryAttempts = 0
		if rt.retryTimer != nil {
			rt.retryTimer.Stop()
			rt.retryTimer = nil
		}
		if rt.legacyTimer != nil {
			rt.legacyTimer.Stop()
			rt.legacyTimer = nil
		}
		cancel := rt.backfillCancel
		rt.backfillCancel = nil
		legacyCancel := rt.legacyCancel
		rt.legacyCancel = nil
		if uls.RunAttributionConfigInstalled() {
			slog.Info("tokenomics.usage_ledger disabled; clearing run_attribution writer")
		}
		uls.SetRunAttributionConfig(nil)
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if legacyCancel != nil {
			legacyCancel()
		}
		return
	}

	hubID := s.usageLedgerHubID()
	transitionedOn := false
	if !rt.enabled {
		// OFF -> ON transition: bump generation and clear backfillSucceeded so
		// any runs minted or session_metrics posted while off are caught up.
		rt.gen++
		rt.enabled = true
		rt.backfillSucceeded = false
		rt.retryAttempts = 0
		rt.legacyRetryAttempts = 0
		transitionedOn = true
	}
	rt.hubID = hubID
	wasInstalled := uls.RunAttributionConfigInstalled()
	uls.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: hubID})
	if !wasInstalled {
		slog.Info("tokenomics.usage_ledger enabled; atomic run_attribution writer active", "hub_id", hubID)
	}

	var runBackfill func()
	if !rt.backfillSucceeded && !rt.backfillRunning {
		runBackfill = s.prepareBackfillLocked(ctx, uls)
	}

	// Order legacy catch-up AFTER the active-run attribution backfill completes
	// (R1c), while still running it asynchronously in its own goroutine so it
	// never blocks Hub startup (R2). If the active-run backfill is already
	// complete for this generation, launch legacy catch-up immediately;
	// otherwise prepareBackfillLocked starts it upon active-run completion.
	var runLegacy func()
	if rt.backfillSucceeded && !rt.legacyRunning && (transitionedOn || rt.legacyTimer == nil) {
		runLegacy = s.prepareLegacyCatchupLocked(context.Background(), uls)
	}
	rt.mu.Unlock()

	if runLegacy != nil {
		go runLegacy()
	}
	if runBackfill != nil {
		if async {
			go runBackfill()
			return
		}
		runBackfill()
	}
}

// prepareBackfillLocked initializes state for a single active-run attribution
// backfill execution of the current generation. Caller must hold s.usageLedger.mu.
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
		var runLegacy func()
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
			// An OFF -> ON transition happened while this run was unwinding.
			nextRun = s.prepareBackfillLocked(context.Background(), uls)
		case err == nil && ctxErr == nil:
			rt.backfillSucceeded = true
			rt.retryAttempts = 0
			if inserted > 0 {
				slog.Info("tokenomics.usage_ledger: backfilled run_attribution rows for active runs", "inserted", inserted)
			}
			// Start legacy catch-up asynchronously once active-run attribution
			// backfill has completed (R1c/R2).
			if !rt.legacyRunning && rt.legacyTimer == nil {
				runLegacy = s.prepareLegacyCatchupLocked(context.Background(), uls)
			}
		default:
			delay := rt.nextRetryDelay()
			rt.retryAttempts++
			slog.Warn("tokenomics.usage_ledger: active-run attribution backfill failed; scheduling retry",
				"inserted", inserted, "attempt", rt.retryAttempts, "retry_in", delay, "error", err)
			retryGen := rt.gen
			rt.retryTimer = time.AfterFunc(delay, func() {
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

		if runLegacy != nil {
			go runLegacy()
		}
		if nextRun != nil {
			go nextRun()
		}
	}
}

// prepareLegacyCatchupLocked initializes one asynchronous pass of
// BackfillLegacySessionMetrics for the current generation. On success it arms
// a periodic catch-up timer while the experiment stays enabled (R2/R3); on
// failure it arms a bounded exponential backoff retry timer. Caller must hold
// s.usageLedger.mu.
func (s *Server) prepareLegacyCatchupLocked(ctx context.Context, uls usageLedgerHookStore) func() {
	rt := &s.usageLedger
	if rt.legacyTimer != nil {
		rt.legacyTimer.Stop()
		rt.legacyTimer = nil
	}
	myGen := rt.gen
	myHubID := rt.hubID
	legacyCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.legacyRunning = true
	rt.legacyCancel = cancel
	rt.legacyDone = done
	legacyFn := rt.legacyBackfillFn
	if legacyFn == nil {
		legacyFn = uls.BackfillLegacySessionMetrics
	}

	return func() {
		legacyRes, err := legacyFn(legacyCtx, myHubID)
		ctxErr := legacyCtx.Err()
		cancel()

		var nextRun func()
		rt.mu.Lock()
		rt.legacyRunning = false
		if rt.legacyDone == done {
			rt.legacyCancel = nil
			rt.legacyDone = nil
		}

		switch {
		case !rt.enabled:
			// Experiment disabled or Hub shutting down.
		case rt.gen != myGen:
			// An OFF -> ON transition occurred while unwinding; start a fresh
			// pass only if the new generation's active-run attribution backfill
			// has already completed (otherwise its completion branch starts it).
			if rt.backfillSucceeded {
				nextRun = s.prepareLegacyCatchupLocked(context.Background(), uls)
			}
		case err == nil && ctxErr == nil:
			rt.legacyRetryAttempts = 0
			if legacyRes.Processed > 0 {
				slog.Info("tokenomics.usage_ledger: backfilled legacy agent_session_metrics rows",
					"processed", legacyRes.Processed, "converted", legacyRes.Converted, "skipped", legacyRes.Skipped)
			}
			periodicDelay := rt.periodicCatchupInterval()
			periodicGen := rt.gen
			rt.legacyTimer = time.AfterFunc(periodicDelay, func() {
				rt.mu.Lock()
				var triggered func()
				if rt.enabled && rt.gen == periodicGen && !rt.legacyRunning {
					triggered = s.prepareLegacyCatchupLocked(context.Background(), uls)
				}
				rt.mu.Unlock()
				if triggered != nil {
					go triggered()
				}
			})
		default:
			delay := rt.nextLegacyRetryDelay()
			rt.legacyRetryAttempts++
			slog.Warn("tokenomics.usage_ledger: legacy session_metrics catch-up failed; scheduling retry",
				"processed", legacyRes.Processed, "attempt", rt.legacyRetryAttempts, "retry_in", delay, "error", err)
			retryGen := rt.gen
			rt.legacyTimer = time.AfterFunc(delay, func() {
				rt.mu.Lock()
				var triggered func()
				if rt.enabled && rt.gen == retryGen && !rt.legacyRunning {
					triggered = s.prepareLegacyCatchupLocked(context.Background(), uls)
				}
				rt.mu.Unlock()
				if triggered != nil {
					go triggered()
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

// enqueueLiveLegacySessionMetrics schedules asynchronous live conversion of an
// unmarked agent_session_metrics row onto a bounded per-server worker queue
// running on the server's worker context with a bounded timeout (R3). Never
// blocks the caller; if the queue is full or the server is shutting down, the
// job is dropped and periodic catch-up converts the row via converted_at IS NULL.
func (s *Server) enqueueLiveLegacySessionMetrics(metrics *store.AgentSessionMetrics, preferredRunID string) {
	if s == nil || s.store == nil || metrics == nil {
		return
	}
	rt := &s.usageLedger
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	if rt.liveQueue == nil {
		capSize := rt.liveQueueCap
		if capSize <= 0 {
			capSize = defaultLiveLegacyQueueSize
		}
		rt.liveQueue = make(chan liveLegacyConvertJob, capSize)
		workerCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		rt.liveCancel = cancel
		rt.liveDone = done
		queue := rt.liveQueue
		go s.runLiveLegacyConvertWorker(workerCtx, queue, done)
	}
	queue := rt.liveQueue
	hubID := s.usageLedgerHubID()
	job := liveLegacyConvertJob{
		hubID:          hubID,
		metrics:        metrics,
		preferredRunID: preferredRunID,
	}
	// Hold rt.mu across the non-blocking send so stopUsageLedger cannot race
	// with enqueueing. Increment liveInFlight before sending so the worker
	// cannot finish (e.g. early return when the experiment flips OFF) and call
	// Done() before Add(1) executes.
	rt.liveInFlight.Add(1)
	select {
	case queue <- job:
	default:
		rt.liveInFlight.Done()
		s.agentMetricsLog.Warn("Live legacy session_metrics conversion queue full; deferring to catch-up",
			"agent_id", metrics.AgentID, "session_id", metrics.SessionID, "metrics_id", metrics.ID)
	}
	rt.mu.Unlock()
}

func (s *Server) runLiveLegacyConvertWorker(workerCtx context.Context, queue <-chan liveLegacyConvertJob, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-workerCtx.Done():
			// Drain any queued jobs so liveInFlight does not leak on shutdown.
			for {
				select {
				case <-queue:
					s.usageLedger.liveInFlight.Done()
				default:
					return
				}
			}
		case job := <-queue:
			s.processLiveLegacyConvertJob(workerCtx, job)
			s.usageLedger.liveInFlight.Done()
		}
	}
}

func (s *Server) processLiveLegacyConvertJob(workerCtx context.Context, job liveLegacyConvertJob) {
	// Re-check the experiment before running a queued job (O-c): if the
	// experiment was turned OFF while this job was queued, leave converted_at
	// NULL so the next OFF->ON catch-up converts it.
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		return
	}

	rt := &s.usageLedger
	rt.mu.Lock()
	timeout := rt.liveTimeout
	if timeout <= 0 {
		timeout = defaultLiveLegacyConvertTimeout
	}
	convertFn := rt.liveConvertFn
	rt.mu.Unlock()

	if convertFn == nil {
		convertFn = s.store.ConvertLegacySessionMetricsRow
	}

	// Derive from workerCtx so Hub shutdown cancels in-flight conversions,
	// while remaining completely decoupled from the HTTP request context (R3).
	convCtx, cancel := context.WithTimeout(workerCtx, timeout)
	defer cancel()

	if _, err := convertFn(convCtx, job.hubID, job.metrics, job.preferredRunID); err != nil {
		s.agentMetricsLog.Warn("Failed to convert legacy session metrics to usage ledger; catch-up will retry",
			"agent_id", job.metrics.AgentID, "session_id", job.metrics.SessionID, "metrics_id", job.metrics.ID, "error", err)
	}
}

// stopUsageLedger cancels any in-flight background backfills and live
// conversion worker, stops all armed timers, and waits for all goroutines to
// exit during Hub shutdown.
func (s *Server) stopUsageLedger() {
	if s == nil {
		return
	}
	rt := &s.usageLedger
	for {
		rt.mu.Lock()
		rt.stopped = true
		if rt.enabled {
			rt.gen++
			rt.enabled = false
		}
		if rt.retryTimer != nil {
			rt.retryTimer.Stop()
			rt.retryTimer = nil
		}
		if rt.legacyTimer != nil {
			rt.legacyTimer.Stop()
			rt.legacyTimer = nil
		}
		cancel := rt.backfillCancel
		done := rt.backfillDone
		rt.backfillCancel = nil

		legacyCancel := rt.legacyCancel
		legacyDone := rt.legacyDone
		rt.legacyCancel = nil

		liveCancel := rt.liveCancel
		liveDone := rt.liveDone
		rt.liveCancel = nil
		rt.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if legacyCancel != nil {
			legacyCancel()
		}
		if liveCancel != nil {
			liveCancel()
		}
		if done == nil && legacyDone == nil && liveDone == nil {
			return
		}
		if done != nil {
			<-done
		}
		if legacyDone != nil {
			<-legacyDone
		}
		if liveDone != nil {
			<-liveDone
			rt.mu.Lock()
			if rt.liveDone == liveDone {
				rt.liveDone = nil
				rt.liveQueue = nil
			}
			rt.mu.Unlock()
		}
	}
}
