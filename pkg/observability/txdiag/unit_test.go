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

package txdiag

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

const flushTimeout = 5 * time.Second

type harness struct {
	clk *FakeClock
	h   *CaptureHandler
	rec *FakeRecorder
	mon *Monitor
	drv *fakeDriver
	con *fakeConnector
	db  *sql.DB
}

// newHarness opens a one-connection DB over the fake driver, wrapped with a
// Monitor using a fake clock, a capturing handler and a fake recorder.
func newHarness(t *testing.T, dialectName string, mutate func(*Config, *fakeDriver)) *harness {
	t.Helper()
	hs := &harness{clk: NewFakeClock(), h: &CaptureHandler{}, rec: &FakeRecorder{}, drv: newFakeDriver()}
	cfg := Config{
		HoldWarnThreshold:     2 * time.Second,
		LockWaitWarnThreshold: time.Second,
		Logger:                newLogger(hs.h),
		Now:                   hs.clk.Now,
		Dialect:               dialectName,
	}
	if mutate != nil {
		mutate(&cfg, hs.drv)
	}
	hs.mon = NewMonitor(cfg)
	hs.mon.SetRecorder(hs.rec)
	hs.con = &fakeConnector{d: hs.drv}
	hs.db = sql.OpenDB(WrapConnector(hs.con, hs.mon))
	hs.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = hs.db.Close() })
	return hs
}

func (hs *harness) flush(t *testing.T) {
	t.Helper()
	if err := hs.mon.Flush(flushTimeout); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// holdTx begins a transaction, advances the clock by d inside it, and
// commits.
func (hs *harness) holdTx(t *testing.T, d time.Duration) {
	t.Helper()
	tx, err := hs.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("INSERT"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	hs.clk.Advance(d)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestHoldWarnFiresOnceAboveThreshold(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.holdTx(t, time.Second) // below 2s
	hs.flush(t)
	if got := hs.h.Lines(MsgHold); len(got) != 0 {
		t.Fatalf("below threshold: got %d hold lines, want 0", len(got))
	}
	hs.holdTx(t, 3*time.Second)
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if l.Str("kind") != KindTx || l.Int("hold_ms") != 3000 || l.Str("outcome") != OutcomeCommit ||
		l.Int("rows_affected") != 1 || l.Int("stmts") != 1 {
		t.Errorf("unexpected hold line: %+v", l.Attrs)
	}
	if l.Level != slogLevelWarn {
		t.Errorf("level = %v, want WARN", l.Level)
	}
	if _, ok := l.Attrs["waiters_max"]; !ok {
		t.Errorf("SQLite hold line has no waiters_max")
	}
	if l.Str("site") == "" {
		t.Errorf("hold line has no site")
	}
	holds, _, _ := hs.rec.Snapshot()
	var tx int
	for _, h := range holds {
		if h.Kind == KindTx {
			tx++
		}
	}
	if tx != 2 {
		t.Errorf("recorded %d tx holds, want 2 (metrics are not threshold-gated)", tx)
	}
}

func TestPostgresHoldLineOmitsWaitersAndConn(t *testing.T) {
	hs := newHarness(t, DialectPostgres, nil)
	hs.holdTx(t, 3*time.Second)
	// A long non-tx checkout: no conn line on Postgres.
	conn, err := hs.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "UPDATE"); err != nil {
		t.Fatal(err)
	}
	hs.clk.Advance(5 * time.Second)
	_ = conn.Close()
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 || lines[0].Str("kind") != KindTx {
		t.Fatalf("got %+v, want one tx line", lines)
	}
	if _, ok := lines[0].Attrs["waiters_max"]; ok {
		t.Errorf("Postgres hold line carries waiters_max")
	}
}

func TestConnHoldWarnsOnSQLiteWhenNoTxLine(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	conn, err := hs.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "UPDATE"); err != nil {
		t.Fatal(err)
	}
	hs.clk.Advance(3 * time.Second)
	_ = conn.Close()
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 || lines[0].Str("kind") != KindConn || lines[0].Str("outcome") != OutcomeReleased {
		t.Fatalf("got %+v, want one conn line", lines)
	}

	// A checkout whose tx already warned gets no conn line.
	hs.clk.Advance(2 * time.Minute) // past the rate-limit window
	hs.holdTx(t, 3*time.Second)
	hs.flush(t)
	var conns int
	for _, l := range hs.h.Lines(MsgHold) {
		if l.Str("kind") == KindConn {
			conns++
		}
	}
	if conns != 1 {
		t.Errorf("got %d conn lines, want 1 (the tx line covers its checkout)", conns)
	}
}

func TestRateLimitPerSiteAndSuppressedCount(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.holdTx(t, 3*time.Second)
	hs.holdTx(t, 3*time.Second) // same site, inside the window
	hs.flush(t)
	if got := len(hs.h.Lines(MsgHold)); got != 1 {
		t.Fatalf("got %d lines inside the window, want 1", got)
	}
	hs.clk.Advance(DefaultWarnInterval)
	hs.holdTx(t, 3*time.Second)
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 2 {
		t.Fatalf("got %d lines after the window, want 2", len(lines))
	}
	if got := lines[1].Int("suppressed"); got != 1 {
		t.Errorf("suppressed = %d, want 1", got)
	}
}

func TestOpCapBoundsOpsAndLogsOnce(t *testing.T) {
	c := newCallsites()
	for i := 0; i < maxOps; i++ {
		if op, capped := c.intern(fmt.Sprintf("op%d", i)); op != fmt.Sprintf("op%d", i) || capped {
			t.Fatalf("op %d: got %q capped=%v", i, op, capped)
		}
	}
	op, capped := c.intern("one-too-many")
	if op != OpOther || !capped {
		t.Fatalf("257th op: got %q capped=%v, want %q true", op, capped, OpOther)
	}
	op, capped = c.intern("another")
	if op != OpOther || capped {
		t.Fatalf("258th op: got %q capped=%v, want %q false", op, capped, OpOther)
	}
	if op, _ := c.intern("op3"); op != "op3" {
		t.Errorf("known op after cap: got %q", op)
	}
}

func TestOpCapLineThroughMonitor(t *testing.T) {
	h := &CaptureHandler{}
	m := NewMonitor(Config{Logger: newLogger(h)})
	t.Cleanup(m.Close)
	for i := 0; i < maxOps; i++ {
		_, _ = m.sites.intern(fmt.Sprintf("op%d", i))
	}
	var pcs [stackDepth]uintptr
	n := runtime.Callers(1, pcs[:])
	if got := m.op(pcs[:n]); got != OpOther {
		t.Fatalf("op = %q, want %q", got, OpOther)
	}
	_ = m.op(pcs[:n])
	if err := m.Flush(flushTimeout); err != nil {
		t.Fatal(err)
	}
	lines := h.Lines(MsgOpCap)
	if len(lines) != 1 {
		t.Fatalf("got %d op-cap lines, want 1", len(lines))
	}
	if lines[0].Level != slogLevelInfo {
		t.Errorf("op-cap line is not INFO")
	}
}

func TestIsInfra(t *testing.T) {
	cases := []struct {
		fn   string
		want bool
	}{
		{"database/sql.(*DB).BeginTx", true},
		{"runtime.goexit", true},
		{"entgo.io/ent/dialect/sql/sqlgraph.UpdateNode", true},
		{"entgo.io/ent.MutateFunc.Mutate", true},
		{"entgo.io/entx.Other", false},
		{modulePrefix + "pkg/ent.(*Client).Tx", true},
		{modulePrefix + "pkg/ent/entc.UTCTimeHook.func1", true},
		{modulePrefix + "pkg/ent/hubsetting.init", true},
		{modulePrefix + "pkg/observability/txdiag.(*conn).BeginTx", true},
		{modulePrefix + "pkg/store/entadapter.runInEntTx", true},
		{modulePrefix + "pkg/store/entadapter.runInEntTx.func1", true},
		{modulePrefix + "pkg/store/entadapter.runSerializableEntTx[...]", true},
		{modulePrefix + "pkg/store/entadapter.(*CompositeStore).WithTx", true},
		{modulePrefix + "pkg/store/entadapter.(*CompositeStore).RunSerializable", true},
		{modulePrefix + "pkg/store/entadapter.(*ConduitRegistryStore).begin", true},
		{modulePrefix + "pkg/store/entadapter.(*AgentStore).beginLaunchTx", true},
		{modulePrefix + "pkg/store/entadapter.(*CompositeStore).WithTxOther", false},
		{modulePrefix + "pkg/store/entadapter.(*AgentStore).UpdateAgent", false},
		{modulePrefix + "pkg/observability/txdiag_test.holdWriteTx3s", false},
		{modulePrefix + "pkg/hub.(*Server).handleUpdateGroup.func2", false},
		{"testing.tRunner", false},
	}
	for _, c := range cases {
		if got := isInfra(c.fn); got != c.want {
			t.Errorf("isInfra(%q) = %v, want %v", c.fn, got, c.want)
		}
	}
	if got := shortFunc(modulePrefix + "pkg/hub.(*Server).f"); got != "hub.(*Server).f" {
		t.Errorf("shortFunc = %q", got)
	}
}

func TestBlockingHandlerDoesNotBlockCommitAndDropsAreCounted(t *testing.T) {
	block := make(chan struct{})
	h := &CaptureHandler{Block: block, Entered: make(chan struct{}, 1)}
	clk := NewFakeClock()
	rec := &FakeRecorder{}
	m := NewMonitor(Config{HoldWarnThreshold: 2 * time.Second, Logger: newLogger(h), Now: clk.Now, Dialect: DialectSQLite})
	m.SetRecorder(rec)
	released := false
	release := func() {
		if !released {
			released = true
			close(block)
		}
	}
	t.Cleanup(func() { release(); m.Close() })

	// First record: the emitter takes it and blocks in the handler.
	m.warn(mkrec("first"))
	<-h.Entered
	// Fill the queue, then overflow it with distinct keys.
	for i := 0; i < queueSize; i++ {
		m.warn(mkrec(fmt.Sprintf("fill%d", i)))
	}
	m.warn(mkrec("dropped-key"))

	// A Commit over the threshold with the handler stuck and the queue full
	// still returns promptly.
	con := &fakeConnector{d: newFakeDriver()}
	db := sql.OpenDB(WrapConnector(con, m))
	db.SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		tx, err := db.Begin()
		if err != nil {
			done <- err
			return
		}
		clk.Advance(3 * time.Second)
		done <- tx.Commit()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Commit blocked behind a stuck log handler")
	}
	_, _, dropped := rec.Snapshot()
	if dropped < 2 {
		t.Fatalf("dropped = %d, want >= 2 (the overflow record and the Commit's record)", dropped)
	}

	// O1: the dropped key was not burned: once the handler is free, a new
	// record for it is emitted (no clock advance) and reports the drop.
	release()
	if err := m.Flush(flushTimeout); err != nil {
		t.Fatal(err)
	}
	m.warn(mkrec("dropped-key"))
	if err := m.Flush(flushTimeout); err != nil {
		t.Fatal(err)
	}
	var got []LogLine
	for _, l := range h.Lines(MsgHold) {
		if l.Str("op") == "dropped-key" && l.Int("suppressed") >= 1 && l.Int("dropped") >= 1 {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("want one line reporting suppressed>=1 and dropped>=1 for the dropped key; lines: %+v", h.Lines(MsgHold))
	}
	_ = db.Close()
}

func TestMonitorLifecycle(t *testing.T) {
	t.Run("leak", func(t *testing.T) {
		before := runtime.NumGoroutine()
		for i := 0; i < 20; i++ {
			m := NewMonitor(Config{Logger: newLogger(&CaptureHandler{})})
			db := sql.OpenDB(WrapConnector(&fakeConnector{d: newFakeDriver()}, m))
			if err := db.Ping(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				t.Fatalf("goroutines: %d before, %d after closing 20 wrapped DBs", before, runtime.NumGoroutine())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("send after close and double close", func(t *testing.T) {
		hs := newHarness(t, DialectSQLite, nil)
		hs.mon.Close()
		hs.mon.Close() // idempotent
		hs.holdTx(t, 3*time.Second)
		conn, err := hs.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		hs.clk.Advance(3 * time.Second)
		_ = conn.Close()
		_, _, dropped := hs.rec.Snapshot()
		if dropped < 1 {
			t.Errorf("dropped = %d after Close, want >= 1", dropped)
		}
		if err := hs.mon.Flush(flushTimeout); !errors.Is(err, ErrClosed) {
			t.Errorf("Flush after Close = %v, want ErrClosed", err)
		}
		_ = hs.db.Close()
		if !hs.con.closed.Load() {
			t.Errorf("DB.Close did not close the base connector")
		}
	})
	t.Run("bounded close", func(t *testing.T) {
		block := make(chan struct{})
		h := &CaptureHandler{Block: block, Entered: make(chan struct{}, 1)}
		m := NewMonitor(Config{Logger: newLogger(h)})
		exited := m.exited
		t.Cleanup(func() {
			close(block)
			<-exited
		})
		m.warn(mkrec("x"))
		<-h.Entered
		start := time.Now()
		m.Close()
		if d := time.Since(start); d > drainDeadline+closeMargin+500*time.Millisecond {
			t.Fatalf("Close took %v with a stuck handler", d)
		}
	})
	t.Run("flush waits for the in-flight line", func(t *testing.T) {
		h := &CaptureHandler{}
		m := NewMonitor(Config{Logger: newLogger(&slowHandler{CaptureHandler: h, d: 50 * time.Millisecond})})
		t.Cleanup(m.Close)
		m.warn(mkrec("a"))
		m.warn(mkrec("b"))
		if err := m.Flush(flushTimeout); err != nil {
			t.Fatal(err)
		}
		if got := len(h.Lines(MsgHold)); got != 2 {
			t.Fatalf("after Flush: %d lines, want 2", got)
		}
	})
}

func TestNoOpenCheckoutIsANoOp(t *testing.T) {
	rec := &FakeRecorder{}
	m := NewMonitor(Config{HoldWarnThreshold: time.Nanosecond, Dialect: DialectSQLite, Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	m.SetRecorder(rec)
	c := &conn{base: &fullConn{minimalConn{d: newFakeDriver()}}, mon: m}
	if !c.IsValid() {
		t.Fatal("IsValid = false")
	}
	_ = c.Close()
	if holds, _, _ := rec.Snapshot(); len(holds) != 0 {
		t.Fatalf("recorded %+v for a connection with no checkout", holds)
	}
}

func TestCheckoutEndedByCloseOnBadConn(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	conn, err := hs.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(dc any) error {
		if _, ok := dc.(interface{ Unwrap() driver.Conn }); !ok {
			t.Errorf("Raw yields %T, which has no Unwrap", dc)
		}
		if _, err := dc.(driver.ExecerContext).ExecContext(context.Background(), "UPDATE", nil); err != nil {
			return err
		}
		hs.clk.Advance(3 * time.Second)
		return driver.ErrBadConn // database/sql closes the conn, skipping IsValid
	})
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Raw = %v", err)
	}
	_ = conn.Close()
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 || lines[0].Str("kind") != KindConn {
		t.Fatalf("got %+v, want one conn line ended by Close", lines)
	}
}

func TestProbeStampIsOneShot(t *testing.T) {
	m := NewMonitor(Config{Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	p := &probe{requested: time.Unix(100, 0), mon: m}
	p.waiting.Store(true)
	m.pendingInc()
	p.stamp(time.Unix(103, 0))
	p.stamp(time.Unix(200, 0))
	if got := time.Unix(0, p.acquired.Load()); !got.Equal(time.Unix(103, 0)) {
		t.Fatalf("acquired = %v, want the first stamp", got)
	}
	if got := m.pending.Load(); got != 0 {
		t.Fatalf("pending = %d after stamp, want 0", got)
	}
	p.stopWaiting()
	if got := m.pending.Load(); got != 0 {
		t.Fatalf("pending = %d after a second stop, want 0", got)
	}
}

// TestLockWaitOutcomes drives an ent request that waits behind a held
// connection and ends by acquisition, deadline or cancellation, using a
// hand-closed ctx (no real deadline).
func TestLockWaitOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fire    error // nil: the holder releases instead
		outcome string
	}{
		{"acquired", nil, OutcomeAcquired},
		{"deadline", context.DeadlineExceeded, OutcomeDeadline},
		{"canceled", context.Canceled, OutcomeCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t, DialectSQLite, nil)
			ed := WrapEntDriver(entsql.OpenDB(dialect.SQLite, hs.db), hs.mon)
			holder, err := hs.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := NewManualCtx()
			res := make(chan error, 1)
			go func() {
				tx, err := ed.Tx(ctx)
				if err == nil {
					err = tx.Rollback()
				}
				res <- err
			}()
			waitForWaiters(t, hs.db, 1)
			hs.clk.Advance(2 * time.Second)
			if tc.fire != nil {
				ctx.Fire(tc.fire)
			} else {
				_ = holder.Rollback()
			}
			err = <-res
			if tc.fire == nil && err != nil {
				t.Fatalf("Tx: %v", err)
			}
			if tc.fire != nil && !errors.Is(err, tc.fire) {
				t.Fatalf("Tx = %v, want %v", err, tc.fire)
			}
			if tc.fire != nil {
				_ = holder.Rollback()
			}
			hs.flush(t)
			lines := hs.h.Lines(MsgLockWait)
			if len(lines) != 1 {
				t.Fatalf("got %d lock_wait lines, want 1", len(lines))
			}
			if l := lines[0]; l.Str("outcome") != tc.outcome || l.Int("wait_ms") != 2000 || l.Str("req") != reqTx {
				t.Errorf("lock_wait line: %+v", l.Attrs)
			}
			// The holder is named: one checkout overlapped the 2 s wait.
			if l := lines[0]; l.Int("holders") != 1 || l.Int("top_holder_ms") != 2000 || l.Str("top_holder_op") == "" {
				t.Errorf("holder fields: %+v", l.Attrs)
			}
			_, hasOpen := lines[0].Attrs["holder_open_op"]
			if tc.fire != nil {
				if !hasOpen || lines[0].Int("holder_open_ms") != 2000 {
					t.Errorf("a waiter that gave up must name the open holder: %+v", lines[0].Attrs)
				}
			} else if hasOpen {
				t.Errorf("an acquired waiter reports an open holder: %+v", lines[0].Attrs)
			}
			if got := hs.mon.pending.Load(); got != 0 {
				t.Errorf("pending = %d after the request, want 0", got)
			}
		})
	}
}

// mkrec builds a hold record for op, as Monitor.hold would.
func mkrec(op string) record {
	return record{msg: msgHold, kind: KindTx, op: op, attrs: []slog.Attr{slog.String("op", op)}}
}

// waitForWaiters waits (bounded) until n callers are parked in db's pool.
func waitForWaiters(t *testing.T, db *sql.DB, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for db.Stats().WaitCount < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pool waiters (have %d)", n, db.Stats().WaitCount)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestForwardingFullConn(t *testing.T) {
	hs := newHarness(t, DialectPostgres, nil)
	ctx := context.Background()
	if _, err := hs.db.ExecContext(ctx, "UPDATE", customArg{"x"}); err != nil {
		t.Fatalf("exec with a driver-specific arg: %v", err)
	}
	if hs.drv.calls.count("CheckNamedValue") == 0 {
		t.Error("CheckNamedValue was not forwarded")
	}
	if err := hs.db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if hs.drv.calls.count("Ping") == 0 {
		t.Error("Ping was not forwarded")
	}
	if _, err := hs.db.ExecContext(ctx, "UPDATE"); err != nil {
		t.Fatal(err)
	}
	if hs.drv.calls.count("ResetSession") == 0 || hs.drv.calls.count("IsValid") == 0 {
		t.Error("ResetSession or IsValid was not forwarded")
	}
	if hs.drv.calls.count("ExecContext") != 2 {
		t.Errorf("ExecContext forwarded %d times, want 2", hs.drv.calls.count("ExecContext"))
	}
	// IsValid false makes database/sql discard the connection.
	hs.drv.valid.Store(false)
	if _, err := hs.db.ExecContext(ctx, "UPDATE"); err != nil {
		t.Fatal(err)
	}
	if hs.drv.calls.count("Close") == 0 {
		t.Error("an invalid connection was not closed")
	}
	if hs.db.Driver() != driver.Driver(hs.drv) {
		t.Error("DB.Driver() is not the base driver")
	}
}

func TestForwardingMinimalConn(t *testing.T) {
	hs := newHarness(t, DialectPostgres, func(_ *Config, d *fakeDriver) { d.minimal = true })
	ctx := context.Background()
	if _, err := hs.db.ExecContext(ctx, "UPDATE", "plain"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if hs.drv.calls.count("Prepare") == 0 || hs.drv.calls.count("StmtExec") == 0 {
		t.Error("without ExecerContext, database/sql did not fall back to Prepare")
	}
	if _, err := hs.db.ExecContext(ctx, "UPDATE", customArg{"x"}); err == nil {
		t.Error("a driver-specific arg passed without a NamedValueChecker")
	}
	tx, err := hs.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if hs.drv.calls.count("Begin") != 1 {
		t.Error("Begin fallback not used")
	}
	if _, err := hs.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable}); err == nil {
		t.Error("non-default isolation accepted by a driver without ConnBeginTx")
	}
	if err := hs.db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDriverErrorsPassThroughUnchanged(t *testing.T) {
	want := &errFake{code: 5}
	hs := newHarness(t, DialectSQLite, func(_ *Config, d *fakeDriver) { d.execErr = want })
	tx, err := hs.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec("UPDATE")
	var got *errFake
	if !errors.As(err, &got) || got != want {
		t.Fatalf("Exec error = %v (%T), want the driver's own error value", err, err)
	}
	hs.clk.Advance(3 * time.Second)
	_ = tx.Rollback()
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 || lines[0].Int("busy") != 1 || lines[0].Str("outcome") != OutcomeRollback {
		t.Fatalf("got %+v, want one rollback line with busy=1", lines)
	}
}

func TestConnectorCloseClosesMonitorAndBase(t *testing.T) {
	m := NewMonitor(Config{Logger: newLogger(&CaptureHandler{})})
	con := &fakeConnector{d: newFakeDriver()}
	db := sql.OpenDB(WrapConnector(con, m))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !m.closed.Load() || !con.closed.Load() {
		t.Fatalf("monitor closed=%v base closed=%v, want both", m.closed.Load(), con.closed.Load())
	}
	select {
	case <-m.exited:
	case <-time.After(time.Second):
		t.Fatal("emitter did not exit")
	}
}

func TestDSNConnector(t *testing.T) {
	d := newFakeDriver()
	c := DSNConnector{DSN: "x", Drv: d}
	if c.Driver() != driver.Driver(d) {
		t.Fatal("Driver() mismatch")
	}
	conn, err := c.Connect(context.Background())
	if err != nil || conn == nil {
		t.Fatalf("Connect = %v, %v", conn, err)
	}
}

func TestQuietConfigDiscards(t *testing.T) {
	c := QuietConfig()
	if c.HoldWarnThreshold != 2*time.Second || c.LockWaitWarnThreshold != time.Second || c.Logger == nil {
		t.Fatalf("QuietConfig = %+v", c)
	}
	if c.Logger.Enabled(context.Background(), slogLevelWarn) {
		t.Fatal("QuietConfig logger is enabled")
	}
}

// seedOp makes pc symbolize as a single non-infrastructure frame named fn,
// so ring tests can give each checkout its own op.
func seedOp(m *Monitor, pc uintptr, fn string) [stackDepth]uintptr {
	m.sites.mu.Lock()
	m.sites.pcs[pc] = []frameInfo{{fn: fn, file: "x.go", line: 1}}
	m.sites.mu.Unlock()
	var pcs [stackDepth]uintptr
	pcs[0] = pc
	return pcs
}

func attrMap(attrs []slog.Attr) map[string]slog.Value {
	out := map[string]slog.Value{}
	for _, a := range attrs {
		out[a.Key] = a.Value
	}
	return out
}

// TestHolderRingConvoy: a waiter behind a convoy of five short holders is
// told how many held the connection during its wait and which held longest.
func TestHolderRingConvoy(t *testing.T) {
	m := NewMonitor(Config{Dialect: DialectSQLite, Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	m.holders.closed(at(-1000), at(-500), seedOp(m, 100, "hub.before"), 1) // before the wait
	spans := []struct {
		fn         string
		start, end int
	}{
		{"hub.a", 0, 200}, {"hub.b", 200, 400}, {"hub.c", 400, 800}, {"hub.d", 800, 1000}, {"hub.e", 1000, 1200},
	}
	for i, s := range spans {
		m.holders.closed(at(s.start), at(s.end), seedOp(m, uintptr(101+i), s.fn), 1)
	}
	got := attrMap(m.holders.attrs(m, at(0), at(2000), false))
	if got["holders"].Int64() != 5 || got["top_holder_op"].String() != "hub.c" || got["top_holder_ms"].Int64() != 400 {
		t.Fatalf("convoy attrs: %+v", got)
	}
	if _, ok := got["holder_open_op"]; ok {
		t.Fatalf("no open checkout, yet holder_open_op is set: %+v", got)
	}
}

// TestHolderRingDeadlineWaiterNamesOpenHolder: a waiter that gives up while
// one checkout is still open is told who that holder is and for how long it
// had held the connection.
func TestHolderRingDeadlineWaiterNamesOpenHolder(t *testing.T) {
	m := NewMonitor(Config{Dialect: DialectSQLite, Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.holders.opened(t0, seedOp(m, 200, "hub.slow"), 1)
	got := attrMap(m.holders.attrs(m, t0.Add(100*time.Millisecond), t0.Add(3*time.Second), true))
	if got["holders"].Int64() != 1 || got["top_holder_op"].String() != "hub.slow" || got["top_holder_ms"].Int64() != 2900 {
		t.Fatalf("attrs: %+v", got)
	}
	if got["holder_open_op"].String() != "hub.slow" || got["holder_open_ms"].Int64() != 3000 {
		t.Fatalf("open holder: %+v", got)
	}
	// Once that checkout ends, it is no longer open.
	m.holders.closed(t0, t0.Add(4*time.Second), seedOp(m, 200, "hub.slow"), 1)
	got = attrMap(m.holders.attrs(m, t0.Add(100*time.Millisecond), t0.Add(3*time.Second), true))
	if _, ok := got["holder_open_op"]; ok {
		t.Fatalf("closed checkout still reported open: %+v", got)
	}
}

func TestHolderRingWrapsAtCapacity(t *testing.T) {
	m := NewMonitor(Config{Dialect: DialectSQLite, Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pcs := seedOp(m, 300, "hub.short")
	for i := 0; i < holderRingSize+44; i++ {
		s := t0.Add(time.Duration(i) * time.Millisecond)
		m.holders.closed(s, s.Add(time.Millisecond), pcs, 1)
	}
	got := attrMap(m.holders.attrs(m, t0, t0.Add(time.Hour), false))
	if got["holders"].Int64() != holderRingSize {
		t.Fatalf("holders = %v, want %d (ring exhausted)", got["holders"], holderRingSize)
	}
}

func TestLockWaitHolderFieldsOmittedOnPostgres(t *testing.T) {
	hs := newHarness(t, DialectPostgres, nil)
	ed := WrapEntDriver(entsql.OpenDB(dialect.Postgres, hs.db), hs.mon)
	holder, err := hs.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		tx, err := ed.Tx(context.Background())
		if err == nil {
			err = tx.Rollback()
		}
		res <- err
	}()
	waitForWaiters(t, hs.db, 1)
	hs.clk.Advance(2 * time.Second)
	_ = holder.Rollback()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	hs.flush(t)
	lines := hs.h.Lines(MsgLockWait)
	if len(lines) != 1 {
		t.Fatalf("got %d lock_wait lines, want 1", len(lines))
	}
	for _, k := range []string{"holders", "top_holder_op", "holder_open_op"} {
		if _, ok := lines[0].Attrs[k]; ok {
			t.Errorf("Postgres lock_wait line carries %s: %+v", k, lines[0].Attrs)
		}
	}
}

// TestCommitTimeIsSeparateFromIdle: a slow driver Commit (fsync) is
// reported as commit_ms, not as idle time, and recorded in the commit
// histogram.
func TestCommitTimeIsSeparateFromIdle(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.drv.onCommit = func() { hs.clk.Advance(3 * time.Second) }
	hs.holdTx(t, 0)
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want 1", len(lines))
	}
	l := lines[0]
	if l.Int("hold_ms") != 3000 || l.Int("commit_ms") != 3000 || l.Int("idle_ms") != 0 {
		t.Errorf("hold line: %+v", l.Attrs)
	}
	commits := hs.rec.CommitsSnapshot()
	if len(commits) != 1 || commits[0].MS != 3000 || commits[0].Outcome != OutcomeCommit {
		t.Errorf("commit histogram calls: %+v", commits)
	}
}

// TestSlowWorkInsideTxIsIdleNotCommit: time spent inside the transaction
// before a fast Commit is idle time, and commit_ms stays 0.
func TestSlowWorkInsideTxIsIdleNotCommit(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.holdTx(t, 3*time.Second)
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want 1", len(lines))
	}
	if l := lines[0]; l.Int("commit_ms") != 0 || l.Int("idle_ms") != 3000 {
		t.Errorf("hold line: %+v", l.Attrs)
	}
	if commits := hs.rec.CommitsSnapshot(); len(commits) != 1 || commits[0].MS != 0 {
		t.Errorf("commit histogram calls: %+v", commits)
	}
}

// TestPendingReleasedWhenInnerDriverPanics: a panic inside the wrapped
// driver (recovered by the caller) must not leave the request counted as a
// pending waiter, or every later waiters_max would be inflated.
func TestPendingReleasedWhenInnerDriverPanics(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	ed := WrapEntDriver(entsql.OpenDB(dialect.SQLite, hs.db), hs.mon)
	// Panic while the connection is being opened, before any driver call
	// could mark the request acquired: only the deferred finish releases it.
	hs.drv.onOpen = func() { panic("driver panic") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the driver panic to propagate")
			}
		}()
		_ = ed.Exec(context.Background(), "UPDATE", []any{}, nil)
	}()
	if got := hs.mon.pending.Load(); got != 0 {
		t.Fatalf("pending = %d after a recovered panic, want 0", got)
	}
}

// TestOpCapLineRetriedWhenQueueFull: if the queue is full when the op cap
// is first crossed, the cap line is queued by a later call instead of being
// lost.
func TestOpCapLineRetriedWhenQueueFull(t *testing.T) {
	block := make(chan struct{})
	h := &CaptureHandler{Block: block, Entered: make(chan struct{}, 1)}
	m := NewMonitor(Config{Logger: newLogger(h)})
	released := false
	t.Cleanup(func() {
		if !released {
			close(block)
		}
		m.Close()
	})
	m.warn(mkrec("first"))
	<-h.Entered
	for i := 0; i < queueSize; i++ {
		m.warn(mkrec(fmt.Sprintf("fill%d", i)))
	}
	for i := 0; i < maxOps; i++ {
		_, _ = m.sites.intern(fmt.Sprintf("op%d", i))
	}
	var pcs [stackDepth]uintptr
	n := runtime.Callers(1, pcs[:])
	if got := m.op(pcs[:n]); got != OpOther { // queue full: cap line not queued
		t.Fatalf("op = %q", got)
	}
	if m.capLogged.Load() {
		t.Fatal("cap line marked logged although the queue was full")
	}
	close(block)
	released = true
	if err := m.Flush(flushTimeout); err != nil {
		t.Fatal(err)
	}
	_ = m.op(pcs[:n]) // retried now
	_ = m.op(pcs[:n]) // not again
	if err := m.Flush(flushTimeout); err != nil {
		t.Fatal(err)
	}
	if got := len(h.Lines(MsgOpCap)); got != 1 {
		t.Fatalf("got %d op-cap lines, want 1", got)
	}
}

// TestBeginTimeIsSeparateFromIdle: a slow BEGIN is reported as begin_ms and
// is not counted as idle time.
func TestBeginTimeIsSeparateFromIdle(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.drv.onBegin = func() { hs.clk.Advance(time.Second) }
	hs.drv.onCommit = func() { hs.clk.Advance(2 * time.Second) }
	hs.holdTx(t, 0)
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want 1", len(lines))
	}
	if l := lines[0]; l.Int("hold_ms") != 3000 || l.Int("begin_ms") != 1000 || l.Int("commit_ms") != 2000 || l.Int("idle_ms") != 0 {
		t.Errorf("hold line: %+v", l.Attrs)
	}
}

// TestConnLineCountsInnerTxBeginAndCommit: on a conn hold line, an inner
// transaction's BEGIN and COMMIT are reported as begin_ms and commit_ms, not
// as idle time.
func TestConnLineCountsInnerTxBeginAndCommit(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.drv.onBegin = func() { hs.clk.Advance(500 * time.Millisecond) }
	hs.drv.onCommit = func() { hs.clk.Advance(500 * time.Millisecond) }
	ctx := context.Background()
	conn, err := hs.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil { // a 1 s transaction: under the threshold
		t.Fatal(err)
	}
	hs.clk.Advance(1500 * time.Millisecond) // Go work while still holding the conn
	_ = conn.Close()
	hs.flush(t)
	lines := hs.h.Lines(MsgHold)
	if len(lines) != 1 || lines[0].Str("kind") != KindConn {
		t.Fatalf("got %+v, want one conn line", lines)
	}
	if l := lines[0]; l.Int("hold_ms") != 2500 || l.Int("begin_ms") != 500 || l.Int("commit_ms") != 500 || l.Int("idle_ms") != 1500 {
		t.Errorf("conn line: %+v", l.Attrs)
	}
}

// TestNoRecorderBelowThresholdSkipsStackResolution: in the default
// metrics-off configuration, a fast ent request resolves no stack at all
// (neither the hold nor the lock-wait path symbolizes a frame).
func TestNoRecorderBelowThresholdSkipsStackResolution(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.mon.SetRecorder(nil)
	ed := WrapEntDriver(entsql.OpenDB(dialect.SQLite, hs.db), hs.mon)
	if err := ed.Exec(context.Background(), "UPDATE", []any{}, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := ed.Tx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	hs.mon.sites.mu.RLock()
	n := len(hs.mon.sites.pcs)
	hs.mon.sites.mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d PCs symbolized with no recorder and no slow request, want 0", n)
	}
}

// TestNoRecorderSlowWaiterStillLogs: the metrics-off early return in finish
// does not drop a waiter over the threshold.
func TestNoRecorderSlowWaiterStillLogs(t *testing.T) {
	hs := newHarness(t, DialectSQLite, nil)
	hs.mon.SetRecorder(nil)
	ed := WrapEntDriver(entsql.OpenDB(dialect.SQLite, hs.db), hs.mon)
	holder, err := hs.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		tx, err := ed.Tx(context.Background())
		if err == nil {
			err = tx.Rollback()
		}
		res <- err
	}()
	waitForWaiters(t, hs.db, 1)
	hs.clk.Advance(2 * time.Second)
	_ = holder.Rollback()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	hs.flush(t)
	lines := hs.h.Lines(MsgLockWait)
	if len(lines) != 1 || lines[0].Int("wait_ms") != 2000 || lines[0].Str("op") == "" {
		t.Fatalf("got %+v, want one lock_wait line with wait_ms=2000", lines)
	}
}

// TestPastCapOpsDoNotGrowTheRegistry: once the cap is reached, further call
// sites resolve to OpOther (through the read-locked fast path once their
// frames are cached) and are never added, so the cap stays at maxOps real
// ops.
func TestPastCapOpsDoNotGrowTheRegistry(t *testing.T) {
	m := NewMonitor(Config{Logger: newLogger(&CaptureHandler{})})
	t.Cleanup(m.Close)
	for i := 0; i < maxOps; i++ {
		_, _ = m.sites.intern(fmt.Sprintf("op%d", i))
	}
	pcs := seedOp(m, 500, "hub.pastcap")
	for i := 0; i < 3; i++ {
		if got := m.sites.op(pcs[:1]); got != OpOther {
			t.Fatalf("op = %q, want %q", got, OpOther)
		}
	}
	m.sites.mu.RLock()
	n, full := len(m.sites.ops), m.sites.full
	m.sites.mu.RUnlock()
	if n != maxOps || !full {
		t.Fatalf("registry holds %d ops (full=%v), want %d and full", n, full, maxOps)
	}
	// The past-cap fast path needs only the read lock: it must return while
	// another reader holds it (the exclusive-lock path would block here).
	m.sites.mu.RLock()
	done := make(chan string, 1)
	go func() { done <- m.sites.op(pcs[:1]) }()
	select {
	case got := <-done:
		m.sites.mu.RUnlock()
		if got != OpOther {
			t.Fatalf("op = %q, want %q", got, OpOther)
		}
	case <-time.After(2 * time.Second):
		m.sites.mu.RUnlock()
		<-done
		t.Fatal("past-cap op resolution waited for the exclusive lock")
	}
}
