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
	"errors"
	"log/slog"
	"runtime"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// Request kinds of a lock_wait line.
const (
	reqTx    = "tx"
	reqExec  = "exec"
	reqQuery = "query"
)

type probeKey struct{}

// probe records when an ent request asked for a connection and when the
// driver wrapper first saw a call for it (acquired, stamped once).
type probe struct {
	requested time.Time
	acquired  atomic.Int64 // UnixNano; 0 until stamped
	waiting   atomic.Bool  // still counted in Monitor.pending
	mon       *Monitor
}

// stamp marks the probe acquired at now. Only the first call has effect.
func (p *probe) stamp(now time.Time) {
	if p.acquired.CompareAndSwap(0, now.UnixNano()) {
		p.stopWaiting()
	}
}

func (p *probe) stopWaiting() {
	if p.waiting.CompareAndSwap(true, false) {
		p.mon.pendingDec()
	}
}

// stampProbe stamps the probe carried by ctx, if any.
func stampProbe(ctx context.Context, now time.Time) {
	if ctx == nil {
		return
	}
	if p, ok := ctx.Value(probeKey{}).(*probe); ok && p.acquired.Load() == 0 {
		p.stamp(now)
	}
}

// EntDriver wraps an ent SQL driver so each request records how long it
// waited for a connection. It forwards everything else unchanged.
type EntDriver struct {
	inner *entsql.Driver
	mon   *Monitor
}

var _ dialect.Driver = (*EntDriver)(nil)

// WrapEntDriver returns d wrapped with lock-wait probes reporting to m.
func WrapEntDriver(d *entsql.Driver, m *Monitor) *EntDriver {
	return &EntDriver{inner: d, mon: m}
}

// Unwrap returns the wrapped ent driver.
func (d *EntDriver) Unwrap() dialect.Driver { return d.inner }

// Monitor returns the Monitor the driver reports to.
func (d *EntDriver) Monitor() *Monitor { return d.mon }

// DB returns the wrapped driver's *sql.DB, so code that asks a driver for
// its DB through an interface keeps working on the wrapper.
func (d *EntDriver) DB() *sql.DB { return d.inner.DB() }

// Dialect implements dialect.Driver.
func (d *EntDriver) Dialect() string { return d.inner.Dialect() }

// Close implements dialect.Driver.
func (d *EntDriver) Close() error { return d.inner.Close() }

// Exec implements dialect.ExecQuerier.
// The probe is finished in a defer, so a panic in the inner driver still
// releases its pending count.
func (d *EntDriver) Exec(ctx context.Context, query string, args, v any) (err error) {
	ctx, p := d.start(ctx)
	defer func() { d.finish(p, err, reqExec) }()
	return d.inner.Exec(ctx, query, args, v)
}

// Query implements dialect.ExecQuerier.
func (d *EntDriver) Query(ctx context.Context, query string, args, v any) (err error) {
	ctx, p := d.start(ctx)
	defer func() { d.finish(p, err, reqQuery) }()
	return d.inner.Query(ctx, query, args, v)
}

// Tx implements dialect.Driver.
func (d *EntDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	return d.BeginTx(ctx, nil)
}

// BeginTx starts a transaction with options, as *entsql.Driver does.
func (d *EntDriver) BeginTx(ctx context.Context, opts *entsql.TxOptions) (tx dialect.Tx, err error) {
	ctx, p := d.start(ctx)
	defer func() { d.finish(p, err, reqTx) }()
	return d.inner.BeginTx(ctx, opts)
}

// start attaches a probe to ctx. A ctx that already carries one (defence in
// depth; the wrapper's own child ctx never reaches caller code) is passed
// through, so one request yields one measurement.
func (d *EntDriver) start(ctx context.Context) (context.Context, *probe) {
	if _, ok := ctx.Value(probeKey{}).(*probe); ok {
		return ctx, nil
	}
	p := &probe{requested: d.mon.now(), mon: d.mon}
	p.waiting.Store(true)
	d.mon.pendingInc()
	return context.WithValue(ctx, probeKey{}, p), p
}

// finish computes the wait and records it.
func (d *EntDriver) finish(p *probe, err error, req string) {
	if p == nil {
		return
	}
	p.stopWaiting()
	m := d.mon
	var end time.Time // when the wait ended: acquisition, or giving up
	outcome := OutcomeAcquired
	if at := p.acquired.Load(); at != 0 {
		end = time.Unix(0, at)
	} else {
		end = m.now()
		switch {
		case err == nil:
			// No driver call was seen (for example an ent call that ran
			// no statement); nothing was waited for.
			return
		case errors.Is(err, context.DeadlineExceeded):
			outcome = OutcomeDeadline
		case errors.Is(err, context.Canceled):
			outcome = OutcomeCanceled
		default:
			outcome = OutcomeError
		}
	}
	wait := end.Sub(p.requested)
	if wait < 0 {
		wait = 0
	}
	rec := m.recorder()
	over := m.cfg.LockWaitWarnThreshold > 0 && wait >= m.cfg.LockWaitWarnThreshold
	if rec == nil && !over {
		return // nothing to record: skip the stack capture and op resolution
	}
	var pcs [stackDepth]uintptr
	n := runtime.Callers(3, pcs[:])
	op := m.op(pcs[:n])
	if rec != nil {
		rec.RecordLockWait(context.Background(), ms(wait), op, outcome)
	}
	if !over {
		return
	}
	r := record{msg: msgLockWait, kind: "lock_wait", op: op, attrs: []slog.Attr{
		slog.String("op", op),
		slog.Int64("wait_ms", wait.Milliseconds()),
		slog.String("outcome", outcome),
		slog.String("req", req),
	}}
	if m.sqlite {
		r.attrs = append(r.attrs, m.holders.attrs(m, p.requested, end, outcome != OutcomeAcquired)...)
	}
	r.npcs = copy(r.pcs[:], pcs[:n])
	m.warn(r)
}
