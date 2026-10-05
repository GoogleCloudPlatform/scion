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
// EmitDecisionAudit never blocks on the store and never logs: it appends to
// one of two fixed-capacity in-memory FIFOs (deny/unknown records and allow
// records) under a short mutex and returns. A small fixed pool of workers
// drains the deny FIFO first, retrying
// transient store errors with capped, jittered exponential backoff. When the
// queue is full, allow records are shed before deny records. Every lost
// record is counted (by reason and decision); a writer-owned goroutine logs
// a summary of new drops once per dropLogEvery. Close drains the queue on
// shutdown, bounded by a deadline.
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
	// decisionAuditDrainTimeout and decisionAuditAbortGrace together bound
	// Close at 5s: the drain runs for up to decisionAuditDrainTimeout, then
	// in-flight writes are cancelled and Close waits up to
	// decisionAuditAbortGrace for the workers to return.
	decisionAuditDrainTimeout = 4 * time.Second
	decisionAuditAbortGrace   = time.Second
	decisionAuditDropLogEvery = 30 * time.Second
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

// DecisionAuditWriteOutcome is the closed set of "outcome" labels on the
// decision audit write duration histogram.
type DecisionAuditWriteOutcome string

const (
	// DecisionAuditWriteOK: the attempt wrote the record.
	DecisionAuditWriteOK DecisionAuditWriteOutcome = "ok"
	// DecisionAuditWriteDuplicate: a retry found the record already
	// written by an earlier attempt that committed but timed out. The
	// record counts as written.
	DecisionAuditWriteDuplicate DecisionAuditWriteOutcome = "duplicate"
	// DecisionAuditWriteError: the attempt failed.
	DecisionAuditWriteError DecisionAuditWriteOutcome = "error"
)

// DecisionAuditMetricsRecorder receives decision audit writer events. The
// "decision" label is "allow", "deny" or "unknown"; "unknown" is defensive
// only, as BuildDecisionAuditRecord produces only allow and deny records.
// Queue depth is not pushed through this interface: it is read on demand
// from StoreDecisionAuditEmitter.QueueDepth.
type DecisionAuditMetricsRecorder interface {
	RecordDecisionAuditDrop(reason DecisionAuditDropReason, decision string)
	RecordDecisionAuditWrite(latency time.Duration, outcome DecisionAuditWriteOutcome)
}

type noopDecisionAuditMetrics struct{}

func (noopDecisionAuditMetrics) RecordDecisionAuditDrop(DecisionAuditDropReason, string) {}
func (noopDecisionAuditMetrics) RecordDecisionAuditWrite(time.Duration, DecisionAuditWriteOutcome) {
}

type decisionAuditWriterConfig struct {
	queueSize      int
	workers        int
	maxAttempts    int
	attemptTimeout time.Duration
	backoffBase    time.Duration
	backoffMax     time.Duration
	drainTimeout   time.Duration
	abortGrace     time.Duration
	dropLogEvery   time.Duration
	dropLogTick    <-chan time.Time // tests only; nil means a dropLogEvery ticker
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
		abortGrace:     decisionAuditAbortGrace,
		dropLogEvery:   decisionAuditDropLogEvery,
	}
}

type decisionAuditDropKey struct {
	reason   DecisionAuditDropReason
	decision string
}

type decisionAuditMetricsBox struct{ r DecisionAuditMetricsRecorder }

// decisionAuditFIFO is a slice-backed FIFO. push and pop are amortised
// O(1); the backing array is released when the FIFO empties.
type decisionAuditFIFO struct{ items []*store.DecisionAuditRecord }

func (q *decisionAuditFIFO) len() int { return len(q.items) }

func (q *decisionAuditFIFO) push(r *store.DecisionAuditRecord) { q.items = append(q.items, r) }

func (q *decisionAuditFIFO) pop() *store.DecisionAuditRecord {
	r := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	if len(q.items) == 0 {
		q.items = nil
	}
	return r
}

// StoreDecisionAuditEmitter implements DecisionAuditEmitter using the store,
// through a bounded buffered writer (see the comment at the top of this
// file). Call Close to drain pending records on shutdown.
type StoreDecisionAuditEmitter struct {
	store   store.Store
	logger  *slog.Logger
	cfg     decisionAuditWriterConfig
	metrics atomic.Pointer[decisionAuditMetricsBox]

	mu     sync.Mutex
	denies decisionAuditFIFO // deny and unknown records; written first
	allows decisionAuditFIFO // allow records; shed first when full
	closed bool

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
}

// NewStoreDecisionAuditEmitter creates a store-backed decision audit
// emitter and starts its writer workers and drop log goroutine.
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
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		drops:   map[decisionAuditDropKey]int64{},
		pending: map[decisionAuditDropKey]int64{},
	}
	e.abortCtx, e.abort = context.WithCancel(context.Background())
	// Started eagerly rather than on the first record, so the goroutine
	// count of a Server is fixed once New returns.
	e.wg.Add(cfg.workers + 1)
	for i := 0; i < cfg.workers; i++ {
		go e.worker()
	}
	go e.dropLogLoop()
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

// QueueDepth reports the number of queued records waiting to be written. It
// does not count records a worker is writing or retrying (in-flight). It reads
// the queue under the lock, so a metric observing it is never stale.
func (e *StoreDecisionAuditEmitter) QueueDepth() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.denies.len() + e.allows.len()
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
	if e.denies.len()+e.allows.len() >= e.cfg.queueSize {
		if decision == "allow" || e.allows.len() == 0 {
			e.mu.Unlock()
			e.recordDrop(DecisionAuditDropQueueFull, decision, nil)
			return
		}
		e.allows.pop()
		shed = true
	}
	if decision == "allow" {
		e.allows.push(record)
	} else {
		e.denies.push(record)
	}
	e.mu.Unlock()

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

// next pops the oldest queued deny/unknown record, or failing that the
// oldest allow record, waiting for one if the queue is empty. It reports
// false once the emitter is closed and the queue is empty, or once the
// drain deadline has passed.
func (e *StoreDecisionAuditEmitter) next() (*store.DecisionAuditRecord, bool) {
	for {
		e.mu.Lock()
		if e.abortCtx.Err() != nil {
			e.mu.Unlock()
			return nil, false
		}
		var record *store.DecisionAuditRecord
		switch {
		case e.denies.len() > 0:
			record = e.denies.pop()
		case e.allows.len() > 0:
			record = e.allows.pop()
		}
		if record != nil {
			more := e.denies.len()+e.allows.len() > 0
			e.mu.Unlock()
			if more {
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
	aborted := false
	backoff := e.cfg.backoffBase
	for attempt := 1; attempt <= e.cfg.maxAttempts; attempt++ {
		start := time.Now()
		err = e.attempt(record)
		outcome := DecisionAuditWriteError
		switch {
		case err == nil:
			outcome = DecisionAuditWriteOK
		case attempt > 1 && errors.Is(err, store.ErrAlreadyExists):
			outcome = DecisionAuditWriteDuplicate
		}
		e.recorder().RecordDecisionAuditWrite(time.Since(start), outcome)
		if outcome != DecisionAuditWriteError {
			return
		}
		if !decisionAuditRetryable(err) || attempt == e.cfg.maxAttempts {
			break
		}
		if !e.sleep(backoff) {
			aborted = true
			break
		}
		if backoff *= 2; backoff > e.cfg.backoffMax {
			backoff = e.cfg.backoffMax
		}
	}
	// The reason comes from how the loop ended, not from the abort state
	// now: a permanent error stays write_failed even if the drain deadline
	// passes right after it.
	reason := DecisionAuditDropWriteFailed
	if aborted || (errors.Is(err, context.Canceled) && e.abortCtx.Err() != nil) {
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

// recordDrop counts a lost record. It never logs, so it is safe on the
// decision path; dropLogLoop and Close report the counts.
func (e *StoreDecisionAuditEmitter) recordDrop(reason DecisionAuditDropReason, decision string, err error) {
	e.recorder().RecordDecisionAuditDrop(reason, decision)
	e.dropMu.Lock()
	defer e.dropMu.Unlock()
	k := decisionAuditDropKey{reason: reason, decision: decision}
	e.drops[k]++
	e.pending[k]++
	if err != nil {
		e.lastErr = err
	}
}

// dropLogLoop logs a summary of new drops once per dropLogEvery until
// Close, which logs the final summary itself.
func (e *StoreDecisionAuditEmitter) dropLogLoop() {
	defer e.wg.Done()
	tick := e.cfg.dropLogTick
	if tick == nil {
		t := time.NewTicker(e.cfg.dropLogEvery)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-tick:
			e.logDrops()
		case <-e.stop:
			return
		}
	}
}

// logDrops emits one WARN summarising drops since the previous summary. It
// logs nothing if there were none.
func (e *StoreDecisionAuditEmitter) logDrops() {
	e.dropMu.Lock()
	pending, lastErr := e.pending, e.lastErr
	e.pending, e.lastErr = map[decisionAuditDropKey]int64{}, nil
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

// Close stops accepting records and drains the queue. The drain runs until
// the drain timeout passes or ctx is done, whichever is first; then
// in-flight writes are cancelled and Close waits up to the abort grace for
// the workers to return (5s in total by default). Records still pending
// then are counted as shutdown drops, as are records emitted after Close
// (those are counted in the metric and drop totals, not logged). Close
// logs a final drop summary. It is safe to call more than once and
// concurrently; other calls wait for the first to finish.
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
		stuck := false
		select {
		case <-done:
		case <-drainCtx.Done():
			e.abort()
			select {
			case <-done:
			case <-time.After(e.cfg.abortGrace):
				stuck = true
			}
		}
		e.abort()

		e.mu.Lock()
		rest := make([]*store.DecisionAuditRecord, 0, e.denies.len()+e.allows.len())
		rest = append(rest, e.denies.items...)
		rest = append(rest, e.allows.items...)
		e.denies, e.allows = decisionAuditFIFO{}, decisionAuditFIFO{}
		e.mu.Unlock()
		for _, r := range rest {
			e.recordDrop(DecisionAuditDropShutdown, decisionAuditLabel(r), nil)
		}
		e.logDrops()
		if stuck {
			// A worker is stuck in a store call that ignores ctx. Its
			// record is counted when the call returns; log it then.
			go func() {
				<-done
				e.logDrops()
			}()
		}
	})
}
