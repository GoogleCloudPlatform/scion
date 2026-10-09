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

// Package asyncwrite provides a generic, bounded, non-blocking write queue
// drained by exactly one worker goroutine.
//
// The package knows nothing about logging or about the values it carries: a
// producer hands it an opaque value plus the number of payload bytes the
// producer has accounted for that value, and the worker passes the value to
// an injected write function.
//
// # Guarantees
//
//   - TryEnqueue never waits on I/O or on the worker. It does O(1) work under
//     a mutex and either admits the item or rejects it (ErrFull, ErrClosed).
//   - Admitted items are written in FIFO order by a single worker.
//   - Every admitted item gets exactly one terminal outcome (written, error,
//     timeout or shutdown), decided by whichever party claims it first. At
//     every quiescent observation, Enqueued equals Written + WriteErrors +
//     WriteTimeouts + DroppedShutdown + Queued + InflightUnresolved, where
//     InflightUnresolved is 0 or 1.
//   - Rejected attempts (queue_full, oversize, unsupported, closed) are
//     counted as failures but are never admitted, so they do not appear in
//     the conservation identity.
//
// # Loss semantics
//
// A "written" outcome means only that the write function returned nil (for
// a log handler: the handler accepted the record; for a cloud client that is
// its local buffer, not remote ingestion). Losses are counted and never
// retried.
//
// # Noncooperative writes
//
// Nothing can preempt a goroutine blocked inside the write function. A write
// that exceeds WriteBudget is claimed as a timeout by a per-write timer
// callback, but the worker stays blocked until the write returns. Close is
// bounded by its deadline and returns ErrWorkerStuck in that case; exactly one
// worker goroutine per Writer may then outlive Close, holding exactly one
// in-flight item. No per-write goroutine is ever created.
package asyncwrite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Default parameters (design D2 / C1.1).
const (
	DefaultCapacity       = 2048
	DefaultMaxQueuedBytes = 2 << 20 // 2 MiB of accounted payload
	DefaultWriteBudget    = 2 * time.Second
	DefaultDrainTimeout   = 5 * time.Second
	DefaultFailureWindow  = 5 * time.Minute
)

// Errors returned by TryEnqueue and Close.
var (
	// ErrFull reports that admitting the item would exceed the count bound
	// or the queued-byte bound. The item was dropped (drop-newest).
	ErrFull = errors.New("asyncwrite: queue full")
	// ErrClosed reports that the writer has been closed.
	ErrClosed = errors.New("asyncwrite: writer closed")
	// ErrWorkerStuck reports that Close reached its deadline while the
	// worker was still inside a write.
	ErrWorkerStuck = errors.New("asyncwrite: worker stuck in write")
)

// Result is the closed set of outcomes reported to a Recorder.
type Result uint8

const (
	// ResultWritten is the terminal outcome of a successful write.
	ResultWritten Result = iota + 1
	// ResultError is the terminal outcome of a write that returned an error
	// or panicked.
	ResultError
	// ResultTimeout is the terminal outcome of a write that exceeded the
	// write budget.
	ResultTimeout
	// ResultShutdown is the terminal outcome of an item still queued at the
	// Close deadline.
	ResultShutdown
	// ResultQueueFull is a rejection: the count or byte bound was reached.
	ResultQueueFull
	// ResultOversize is a rejection reported by a producer (Reject) for a
	// value exceeding its per-record limits.
	ResultOversize
	// ResultUnsupported is a rejection reported by a producer (Reject) for a
	// value of an unsupported shape.
	ResultUnsupported
	// ResultClosed is a rejection: TryEnqueue after Close.
	ResultClosed
	// ResultLateReturn is not a failure: a write returned after its timer
	// had already claimed it as a timeout.
	ResultLateReturn
)

// String returns the bounded metric label for the result.
func (r Result) String() string {
	switch r {
	case ResultWritten:
		return "written"
	case ResultError:
		return "error"
	case ResultTimeout:
		return "timeout"
	case ResultShutdown:
		return "shutdown"
	case ResultQueueFull:
		return "queue_full"
	case ResultOversize:
		return "oversize"
	case ResultUnsupported:
		return "unsupported"
	case ResultClosed:
		return "closed"
	case ResultLateReturn:
		return "late_return"
	default:
		return "unknown"
	}
}

// IsFailure reports whether the result is counted as a write failure
// (scion.logging.write.failures). Rejections and the terminal failure
// outcomes are failures; written and late_return are not.
func (r Result) IsFailure() bool {
	switch r {
	case ResultError, ResultTimeout, ResultShutdown, ResultQueueFull,
		ResultOversize, ResultUnsupported, ResultClosed:
		return true
	default:
		return false
	}
}

// IsRejection reports whether the result describes an attempt that was
// never admitted to the queue.
func (r Result) IsRejection() bool {
	switch r {
	case ResultQueueFull, ResultOversize, ResultUnsupported, ResultClosed:
		return true
	default:
		return false
	}
}

// Recorder optionally mirrors writer outcomes into an external metrics
// system. Implementations must be cheap and non-blocking: Record may be
// called from a request goroutine, the worker, or a timer callback.
type Recorder interface {
	Record(writer string, result Result)
}

// Timer is the subset of *time.Timer the writer needs.
type Timer interface {
	Stop() bool
}

// AfterFunc schedules f after d. It matches time.AfterFunc and is injectable
// for deterministic tests.
type AfterFunc func(d time.Duration, f func()) Timer

func realAfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Config configures a Writer.
type Config struct {
	// Name is a bounded metric label; use a compile-time constant
	// (for example "audit").
	Name string
	// Capacity is the maximum number of queued items (default 2048).
	Capacity int
	// MaxQueuedBytes bounds the sum of producer-accounted bytes over queued
	// items (default 2 MiB). It bounds retained accounted payload, not
	// process RSS.
	MaxQueuedBytes int64
	// WriteBudget is the per-write budget after which a write is claimed as
	// a timeout (default 2s).
	WriteBudget time.Duration
	// DrainTimeout is the upper bound on Close (default 5s).
	DrainTimeout time.Duration
	// FailureWindow is how long a failure keeps Health degraded
	// (default 5m).
	FailureWindow time.Duration

	// Now is the clock (default time.Now). Injectable for tests.
	Now func() time.Time
	// AfterFunc schedules write-budget and drain-deadline timers
	// (default time.AfterFunc). Injectable for tests.
	AfterFunc AfterFunc
}

func (c Config) withDefaults() Config {
	if c.Capacity == 0 {
		c.Capacity = DefaultCapacity
	}
	if c.MaxQueuedBytes == 0 {
		c.MaxQueuedBytes = DefaultMaxQueuedBytes
	}
	if c.WriteBudget == 0 {
		c.WriteBudget = DefaultWriteBudget
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.FailureWindow == 0 {
		c.FailureWindow = DefaultFailureWindow
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.AfterFunc == nil {
		c.AfterFunc = realAfterFunc
	}
	return c
}

func (c Config) validate() error {
	switch {
	case c.Name == "":
		return errors.New("asyncwrite: Name is required")
	case c.Capacity < 1:
		return fmt.Errorf("asyncwrite: Capacity must be positive, got %d", c.Capacity)
	case c.MaxQueuedBytes < 1:
		return fmt.Errorf("asyncwrite: MaxQueuedBytes must be positive, got %d", c.MaxQueuedBytes)
	case c.WriteBudget < 0 || c.DrainTimeout < 0 || c.FailureWindow < 0:
		return errors.New("asyncwrite: durations must not be negative")
	}
	return nil
}

// Stats holds in-process counters. They are always present, whether or not
// a Recorder is attached, and Health reads them.
type Stats struct {
	// Enqueued counts admitted items only.
	Enqueued atomic.Uint64
	// Terminal outcomes of admitted items.
	Written         atomic.Uint64
	WriteErrors     atomic.Uint64
	WriteTimeouts   atomic.Uint64
	DroppedShutdown atomic.Uint64
	// Rejected attempts (never admitted).
	DroppedFull        atomic.Uint64
	DroppedOversize    atomic.Uint64
	DroppedUnsupported atomic.Uint64
	DroppedClosed      atomic.Uint64
	// LateReturns counts writes that returned after their timer had
	// already claimed them as timeouts. Not a failure.
	LateReturns atomic.Uint64
	// LastFailureUnixNano is the sampled observation time of the most
	// recent failure, stored with a monotonic max. Zero means none.
	LastFailureUnixNano atomic.Int64
}

// Stats returns the writer's live counters. Callers must treat them as
// read-only.
func (w *Writer[T]) Stats() *Stats { return &w.stats }

// Snapshot is a plain copy of the counters plus queue state.
type Snapshot struct {
	Enqueued, Written, WriteErrors, WriteTimeouts, DroppedShutdown  uint64
	DroppedFull, DroppedOversize, DroppedUnsupported, DroppedClosed uint64
	LateReturns                                                     uint64
	LastFailure                                                     time.Time
	Queued                                                          int
	QueuedBytes                                                     int64
	// InflightUnresolved is 1 while a popped item has no terminal outcome.
	InflightUnresolved int
	Stalled            bool
	Closed             bool
}

// HealthReason is the closed set of health reasons.
type HealthReason uint8

const (
	HealthOK HealthReason = iota
	HealthRecentFailures
	HealthStalled
	HealthClosed
)

// String returns a fixed, non-sensitive description.
func (r HealthReason) String() string {
	switch r {
	case HealthOK:
		return "healthy"
	case HealthRecentFailures:
		return "recent write failures"
	case HealthStalled:
		return "writer stalled"
	case HealthClosed:
		return "writer closed"
	default:
		return "unknown"
	}
}

// Health is a point-in-time health evaluation.
type Health struct {
	Healthy bool
	Reason  HealthReason
}

// ticket states.
const (
	stateInflight uint32 = iota
	stateWritten
	stateError
	stateTimeout
)

// ticket is allocated by the worker for one write. gen is immutable; state
// is decided once by CAS from stateInflight.
type ticket struct {
	gen   uint64
	state atomic.Uint32
}

type entry[T any] struct {
	v     T
	bytes int64
}

// Writer is a bounded, non-blocking FIFO drained by one worker.
type Writer[T any] struct {
	cfg   Config
	write func(T) error
	stats Stats

	recorder atomic.Pointer[recorderBox]

	mu          sync.Mutex
	ring        []entry[T]
	head        int
	count       int
	queuedBytes int64
	closed      bool
	busy        bool // worker holds a popped item (set/cleared under mu)
	gen         uint64

	wake chan struct{} // 1-slot
	done chan struct{} // closed when the worker exits

	// current is the in-flight ticket; written only by the worker.
	current atomic.Pointer[ticket]

	closeMu sync.Mutex

	hooks testHooks
}

// testHooks are test-only observation points; all nil in production. They
// are fixed before the worker starts, so reading them is race-free.
type testHooks struct {
	afterClaim func() // right after a winning terminal CAS, before ts
	afterWrite func() // after the worker cleared current for an item
}

type recorderBox struct{ r Recorder }

// New creates a Writer and starts its single worker goroutine. write is
// called sequentially from that goroutine; it must not call back into the
// Writer.
func New[T any](cfg Config, write func(T) error) (*Writer[T], error) {
	return newWriter(cfg, write, testHooks{})
}

func newWriter[T any](cfg Config, write func(T) error, hooks testHooks) (*Writer[T], error) {
	if write == nil {
		return nil, errors.New("asyncwrite: write function is required")
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	w := &Writer[T]{
		cfg:   cfg,
		write: write,
		ring:  make([]entry[T], cfg.Capacity),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
		hooks: hooks,
	}
	go w.run()
	return w, nil
}

// Name returns the writer's metric label.
func (w *Writer[T]) Name() string { return w.cfg.Name }

// SetRecorder attaches (or, with nil, detaches) an optional Recorder.
func (w *Writer[T]) SetRecorder(r Recorder) {
	if r == nil {
		w.recorder.Store(nil)
		return
	}
	w.recorder.Store(&recorderBox{r: r})
}

func (w *Writer[T]) record(r Result) {
	if box := w.recorder.Load(); box != nil {
		box.r.Record(w.cfg.Name, r)
	}
}

// TryEnqueue admits v, whose producer-accounted payload size is bytes, or
// rejects it without blocking. It returns ErrClosed after Close and ErrFull
// when either bound would be exceeded.
func (w *Writer[T]) TryEnqueue(v T, bytes int64) error {
	if bytes < 0 {
		bytes = 0
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.reject(ResultClosed)
		return ErrClosed
	}
	if w.count >= len(w.ring) || bytes > w.cfg.MaxQueuedBytes-w.queuedBytes {
		w.mu.Unlock()
		w.reject(ResultQueueFull)
		return ErrFull
	}
	w.ring[(w.head+w.count)%len(w.ring)] = entry[T]{v: v, bytes: bytes}
	w.count++
	w.queuedBytes += bytes
	w.stats.Enqueued.Add(1)
	w.mu.Unlock()
	w.signal()
	return nil
}

// Reject counts a producer-side rejection (ResultOversize or
// ResultUnsupported) for an attempt that was never admitted. Other results
// are ignored.
func (w *Writer[T]) Reject(r Result) {
	if r != ResultOversize && r != ResultUnsupported {
		return
	}
	w.reject(r)
}

func (w *Writer[T]) reject(r Result) {
	switch r {
	case ResultQueueFull:
		w.stats.DroppedFull.Add(1)
	case ResultOversize:
		w.stats.DroppedOversize.Add(1)
	case ResultUnsupported:
		w.stats.DroppedUnsupported.Add(1)
	case ResultClosed:
		w.stats.DroppedClosed.Add(1)
	}
	w.noteFailure(w.cfg.Now())
	w.record(r)
}

func (w *Writer[T]) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// noteFailure stores ts into LastFailure with a monotonic max.
func (w *Writer[T]) noteFailure(ts time.Time) {
	n := ts.UnixNano()
	for {
		old := w.stats.LastFailureUnixNano.Load()
		if n <= old || w.stats.LastFailureUnixNano.CompareAndSwap(old, n) {
			return
		}
	}
}

func (w *Writer[T]) run() {
	defer close(w.done)
	for {
		w.mu.Lock()
		if w.count == 0 {
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			<-w.wake
			continue
		}
		e := w.ring[w.head]
		var zero entry[T]
		w.ring[w.head] = zero
		w.head = (w.head + 1) % len(w.ring)
		w.count--
		w.queuedBytes -= e.bytes
		w.gen++
		tk := &ticket{gen: w.gen}
		w.busy = true
		w.current.Store(tk)
		w.mu.Unlock()

		w.writeOne(tk, e.v)

		w.mu.Lock()
		w.current.Store(nil)
		w.busy = false
		w.mu.Unlock()
		if w.hooks.afterWrite != nil {
			w.hooks.afterWrite()
		}
	}
}

func (w *Writer[T]) writeOne(tk *ticket, v T) {
	t := w.cfg.AfterFunc(w.cfg.WriteBudget, func() { w.expire(tk) })
	err := w.safeWrite(v)
	if t != nil {
		t.Stop() // false (callback started) is harmless: it acts only on tk
	}
	outcome := stateWritten
	if err != nil {
		outcome = stateError
	}
	if !tk.state.CompareAndSwap(stateInflight, outcome) {
		w.stats.LateReturns.Add(1)
		w.record(ResultLateReturn)
		return
	}
	if w.hooks.afterClaim != nil {
		w.hooks.afterClaim()
	}
	if outcome == stateWritten {
		w.stats.Written.Add(1)
		w.record(ResultWritten)
		return
	}
	ts := w.cfg.Now()
	w.stats.WriteErrors.Add(1)
	w.noteFailure(ts)
	w.record(ResultError)
}

func (w *Writer[T]) safeWrite(v T) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("asyncwrite: write panicked: %v", p)
		}
	}()
	return w.write(v)
}

// expire is the write-budget timer callback for tk. It performs atomics
// only and acts only on its own ticket.
func (w *Writer[T]) expire(tk *ticket) {
	if !tk.state.CompareAndSwap(stateInflight, stateTimeout) {
		return
	}
	if w.hooks.afterClaim != nil {
		w.hooks.afterClaim()
	}
	ts := w.cfg.Now()
	w.stats.WriteTimeouts.Add(1)
	w.noteFailure(ts)
	w.record(ResultTimeout)
}

// Stalled reports whether the in-flight write has exceeded its budget. It is
// derived at read time from the current ticket and is never published.
func (w *Writer[T]) Stalled() bool {
	c := w.current.Load()
	return c != nil && c.state.Load() == stateTimeout
}

// QueueDepth returns the number of queued (not in-flight) items.
func (w *Writer[T]) QueueDepth() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(w.count)
}

// Snapshot returns a copy of the counters and queue state. Counters are read
// individually, so the copy is only exactly consistent at quiescence.
func (w *Writer[T]) Snapshot() Snapshot {
	w.mu.Lock()
	queued, qb, closed := w.count, w.queuedBytes, w.closed
	w.mu.Unlock()
	s := Snapshot{
		Enqueued:           w.stats.Enqueued.Load(),
		Written:            w.stats.Written.Load(),
		WriteErrors:        w.stats.WriteErrors.Load(),
		WriteTimeouts:      w.stats.WriteTimeouts.Load(),
		DroppedShutdown:    w.stats.DroppedShutdown.Load(),
		DroppedFull:        w.stats.DroppedFull.Load(),
		DroppedOversize:    w.stats.DroppedOversize.Load(),
		DroppedUnsupported: w.stats.DroppedUnsupported.Load(),
		DroppedClosed:      w.stats.DroppedClosed.Load(),
		LateReturns:        w.stats.LateReturns.Load(),
		Queued:             queued,
		QueuedBytes:        qb,
		Closed:             closed,
	}
	if n := w.stats.LastFailureUnixNano.Load(); n != 0 {
		s.LastFailure = time.Unix(0, n)
	}
	if c := w.current.Load(); c != nil {
		st := c.state.Load()
		if st == stateInflight {
			s.InflightUnresolved = 1
		}
		s.Stalled = st == stateTimeout
	}
	return s
}

// Health evaluates writer health at now. Precedence: closed, stalled,
// recent failures (within FailureWindow), healthy.
func (w *Writer[T]) Health(now time.Time) Health {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	switch {
	case closed:
		return Health{Reason: HealthClosed}
	case w.Stalled():
		return Health{Reason: HealthStalled}
	}
	if n := w.stats.LastFailureUnixNano.Load(); n != 0 && now.Sub(time.Unix(0, n)) < w.cfg.FailureWindow {
		return Health{Reason: HealthRecentFailures}
	}
	return Health{Healthy: true, Reason: HealthOK}
}

// Close stops admission and waits for the worker to drain, bounded by
// min(ctx deadline, DrainTimeout). Items still queued at the deadline are
// counted as shutdown. Close never assigns the in-flight item an outcome and
// does not stop its timer. It returns ErrWorkerStuck if the worker is still
// inside a write at the deadline, else nil. Close is idempotent.
func (w *Writer[T]) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.closeMu.Lock()
	defer w.closeMu.Unlock()

	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.signal()

	select {
	case <-w.done:
		return nil
	default:
	}

	deadline := make(chan struct{})
	var once sync.Once
	t := w.cfg.AfterFunc(w.cfg.DrainTimeout, func() { once.Do(func() { close(deadline) }) })
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()

	select {
	case <-w.done:
		return nil
	case <-deadline:
	case <-ctx.Done():
	}

	w.mu.Lock()
	dropped := w.count
	var zero entry[T]
	for i := 0; i < w.count; i++ {
		w.ring[(w.head+i)%len(w.ring)] = zero
	}
	w.head, w.count, w.queuedBytes = 0, 0, 0
	busy := w.busy
	w.mu.Unlock()

	if dropped > 0 {
		w.stats.DroppedShutdown.Add(uint64(dropped))
		w.noteFailure(w.cfg.Now())
		for i := 0; i < dropped; i++ {
			w.record(ResultShutdown)
		}
	}
	if busy {
		return ErrWorkerStuck
	}
	return nil
}
