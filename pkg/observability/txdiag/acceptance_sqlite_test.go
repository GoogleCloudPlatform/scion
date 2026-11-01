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

//go:build !no_sqlite

package txdiag_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"modernc.org/sqlite"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
)

const flushTimeout = 5 * time.Second

type env struct {
	client *ent.Client
	db     *sql.DB
	mon    *txdiag.Monitor
	clk    *txdiag.FakeClock
	h      *txdiag.CaptureHandler
	rec    *txdiag.FakeRecorder
}

// openEnv opens a migrated, one-connection SQLite hub database in a temp
// file, wrapped with diagnostics at the production thresholds, a fake clock
// and a capturing log handler.
func openEnv(t *testing.T, mutate func(*txdiag.Config)) *env {
	t.Helper()
	e := &env{clk: txdiag.NewFakeClock(), h: &txdiag.CaptureHandler{}, rec: &txdiag.FakeRecorder{}}
	cfg := txdiag.Config{
		HoldWarnThreshold:     2 * time.Second,
		LockWaitWarnThreshold: time.Second,
		Logger:                slog.New(e.h),
		Now:                   e.clk.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	dsn := "file:" + filepath.Join(t.TempDir(), "hub.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1, Diag: &cfg})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e.client = client
	if e.db = entc.SQLDB(client.Driver()); e.db == nil {
		t.Fatal("wrapped client exposes no *sql.DB")
	}
	mon, ok := entc.DiagMonitor(client)
	if !ok {
		t.Fatal("wrapped client has no diagnostics Monitor")
	}
	e.mon = mon
	mon.SetRecorder(e.rec)
	e.flush(t) // settle anything the migration queued
	return e
}

func (e *env) flush(t *testing.T) {
	t.Helper()
	if err := e.mon.Flush(flushTimeout); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func (e *env) waitForWaiters(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.db.Stats().WaitCount < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pool waiters (have %d)", n, e.db.Stats().WaitCount)
		}
		time.Sleep(time.Millisecond)
	}
}

func newSetting(ctx context.Context, c *ent.Client, section string) (*ent.HubSetting, error) {
	return c.HubSetting.Create().SetSection(section).SetValue(json.RawMessage(`{}`)).Save(ctx)
}

// holdWriteTx3s is the acceptance case: a write transaction that is held
// for 3 s (the fake clock stands in for the sleep).
func holdWriteTx3s(ctx context.Context, e *env, section string) error {
	tx, err := e.client.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := newSetting(ctx, tx.Client(), section); err != nil {
		_ = tx.Rollback()
		return err
	}
	e.clk.Advance(3 * time.Second)
	return tx.Commit()
}

func TestAcceptanceLongWriteTxLogsOneWarn(t *testing.T) {
	e := openEnv(t, nil)
	ctx := context.Background()
	if err := holdWriteTx3s(ctx, e, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := holdWriteTx3s(ctx, e, "s2"); err != nil { // same site, inside the window
		t.Fatal(err)
	}
	e.flush(t)
	lines := e.h.Lines(txdiag.MsgHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want exactly 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if got := l.Str("op"); got != "txdiag_test.holdWriteTx3s" {
		t.Errorf("op = %q, want the call site txdiag_test.holdWriteTx3s", got)
	}
	if !strings.HasPrefix(l.Str("site"), "txdiag_test.holdWriteTx3s acceptance_sqlite_test.go:") {
		t.Errorf("site = %q", l.Str("site"))
	}
	if l.Int("hold_ms") < 3000 || l.Str("kind") != txdiag.KindTx || l.Str("outcome") != txdiag.OutcomeCommit {
		t.Errorf("hold line: %+v", l.Attrs)
	}
	if got := l.Int("rows_affected"); got != 1 {
		t.Errorf("rows_affected = %d, want 1", got)
	}
	if lw := e.h.Lines(txdiag.MsgLockWait); len(lw) != 0 {
		t.Errorf("unexpected lock_wait lines: %+v", lw)
	}
}

// Distinct named helpers, so the rate limiter never merges their lines.
func waitWriterTx(ctx context.Context, c *ent.Client) error {
	tx, err := c.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := newSetting(ctx, tx.Client(), "b"); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func waitWriterNonTx(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.HubSetting.UpdateOneID(id).SetRevision(7).Exec(ctx)
}

func TestAcceptanceConcurrentWritersLogLockWaits(t *testing.T) {
	e := openEnv(t, nil)
	ctx := context.Background()
	existing, err := newSetting(ctx, e.client, "existing")
	if err != nil {
		t.Fatal(err)
	}
	base := e.db.Stats().WaitCount

	holder, err := e.client.Tx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSetting(ctx, holder.Client(), "a"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- waitWriterTx(ctx, e.client) }()
	go func() { defer wg.Done(); errs <- waitWriterNonTx(ctx, e.client, existing.ID) }()
	e.waitForWaiters(t, base+2)
	e.clk.Advance(3 * time.Second)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.flush(t)

	waits := map[string]txdiag.LogLine{}
	for _, l := range e.h.Lines(txdiag.MsgLockWait) {
		waits[l.Str("op")] = l
	}
	for _, op := range []string{"txdiag_test.waitWriterTx", "txdiag_test.waitWriterNonTx"} {
		l, ok := waits[op]
		if !ok {
			t.Errorf("no lock_wait line for %s; lines: %+v", op, e.h.Lines(txdiag.MsgLockWait))
			continue
		}
		if l.Int("wait_ms") < 3000 || l.Str("outcome") != txdiag.OutcomeAcquired {
			t.Errorf("%s lock_wait line: %+v", op, l.Attrs)
		}
	}
	if len(waits) != 2 {
		t.Errorf("got lock_wait lines for %d ops, want 2: %+v", len(waits), e.h.Lines(txdiag.MsgLockWait))
	}
	holds := e.h.Lines(txdiag.MsgHold)
	if len(holds) != 1 {
		t.Fatalf("got %d hold lines, want 1 (the holder): %+v", len(holds), holds)
	}
	if got := holds[0].Int("waiters_max"); got != 2 {
		t.Errorf("holder waiters_max = %d, want 2", got)
	}
}

func deadlineWaiter(ctx context.Context, c *ent.Client) error {
	tx, err := c.Tx(ctx)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

func TestAcceptanceDeadlineWaiter(t *testing.T) {
	e := openEnv(t, nil)
	ctx := context.Background()
	base := e.db.Stats().WaitCount
	holder, err := e.client.Tx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSetting(ctx, holder.Client(), "a"); err != nil {
		t.Fatal(err)
	}
	mctx := txdiag.NewManualCtx()
	res := make(chan error, 1)
	go func() { res <- deadlineWaiter(mctx, e.client) }()
	e.waitForWaiters(t, base+1)
	e.clk.Advance(2 * time.Second)
	mctx.Fire(context.DeadlineExceeded)
	if err := <-res; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want DeadlineExceeded", err)
	}
	_ = holder.Rollback()
	e.flush(t)
	lines := e.h.Lines(txdiag.MsgLockWait)
	if len(lines) != 1 {
		t.Fatalf("got %d lock_wait lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if l.Str("outcome") != txdiag.OutcomeDeadline || l.Int("wait_ms") < 2000 || l.Str("op") != "txdiag_test.deadlineWaiter" {
		t.Errorf("lock_wait line: %+v", l.Attrs)
	}
}

// updateViaHooks makes a non-transactional ent mutation, which runs through
// the entc mutation hooks and sqlgraph.
func updateViaHooks(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.HubSetting.UpdateOneID(id).SetRevision(9).Exec(ctx)
}

// TestNonTxMutationIsNamedByItsCaller: every line from a non-tx mutation
// names the caller, never an entc hook or generated ent frame.
func TestNonTxMutationIsNamedByItsCaller(t *testing.T) {
	e := openEnv(t, nil)
	ctx := context.Background()
	s, err := newSetting(ctx, e.client, "x")
	if err != nil {
		t.Fatal(err)
	}
	e.flush(t)
	before := len(e.h.Lines(""))
	// Every clock read advances 1 s, so the mutation's hold crosses the 2 s
	// threshold while it runs (no "0" threshold: 0 means off).
	e.clk.SetStep(time.Second)
	err = updateViaHooks(ctx, e.client, s.ID)
	e.clk.SetStep(0)
	if err != nil {
		t.Fatal(err)
	}
	e.flush(t)
	lines := e.h.Lines("")[before:]
	var holds int
	for _, l := range lines {
		if l.Msg == txdiag.MsgHold {
			holds++
		}
		if got := l.Str("op"); got != "txdiag_test.updateViaHooks" {
			t.Errorf("%s line op = %q, want txdiag_test.updateViaHooks", l.Msg, got)
		}
		if !strings.HasPrefix(l.Str("site"), "txdiag_test.updateViaHooks ") {
			t.Errorf("site = %q, want it to start at txdiag_test.updateViaHooks", l.Str("site"))
		}
	}
	if holds == 0 {
		t.Fatalf("no hold line for the mutation; lines: %+v", lines)
	}
}

// realClockSmokeOp is the op of realClockSmokeTx's transaction: the hold
// line's stable marker, so the smoke test judges only its own transaction.
const realClockSmokeOp = "txdiag_test.realClockSmokeTx"

// realClockSmokeTx holds a write transaction for d of real time.
func realClockSmokeTx(ctx context.Context, e *env, d time.Duration) error {
	tx, err := e.client.Tx(ctx)
	if err != nil {
		return err
	}
	// A no-op after a successful Commit: database/sql returns ErrTxDone
	// without reaching the driver, so no second hold line is recorded.
	defer func() { _ = tx.Rollback() }()
	if _, err := newSetting(ctx, tx.Client(), "smoke"); err != nil {
		return err
	}
	time.Sleep(d)
	return tx.Commit()
}

// ownHoldLine returns the single hold line whose op is op, or an error when
// there is not exactly one or it held for less than minMs. Lines from other
// transactions are ignored: on a slow runner the fixture's own setup
// transaction (openEnv's migration, hundreds of statements) can cross the
// threshold too (ptone/scion#6906), and it must neither fail the test nor
// stand in for the line under test.
func ownHoldLine(lines []txdiag.LogLine, op string, minMs int64) (txdiag.LogLine, error) {
	var own []txdiag.LogLine
	for _, l := range lines {
		if l.Str("op") == op {
			own = append(own, l)
		}
	}
	if len(own) != 1 {
		return txdiag.LogLine{}, fmt.Errorf("got %d hold lines with op %q among %+v, want exactly one", len(own), op, lines)
	}
	if got := own[0].Int("hold_ms"); got < minMs {
		return txdiag.LogLine{}, fmt.Errorf("hold line %+v has hold_ms %d, want >= %d", own[0], got, minMs)
	}
	return own[0], nil
}

// TestRealClockSmoke runs the hold detector on the real clock (Now nil): a
// transaction held for 300 ms of real time against a 200 ms threshold must
// produce one hold line for that transaction.
func TestRealClockSmoke(t *testing.T) {
	e := openEnv(t, func(c *txdiag.Config) {
		c.Now = nil
		c.HoldWarnThreshold = 200 * time.Millisecond
	})
	if err := realClockSmokeTx(context.Background(), e, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	e.flush(t)
	l, err := ownHoldLine(e.h.Lines(txdiag.MsgHold), realClockSmokeOp, 300)
	if err != nil {
		t.Fatal(err)
	}
	if l.Str("kind") != txdiag.KindTx || l.Str("outcome") != txdiag.OutcomeCommit {
		t.Errorf("hold line %+v, want kind %q and outcome %q", l, txdiag.KindTx, txdiag.OutcomeCommit)
	}
}

// TestRealClockSmokeFailsWithoutTheHold is the negative control for
// TestRealClockSmoke, on the same real-clock path: when the transaction's
// hold is not reported (the threshold is above it), the check must fail,
// even though other hold lines may be present.
func TestRealClockSmokeFailsWithoutTheHold(t *testing.T) {
	e := openEnv(t, func(c *txdiag.Config) {
		c.Now = nil
		c.HoldWarnThreshold = time.Hour
	})
	if err := realClockSmokeTx(context.Background(), e, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	e.flush(t)
	if l, err := ownHoldLine(e.h.Lines(txdiag.MsgHold), realClockSmokeOp, 300); err == nil {
		t.Fatalf("ownHoldLine accepted %+v although the hold was not reported", l)
	}
}

// TestOwnHoldLineIgnoresOtherTransactions pins the #6906 case without a
// clock: a long setup-transaction line alone must not satisfy the check,
// and next to the line under test it must not break it.
func TestOwnHoldLineIgnoresOtherTransactions(t *testing.T) {
	line := func(op string, holdMs int64) txdiag.LogLine {
		return txdiag.LogLine{Msg: txdiag.MsgHold, Attrs: map[string]slog.Value{
			"op": slog.StringValue(op), "hold_ms": slog.Int64Value(holdMs), "stmts": slog.Int64Value(310),
		}}
	}
	setup := line("entc.AutoMigrate", 394)
	own := line(realClockSmokeOp, 374)
	if _, err := ownHoldLine([]txdiag.LogLine{setup}, realClockSmokeOp, 300); err == nil {
		t.Error("the fixture's setup line alone satisfied the check")
	}
	if l, err := ownHoldLine([]txdiag.LogLine{setup, own}, realClockSmokeOp, 300); err != nil || l.Int("hold_ms") != 374 {
		t.Errorf("setup plus own line: got %+v, %v; want the own line", l, err)
	}
	if _, err := ownHoldLine([]txdiag.LogLine{line(realClockSmokeOp, 120)}, realClockSmokeOp, 300); err == nil {
		t.Error("a short own hold satisfied the check")
	}
	if _, err := ownHoldLine([]txdiag.LogLine{own, own}, realClockSmokeOp, 300); err == nil {
		t.Error("two own lines satisfied the check")
	}
}

func TestRegisteredDriverHooksApply(t *testing.T) {
	before := hookCalls.Load()
	e := openEnv(t, nil)
	if err := e.db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hookCalls.Load() <= before {
		t.Fatal("a connection hook registered on the modernc driver did not run on the wrapped open path")
	}
	db0, err := sql.Open("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db0.Close() }()
	if e.db.Driver() != db0.Driver() {
		t.Errorf("wrapped DB's driver %T is not the registered instance", e.db.Driver())
	}
}

func TestRawYieldsWrapperThatUnwrapsToModerncConn(t *testing.T) {
	e := openEnv(t, nil)
	conn, err := e.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	err = conn.Raw(func(dc any) error {
		u, ok := dc.(interface{ Unwrap() driver.Conn })
		if !ok {
			return fmt.Errorf("Raw yields %T, which has no Unwrap", dc)
		}
		if _, ok := u.Unwrap().(interface {
			NewRestore(string) (*sqlite.Backup, error)
		}); !ok {
			return fmt.Errorf("Unwrap() = %T, which has no NewRestore", u.Unwrap())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWrappedAndUnwrappedClients(t *testing.T) {
	e := openEnv(t, nil)
	if _, ok := entc.SQLDriver(e.client.Driver()); !ok {
		t.Error("SQLDriver does not unwrap the diagnostics driver")
	}
	if _, ok := e.client.Driver().(*txdiag.EntDriver); !ok {
		t.Errorf("client driver is %T, want *txdiag.EntDriver", e.client.Driver())
	}

	// Kill switch: without Diag the open path is unwrapped.
	dsn := "file:" + filepath.Join(t.TempDir(), "plain.db")
	plain, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if _, ok := entc.DiagMonitor(plain); ok {
		t.Error("an unwrapped client reports a Monitor")
	}
	if _, ok := plain.Driver().(*txdiag.EntDriver); ok {
		t.Error("an unwrapped client has the diagnostics driver")
	}
	if _, ok := entc.SQLDriver(plain.Driver()); !ok {
		t.Error("SQLDriver fails on an unwrapped client")
	}
}

// benchVariants are the three ways the benchmarks open the store: without
// diagnostics, with diagnostics and no recorder (metrics export off), and
// with diagnostics and the hub's real dbmetrics recorder (metrics export on).
var benchVariants = []string{"unwrapped", "wrapped", "wrapped+recorder"}

func openBenchClient(b *testing.B, variant string) *ent.Client {
	b.Helper()
	pool := entc.PoolConfig{MaxOpenConns: 1}
	if variant != "unwrapped" {
		pool.Diag = txdiag.QuietConfig()
	}
	client, err := entc.OpenSQLite("file:"+filepath.Join(b.TempDir(), "b.db"), pool)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		b.Fatal(err)
	}
	if variant == "wrapped+recorder" {
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
		b.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
		rec, err := dbmetrics.New(mp)
		if err != nil {
			b.Fatal(err)
		}
		mon, ok := entc.DiagMonitor(client)
		tr, isRec := rec.(txdiag.Recorder)
		if !ok || !isRec {
			b.Fatal("cannot attach the dbmetrics recorder")
		}
		mon.SetRecorder(tr)
	}
	return client
}

// BenchmarkTx measures one write transaction (BEGIN, INSERT, COMMIT).
func BenchmarkTx(b *testing.B) {
	for _, v := range benchVariants {
		b.Run(v, func(b *testing.B) {
			client := openBenchClient(b, v)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx, err := client.Tx(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := newSetting(ctx, tx.Client(), fmt.Sprintf("s%d", i)); err != nil {
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStmt measures one cheap statement outside a transaction (a
// primary-key read): the per-checkout cost of the wrapper and probe.
func BenchmarkStmt(b *testing.B) {
	for _, v := range benchVariants {
		b.Run(v, func(b *testing.B) {
			client := openBenchClient(b, v)
			ctx := context.Background()
			s, err := newSetting(ctx, client, "bench")
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := client.HubSetting.Get(ctx, s.ID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
