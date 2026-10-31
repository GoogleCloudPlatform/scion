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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeChatRetentionClock is a manual clock: sleep advances it.
type fakeChatRetentionClock struct{ t time.Time }

func (c *fakeChatRetentionClock) now() time.Time { return c.t }

// fakeChatRetentionMsg is one message of fakeChatRetentionStore.
type fakeChatRetentionMsg struct {
	id      string
	created time.Time
	project string
	state   string
}

// fakeChatRetentionStore is an in-memory store.ChatRetentionStore. It
// counts the rows each selection examines (the rows a created-index range
// scan from the cursor would visit, pending ones included), and purgeCost
// advances the clock to simulate the transaction time.
type fakeChatRetentionStore struct {
	clock     *fakeChatRetentionClock
	msgs      []fakeChatRetentionMsg
	cursor    store.ChatRetentionCursor
	hasCursor bool
	runs      []*store.ChatRetentionRun

	purgeCost func(call int) time.Duration
	purgeErr  error
	// afterPurge, when set, runs after each successful purge (a test
	// cancels the tick's context from it).
	afterPurge  func(call int)
	purgeCalls  int
	selectCalls int
	scanned     int
	lastSelect  store.ChatRetentionPosition
	deadlines   []bool
}

func (f *fakeChatRetentionStore) add(id string, created time.Time, project, state string) {
	f.msgs = append(f.msgs, fakeChatRetentionMsg{id: id, created: created, project: project, state: state})
	sort.Slice(f.msgs, func(i, j int) bool {
		if !f.msgs[i].created.Equal(f.msgs[j].created) {
			return f.msgs[i].created.Before(f.msgs[j].created)
		}
		return f.msgs[i].id < f.msgs[j].id
	})
}

func (f *fakeChatRetentionStore) SelectChatRetentionCandidates(ctx context.Context, cutoff time.Time, after store.ChatRetentionPosition, limit int) ([]store.ChatRetentionCandidate, error) {
	f.selectCalls++
	f.lastSelect = after
	var out []store.ChatRetentionCandidate
	for _, m := range f.msgs {
		if !m.created.Before(cutoff) {
			break
		}
		if !after.IsStart() && (m.created.Before(after.Created) || (m.created.Equal(after.Created) && m.id <= after.ID)) {
			continue
		}
		f.scanned++
		if m.state == "pending" || m.state == "deferred" {
			continue
		}
		out = append(out, store.ChatRetentionCandidate{ID: m.id, Created: m.created, ProjectID: m.project})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeChatRetentionStore) PurgeChatMessages(ctx context.Context, ids []string, projectOf map[string]string, cursor store.ChatRetentionCursor) (store.ChatRetentionPurgeResult, error) {
	f.purgeCalls++
	_, hasDeadline := ctx.Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)
	if f.purgeCost != nil {
		f.clock.t = f.clock.t.Add(f.purgeCost(f.purgeCalls))
	}
	res := store.ChatRetentionPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}
	if f.purgeErr != nil {
		return res, f.purgeErr
	}
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	kept := f.msgs[:0]
	for _, m := range f.msgs {
		if drop[m.id] {
			c := res.ByProject[m.project]
			c.Main++
			c.Mentions++
			res.ByProject[m.project] = c
			continue
		}
		kept = append(kept, m)
	}
	f.msgs = kept
	f.cursor, f.hasCursor = cursor, true
	if f.afterPurge != nil {
		f.afterPurge(f.purgeCalls)
	}
	return res, nil
}

func (f *fakeChatRetentionStore) GetChatRetentionCursor(ctx context.Context, phase string) (store.ChatRetentionCursor, error) {
	if !f.hasCursor {
		return store.ChatRetentionCursor{Phase: phase}, nil
	}
	return f.cursor, nil
}

func (f *fakeChatRetentionStore) SaveChatRetentionCursor(ctx context.Context, c store.ChatRetentionCursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.cursor, f.hasCursor = c, true
	return nil
}

func (f *fakeChatRetentionStore) LatestChatRetentionRun(ctx context.Context, trigger string, since time.Time) (*store.ChatRetentionRun, error) {
	var latest *store.ChatRetentionRun
	for _, r := range f.runs {
		if r.Trigger == trigger && !r.StartedAt.Before(since) && (latest == nil || r.StartedAt.After(latest.StartedAt)) {
			latest = r
		}
	}
	if latest == nil {
		return nil, nil
	}
	cp := *latest
	cp.Counts = map[string]store.ChatRetentionCounts{}
	for k, v := range latest.Counts {
		cp.Counts[k] = v
	}
	return &cp, nil
}

func (f *fakeChatRetentionStore) SaveChatRetentionRun(ctx context.Context, run *store.ChatRetentionRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cp := *run
	if run.ID == "" {
		run.ID = fmt.Sprintf("run-%d", len(f.runs)+1)
		cp.ID = run.ID
		f.runs = append(f.runs, &cp)
		return nil
	}
	for i, r := range f.runs {
		if r.ID == run.ID {
			f.runs[i] = &cp
			return nil
		}
	}
	return errors.New("no such run")
}

func (f *fakeChatRetentionStore) PruneChatRetentionRuns(ctx context.Context, before time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	kept := f.runs[:0]
	n := 0
	for _, r := range f.runs {
		if r.StartedAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	f.runs = kept
	return n, nil
}

// chatRetentionUnitHarness is a sweeper on a fake store and clock, with the
// pauses it took recorded.
type chatRetentionUnitHarness struct {
	clock  *fakeChatRetentionClock
	st     *fakeChatRetentionStore
	sw     *chatRetentionSweeper
	pauses []time.Duration
}

func newChatRetentionUnitHarness() *chatRetentionUnitHarness {
	clock := &fakeChatRetentionClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	h := &chatRetentionUnitHarness{clock: clock, st: &fakeChatRetentionStore{clock: clock}}
	h.sw = &chatRetentionSweeper{store: h.st, now: clock.now, sleep: func(ctx context.Context, d time.Duration) error {
		h.pauses = append(h.pauses, d)
		clock.t = clock.t.Add(d)
		return ctx.Err()
	}}
	return h
}

// seedOld adds n delivered messages of project, a minute apart, starting
// 100 days before the clock.
func (h *chatRetentionUnitHarness) seedOld(n int, project string) {
	base := h.clock.t.Add(-100 * 24 * time.Hour)
	for i := 0; i < n; i++ {
		h.st.add(fmt.Sprintf("%s-%06d", project, i), base.Add(time.Duration(i)*time.Minute), project, "delivered")
	}
}

func (h *chatRetentionUnitHarness) policy(cfg opsettings.ChatRetentionConfig) chatRetentionPolicy {
	return newChatRetentionPolicy(true, cfg, h.clock.t)
}

func chatRetentionTestConfig(mut func(*opsettings.ChatRetentionConfig)) opsettings.ChatRetentionConfig {
	c := opsettings.DefaultChatRetentionConfig()
	c.Enabled = true
	c.MessagesRetentionDays = 30
	if mut != nil {
		mut(&c)
	}
	return c
}

func TestNextChatRetentionChunk(t *testing.T) {
	tests := []struct {
		cur, max int
		tx       time.Duration
		want     int
	}{
		{200, 200, 300 * time.Millisecond, 100},
		{100, 200, 251 * time.Millisecond, 50},
		{40, 200, time.Second, 25},
		{25, 200, time.Second, 25},
		{200, 200, 10 * time.Millisecond, 200},
		{100, 200, 10 * time.Millisecond, 125},
		{180, 200, 10 * time.Millisecond, 200},
		{25, 200, 49 * time.Millisecond, 31},
		{3, 200, 0, 4},
		{100, 200, 50 * time.Millisecond, 100},
		{100, 200, 250 * time.Millisecond, 100},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, nextChatRetentionChunk(tt.cur, tt.max, tt.tx), "cur=%d tx=%v", tt.cur, tt.tx)
	}
}

// TestChatRetentionPolicy_Gating: nothing is purged unless the experiment
// is on, enabled is true and a window is set.
func TestChatRetentionPolicy_Gating(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		experiment bool
		enabled    bool
		days       int
		purges     bool
	}{
		{"experiment off", false, true, 30, false},
		{"enabled off", true, false, 30, false},
		{"window null", true, true, 0, false},
		{"all off", false, false, 0, false},
		{"active with window", true, true, 30, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) {
				c.Enabled = tt.enabled
				c.MessagesRetentionDays = tt.days
			})
			p := newChatRetentionPolicy(tt.experiment, cfg, now)
			require.Equal(t, tt.purges, p.purges())
			if tt.purges {
				require.True(t, p.MainCutoff.Equal(now.Add(-30*24*time.Hour)))
			}

			h := newChatRetentionUnitHarness()
			h.seedOld(10, "p1")
			res, err := h.sw.RunTick(context.Background(), newChatRetentionPolicy(tt.experiment, cfg, h.clock.t))
			require.NoError(t, err)
			require.Equal(t, tt.purges, res.Ran)
			if !tt.purges {
				assert.Len(t, h.st.msgs, 10, "nothing purged")
				assert.Zero(t, h.st.selectCalls+h.st.purgeCalls, "the store is not touched")
				assert.Empty(t, h.st.runs, "no run row")
			} else {
				assert.Empty(t, h.st.msgs)
			}
		})
	}
}

// TestChatRetentionSweeper_SlowTxShrinksAndBudgetStops: a slow purge
// transaction halves the chunk size down to the floor; every chunk is
// followed by the configured pause; the tick stops at its budget.
func TestChatRetentionSweeper_SlowTxShrinksAndBudgetStops(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(10000, "p1")
	h.st.purgeCost = func(int) time.Duration { return 300 * time.Millisecond }
	cfg := chatRetentionTestConfig(nil)

	res, err := h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	require.True(t, res.Ran)
	assert.True(t, res.BudgetExhausted)
	assert.False(t, res.ReachedCutoff)
	require.GreaterOrEqual(t, len(res.ChunkSizes), 5)
	assert.Equal(t, []int{200, 100, 50, 25, 25}, res.ChunkSizes[:5])
	assert.Equal(t, 300*time.Millisecond, res.MaxTx)
	// (300 ms tx + 500 ms pause) per chunk in a 30 s budget.
	assert.Equal(t, 38, res.Chunks)
	for _, p := range h.pauses {
		assert.Equal(t, 500*time.Millisecond, p)
	}
	for _, d := range h.st.deadlines {
		assert.True(t, d, "each chunk runs under a timeout")
	}
	run := h.st.runs[0]
	assert.Equal(t, store.ChatRetentionStatusPartial, run.Status)
}

// TestChatRetentionSweeper_FastTxGrowsToCap: after slow transactions shrink
// the chunk, fast ones grow it by 25% up to chunk_size.
func TestChatRetentionSweeper_FastTxGrowsToCap(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(5000, "p1")
	h.st.purgeCost = func(call int) time.Duration {
		if call <= 3 {
			return 300 * time.Millisecond
		}
		return 10 * time.Millisecond
	}
	res, err := h.sw.RunTick(context.Background(), h.policy(chatRetentionTestConfig(nil)))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.ChunkSizes), 12)
	assert.Equal(t, []int{200, 100, 50, 25, 31, 38, 47, 58, 72, 90, 112, 140}, res.ChunkSizes[:12])
	for _, c := range res.ChunkSizes {
		assert.LessOrEqual(t, c, 200)
	}
	assert.Equal(t, 200, res.ChunkSizes[len(res.ChunkSizes)-1])
}

// TestChatRetentionSweeper_ReachesCutoff: a walk that returns a short chunk
// reaches the cutoff: the cursor records it as the last applied cutoff, and
// the run row carries per-project counts and the policy but no message ID.
func TestChatRetentionSweeper_ReachesCutoff(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(150, "proj-a")
	h.seedOld(70, "proj-b")
	recent := h.clock.t.Add(-time.Hour)
	h.st.add("recent-1", recent, "proj-a", "delivered")
	cfg := chatRetentionTestConfig(nil)
	p := h.policy(cfg)

	res, err := h.sw.RunTick(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, res.ReachedCutoff)
	assert.Equal(t, int64(220), res.Purged.Main)
	require.Len(t, h.st.msgs, 1)
	assert.Equal(t, "recent-1", h.st.msgs[0].id)

	require.NotNil(t, h.st.cursor.LastAppliedCutoff)
	assert.True(t, h.st.cursor.LastAppliedCutoff.Equal(p.MainCutoff))

	require.Len(t, h.st.runs, 1)
	run := h.st.runs[0]
	assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
	assert.Equal(t, 30, run.Policy.MessagesRetentionDays)
	assert.True(t, run.Policy.MainCutoff.Equal(p.MainCutoff))
	assert.Equal(t, int64(150), run.Counts["proj-a"].Main)
	assert.Equal(t, int64(70), run.Counts["proj-b"].Main)
	doc, err := json.Marshal(run)
	require.NoError(t, err)
	assert.NotContains(t, string(doc), "proj-a-0000", "the run holds no message IDs")
}

// TestChatRetentionSweeper_TickInterval: a tick less than
// tick_interval_minutes after the last one is skipped.
func TestChatRetentionSweeper_TickInterval(t *testing.T) {
	h := newChatRetentionUnitHarness()
	cfg := chatRetentionTestConfig(nil)
	res, err := h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	require.True(t, res.Ran)

	h.seedOld(5, "p1")
	h.clock.t = h.clock.t.Add(2 * time.Minute)
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.False(t, res.Ran, "2 min after the last tick with a 10 min interval")
	assert.Len(t, h.st.msgs, 5)

	h.clock.t = h.clock.t.Add(8 * time.Minute)
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.True(t, res.Ran)
	assert.Empty(t, h.st.msgs)
}

// TestChatRetentionSweeper_CursorNoRescanAndDailyReset: rows skipped as
// pending are not scanned again by later ticks of the same day; once a day
// the cursor goes back to the start, so a skipped row that was dispatched
// since is purged.
func TestChatRetentionSweeper_CursorNoRescanAndDailyReset(t *testing.T) {
	h := newChatRetentionUnitHarness()
	base := h.clock.t.Add(-100 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		h.st.add(fmt.Sprintf("pending-%03d", i), base.Add(time.Duration(i)*time.Second), "p1", "pending")
	}
	h.st.add("deferred-1", base.Add(time.Minute), "p1", "deferred")
	h.seedOld(10, "p1")
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.ChunkSize = 25 })

	res, err := h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.True(t, res.ReachedCutoff)
	assert.Equal(t, int64(10), res.Purged.Main)
	require.Len(t, h.st.msgs, 51, "pending and deferred rows are kept")
	firstScan := h.st.scanned
	assert.GreaterOrEqual(t, firstScan, 51)

	// New old rows arrive; the next tick of the same day resumes after the
	// cursor and does not examine the skipped rows again.
	h.clock.t = h.clock.t.Add(10 * time.Minute)
	h.st.add("late-1", h.clock.t.Add(-40*24*time.Hour), "p1", "delivered")
	h.st.scanned = 0
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Purged.Main)
	assert.Equal(t, 1, h.st.scanned, "only the new row is examined")

	// A pending row is dispatched; the same day it is not picked up.
	h.st.msgs[0].state = "delivered"
	h.clock.t = h.clock.t.Add(10 * time.Minute)
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.Zero(t, res.Purged.Main)

	// A day after the last reset the walk starts over and purges it.
	h.clock.t = h.clock.t.Add(24 * time.Hour)
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Purged.Main)
	require.NotNil(t, h.st.cursor.ResetAt)
	assert.True(t, h.st.cursor.ResetAt.After(h.clock.t.Add(-time.Hour)))
	assert.Len(t, h.st.msgs, 50)
}

// TestChatRetentionSweeper_RunAccumulates: ticks within 24 hours add to the
// same sweep run; a tick after that opens a new one; runs older than a
// year are pruned.
func TestChatRetentionSweeper_RunAccumulates(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.st.runs = append(h.st.runs, &store.ChatRetentionRun{ID: "ancient", Trigger: store.ChatRetentionTriggerSweep, StartedAt: h.clock.t.Add(-400 * 24 * time.Hour)})
	cfg := chatRetentionTestConfig(nil)
	h.seedOld(3, "p1")
	_, err := h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	require.Len(t, h.st.runs, 1, "the year-old run is pruned")

	h.clock.t = h.clock.t.Add(time.Hour)
	h.seedOld(4, "p1")
	_, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	require.Len(t, h.st.runs, 1)
	assert.Equal(t, int64(7), h.st.runs[0].Counts["p1"].Main)

	h.clock.t = h.clock.t.Add(24 * time.Hour)
	h.seedOld(2, "p1")
	_, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	require.Len(t, h.st.runs, 2)
	assert.Equal(t, int64(2), h.st.runs[1].Counts["p1"].Main)
}

// TestChatRetentionSweeper_PurgeFailure: a failed purge stops the tick,
// records the error on the run and leaves the cursor where it was.
func TestChatRetentionSweeper_PurgeFailure(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(10, "p1")
	h.st.purgeErr = errors.New("disk on fire")
	_, err := h.sw.RunTick(context.Background(), h.policy(chatRetentionTestConfig(nil)))
	require.Error(t, err)
	require.Len(t, h.st.runs, 1)
	assert.Equal(t, store.ChatRetentionStatusFailed, h.st.runs[0].Status)
	assert.True(t, strings.Contains(h.st.runs[0].Error, "disk on fire"))
	assert.True(t, h.st.cursor.Position.IsStart())
	assert.Nil(t, h.st.cursor.LastAppliedCutoff)
	assert.Len(t, h.st.msgs, 10)

	// A clean tick in the same 24 h run adds its counts, but the run stays
	// failed with the first error until it closes.
	h.st.purgeErr = nil
	h.clock.t = h.clock.t.Add(10 * time.Minute)
	res, err := h.sw.RunTick(context.Background(), h.policy(chatRetentionTestConfig(nil)))
	require.NoError(t, err)
	assert.True(t, res.ReachedCutoff)
	require.Len(t, h.st.runs, 1)
	assert.Equal(t, store.ChatRetentionStatusFailed, h.st.runs[0].Status)
	assert.Contains(t, h.st.runs[0].Error, "disk on fire")
	assert.Equal(t, int64(10), h.st.runs[0].Counts["p1"].Main)

	// The next run row, a day later, starts clean.
	h.clock.t = h.clock.t.Add(25 * time.Hour)
	h.seedOld(1, "p1")
	_, err = h.sw.RunTick(context.Background(), h.policy(chatRetentionTestConfig(nil)))
	require.NoError(t, err)
	require.Len(t, h.st.runs, 2)
	assert.Equal(t, store.ChatRetentionStatusOK, h.st.runs[1].Status)
	assert.Empty(t, h.st.runs[1].Error)
}

// TestChatRetentionSweeper_HandlerDeadline: a context deadline shorter than
// the tick budget ends the walk early enough for a last chunk and the
// end-of-tick writes; the run row records the committed counts, and an
// immediate second tick is skipped by the interval gate.
func TestChatRetentionSweeper_HandlerDeadline(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(10000, "p1")
	h.st.purgeCost = func(int) time.Duration { return 300 * time.Millisecond }
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.TickBudgetSeconds = 40 })
	// 10 s left: the walk may run until 10 - 5 (chunk timeout) - 2 (margin)
	// = 3 s of the sweeper's clock.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := h.sw.RunTick(ctx, h.policy(cfg))
	require.NoError(t, err)
	assert.True(t, res.BudgetExhausted)
	assert.Positive(t, res.Chunks)
	assert.LessOrEqual(t, res.Chunks, 4, "300 ms tx + 500 ms pause per chunk in about 3 s")
	require.Len(t, h.st.runs, 1)
	run := h.st.runs[0]
	assert.Equal(t, store.ChatRetentionStatusPartial, run.Status)
	require.NotNil(t, run.FinishedAt)
	assert.Equal(t, res.Purged.Main, run.Counts["p1"].Main)
	assert.Positive(t, run.Counts["p1"].Main)

	h.clock.t = h.clock.t.Add(time.Minute)
	res, err = h.sw.RunTick(context.Background(), h.policy(cfg))
	require.NoError(t, err)
	assert.False(t, res.Ran, "the interval gate applies to the recorded run")
}

// TestChatRetentionSweeper_ContextDoneMidTick: when the tick's context ends
// mid-walk (the handler deadline, or a shutdown), the end-of-tick writes
// still run: the run row is saved failed with the error and the counts of
// the chunks already committed, and the cursor keeps their position.
func TestChatRetentionSweeper_ContextDoneMidTick(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(1000, "p1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.st.afterPurge = func(call int) {
		if call == 2 {
			cancel()
		}
	}

	res, err := h.sw.RunTick(ctx, h.policy(chatRetentionTestConfig(nil)))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, res.Chunks)
	require.Len(t, h.st.runs, 1)
	run := h.st.runs[0]
	assert.Equal(t, store.ChatRetentionStatusFailed, run.Status)
	assert.Contains(t, run.Error, "context canceled")
	require.NotNil(t, run.FinishedAt)
	assert.Equal(t, int64(400), run.Counts["p1"].Main)
	assert.Equal(t, res.Purged.Main, run.Counts["p1"].Main)
	assert.False(t, h.st.cursor.Position.IsStart())
}

// TestChatRetentionSweeper_LastAppliedCutoffMonotonic: lengthening the
// window does not move the last applied cutoff back; shortening it again
// moves it forward.
func TestChatRetentionSweeper_LastAppliedCutoffMonotonic(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedOld(5, "p1")
	p30 := h.policy(chatRetentionTestConfig(nil))
	res, err := h.sw.RunTick(context.Background(), p30)
	require.NoError(t, err)
	require.NotNil(t, res.LastAppliedCutoff)
	assert.True(t, res.LastAppliedCutoff.Equal(p30.MainCutoff))

	h.clock.t = h.clock.t.Add(10 * time.Minute)
	p365 := h.policy(chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.MessagesRetentionDays = 365 }))
	res, err = h.sw.RunTick(context.Background(), p365)
	require.NoError(t, err)
	assert.True(t, res.ReachedCutoff)
	require.NotNil(t, h.st.cursor.LastAppliedCutoff)
	assert.True(t, h.st.cursor.LastAppliedCutoff.Equal(p30.MainCutoff), "a longer window keeps the newer cutoff")
	assert.True(t, res.LastAppliedCutoff.Equal(p30.MainCutoff))

	h.clock.t = h.clock.t.Add(10 * time.Minute)
	p30later := h.policy(chatRetentionTestConfig(nil))
	_, err = h.sw.RunTick(context.Background(), p30later)
	require.NoError(t, err)
	assert.True(t, h.st.cursor.LastAppliedCutoff.Equal(p30later.MainCutoff), "a newer cutoff moves it forward")
}

// TestChatHistoryRetentionMarker_Conditions covers the marker conditions
// that need no store: oldest page only, retention active, a cutoff, and a
// topic created before it; a kept message at or before the cutoff
// suppresses it.
func TestChatHistoryRetentionMarker_Conditions(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	oldTopic := &WebChatTopic{CreatedAt: cutoff.Add(-24 * time.Hour)}
	newTopic := &WebChatTopic{CreatedAt: cutoff.Add(24 * time.Hour)}
	page := func(next string, created ...time.Time) *store.ListResult[store.Message] {
		r := &store.ListResult[store.Message]{NextCursor: next}
		for _, c := range created {
			r.Items = append(r.Items, store.Message{CreatedAt: c})
		}
		return r
	}
	tests := []struct {
		name    string
		active  bool
		cutoff  *time.Time
		result  *store.ListResult[store.Message]
		topic   *WebChatTopic
		convID  string
		wantSet bool
	}{
		{"oldest page, old topic", true, &cutoff, page("", now), oldTopic, "", true},
		{"empty page, old topic", true, &cutoff, page(""), oldTopic, "", true},
		{"more pages", true, &cutoff, page("cursor", now), oldTopic, "", false},
		{"retention inactive", false, &cutoff, page("", now), oldTopic, "", false},
		{"no cutoff yet", true, nil, page("", now), oldTopic, "", false},
		{"topic newer than cutoff", true, &cutoff, page("", now), newTopic, "", false},
		{"kept message before cutoff", true, &cutoff, page("", now, cutoff.Add(-time.Hour)), oldTopic, "", false},
		{"legacy DM without conversation", true, &cutoff, page("", now), nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{}
			srv.SetChatRetentionStore(&fakeChatRetentionStore{clock: &fakeChatRetentionClock{t: now}})
			// Prime the cutoff cache so no store read happens.
			srv.chatRetention.cutoff = tt.cutoff
			srv.chatRetention.cutoffAt = time.Now()
			fake := newFakeHubSettingStore()
			fake.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{"hub.chat_retention":%t}}`, tt.active)))
			fake.seed("chat_retention", json.RawMessage(`{"enabled":true,"messages_retention_days":30}`))
			ops := NewOperationalSettings(fake, emptyKoanf(), emptyKoanf())
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)
			srv.SetOperationalSettings(ops)

			got := srv.chatHistoryRetentionMarker(context.Background(), tt.result, tt.convID, tt.topic)
			if !tt.wantSet {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.True(t, got.PurgedBefore.Equal(cutoff))
		})
	}
}

// TestChatRetentionBudgetEnd: the walk ends at start + budget, or at start +
// the time left before the deadline minus one chunk timeout and the margin,
// whichever is first; both terms are anchored at start.
func TestChatRetentionBudgetEnd(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.TickBudgetSeconds = 40 })
	assert.Equal(t, start.Add(40*time.Second), chatRetentionBudgetEnd(start, 0, false, cfg), "no deadline")
	assert.Equal(t, start.Add(40*time.Second), chatRetentionBudgetEnd(start, 55*time.Second, true, cfg), "55 s handler: the budget governs")
	assert.Equal(t, start.Add(3*time.Second), chatRetentionBudgetEnd(start, 10*time.Second, true, cfg), "10 s left: 10 - 5 - 2")
	assert.True(t, chatRetentionBudgetEnd(start, 5*time.Second, true, cfg).Before(start), "under 7 s left: no chunk")
}
