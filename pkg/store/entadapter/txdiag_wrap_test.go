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

package entadapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
)

// stepClock advances by step on every read while stepping is on.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *stepClock) setStep(d time.Duration) {
	c.mu.Lock()
	c.step = d
	c.mu.Unlock()
}

// opHandler records the op attribute of every hold line.
type opHandler struct {
	mu  sync.Mutex
	ops []string
}

func (h *opHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *opHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "db_tx_long_hold" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "op" {
			h.mu.Lock()
			h.ops = append(h.ops, a.Value.String())
			h.mu.Unlock()
		}
		return true
	})
	return nil
}
func (h *opHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *opHandler) WithGroup(string) slog.Handler      { return h }

func (h *opHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ops...)
}

// txdiagRunInEntTxCaller opens its transaction through the runInEntTx
// funnel; the hold line must name this function, not the funnel.
func txdiagRunInEntTxCaller(ctx context.Context, client *ent.Client) error {
	return runInEntTx(ctx, client, false, func(c *ent.Client) error {
		_, err := c.HubSetting.Create().SetSection("funnel").SetValue(json.RawMessage(`{}`)).Save(ctx)
		return err
	})
}

func openDiagCompositeStore(t *testing.T) (*CompositeStore, *ent.Client, *stepClock, *opHandler) {
	t.Helper()
	clk := &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := &opHandler{}
	cfg := &txdiag.Config{
		HoldWarnThreshold:     2 * time.Second,
		LockWaitWarnThreshold: time.Second,
		Logger:                slog.New(h),
		Now:                   clk.Now,
	}
	client, err := entc.OpenSQLite("file:"+filepath.Join(t.TempDir(), "hub.db"), entc.PoolConfig{MaxOpenConns: 1, Diag: cfg})
	require.NoError(t, err)
	s := NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	return s, client, clk, h
}

func TestRunInEntTxCallerNamesTheOp(t *testing.T) {
	_, client, clk, h := openDiagCompositeStore(t)
	mon, ok := entc.DiagMonitor(client)
	require.True(t, ok)
	clk.setStep(time.Second) // every hold now crosses the 2 s threshold
	err := txdiagRunInEntTxCaller(context.Background(), client)
	clk.setStep(0)
	require.NoError(t, err)
	require.NoError(t, mon.Flush(5*time.Second))
	ops := h.snapshot()
	require.NotEmpty(t, ops, "no hold line")
	for _, op := range ops {
		require.Equal(t, "entadapter.txdiagRunInEntTxCaller", op)
	}
}

// TestWrappedCompositeStoreExposesDB: with diagnostics on, the store's
// *sql.DB (used by the raw-SQL stores and Ping) is still reachable.
func TestWrappedCompositeStoreExposesDB(t *testing.T) {
	s, _, _, _ := openDiagCompositeStore(t)
	require.NotNil(t, s.DB())
	require.NoError(t, s.Ping(context.Background()))
}
