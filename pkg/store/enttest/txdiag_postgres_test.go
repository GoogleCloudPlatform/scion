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

//go:build integration

package enttest

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
)

// pgDiagClock is a manual clock for the Postgres diagnostics tests.
type pgDiagClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pgDiagClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pgDiagClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// pgDiagHandler records the attributes of every line.
type pgDiagHandler struct {
	mu    sync.Mutex
	lines []map[string]slog.Value
	msgs  []string
}

func (h *pgDiagHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *pgDiagHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value; return true })
	h.mu.Lock()
	h.lines = append(h.lines, m)
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *pgDiagHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *pgDiagHandler) WithGroup(string) slog.Handler      { return h }

func (h *pgDiagHandler) byMsg(msg string) []map[string]slog.Value {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]slog.Value
	for i, m := range h.msgs {
		if m == msg {
			out = append(out, h.lines[i])
		}
	}
	return out
}

// openPostgresDiag opens a wrapped Postgres client on a fresh migrated
// schema, with a manual clock and a capturing handler.
func openPostgresDiag(t *testing.T) (*ent.Client, *txdiag.Monitor, *pgDiagClock, *pgDiagHandler) {
	t.Helper()
	if !Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration)")
	}
	clk := &pgDiagClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := &pgDiagHandler{}
	cfg := &txdiag.Config{
		HoldWarnThreshold:     2 * time.Second,
		LockWaitWarnThreshold: time.Second,
		Logger:                slog.New(h),
		Now:                   clk.Now,
	}
	client, err := entc.OpenPostgres(NewSchemaURL(t), entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, Diag: cfg})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	mon, ok := entc.DiagMonitor(client)
	if !ok {
		t.Fatal("wrapped Postgres client has no Monitor")
	}
	return client, mon, clk, h
}

// TestTxDiag_PostgresLongTxWarns: on Postgres the wrapper is active, so a
// write transaction held 3 s logs one tx hold line (no SQLite-only fields).
func TestTxDiag_PostgresLongTxWarns(t *testing.T) {
	client, mon, clk, h := openPostgresDiag(t)
	ctx := context.Background()
	tx, err := client.Tx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.HubSetting.Create().SetSection("pg-diag").SetValue(json.RawMessage(`{}`)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	clk.advance(3 * time.Second)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mon.Flush(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	lines := h.byMsg(txdiag.MessageHold)
	if len(lines) != 1 {
		t.Fatalf("got %d hold lines, want 1: %v", len(lines), lines)
	}
	l := lines[0]
	if l["kind"].String() != txdiag.KindTx || l["hold_ms"].Int64() < 3000 || l["outcome"].String() != txdiag.OutcomeCommit {
		t.Errorf("hold line: %v", l)
	}
	for _, k := range []string{"waiters_max", "busy"} {
		if _, ok := l[k]; ok {
			t.Errorf("Postgres hold line carries SQLite-only %s", k)
		}
	}
}

// TestTxDiag_PostgresUUIDRoundTrip: uuid parameters still bind through the
// wrapper: through ent, as a raw $1 parameter, and as a []uuid.UUID for
// ANY($1), which proves the wrapper forwards pgx's NamedValueChecker.
func TestTxDiag_PostgresUUIDRoundTrip(t *testing.T) {
	client, _, _, _ := openPostgresDiag(t)
	ctx := context.Background()
	s, err := client.HubSetting.Create().SetSection("pg-uuid").SetValue(json.RawMessage(`{}`)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.HubSetting.Get(ctx, s.ID)
	if err != nil || got.Section != "pg-uuid" {
		t.Fatalf("ent Get by uuid = %v, %v", got, err)
	}
	db := entc.SQLDB(client.Driver())
	if db == nil {
		t.Fatal("wrapped client exposes no *sql.DB")
	}
	var section string
	if err := db.QueryRowContext(ctx, "SELECT section FROM hub_settings WHERE id = $1", s.ID).Scan(&section); err != nil {
		t.Fatalf("raw uuid bind: %v", err)
	}
	if section != "pg-uuid" {
		t.Fatalf("section = %q", section)
	}
	// A []uuid.UUID argument is not a driver.Value, so database/sql's default
	// converter rejects it; it binds only if the wrapper forwards pgx's
	// NamedValueChecker (and pgx has the _uuid type registered).
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM hub_settings WHERE id = ANY($1)", []uuid.UUID{s.ID, uuid.New()}).Scan(&n); err != nil {
		t.Fatalf("[]uuid.UUID bind through the wrapper: %v", err)
	}
	if n != 1 {
		t.Fatalf("ANY($1) matched %d rows, want 1", n)
	}
	var back uuid.UUID
	if err := db.QueryRowContext(ctx, "SELECT id FROM hub_settings WHERE section = $1", "pg-uuid").Scan(&back); err != nil || back != s.ID {
		t.Fatalf("uuid scan = %v, %v; want %v", back, err, s.ID)
	}
}
