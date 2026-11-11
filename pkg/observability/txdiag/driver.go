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
	"database/sql/driver"
	"errors"
	"io"
	"runtime"
	"sync"
	"time"
)

// WrapConnector returns a connector whose connections report to m. Its
// Close (called by database/sql's DB.Close) closes m and then the base
// connector, if that is an io.Closer.
func WrapConnector(base driver.Connector, m *Monitor) driver.Connector {
	return &connector{base: base, mon: m}
}

type connector struct {
	base driver.Connector
	mon  *Monitor
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	bc, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{base: bc, mon: c.mon}, nil
}

func (c *connector) Driver() driver.Driver { return c.base.Driver() }

func (c *connector) Close() error {
	c.mon.Close()
	if cl, ok := c.base.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// DSNConnector is a connector for a driver that does not implement
// driver.DriverContext: Connect opens dsn on drv, exactly as sql.Open would.
type DSNConnector struct {
	DSN string
	Drv driver.Driver
}

// Connect implements driver.Connector.
func (c DSNConnector) Connect(context.Context) (driver.Conn, error) { return c.Drv.Open(c.DSN) }

// Driver implements driver.Connector.
func (c DSNConnector) Driver() driver.Driver { return c.Drv }

// conn wraps one driver connection. database/sql uses a connection from one
// goroutine at a time; mu guards the checkout state anyway, so the wrapper
// does not depend on that.
type conn struct {
	base driver.Conn
	mon  *Monitor

	mu       sync.Mutex
	open     bool // a checkout is in progress
	start    time.Time
	pcs      [stackDepth]uintptr
	npcs     int
	stats    holdStats
	txWarned bool // a tx WARN fired during this checkout
	cur      *tx  // the open transaction, if any
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

// Unwrap returns the base driver connection, for callers of
// (*sql.Conn).Raw that need the driver's own type.
func (c *conn) Unwrap() driver.Conn { return c.base }

// touch is called at the start of every driver call: it stamps the
// request's probe (once) and opens a checkout if none is open.
func (c *conn) touch(ctx context.Context) {
	now := c.mon.now()
	stampProbe(ctx, now)
	c.mu.Lock()
	if !c.open {
		c.open = true
		c.start = now
		// The checkout's stack feeds only the conn hold and the holder
		// ring, which are SQLite-only.
		c.npcs = 0
		if c.mon.sqlite {
			c.npcs = runtime.Callers(3, c.pcs[:])
		}
		c.stats = holdStats{}
		c.txWarned = false
		pcs, n := c.pcs, c.npcs
		c.mu.Unlock()
		if c.mon.sqlite {
			c.mon.checkoutStarted()
			c.mon.holders.opened(now, pcs, n)
		}
		return
	}
	c.mu.Unlock()
}

// endCheckout closes the open checkout, if any, recording the conn hold
// (SQLite only). With no open checkout it does nothing.
func (c *conn) endCheckout() {
	c.mu.Lock()
	if !c.open {
		c.mu.Unlock()
		return
	}
	c.open = false
	start, stats, warned := c.start, c.stats, c.txWarned
	pcs := c.pcs
	n := c.npcs
	c.mu.Unlock()
	if !c.mon.sqlite {
		return
	}
	c.mon.holders.closed(start, c.mon.now(), pcs, n)
	if warned {
		return
	}
	c.mon.hold(KindConn, OutcomeReleased, pcs[:n], start, 0, stats)
}

// stmtDone records one statement on the open transaction or checkout.
func (c *conn) stmtDone(d time.Duration, rows int64, err error) {
	busy := c.mon.sqlite && isBusy(err)
	c.mu.Lock()
	if c.cur != nil {
		c.cur.stats.add(d, rows, busy)
	}
	c.stats.add(d, rows, busy)
	c.mu.Unlock()
}

// isBusy reports whether err is SQLITE_BUSY (5) or one of its extended
// codes, through the driver error's Code method.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var coder interface{ Code() int }
	if errors.As(err, &coder) {
		return coder.Code()&0xff == 5
	}
	return false
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	c.touch(context.Background())
	return c.base.Prepare(query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.touch(ctx)
	if p, ok := c.base.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.base.Prepare(query)
}

func (c *conn) Close() error {
	c.endCheckout()
	return c.base.Close()
}

// Begin is the deprecated non-context form; database/sql uses BeginTx.
func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.touch(ctx)
	start := c.mon.now()
	var t tx
	t.npcs = runtime.Callers(2, t.pcs[:])
	var (
		bt  driver.Tx
		err error
	)
	if cb, ok := c.base.(driver.ConnBeginTx); ok {
		bt, err = cb.BeginTx(ctx, opts)
	} else {
		// database/sql's own fallback for a driver without ConnBeginTx.
		if opts.Isolation != driver.IsolationLevel(0) {
			return nil, errors.New("sql: driver does not support non-default isolation level")
		}
		if opts.ReadOnly {
			return nil, errors.New("sql: driver does not support read-only transactions")
		}
		bt, err = c.base.Begin() //nolint:staticcheck // fallback for drivers without ConnBeginTx
	}
	if err != nil {
		return nil, err
	}
	t.base = bt
	t.c = c
	t.start = start
	t.begin = c.mon.now().Sub(start)
	c.mu.Lock()
	c.cur = &t
	c.mu.Unlock()
	return &t, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.touch(ctx)
	start := c.mon.now()
	res, err := e.ExecContext(ctx, query, args)
	var rows int64
	if err == nil && res != nil {
		if n, rerr := res.RowsAffected(); rerr == nil && n > 0 {
			rows = n
		}
	}
	c.stmtDone(c.mon.now().Sub(start), rows, err)
	return res, err
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.touch(ctx)
	start := c.mon.now()
	rows, err := q.QueryContext(ctx, query, args)
	c.stmtDone(c.mon.now().Sub(start), 0, err)
	return rows, err
}

func (c *conn) Ping(ctx context.Context) error {
	c.touch(ctx)
	if p, ok := c.base.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession runs at the start of a reused checkout, on the waiter's ctx.
// The probe is stamped before forwarding, since a driver's ResetSession may
// do I/O.
func (c *conn) ResetSession(ctx context.Context) error {
	c.touch(ctx)
	if r, ok := c.base.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid runs on every return of the connection to the pool; it ends the
// checkout.
func (c *conn) IsValid() bool {
	c.endCheckout()
	if v, ok := c.base.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.base.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// tx wraps a driver transaction and records its hold at Commit or Rollback.
type tx struct {
	base  driver.Tx
	c     *conn
	start time.Time
	begin time.Duration
	pcs   [stackDepth]uintptr
	npcs  int
	stats holdStats
	done  bool
}

// Commit times the driver's Commit call on its own (commit_ms): on SQLite
// that is where the WAL is written and synced, so a slow disk shows up there
// rather than as idle time. A write outside an explicit transaction commits
// inside its own statement, so its commit time is part of that statement's
// stmt_ms and cannot be separated here. Rollback is not timed.
func (t *tx) Commit() error {
	start := t.c.mon.now()
	err := t.base.Commit()
	d := t.c.mon.now().Sub(start)
	t.c.mu.Lock()
	t.stats.commitTime = d
	t.stats.committed = true
	t.c.mu.Unlock()
	t.finish(OutcomeCommit, err)
	return err
}

func (t *tx) Rollback() error {
	err := t.base.Rollback()
	t.finish(OutcomeRollback, err)
	return err
}

func (t *tx) finish(outcome string, err error) {
	c := t.c
	c.mu.Lock()
	if t.done {
		c.mu.Unlock()
		return
	}
	t.done = true
	if c.cur == t {
		c.cur = nil
	}
	stats := t.stats
	// The enclosing checkout's conn line counts this transaction's BEGIN and
	// COMMIT as such, not as idle time.
	c.stats.beginTime += t.begin
	c.stats.commitTime += stats.commitTime
	c.mu.Unlock()
	if err != nil {
		outcome = OutcomeError
		if c.mon.sqlite && isBusy(err) {
			stats.busy++
		}
	}
	if c.mon.hold(KindTx, outcome, t.pcs[:t.npcs], t.start, t.begin, stats) {
		c.mu.Lock()
		c.txWarned = true
		c.mu.Unlock()
	}
}
