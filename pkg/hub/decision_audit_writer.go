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
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Decision audit records are persisted by a bounded, buffered writer so that
// transient database contention delays records instead of losing them.
// EmitDecisionAudit never blocks on the store: it appends to a fixed-size
// in-memory queue under a short mutex and returns. A small fixed pool of
// workers drains the queue, retrying transient store errors with capped,
// jittered exponential backoff. When the queue is full, allow records are
// shed before deny records. Every lost record is counted (by reason and
// decision) and summarised in a rate-limited log line. Close drains the
// queue on shutdown, bounded by a deadline.
//
// Nothing here feeds back into the authorization decision: a record that
// cannot be written is counted and dropped, never surfaced to the caller.

const (
	decisionAuditQueueSize      = 4096
	decisionAuditWorkers        = 4
	decisionAuditMaxAttempts    = 5
	decisionAuditAttemptTimeout = 2 * time.Second
	decisionAuditBackoffBase    = 100 * time.Millisecond
	decisionAuditBackoffMax     = 2 * time.Second
	decisionAuditDrainTimeout   = 5 * time.Second
	decisionAuditDropLogEvery   = 30 * time.Second
	// decisionAuditAbortGrace bounds how long Close waits for workers to
	// return after the drain deadline cancels their in-flight writes.
	decisionAuditAbortGrace = time.Second
)

// DecisionAuditDropReason is the closed set of "reason" labels on the
// decision audit drop counter.
type DecisionAuditDropReason string

const (
	// DecisionAuditDropQueueFull: the queue was full; the record was either
	// rejected on arrival or (allow records only) shed to make room for a
	// deny record.
	DecisionAuditDropQueueFull DecisionAuditDropReason = "queue_full"
	// DecisionAuditDropWriteFailed: the store rejected the record with a
	// permanent error, or every retry attempt failed.
	DecisionAuditDropWriteFailed DecisionAuditDropReason = "write_failed"
	// DecisionAuditDropShutdown: the record arrived after Close, or was
	// still pending when the drain deadline passed.
	DecisionAuditDropShutdown DecisionAuditDropReason = "shutdown"
)

// DecisionAuditMetricsRecorder receives decision audit writer metrics. The
// "decision" label is "allow", "deny" or "unknown".
type DecisionAuditMetricsRecorder interface {
	RecordDecisionAuditDrop(reason DecisionAuditDropReason, decision string)
	SetDecisionAuditQueueDepth(depth int64)
	RecordDecisionAuditWrite(latency time.Duration, success bool)
}

type noopDecisionAuditMetrics struct{}

func (noopDecisionAuditMetrics) RecordDecisionAuditDrop(DecisionAuditDropReason, string) {}
func (noopDecisionAuditMetrics) SetDecisionAuditQueueDepth(int64)                        {}
func (noopDecisionAuditMetrics) RecordDecisionAuditWrite(time.Duration, bool)            {}

type decisionAuditWriterConfig struct {
	queueSize      int
	workers        int
	maxAttempts    int
	attemptTimeout time.Duration
	backoffBase    time.Duration
	backoffMax     time.Duration
	drainTimeout   time.Duration
	dropLogEvery   time.Duration
}

func defaultDecisionAuditWriterConfig() decisionAuditWriterConfig {
	return decisionAuditWriterConfig{
		queueSize:      decisionAuditQueueSize,
		workers:        decisionAuditWorkers,
		maxAttempts:    decisionAuditMaxAttempts,
		attemptTimeout: decisionAuditAttemptTimeout,
		backoffBase:    decisionAuditBackoffBase,
		backoffMax:     decisionAuditBackoffMax,
		drainTimeout:   decisionAuditDrainTimeout,
		dropLogEvery:   decisionAuditDropLogEvery,
	}
}

type decisionAuditDropKey struct {
	reason   DecisionAuditDropReason
	decision string
}

type decisionAuditMetricsBox struct{ r DecisionAuditMetricsRecorder }

// StoreDecisionAuditEmitter implements DecisionAuditEmitter using the store,
// through a bounded buffered writer (see the comment at the top of this
// file). Call Close to drain pending records on shutdown.
type StoreDecisionAuditEmitter struct {
	store   store.Store
	logger  *slog.Logger
	cfg     decisionAuditWriterConfig
	metrics atomic.Pointer[decisionAuditMetricsBox]

	mu         sync.Mutex
	queue      []*store.DecisionAuditRecord
	allowCount int // allow records currently in queue
	closed     bool

	wake      chan struct{}
	stop      chan struct{}
	abortCtx  context.Context // cancelled when the drain deadline passes
	abort     context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once

	dropMu  sync.Mutex
	drops   map[decisionAuditDropKey]int64
	pending map[decisionAuditDropKey]int64 // since the last drop log
	lastErr error
	lastLog time.Time
}

// NewStoreDecisionAuditEmitter creates a store-backed decision audit emitter
// and starts its writer workers.
func NewStoreDecisionAuditEmitter(s store.Store, logger *slog.Logger) *StoreDecisionAuditEmitter {
	return newStoreDecisionAuditEmitter(s, logger, defaultDecisionAuditWriterConfig())
}

func newStoreDecisionAuditEmitter(s store.Store, logger *slog.Logger, cfg decisionAuditWriterConfig) *StoreDecisionAuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	e := &StoreDecisionAuditEmitter{
		store:   s,
		logger:  logger,
		cfg:     cfg,
		queue:   make([]*store.DecisionAuditRecord, 0, cfg.queueSize),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		drops:   map[decisionAuditDropKey]int64{},
		pending: map[decisionAuditDropKey]int64{},
	}
	e.abortCtx, e.abort = context.WithCancel(context.Background())
	for i := 0; i < cfg.workers; i++ {
		e.wg.Add(1)
		go e.worker()
	}
	return e
}

// SetMetrics swaps in a metrics recorder. A nil recorder disables metrics.
func (e *StoreDecisionAuditEmitter) SetMetrics(r DecisionAuditMetricsRecorder) {
	if r == nil {
		e.metrics.Store(nil)
		return
	}
	e.metrics.Store(&decisionAuditMetricsBox{r: r})
}

func (e *StoreDecisionAuditEmitter) recorder() DecisionAuditMetricsRecorder {
	if b := e.metrics.Load(); b != nil {
		return b.r
	}
	return noopDecisionAuditMetrics{}
}

func decisionAuditLabel(record *store.DecisionAuditRecord) string {
	switch record.Result {
	case "allow", "deny":
		return record.Result
	default:
		return "unknown"
	}
}

// EmitDecisionAudit enqueues a decision audit record without blocking. If
// the queue is full, a non-allow record evicts the oldest queued allow
// record; otherwise the incoming record is dropped and counted.
func (e *StoreDecisionAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	if record == nil {
		return
	}
	decision := decisionAuditLabel(record)
	shed := false

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.recordDrop(DecisionAuditDropShutdown, decision, nil)
		return
	}
	if len(e.queue) >= e.cfg.queueSize {
		if decision == "allow" || e.allowCount == 0 {
			e.mu.Unlock()
			e.recordDrop(DecisionAuditDropQueueFull, decision, nil)
			return
		}
		for i, r := range e.queue {
			if decisionAuditLabel(r) == "allow" {
				e.queue = append(e.queue[:i], e.queue[i+1:]...)
				e.allowCount--
				shed = true
				break
			}
		}
	}
	e.queue = append(e.queue, record)
	if decision == "allow" {
		e.allowCount++
	}
	depth := len(e.queue)
	e.mu.Unlock()

	e.recorder().SetDecisionAuditQueueDepth(int64(depth))
	e.signal()
	if shed {
		e.recordDrop(DecisionAuditDropQueueFull, "allow", nil)
	}
}

func (e *StoreDecisionAuditEmitter) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *StoreDecisionAuditEmitter) worker() {
	defer e.wg.Done()
	for {
		record, ok := e.next()
		if !ok {
			return
		}
		e.write(record)
	}
}

// next pops the oldest queued record, waiting for one if the queue is
// empty. It reports false once the emitter is closed and the queue is
// empty, or once the drain deadline has passed.
func (e *StoreDecisionAuditEmitter) next() (*store.DecisionAuditRecord, bool) {
	for {
		e.mu.Lock()
		if e.abortCtx.Err() != nil {
			e.mu.Unlock()
			return nil, false
		}
		if n := len(e.queue); n > 0 {
			record := e.queue[0]
			e.queue[0] = nil
			e.queue = e.queue[1:]
			if decisionAuditLabel(record) == "allow" {
				e.allowCount--
			}
			e.mu.Unlock()
			e.recorder().SetDecisionAuditQueueDepth(int64(n - 1))
			if n > 1 {
				e.signal() // hand the wake-up on to another idle worker
			}
			return record, true
		}
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-e.wake:
		case <-e.stop:
		}
	}
}

// write persists one record, retrying transient errors with capped
// exponential backoff. The record ID is assigned before the first attempt
// so a retry after an attempt that committed but timed out is recognised as
// a duplicate rather than written twice.
func (e *StoreDecisionAuditEmitter) write(record *store.DecisionAuditRecord) {
	if record.ID == "" {
		record.ID = uuid.NewString()
	}
	var err error
	backoff := e.cfg.backoffBase
	for attempt := 1; attempt <= e.cfg.maxAttempts; attempt++ {
		start := time.Now()
		err = e.attempt(record)
		e.recorder().RecordDecisionAuditWrite(time.Since(start), err == nil)
		if err == nil || (attempt > 1 && errors.Is(err, store.ErrAlreadyExists)) {
			return
		}
		if !decisionAuditRetryable(err) || attempt == e.cfg.maxAttempts || !e.sleep(backoff) {
			break
		}
		if backoff *= 2; backoff > e.cfg.backoffMax {
			backoff = e.cfg.backoffMax
		}
	}
	reason := DecisionAuditDropWriteFailed
	if e.abortCtx.Err() != nil {
		reason = DecisionAuditDropShutdown
	}
	e.recordDrop(reason, decisionAuditLabel(record), err)
}

func (e *StoreDecisionAuditEmitter) attempt(record *store.DecisionAuditRecord) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in decision audit write: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(e.abortCtx, e.cfg.attemptTimeout)
	defer cancel()
	return e.store.CreateDecisionAudit(ctx, record)
}

// sleep waits for a jittered backoff in [d/2, d]. It reports false if the
// drain deadline passed while waiting.
func (e *StoreDecisionAuditEmitter) sleep(d time.Duration) bool {
	if d > 1 {
		d = d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-e.abortCtx.Done():
		return false
	}
}

func decisionAuditRetryable(err error) bool {
	return !errors.Is(err, store.ErrInvalidInput) && !errors.Is(err, store.ErrAlreadyExists)
}

// recordDrop counts a lost record and logs a summary at most once per
// dropLogEvery.
func (e *StoreDecisionAuditEmitter) recordDrop(reason DecisionAuditDropReason, decision string, err error) {
	e.recorder().RecordDecisionAuditDrop(reason, decision)
	e.dropMu.Lock()
	k := decisionAuditDropKey{reason: reason, decision: decision}
	e.drops[k]++
	e.pending[k]++
	if err != nil {
		e.lastErr = err
	}
	if time.Since(e.lastLog) < e.cfg.dropLogEvery {
		e.dropMu.Unlock()
		return
	}
	e.dropMu.Unlock()
	e.logDrops()
}

// logDrops emits one WARN summarising drops since the previous summary.
func (e *StoreDecisionAuditEmitter) logDrops() {
	e.dropMu.Lock()
	pending, lastErr := e.pending, e.lastErr
	e.pending, e.lastErr, e.lastLog = map[decisionAuditDropKey]int64{}, nil, time.Now()
	e.dropMu.Unlock()
	if len(pending) == 0 {
		return
	}
	var total int64
	parts := make([]string, 0, len(pending))
	for k, n := range pending {
		total += n
		parts = append(parts, fmt.Sprintf("%s/%s=%d", k.reason, k.decision, n))
	}
	sort.Strings(parts)
	attrs := []any{"dropped", total, "by_reason", strings.Join(parts, " "), "log_interval", e.cfg.dropLogEvery}
	if lastErr != nil {
		attrs = append(attrs, "last_error", lastErr)
	}
	e.logger.Warn("decision audit records dropped", attrs...)
}

// droppedCount returns the number of records dropped for reason/decision.
func (e *StoreDecisionAuditEmitter) droppedCount(reason DecisionAuditDropReason, decision string) int64 {
	e.dropMu.Lock()
	defer e.dropMu.Unlock()
	return e.drops[decisionAuditDropKey{reason: reason, decision: decision}]
}

// Close stops accepting records and drains the queue, waiting at most until
// ctx is done or the drain timeout passes, whichever is first. Records still
// pending at the deadline are counted as shutdown drops. Records emitted
// after Close are counted as shutdown drops. Safe to call more than once.
func (e *StoreDecisionAuditEmitter) Close(ctx context.Context) {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		close(e.stop)

		drainCtx, cancel := context.WithTimeout(ctx, e.cfg.drainTimeout)
		defer cancel()
		done := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-drainCtx.Done():
			e.abort()
			select {
			case <-done:
			case <-time.After(decisionAuditAbortGrace):
			}
		}
		e.abort()

		e.mu.Lock()
		rest := e.queue
		e.queue, e.allowCount = nil, 0
		e.mu.Unlock()
		for _, r := range rest {
			e.recordDrop(DecisionAuditDropShutdown, decisionAuditLabel(r), nil)
		}
		e.recorder().SetDecisionAuditQueueDepth(0)
		e.logDrops()
	})
}
