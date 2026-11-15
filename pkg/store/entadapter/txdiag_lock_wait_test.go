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

package entadapter_test

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

type lockWaitRecord struct {
	msg   string
	attrs map[string]string
}

type lockWaitCaptureHandler struct {
	mu      sync.Mutex
	records []lockWaitRecord
}

func (h *lockWaitCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *lockWaitCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, lockWaitRecord{msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}
func (h *lockWaitCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lockWaitCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *lockWaitCaptureHandler) byMsg(msg string) []lockWaitRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []lockWaitRecord
	for _, r := range h.records {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// checkoutPauseClock advances manually and can pause the second clock read
// while db has an in-use connection (right after txdiag.conn.touch opens the
// checkout, before QueryContext finishes), so a concurrent request can queue
// behind a real store query deterministically.
type checkoutPauseClock struct {
	mu         sync.Mutex
	t          time.Time
	db         *sql.DB
	arm        bool
	inUseReads int
	paused     chan struct{}
	resume     chan struct{}
}

func (c *checkoutPauseClock) Now() time.Time {
	c.mu.Lock()
	var wait bool
	if c.arm && c.db != nil && c.db.Stats().InUse > 0 {
		c.inUseReads++
		if c.inUseReads == 2 {
			c.arm = false
			wait = true
			close(c.paused)
		}
	}
	now := c.t
	c.mu.Unlock()
	if wait {
		<-c.resume
		c.mu.Lock()
		now = c.t
		c.mu.Unlock()
	}
	return now
}

func (c *checkoutPauseClock) Arm(db *sql.DB) (paused <-chan struct{}, resume chan<- struct{}) {
	p := make(chan struct{})
	r := make(chan struct{})
	c.mu.Lock()
	c.db = db
	c.arm = true
	c.inUseReads = 0
	c.paused = p
	c.resume = r
	c.mu.Unlock()
	return p, r
}

func (c *checkoutPauseClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// hubCallerListingMessages stands in for a pkg/hub caller invoking the shared
// ListMessages store method (including the multi-frame ParticipantID path).
//
//go:noinline
func hubCallerListingMessages(ctx context.Context, s store.MessageStore, filter store.MessageFilter, opts store.ListOptions) error {
	_, err := s.ListMessages(ctx, filter, opts)
	return err
}

// hubWaiterListingProjects stands in for a concurrent request waiting for the
// SQLite connection.
//
//go:noinline
func hubWaiterListingProjects(ctx context.Context, s store.ProjectStore) error {
	_, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1})
	return err
}

// TestLockWaitNamesStoreCallerOnOneSecondHold verifies ptone/scion#7593 end to
// end: when a shared store method (ListMessages, both standard and
// multi-frame ParticipantID paths) holds the SQLite connection for ~1 s while
// another request waits, the emitted db_lock_wait line names the external
// caller in top_holder_site even though no db_tx_long_hold line fires below
// the 2 s hold threshold.
func TestLockWaitNamesStoreCallerOnOneSecondHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter store.MessageFilter
		opts   store.ListOptions
	}{
		{
			name:   "standard_page",
			filter: store.MessageFilter{RecipientID: "agent-1"},
			opts:   store.ListOptions{Limit: 10, SkipTotalCount: true},
		},
		{
			name:   "participant_page",
			filter: store.MessageFilter{ParticipantID: "agent-1"},
			opts:   store.ListOptions{Limit: 10, SkipTotalCount: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &checkoutPauseClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			h := &lockWaitCaptureHandler{}
			cfg := &txdiag.Config{
				HoldWarnThreshold:     2 * time.Second,
				LockWaitWarnThreshold: time.Second,
				Logger:                slog.New(h),
				Now:                   clk.Now,
			}
			dsn := "file:" + filepath.Join(t.TempDir(), "hub.db")
			client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1, Diag: cfg})
			require.NoError(t, err)
			cs := entadapter.NewCompositeStore(client)
			t.Cleanup(func() { _ = cs.Close() })
			require.NoError(t, entc.AutoMigrate(context.Background(), client))

			db := cs.DB()
			require.NotNil(t, db)
			mon, ok := entc.DiagMonitor(client)
			require.True(t, ok)
			require.NoError(t, mon.Flush(5*time.Second))

			baseWait := db.Stats().WaitCount
			paused, resume := clk.Arm(db)
			var resumeOnce sync.Once
			unpause := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(unpause)

			holderErr := make(chan error, 1)
			go func() {
				holderErr <- hubCallerListingMessages(context.Background(), cs, tc.filter, tc.opts)
			}()
			select {
			case <-paused:
			case err := <-holderErr:
				t.Fatalf("holder completed without pausing checkout clock: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for holder checkout to pause")
			}

			waiterErr := make(chan error, 1)
			go func() {
				waiterErr <- hubWaiterListingProjects(context.Background(), cs)
			}()
			require.Eventually(t, func() bool {
				return db.Stats().WaitCount >= baseWait+1
			}, 5*time.Second, time.Millisecond)

			// 1.1 s crosses LockWaitWarnThreshold (1 s) without crossing
			// HoldWarnThreshold (2 s).
			clk.Advance(1100 * time.Millisecond)
			unpause()

			select {
			case err := <-holderErr:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for holder to finish")
			}
			select {
			case err := <-waiterErr:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for waiter to finish")
			}
			require.NoError(t, mon.Flush(5*time.Second))

			require.Empty(t, h.byMsg(txdiag.MessageHold), "no db_tx_long_hold expected below 2s threshold")
			waits := h.byMsg(txdiag.MessageLockWait)
			require.Len(t, waits, 1)
			got := waits[0].attrs
			require.Contains(t, got["op"], "entadapter.(*ProjectStore).")
			require.Contains(t, got["site"], "entadapter_test.hubWaiterListingProjects")
			require.Contains(t, got["top_holder_op"], "entadapter.(*MessageStore).ListMessages")
			require.Contains(t, got["top_holder_site"], "entadapter_test.hubCallerListingMessages",
				"top_holder_site must name the caller outside entadapter; attrs=%v", got)
		})
	}
}
