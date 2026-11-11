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

// Package txdiag records how long database transactions and connection
// checkouts hold a connection, and how long requests wait to get one.
//
// It wraps the database/sql driver under the hub's *sql.DB (WrapConnector),
// so every transaction and statement on that DB is measured whether it comes
// through ent or raw SQL, and the ent driver above it (WrapEntDriver), so
// every ent request records when it asked for a connection. A hold or wait
// over its threshold is logged as one WARN per call site per interval, and
// every hold and wait is recorded in histograms when a Recorder is attached.
//
// Measurements are taken while database/sql still holds the connection, so
// nothing on that path does I/O: WARN records go through a bounded queue to
// one emitter goroutine, and a full queue drops (and counts) the record.
package txdiag

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Config fields left zero.
const (
	DefaultWarnInterval = time.Minute
	queueSize           = 64
	drainDeadline       = time.Second
	closeMargin         = 100 * time.Millisecond
)

// Kinds of hold.
const (
	KindTx   = "tx"
	KindConn = "conn"
)

// Outcomes.
const (
	OutcomeCommit   = "commit"
	OutcomeRollback = "rollback"
	OutcomeError    = "error"
	OutcomeReleased = "released"
	OutcomeAcquired = "acquired"
	OutcomeCanceled = "canceled"
	OutcomeDeadline = "deadline"
)

// Dialects that change behaviour.
const (
	DialectSQLite   = "sqlite3"
	DialectPostgres = "postgres"
)

// ErrClosed is returned by Flush after Close.
var ErrClosed = errors.New("txdiag: monitor closed")

// Config configures a Monitor. The zero value measures and records metrics
// but logs nothing (both thresholds off).
type Config struct {
	// HoldWarnThreshold is the hold time at or above which a tx or conn
	// hold is logged. Zero turns hold WARNs off; metrics are still recorded.
	HoldWarnThreshold time.Duration
	// LockWaitWarnThreshold is the wait at or above which a lock_wait line
	// is logged. Zero turns lock_wait WARNs off.
	LockWaitWarnThreshold time.Duration
	// WarnInterval is the rate-limit window per (kind, op). Zero means
	// DefaultWarnInterval.
	WarnInterval time.Duration
	// Logger receives the WARN lines. Nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock. Nil means time.Now. Tests inject a fake clock.
	Now func() time.Time
	// Dialect is set by the opener (DialectSQLite or DialectPostgres). The
	// conn kind and waiters_max are SQLite-only.
	Dialect string
}

// QuietConfig returns a Config with the production thresholds whose log
// output is discarded. Shared test openers use it so every test runs
// through the wrapper's code path without the WARN lines of slow tests
// reaching unrelated tests' output.
func QuietConfig() *Config {
	return &Config{
		HoldWarnThreshold:     2 * time.Second,
		LockWaitWarnThreshold: time.Second,
		Logger:                slog.New(slog.DiscardHandler),
	}
}

// Recorder receives the hold and wait measurements. The hub's
// dbmetrics recorder implements it.
type Recorder interface {
	RecordTxHold(ctx context.Context, ms float64, op, kind, outcome string)
	RecordTxCommit(ctx context.Context, ms float64, op, outcome string)
	RecordLockWait(ctx context.Context, ms float64, op, outcome string)
	IncDiagDropped(ctx context.Context, n int64)
}

type recorderBox struct{ r Recorder }

type limKey struct{ kind, op string }

type limState struct {
	last       time.Time
	seen       bool
	suppressed int64
}

// record is one queued WARN (or a Flush sentinel when ack is non-nil).
type record struct {
	msg   string
	kind  string
	op    string
	pcs   [stackDepth]uintptr
	npcs  int
	attrs []slog.Attr
	ack   chan struct{}
}

// Monitor holds the configuration, rate limiter and emitter of one wrapped
// DB. It is created by the opener (NewMonitor) and closed by the wrapped
// connector's Close, which database/sql's DB.Close calls.
type Monitor struct {
	cfg      Config
	now      func() time.Time
	interval time.Duration
	logger   *slog.Logger
	sqlite   bool

	rec atomic.Pointer[recorderBox]

	sites *callsites

	mu      sync.Mutex // guards limiter and dropped
	limiter map[limKey]*limState
	dropped int64 // records dropped since the last emitted line

	queue     chan record // never closed
	done      chan struct{}
	exited    chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once

	// pending counts ent requests that asked for a connection and have not
	// got one; pendingHW is its high-water mark since the current checkout
	// began (SQLite: the number of waiters behind the holder).
	pending   atomic.Int64
	pendingHW atomic.Int64

	// capLogged is set once the op-cap INFO line has been queued.
	capLogged atomic.Bool

	// holders remembers recent connection checkouts (SQLite only), so a
	// lock_wait line can name who held the connection during the wait.
	holders holderRing
}

// NewMonitor returns a running Monitor. The caller must arrange for Close
// to be called (WrapConnector does, through the connector's Close).
func NewMonitor(cfg Config) *Monitor {
	m := &Monitor{
		cfg:      cfg,
		now:      cfg.Now,
		interval: cfg.WarnInterval,
		logger:   cfg.Logger,
		sqlite:   cfg.Dialect == DialectSQLite,
		sites:    newCallsites(),
		limiter:  make(map[limKey]*limState),
		queue:    make(chan record, queueSize),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.interval <= 0 {
		m.interval = DefaultWarnInterval
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	go m.emit()
	return m
}

// SetRecorder attaches (or, with nil, detaches) the metrics recorder. It is
// safe to call at any time.
func (m *Monitor) SetRecorder(r Recorder) {
	if r == nil {
		m.rec.Store(nil)
		return
	}
	m.rec.Store(&recorderBox{r: r})
}

func (m *Monitor) recorder() Recorder {
	if b := m.rec.Load(); b != nil {
		return b.r
	}
	return nil
}

// Close stops the emitter. It is idempotent and bounded: queued records are
// drained for at most drainDeadline, and Close never waits on a log handler
// that is blocked. Records produced after Close are dropped and counted.
func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		close(m.done)
		t := time.NewTimer(drainDeadline + closeMargin)
		defer t.Stop()
		select {
		case <-m.exited:
		case <-t.C:
		}
	})
}

// Flush waits until every record queued before it has been handed to the
// logger, including one in flight. It is a test hook. The sentinel send
// blocks (bounded by timeout) so a full queue cannot drop it.
func (m *Monitor) Flush(timeout time.Duration) error {
	if m.closed.Load() {
		return ErrClosed
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	ack := make(chan struct{})
	select {
	case m.queue <- record{ack: ack}:
	case <-m.done:
		return ErrClosed
	case <-t.C:
		return context.DeadlineExceeded
	}
	select {
	case <-ack:
		return nil
	case <-m.exited:
		return ErrClosed
	case <-t.C:
		return context.DeadlineExceeded
	}
}

// emit is the single goroutine that writes WARN lines.
func (m *Monitor) emit() {
	defer close(m.exited)
	for {
		select {
		case r := <-m.queue:
			m.write(r)
		case <-m.done:
			deadline := time.Now().Add(drainDeadline)
			for time.Now().Before(deadline) {
				select {
				case r := <-m.queue:
					m.write(r)
				default:
					return
				}
			}
			return
		}
	}
}

func (m *Monitor) write(r record) {
	if r.ack != nil {
		close(r.ack)
		return
	}
	attrs := r.attrs
	if r.npcs > 0 {
		attrs = append(attrs, slog.String("site", m.sites.site(r.pcs[:r.npcs])))
	}
	level := slog.LevelWarn
	if r.msg == msgOpCap {
		level = slog.LevelInfo
	}
	m.logger.LogAttrs(context.Background(), level, r.msg, attrs...)
}

const (
	msgHold     = "db_tx_long_hold"
	msgLockWait = "db_lock_wait"
	msgOpCap    = "db_diag_op_cap_reached"
)

// Messages of the WARN lines, for log queries and tests.
const (
	MessageHold     = msgHold
	MessageLockWait = msgLockWait
)

// op resolves the capped op of a stack. The first time an op falls past the
// cap, it queues the one-time cap line; if the queue is full then, a later
// call retries, so the line is not lost.
func (m *Monitor) op(pcs []uintptr) string {
	op := m.sites.op(pcs)
	if op == OpOther && !m.capLogged.Load() && m.capLogged.CompareAndSwap(false, true) {
		if !m.offer(record{msg: msgOpCap, attrs: []slog.Attr{slog.Int("max_ops", maxOps)}}) {
			m.capLogged.Store(false)
		}
	}
	return op
}

// offer queues r without blocking; it reports whether r was queued.
func (m *Monitor) offer(r record) bool {
	if m.closed.Load() {
		return false
	}
	select {
	case m.queue <- r:
		return true
	default:
		return false
	}
}

// warn applies the rate limit for (kind, op) and queues the record. The
// decision, the non-blocking send and the slot commit happen under one
// mutex, and the slot is committed only when the send succeeds, so a dropped
// record never silences its key for the interval.
func (m *Monitor) warn(r record) {
	now := m.now()
	key := limKey{r.kind, r.op}
	m.mu.Lock()
	st := m.limiter[key]
	if st == nil {
		st = &limState{}
		m.limiter[key] = st
	}
	if st.seen && now.Sub(st.last) < m.interval {
		st.suppressed++
		m.mu.Unlock()
		return
	}
	r.attrs = append(r.attrs, slog.Int64("suppressed", st.suppressed), slog.Int64("dropped", m.dropped))
	if m.offer(r) {
		st.seen = true
		st.last = now
		st.suppressed = 0
		m.dropped = 0
		m.mu.Unlock()
		return
	}
	st.suppressed++
	m.dropped++
	m.mu.Unlock()
	if rec := m.recorder(); rec != nil {
		rec.IncDiagDropped(context.Background(), 1)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// holdStats are the per-transaction (or per-checkout) statement figures.
type holdStats struct {
	stmts        int64
	stmtTime     time.Duration
	maxStmt      time.Duration
	rowsAffected int64
	busy         int64
	beginTime    time.Duration // BEGIN calls (a conn hold sums its inner transactions')
	commitTime   time.Duration // the driver Commit call(s)
	committed    bool
}

func (s *holdStats) add(d time.Duration, rows int64, busy bool) {
	s.stmts++
	s.stmtTime += d
	if d > s.maxStmt {
		s.maxStmt = d
	}
	s.rowsAffected += rows
	if busy {
		s.busy++
	}
}

// hold records a finished tx or conn hold and returns whether it was over
// the WARN threshold (so a conn hold can skip its own line).
func (m *Monitor) hold(kind, outcome string, pcs []uintptr, start time.Time, begin time.Duration, st holdStats) bool {
	d := m.now().Sub(start)
	over := m.cfg.HoldWarnThreshold > 0 && d >= m.cfg.HoldWarnThreshold
	rec := m.recorder()
	if rec == nil && !over {
		return false // nothing to record: skip resolving the op
	}
	op := m.op(pcs)
	if rec != nil {
		rec.RecordTxHold(context.Background(), ms(d), op, kind, outcome)
		if st.committed {
			rec.RecordTxCommit(context.Background(), ms(st.commitTime), op, outcome)
		}
	}
	if !over {
		return false
	}
	// idle is time the connection was held outside BEGIN, SQL and COMMIT:
	// Go-side work inside the transaction or checkout.
	idle := d - begin - st.beginTime - st.stmtTime - st.commitTime
	if idle < 0 {
		idle = 0
	}
	attrs := []slog.Attr{
		slog.String("kind", kind),
		slog.String("op", op),
		slog.Int64("hold_ms", d.Milliseconds()),
		slog.Int64("begin_ms", (begin + st.beginTime).Milliseconds()),
		slog.Int64("stmts", st.stmts),
		slog.Int64("stmt_ms", st.stmtTime.Milliseconds()),
		slog.Int64("commit_ms", st.commitTime.Milliseconds()),
		slog.Int64("idle_ms", idle.Milliseconds()),
		slog.Int64("max_stmt_ms", st.maxStmt.Milliseconds()),
		slog.Int64("rows_affected", st.rowsAffected),
		slog.String("outcome", outcome),
	}
	if m.sqlite {
		attrs = append(attrs, slog.Int64("waiters_max", m.pendingHW.Load()), slog.Int64("busy", st.busy))
	}
	r := record{msg: msgHold, kind: kind, op: op, attrs: attrs}
	r.npcs = copy(r.pcs[:], pcs)
	m.warn(r)
	return true
}

// checkoutStarted resets the waiter high-water mark to the waiters already
// queued when a checkout begins.
func (m *Monitor) checkoutStarted() {
	m.pendingHW.Store(m.pending.Load())
}

func (m *Monitor) pendingInc() {
	n := m.pending.Add(1)
	for {
		hw := m.pendingHW.Load()
		if n <= hw || m.pendingHW.CompareAndSwap(hw, n) {
			return
		}
	}
}

func (m *Monitor) pendingDec() { m.pending.Add(-1) }

// holderRingSize is how many finished checkouts holderRing keeps.
const holderRingSize = 256

// checkout is one connection checkout: when it started and ended, and the
// stack recorded at its start.
type checkout struct {
	start, end time.Time
	pcs        [stackDepth]uintptr
	npcs       int
}

// holderRing keeps the last holderRingSize finished checkouts and the one
// still open. Writes happen once per checkout start and end; reads happen
// only when a lock_wait line is about to be logged.
type holderRing struct {
	mu      sync.Mutex
	buf     [holderRingSize]checkout
	next    int // index of the next write
	count   int // filled entries, at most holderRingSize
	open    checkout
	hasOpen bool
}

func (h *holderRing) opened(start time.Time, pcs [stackDepth]uintptr, n int) {
	h.mu.Lock()
	h.open = checkout{start: start, pcs: pcs, npcs: n}
	h.hasOpen = true
	h.mu.Unlock()
}

func (h *holderRing) closed(start, end time.Time, pcs [stackDepth]uintptr, n int) {
	h.mu.Lock()
	h.buf[h.next] = checkout{start: start, end: end, pcs: pcs, npcs: n}
	h.next = (h.next + 1) % holderRingSize
	if h.count < holderRingSize {
		h.count++
	}
	if h.hasOpen && h.open.start.Equal(start) {
		h.hasOpen = false
	}
	h.mu.Unlock()
}

// overlap is how long [s, e] overlaps [from, to].
func overlap(s, e, from, to time.Time) time.Duration {
	if s.Before(from) {
		s = from
	}
	if e.After(to) {
		e = to
	}
	return e.Sub(s)
}

// attrs describes who held the connection during a wait over [from, to]:
// holders (how many checkouts overlapped it; holderRingSize or more means
// the ring was exhausted), the one with the most time in the window, and,
// when the waiter gave up, the checkout still open at that moment.
func (h *holderRing) attrs(m *Monitor, from, to time.Time, gaveUp bool) []slog.Attr {
	h.mu.Lock()
	var (
		n       int
		top     checkout
		topTime time.Duration
	)
	consider := func(c checkout, e time.Time) {
		if d := overlap(c.start, e, from, to); d > 0 {
			n++
			if d > topTime {
				top, topTime = c, d
			}
		}
	}
	for i := 0; i < h.count; i++ {
		c := h.buf[i]
		consider(c, c.end)
	}
	open, hasOpen := h.open, h.hasOpen
	if hasOpen {
		consider(open, to)
	}
	h.mu.Unlock()

	attrs := []slog.Attr{slog.Int("holders", n)}
	if n > 0 {
		attrs = append(attrs,
			slog.String("top_holder_op", m.op(top.pcs[:top.npcs])),
			slog.Int64("top_holder_ms", topTime.Milliseconds()))
	}
	if gaveUp && hasOpen && open.start.Before(to) {
		attrs = append(attrs,
			slog.String("holder_open_op", m.op(open.pcs[:open.npcs])),
			slog.Int64("holder_open_ms", to.Sub(open.start).Milliseconds()))
	}
	return attrs
}
