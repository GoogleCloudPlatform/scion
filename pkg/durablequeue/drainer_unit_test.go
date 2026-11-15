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

package durablequeue_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// memQueueStore is a thread-safe in-memory DurableQueueStore used by fast
// no_sqlite unit tests.
type memQueueStore struct {
	mu        sync.Mutex
	nextSeq   int64
	rows      map[int64]*store.DurableQueueRow
	renews    int
	loseAt    int // when > 0, RenewDurableLease returns held=false starting on this renew call
	trimLimit int // when > 0, caps trimmed rows per TrimDurableQueue pass and sets More
}

func newMemQueueStore() *memQueueStore {
	return &memQueueStore{rows: make(map[int64]*store.DurableQueueRow)}
}

func (m *memQueueStore) EnqueueDurableItem(_ context.Context, _ store.Tx, row store.DurableQueueRow) (int64, error) {
	if err := store.ValidateDurableQueueKind(row.Kind); err != nil {
		return 0, err
	}
	if row.DedupKey == "" {
		row.DedupKey = uuid.NewString()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.rows {
		if existing.Kind == row.Kind && existing.DedupKey == row.DedupKey {
			return 0, store.ErrDuplicate
		}
	}
	m.nextSeq++
	cp := row
	cp.Seq = m.nextSeq
	cp.Payload = bytes.Clone(row.Payload)
	cp.Attempts = 0
	cp.PermanentFailures = 0
	cp.LeaseOwner = ""
	cp.LeaseUntil = nil
	cp.LastError = ""
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	if cp.NotBefore.IsZero() {
		cp.NotBefore = cp.CreatedAt
	}
	if cp.NextAttemptAt.IsZero() {
		cp.NextAttemptAt = cp.NotBefore
	}
	m.rows[cp.Seq] = &cp
	return cp.Seq, nil
}

func (m *memQueueStore) ClaimDurableItems(_ context.Context, kind, owner string, now, leaseUntil time.Time, limit int) ([]store.DurableQueueRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cands []*store.DurableQueueRow
	for _, r := range m.rows {
		if r.Kind != kind || r.NextAttemptAt.After(now) {
			continue
		}
		if r.LeaseUntil != nil && !r.LeaseUntil.Before(now) {
			continue
		}
		cands = append(cands, r)
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].NextAttemptAt.Equal(cands[j].NextAttemptAt) {
			return cands[i].NextAttemptAt.Before(cands[j].NextAttemptAt)
		}
		return cands[i].Seq < cands[j].Seq
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]store.DurableQueueRow, len(cands))
	for i, r := range cands {
		lu := leaseUntil
		r.LeaseOwner = owner
		r.LeaseUntil = &lu
		cp := *r
		cp.Payload = bytes.Clone(r.Payload)
		out[i] = cp
	}
	return out, nil
}

func (m *memQueueStore) RenewDurableLease(_ context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renews++
	r, ok := m.rows[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	if m.loseAt > 0 && m.renews >= m.loseAt {
		r.LeaseOwner = "another-owner"
		return false, nil
	}
	lu := leaseUntil
	r.LeaseUntil = &lu
	return true, nil
}

func (m *memQueueStore) CompleteDurableItem(_ context.Context, seq int64, owner string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	delete(m.rows, seq)
	return true, nil
}

func (m *memQueueStore) FailDurableItem(_ context.Context, seq int64, owner string, f store.DurableQueueFailure) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	if f.CountAttempt {
		r.Attempts++
	}
	if f.Permanent {
		r.PermanentFailures++
	}
	r.NextAttemptAt = f.NextAttemptAt
	r.LastError = f.LastError
	r.LeaseOwner = ""
	r.LeaseUntil = nil
	return true, nil
}

func (m *memQueueStore) TrimDurableQueue(_ context.Context, kind string, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (store.DurableQueueTrim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unleased := func(r *store.DurableQueueRow) bool {
		return r.LeaseUntil == nil || r.LeaseUntil.Before(now)
	}
	var seqs []int64
	for seq := range m.rows {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	metaOnly := func(r *store.DurableQueueRow) store.DurableQueueRow {
		return store.DurableQueueRow{
			Seq:       r.Seq,
			Kind:      r.Kind,
			DedupKey:  r.DedupKey,
			ProjectID: r.ProjectID,
			CreatedAt: r.CreatedAt,
		}
	}

	budget := len(seqs) + 1
	if m.trimLimit > 0 {
		budget = m.trimLimit
	}
	var trim store.DurableQueueTrim
	for _, seq := range seqs {
		r := m.rows[seq]
		if r.Kind != kind || !unleased(r) {
			continue
		}
		if maxAge > 0 && r.CreatedAt.Before(now.Add(-maxAge)) {
			if budget <= 0 {
				trim.More = true
				break
			}
			trim.Age = append(trim.Age, metaOnly(r))
			delete(m.rows, seq)
			budget--
		}
	}
	for _, seq := range seqs {
		r, ok := m.rows[seq]
		if !ok || r.Kind != kind || !unleased(r) {
			continue
		}
		if maxPermanent > 0 && r.PermanentFailures >= maxPermanent {
			if budget <= 0 {
				trim.More = true
				break
			}
			trim.Poison = append(trim.Poison, metaOnly(r))
			delete(m.rows, seq)
			budget--
		}
	}
	_ = maxRows
	return trim, nil
}

func (m *memQueueStore) DurableQueueStats(_ context.Context, kind, projectID string) (store.DurableQueueStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	var st store.DurableQueueStats
	for _, r := range m.rows {
		if r.Kind != kind || (projectID != "" && r.ProjectID != projectID) {
			continue
		}
		st.Pending++
		if r.LeaseUntil != nil && !r.LeaseUntil.Before(now) {
			st.Leased++
		}
		if st.Oldest == nil || r.CreatedAt.Before(*st.Oldest) {
			t := r.CreatedAt
			st.Oldest = &t
		}
	}
	return st, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordingMetrics struct {
	mu          sync.Mutex
	claimed     int
	done        int
	retried     int
	permanent   int
	dropped     map[string]int
	lastPending int64
	lastLeased  int64
	lastOldest  time.Duration
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{dropped: make(map[string]int)}
}

func (m *recordingMetrics) Claimed(_ string, n int) {
	m.mu.Lock()
	m.claimed += n
	m.mu.Unlock()
}

func (m *recordingMetrics) Done(_ string) {
	m.mu.Lock()
	m.done++
	m.mu.Unlock()
}

func (m *recordingMetrics) Retried(_ string) {
	m.mu.Lock()
	m.retried++
	m.mu.Unlock()
}

func (m *recordingMetrics) Permanent(_ string) {
	m.mu.Lock()
	m.permanent++
	m.mu.Unlock()
}

func (m *recordingMetrics) Dropped(_ string, reason string, n int) {
	m.mu.Lock()
	m.dropped[reason] += n
	m.mu.Unlock()
}

func (m *recordingMetrics) ObserveQueue(_ string, pending, leased int64, oldestAge time.Duration) {
	m.mu.Lock()
	m.lastPending = pending
	m.lastLeased = leased
	m.lastOldest = oldestAge
	m.mu.Unlock()
}

func TestNewDrainerValidationAndDefaults(t *testing.T) {
	st := newMemQueueStore()
	nop := func(context.Context, durablequeue.Item, durablequeue.Lease) durablequeue.Result {
		return durablequeue.Result{Done: true}
	}

	_, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{Kind: "test.echo", Handler: nop})
	require.Error(t, err)

	_, err = durablequeue.NewDrainer(durablequeue.DrainerConfig{Store: st, Kind: "BadKind", Handler: nop})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = durablequeue.NewDrainer(durablequeue.DrainerConfig{Store: st, Kind: "test.echo"})
	require.Error(t, err)

	_, err = durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st,
		Kind:           "test.echo",
		Handler:        nop,
		LeaseDuration:  5 * time.Second,
		HandlerTimeout: 5 * time.Second,
	})
	require.Error(t, err)

	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   st,
		Kind:    "test.echo",
		Handler: nop,
		Logger:  quietLogger(),
	})
	require.NoError(t, err)
	assert.Equal(t, "test.echo", d.Kind())
	assert.NotEmpty(t, d.Owner())
}

func TestBackoffSchedule(t *testing.T) {
	assert.Equal(t, time.Second, durablequeue.Backoff(0))
	assert.Equal(t, 2*time.Second, durablequeue.Backoff(1))
	assert.Equal(t, 4*time.Second, durablequeue.Backoff(2))
	assert.Equal(t, 8*time.Second, durablequeue.Backoff(3))
	assert.Equal(t, 5*time.Minute, durablequeue.Backoff(9))
	assert.Equal(t, 5*time.Minute, durablequeue.Backoff(10_000))
}

func TestEnabledGateSkipsClaimAndTrim(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()
	now := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)

	q := durablequeue.NewQueue(st, durablequeue.WithQueueClock(func() time.Time { return now }))
	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "gated-1",
		Payload:   []byte(`{"hello":"world"}`),
		CreatedAt: now.Add(-100 * time.Hour), // would be age-trimmed if drainer ran
		NotBefore: now.Add(-100 * time.Hour),
	})
	require.NoError(t, err)

	var enabled atomic.Bool
	var handled int
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   st,
		Kind:    "test.echo",
		Enabled: enabled.Load,
		MaxAge:  24 * time.Hour,
		Now:     func() time.Time { return now },
		Logger:  quietLogger(),
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handled++
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)
	assert.Zero(t, res.Dropped)
	assert.Equal(t, int64(-1), res.Remaining)
	assert.Zero(t, handled)

	stats, err := st.DurableQueueStats(ctx, "test.echo", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Pending, "queued item must wait untouched while Enabled is false")

	// Enable with a fresh MaxAge so it handles rather than age-trims.
	enabled.Store(true)
	d2, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   st,
		Kind:    "test.echo",
		Enabled: enabled.Load,
		MaxAge:  200 * time.Hour,
		Now:     func() time.Time { return now },
		Logger:  quietLogger(),
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handled++
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)
	res, err = d2.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Done)
	assert.Equal(t, 1, handled)
}

func TestLeaseAutoRenewAndLostLeaseCancel(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()
	now := time.Now().UTC()
	q := durablequeue.NewQueue(st)

	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "auto-renew",
		Payload:   []byte(`{}`),
		CreatedAt: now,
	})
	require.NoError(t, err)

	// LeaseDuration=90ms -> auto-renew every 30ms; handler waits ~75ms so >=2 auto-renews fire.
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st,
		Kind:           "test.echo",
		LeaseDuration:  90 * time.Millisecond,
		HandlerTimeout: 85 * time.Millisecond,
		Logger:         quietLogger(),
		Handler: func(hctx context.Context, _ durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
			held, rerr := lease.Renew(hctx)
			if rerr != nil || !held {
				return durablequeue.Result{Permanent: true, Err: errors.New("manual renew failed")}
			}
			time.Sleep(70 * time.Millisecond)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Done)

	st.mu.Lock()
	renewCount := st.renews
	st.mu.Unlock()
	assert.GreaterOrEqual(t, renewCount, 3, "initial renew + manual renew + background LeaseDuration/3 auto-renew")

	// Now simulate losing the lease on the first background auto-renew tick:
	// the handler context must be cancelled and Done must NOT delete the item.
	st2 := newMemQueueStore()
	st2.loseAt = 2 // 1st renew (pre-handler) succeeds; 2nd renew (auto-renew tick) loses lease
	q2 := durablequeue.NewQueue(st2)
	_, err = q2.Enqueue(ctx, nil, durablequeue.Item{Kind: "test.echo", DedupKey: "lost", Payload: []byte(`{}`)})
	require.NoError(t, err)

	var sawCtxCancel atomic.Bool
	dLost, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st2,
		Kind:           "test.echo",
		LeaseDuration:  60 * time.Millisecond,
		HandlerTimeout: 55 * time.Millisecond,
		Logger:         quietLogger(),
		Handler: func(hctx context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			<-hctx.Done()
			sawCtxCancel.Store(true)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)
	resLost, err := dLost.DrainOnce(ctx)
	require.NoError(t, err)
	assert.True(t, sawCtxCancel.Load())
	assert.Zero(t, resLost.Done, "lost lease must not count as Done")
}

func TestLostLeaseOnFailureSkipsMetricsAndClaimNonceIsolatesSameOwner(t *testing.T) {
	ctx := context.Background()

	// 1. O1: losing a lease before FailDurableItem skips Retried/Permanent metrics
	// and must not trigger immediate poison trim.
	st := newMemQueueStore()
	st.loseAt = 2 // pre-handler Renew succeeds; background auto-renew loses lease
	q := durablequeue.NewQueue(st)
	_, err := q.Enqueue(ctx, nil, durablequeue.Item{Kind: "test.echo", DedupKey: "fail-lost", Payload: []byte(`{}`)})
	require.NoError(t, err)

	metrics := newRecordingMetrics()
	var dropCalls int
	dFailLost, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st,
		Kind:           "test.echo",
		MaxPermanent:   1,
		LeaseDuration:  60 * time.Millisecond,
		HandlerTimeout: 55 * time.Millisecond,
		Metrics:        metrics,
		Logger:         quietLogger(),
		OnDrop: func(_ context.Context, _ []durablequeue.Item, _ string) {
			dropCalls++
		},
		Handler: func(hctx context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			<-hctx.Done()
			return durablequeue.Result{Permanent: true, Err: errors.New("lost lease permanent failure")}
		},
	})
	require.NoError(t, err)

	res, err := dFailLost.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Permanent, "lost lease failure must not count as Permanent")
	assert.Zero(t, res.Retried, "lost lease failure must not count as Retried")
	assert.Zero(t, res.Dropped, "lost lease failure must not trigger poison trim")
	assert.Zero(t, dropCalls)
	metrics.mu.Lock()
	assert.Zero(t, metrics.permanent)
	assert.Zero(t, metrics.retried)
	metrics.mu.Unlock()

	// 2. C2: two drainers configured with the exact same Owner string use distinct
	// per-claim lease nonces, so a stalled pass on d1 cannot release or overwrite
	// a lease re-claimed by d2 while d2 is actively handling the item.
	st2 := newMemQueueStore()
	cur := time.Date(2026, 11, 15, 6, 30, 0, 0, time.UTC)
	var clockMu sync.Mutex
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	q2 := durablequeue.NewQueue(st2, durablequeue.WithQueueClock(nowFn))
	seq, err := q2.Enqueue(ctx, nil, durablequeue.Item{Kind: "test.echo", DedupKey: "same-owner", Payload: []byte(`{}`)})
	require.NoError(t, err)

	d1Entered := make(chan struct{})
	releaseD1 := make(chan struct{})
	d2Entered := make(chan struct{})
	releaseD2 := make(chan struct{})

	d1, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st2,
		Kind:           "test.echo",
		Owner:          "shared-replica-id",
		LeaseDuration:  5 * time.Second,
		HandlerTimeout: 4 * time.Second,
		Now:            nowFn,
		Logger:         quietLogger(),
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			close(d1Entered)
			<-releaseD1
			return durablequeue.Result{Err: errors.New("stale failure from d1")}
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "shared-replica-id", d1.Owner())

	d2, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st2,
		Kind:           "test.echo",
		Owner:          "shared-replica-id",
		LeaseDuration:  5 * time.Second,
		HandlerTimeout: 4 * time.Second,
		Now:            nowFn,
		Logger:         quietLogger(),
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			close(d2Entered)
			<-releaseD2
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	var r1, r2 durablequeue.DrainResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var derr error
		r1, derr = d1.DrainOnce(ctx)
		require.NoError(t, derr)
	}()

	<-d1Entered
	// Advance fake clock past d1's 5s lease so d2 re-claims the item with the same configured Owner.
	advance(6 * time.Second)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var derr error
		r2, derr = d2.DrainOnce(ctx)
		require.NoError(t, derr)
	}()

	<-d2Entered
	// Let stale d1 return a failure WHILE d2 holds the new lease: d1's FailDurableItem
	// must be rejected (different claim nonce), leaving d2's lease intact so d2's Done succeeds!
	close(releaseD1)
	time.Sleep(15 * time.Millisecond)

	st2.mu.Lock()
	liveRow := st2.rows[seq]
	require.NotNil(t, liveRow, "row must still exist while d2 is handling it")
	assert.NotNil(t, liveRow.LeaseUntil, "stale d1 Fail must not clear d2's lease")
	assert.Zero(t, liveRow.Attempts, "stale d1 Fail must not increment attempts")
	st2.mu.Unlock()

	close(releaseD2)
	wg.Wait()

	assert.Zero(t, r1.Retried)
	assert.Equal(t, 1, r2.Done)
}

func TestBlockedSinkProbeBackoffAndUnblock(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()
	cur := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	q := durablequeue.NewQueue(st, durablequeue.WithQueueClock(nowFn))
	for i := 0; i < 3; i++ {
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: fmt.Sprintf("b-%d", i),
			Payload:  []byte(`{}`),
		})
		require.NoError(t, err)
	}

	var sinkBlocked atomic.Bool
	sinkBlocked.Store(true)
	var handlerCalls int

	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:     st,
		Kind:      "test.echo",
		BatchSize: 10,
		Now:       nowFn,
		Logger:    quietLogger(),
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handlerCalls++
			if sinkBlocked.Load() {
				return durablequeue.Result{Err: fmt.Errorf("downstream down: %w", durablequeue.ErrSinkBlocked)}
			}
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	// Pass 1 claims 3 items, blocks on the 1st, and releases items 2 & 3 without counting attempts.
	r1, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, r1.Claimed)
	assert.True(t, r1.Blocked)
	assert.Equal(t, 1, handlerCalls)
	assert.True(t, d.Blocked())

	// Before the 1 s probe backoff elapses, DrainOnce claims nothing.
	advance(500 * time.Millisecond)
	r2, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, r2.Claimed)

	// At +1 s, a single-item probe runs; still blocked -> next probe in 2 s.
	advance(600 * time.Millisecond)
	r3, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, r3.Claimed, "blocked sink must probe with batch size 1")
	assert.True(t, r3.Blocked)

	// Unblock sink; advance 2 s -> probe succeeds (1 item), drainer unblocks, next pass drains the remaining 2!
	sinkBlocked.Store(false)
	advance(2 * time.Second)
	r4, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, r4.Claimed)
	assert.Equal(t, 1, r4.Done)
	assert.False(t, d.Blocked())

	r5, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, r5.Done)
	assert.Zero(t, r5.Remaining)
}

func TestHandlerPanicCountsTowardPoisonAndDrops(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()
	cur := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(st, durablequeue.WithQueueClock(func() time.Time { return cur }))

	seq, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "panicker",
		ProjectID: "proj-1",
		Payload:   []byte(`{"bad":true}`),
	})
	require.NoError(t, err)

	var droppedReasons []string
	var droppedSeqs []int64
	var calls int

	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        st,
		Kind:         "test.echo",
		MaxPermanent: 3,
		Now:          func() time.Time { return cur },
		Logger:       quietLogger(),
		OnDrop: func(_ context.Context, dropped []durablequeue.Item, reason string) {
			droppedReasons = append(droppedReasons, reason)
			for _, it := range dropped {
				droppedSeqs = append(droppedSeqs, it.Seq)
			}
		},
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			calls++
			panic("kaboom")
		},
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		res, err := d.DrainOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, res.Permanent)
		cur = cur.Add(10 * time.Second)
	}
	assert.Equal(t, 3, calls)
	assert.Equal(t, []string{durablequeue.DropReasonPoison}, droppedReasons)
	assert.Equal(t, []int64{seq}, droppedSeqs)

	// Subsequent pass sees nothing left and never retries a 4th time.
	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)
	assert.Equal(t, 3, calls)
}

func TestQueueWakeAndRunShutdownFinalPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := newMemQueueStore()
	q := durablequeue.NewQueue(st)

	// Calling Wake with no drainers or repeatedly never blocks.
	for i := 0; i < 50; i++ {
		q.Wake("test.echo")
		q.Wake("other.kind")
	}

	handled := make(chan string, 10)
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        st,
		Queue:        q,
		Kind:         "test.echo",
		PollInterval: time.Hour, // relies on Wake and final shutdown pass
		Logger:       quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handled <- item.DedupKey
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	doneRun := make(chan struct{})
	go func() {
		defer close(doneRun)
		d.Run(ctx)
	}()

	// Enqueue and Wake("test.echo"): delivered without waiting for the 1h PollInterval.
	_, err = q.Enqueue(context.Background(), nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "woken-1",
		Payload:  []byte(`{}`),
	})
	require.NoError(t, err)
	q.Wake("other.kind") // unrelated kind does not wake test.echo
	q.Wake("test.echo")

	select {
	case got := <-handled:
		assert.Equal(t, "woken-1", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for woken drainer")
	}

	// Enqueue one more item WITHOUT calling Wake, then cancel ctx: Run's bounded
	// final pass on shutdown must drain it before Run returns!
	_, err = q.Enqueue(context.Background(), nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "shutdown-final",
		Payload:  []byte(`{}`),
	})
	require.NoError(t, err)

	cancel()
	select {
	case <-doneRun:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run shutdown")
	}
	select {
	case got := <-handled:
		assert.Equal(t, "shutdown-final", got)
	default:
		t.Fatal("expected bounded final shutdown pass to drain queued item")
	}
}

func TestDrainerRunContinuesBoundedTrimAcrossPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := newMemQueueStore()
	st.trimLimit = 3
	now := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
	q := durablequeue.NewQueue(st, durablequeue.WithQueueClock(func() time.Time { return now }))

	for i := 0; i < 10; i++ {
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      "test.echo",
			DedupKey:  fmt.Sprintf("aged-%02d", i),
			ProjectID: "proj-a",
			Payload:   []byte(`{"large":"payload"}`),
			CreatedAt: now.Add(-100 * time.Hour),
			NotBefore: now.Add(time.Hour), // not claim-due, only trim-eligible
		})
		require.NoError(t, err)
	}

	var (
		dropMu      sync.Mutex
		dropBatches []int
		droppedKeys []string
	)
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:               st,
		Kind:                "test.echo",
		MaxAge:              72 * time.Hour,
		PollInterval:        time.Hour, // only trim.More self-wake can advance passes promptly
		MaintenanceInterval: time.Hour,
		Now:                 func() time.Time { return now },
		Logger:              quietLogger(),
		OnDrop: func(_ context.Context, dropped []durablequeue.Item, reason string) {
			dropMu.Lock()
			defer dropMu.Unlock()
			assert.Equal(t, durablequeue.DropReasonAge, reason)
			dropBatches = append(dropBatches, len(dropped))
			for _, it := range dropped {
				assert.Nil(t, it.Payload)
				droppedKeys = append(droppedKeys, it.DedupKey)
			}
		},
		Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		dropMu.Lock()
		defer dropMu.Unlock()
		return len(droppedKeys) == 10
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	<-done

	dropMu.Lock()
	defer dropMu.Unlock()
	assert.Equal(t, []int{3, 3, 3, 1}, dropBatches)
}
