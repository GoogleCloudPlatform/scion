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
	"sync"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

var (
	// decisionIndexInterval is the cadence between background decision indexer
	// passes after the initial boot-time pass (design-scale §3.6).
	decisionIndexInterval = 5 * time.Minute

	// decisionIndexPageSize is the bounded batch size per transaction for each
	// decision indexer step.
	decisionIndexPageSize = entadapter.DefaultDecisionIndexPageSize

	// decisionIndexSQLiteYield is the pause between pages on SQLite so
	// interactive request writes interleave cleanly on the single writer.
	decisionIndexSQLiteYield = 5 * time.Millisecond
)

// decisionIndexerRuntime tracks the background decision indexer goroutine so
// CleanupBackgroundResources can cancel and join it cleanly.
type decisionIndexerRuntime struct {
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// decisionIndexStep is one bounded-page indexing step executed by a pass.
// P1 registers the settled-hint sweep; P4 appends facet-indexing and
// reconciliation steps without restructuring the loop. Each step receives the
// keyset cursor (empty on the first page of a pass) and returns the number of
// rows updated plus the next cursor (empty when the step has drained).
type decisionIndexStep struct {
	name string
	run  func(ctx context.Context, afterID string, limit int) (updated int, nextCursor string, err error)
}

// decisionIndexPassStats summarizes one indexer pass.
type decisionIndexPassStats struct {
	HadPending bool
	StepCounts map[string]int
	Pages      int
}

func (s *Server) decisionIndexSteps() []decisionIndexStep {
	return []decisionIndexStep{
		{
			name: "settled",
			run:  s.store.SweepSettledDecisionsPage,
		},
		{
			name: "facet_decisions",
			run:  s.store.IndexDecisionFacetsDecisionsPage,
		},
		{
			name: "facet_answers",
			run:  s.store.IndexDecisionFacetsAnswersPage,
		},
	}
}

func (s *Server) isSQLiteStore() bool {
	if d, ok := s.store.(interface{ Dialect() string }); ok {
		return d.Dialect() == dialect.SQLite
	}
	return false
}

// startDecisionIndexer launches the background decision indexer loop: one
// immediate pass after boot (outside Migrate so boot is never blocked), then
// one pass every decisionIndexInterval until ctx is cancelled or
// stopDecisionIndexer runs. Each pass stops immediately when the store reports
// nothing pending.
func (s *Server) startDecisionIndexer(ctx context.Context) {
	if s.store == nil {
		return
	}
	rt := &s.decisionIndexer
	interval := decisionIndexInterval
	loopCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped || rt.cancel != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.cancel = cancel
	rt.running.Add(1)
	rt.mu.Unlock()

	go func() {
		defer rt.running.Done()
		_, _ = s.runDecisionIndexPass(loopCtx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				_, _ = s.runDecisionIndexPass(loopCtx)
			}
		}
	}()
}

// stopDecisionIndexer stops the background decision indexer and waits for any
// pass in progress to return. Safe to call multiple times or without a start.
func (s *Server) stopDecisionIndexer() {
	rt := &s.decisionIndexer
	rt.mu.Lock()
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rt.running.Wait()
}

// runDecisionIndexPass executes one complete indexer pass: it probes the store
// for pending work and, if any exists, drains each registered step in bounded
// pages (yielding between pages on SQLite) until nothing remains.
func (s *Server) runDecisionIndexPass(ctx context.Context) (decisionIndexPassStats, error) {
	stats := decisionIndexPassStats{
		StepCounts: make(map[string]int),
	}
	if s.store == nil || ctx.Err() != nil {
		return stats, ctx.Err()
	}

	log := logging.Subsystem("hub.decision_indexer")
	start := time.Now()
	pending, err := s.store.HasPendingDecisionIndexWork(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("decision indexer pending probe failed", "error", err)
		}
		return stats, err
	}
	if !pending {
		// Still run the O(distinct actors + projects) reconciler at most once
		// per pass so older-binary user/project deletions heal even when no
		// unindexed decisions/answers remain (design-scale §3.6).
		reconciled, rErr := s.store.ReconcileDecisionFacets(ctx)
		if rErr != nil {
			if ctx.Err() == nil {
				log.Warn("decision indexer reconcile step failed", "error", rErr)
			}
			return stats, rErr
		}
		if reconciled > 0 {
			stats.HadPending = true
			stats.StepCounts["reconcile"] = reconciled
			stats.Pages++
			log.Info("decision indexer pass complete",
				"updated", reconciled,
				"reconcile", reconciled,
				"pages", stats.Pages,
				"pending", false,
				"duration", time.Since(start).Round(time.Millisecond),
			)
		}
		return stats, nil
	}
	stats.HadPending = true

	pageSize := decisionIndexPageSize
	if pageSize <= 0 {
		pageSize = entadapter.DefaultDecisionIndexPageSize
	}
	yield := time.Duration(0)
	if s.isSQLiteStore() {
		yield = decisionIndexSQLiteYield
	}

	totalUpdated := 0
	for _, step := range s.decisionIndexSteps() {
		stepTotal := 0
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			n, nextCursor, err := step.run(ctx, cursor, pageSize)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("decision indexer step failed", "step", step.name, "updated_so_far", stepTotal, "error", err)
				}
				return stats, err
			}
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
			if n > 0 {
				stepTotal += n
				totalUpdated += n
				stats.Pages++
				stats.StepCounts[step.name] = stepTotal
			}
			if stats.Pages%10 == 0 && n > 0 {
				log.Info("decision indexer progress", "step", step.name, "updated", stepTotal, "pages", stats.Pages)
			}
			if yield > 0 {
				timer := time.NewTimer(yield)
				select {
				case <-ctx.Done():
					timer.Stop()
					return stats, ctx.Err()
				case <-timer.C:
				}
			}
		}
	}

	reconciled, rErr := s.store.ReconcileDecisionFacets(ctx)
	if rErr != nil {
		if ctx.Err() == nil {
			log.Warn("decision indexer reconcile step failed", "error", rErr)
		}
		return stats, rErr
	}
	if reconciled > 0 {
		stats.StepCounts["reconcile"] = reconciled
		totalUpdated += reconciled
		stats.Pages++
	}

	stillPending, probeErr := s.store.HasPendingDecisionIndexWork(ctx)
	if probeErr != nil && ctx.Err() == nil {
		log.Warn("decision indexer post-pass probe failed", "error", probeErr)
	}
	log.Info("decision indexer pass complete",
		"updated", totalUpdated,
		"settled", stats.StepCounts["settled"],
		"facet_decisions", stats.StepCounts["facet_decisions"],
		"facet_answers", stats.StepCounts["facet_answers"],
		"reconcile", stats.StepCounts["reconcile"],
		"pages", stats.Pages,
		"pending", stillPending,
		"duration", time.Since(start).Round(time.Millisecond),
	)
	return stats, nil
}
