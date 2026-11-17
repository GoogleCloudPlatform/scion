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
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// messagePolicyDecisionTrimTick is how often each replica runs a bounded
	// decision-retention trim pass (consumer design §9.1; target 8).
	messagePolicyDecisionTrimTick = 15 * time.Minute

	// messagePolicyDecisionTrimBatchTimeout bounds one store trim batch so
	// shutdown or slow queries never stall background teardown.
	messagePolicyDecisionTrimBatchTimeout = 10 * time.Second

	// messagePolicyDecisionTrimMaxBatches caps how many consecutive bounded
	// store trim batches a single tick executes when TrimMessagePolicyDecisions
	// reports More == true.
	messagePolicyDecisionTrimMaxBatches = 10
)

// messagePolicyDecisionTrimRuntime holds the replica's periodic decision trim
// loop state.
type messagePolicyDecisionTrimRuntime struct {
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	running sync.WaitGroup

	// busy prevents overlapping passes on the same replica.
	busy atomic.Bool

	// Optional overrides for deterministic testing; zero values use production
	// defaults. Protected by mu when read at start/pass start.
	tickInterval      time.Duration
	batchTimeout      time.Duration
	maxBatches        int
	maxAge            time.Duration
	maxRowsPerProject int
	nowFunc           func() time.Time
}

type messagePolicyDecisionTrimConfig struct {
	tickInterval      time.Duration
	batchTimeout      time.Duration
	maxBatches        int
	maxAge            time.Duration
	maxRowsPerProject int
	nowFunc           func() time.Time
}

func (rt *messagePolicyDecisionTrimRuntime) snapshotConfig() messagePolicyDecisionTrimConfig {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	cfg := messagePolicyDecisionTrimConfig{
		tickInterval:      rt.tickInterval,
		batchTimeout:      rt.batchTimeout,
		maxBatches:        rt.maxBatches,
		maxAge:            rt.maxAge,
		maxRowsPerProject: rt.maxRowsPerProject,
		nowFunc:           rt.nowFunc,
	}
	if cfg.tickInterval <= 0 {
		cfg.tickInterval = messagePolicyDecisionTrimTick
	}
	if cfg.batchTimeout <= 0 {
		cfg.batchTimeout = messagePolicyDecisionTrimBatchTimeout
	}
	if cfg.maxBatches <= 0 {
		cfg.maxBatches = messagePolicyDecisionTrimMaxBatches
	}
	if cfg.maxAge <= 0 {
		cfg.maxAge = store.DefaultMessagePolicyDecisionMaxAge
	}
	if cfg.maxRowsPerProject <= 0 {
		cfg.maxRowsPerProject = store.DefaultMaxMessagePolicyDecisionsPerProject
	}
	if cfg.nowFunc == nil {
		cfg.nowFunc = func() time.Time { return time.Now().UTC() }
	}
	return cfg
}

// startMessagePolicyDecisionTrim starts the replica's periodic decision-retention
// trim loop. It runs whether or not hub.message_policies is currently enabled so
// decision rows recorded during a trial still age out after disabling the flag.
func (s *Server) startMessagePolicyDecisionTrim(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	rt := &s.messagePolicyTrim
	loopCtx, cancel := context.WithCancel(ctx)

	rt.mu.Lock()
	if rt.stopped || rt.cancel != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	tickInterval := rt.tickInterval
	if tickInterval <= 0 {
		tickInterval = messagePolicyDecisionTrimTick
	}
	rt.cancel = cancel
	rt.running.Add(1)
	rt.mu.Unlock()

	go func() {
		defer rt.running.Done()
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				_, _, _ = s.messagePolicyDecisionTrimPass(loopCtx)
			}
		}
	}()
}

// stopMessagePolicyDecisionTrim stops the periodic decision trim loop and waits
// for any in-flight pass (bounded by messagePolicyDecisionTrimBatchTimeout and
// the cancelled context). Safe to call multiple times or without start.
func (s *Server) stopMessagePolicyDecisionTrim() {
	if s == nil {
		return
	}
	rt := &s.messagePolicyTrim
	rt.mu.Lock()
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rt.running.Wait()
}

// messagePolicyDecisionTrimPass runs up to messagePolicyDecisionTrimMaxBatches
// bounded store trim batches. Concurrent callers while a pass is already in
// progress return ran=false without blocking.
func (s *Server) messagePolicyDecisionTrimPass(ctx context.Context) (store.MessagePolicyDecisionTrim, bool, error) {
	if s == nil || s.store == nil {
		return store.MessagePolicyDecisionTrim{}, false, nil
	}
	rt := &s.messagePolicyTrim
	if !rt.busy.CompareAndSwap(false, true) {
		return store.MessagePolicyDecisionTrim{}, false, nil
	}
	defer rt.busy.Store(false)

	cfg := rt.snapshotConfig()
	now := cfg.nowFunc().UTC()

	var agg store.MessagePolicyDecisionTrim
	batches := 0
	for batches < cfg.maxBatches {
		if ctx.Err() != nil {
			break
		}
		batchCtx, cancel := context.WithTimeout(ctx, cfg.batchTimeout)
		res, err := s.store.TrimMessagePolicyDecisions(batchCtx, now, cfg.maxAge, cfg.maxRowsPerProject)
		cancel()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				slog.Warn("message policy decision trim failed",
					"subsystem", "hub.messages",
					"batches_completed", batches,
					"error", err,
				)
			}
			return agg, batches > 0, err
		}
		batches++
		agg.AgedDeleted += res.AgedDeleted
		agg.OverflowDeleted += res.OverflowDeleted
		agg.More = res.More
		if !res.More {
			break
		}
	}

	if agg.TotalDeleted() > 0 {
		slog.Debug("trimmed message policy decisions",
			"subsystem", "hub.messages",
			"aged_deleted", agg.AgedDeleted,
			"overflow_deleted", agg.OverflowDeleted,
			"batches", batches,
			"more", agg.More,
		)
	}
	return agg, true, nil
}
