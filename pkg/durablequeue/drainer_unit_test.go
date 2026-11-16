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
	"strings"
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
	claims    int
	renews    int
	loseAt    int // when > 0, RenewDurableLease returns held=false starting on this renew call
	renewHook func(ctx context.Context, call int) error
	trimLimit int // when > 0, caps trimmed rows per TrimDurableQueue pass and sets More
}

var _ store.DurableQueueStore = (*memQueueStore)(nil)

func newMemQueueStore() *memQueueStore {
	return &memQueueStore{rows: make(map[int64]*store.DurableQueueRow)}
}

func (m *memQueueStore) claimCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claims
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
	m.claims++
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

func (m *memQueueStore) RenewDurableLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error) {
	m.mu.Lock()
	m.renews++
	call := m.renews
	hook := m.renewHook
	m.mu.Unlock()

	if hook != nil {
		if err := hook(ctx, call); err != nil {
			return false, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	if m.loseAt > 0 && call >= m.loseAt {
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

func (m *memQueueStore) DurableQueueStats(_ context.Context, kind, projectID string, now time.Time) (store.DurableQueueStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now = now.UTC()
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

func (m *memQueueStore) NudgeDurableItems(_ context.Context, kind string, dedupKeys []string, now time.Time) (int, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return 0, err
	}
	if len(dedupKeys) > store.MaxDurableQueueNudgeKeys {
		return 0, fmt.Errorf("%w: durable queue nudge key count %d exceeds %d", store.ErrInvalidInput, len(dedupKeys), store.MaxDurableQueueNudgeKeys)
	}
	if len(dedupKeys) == 0 {
		return 0, nil
	}
	wanted := make(map[string]struct{}, len(dedupKeys))
	for _, k := range dedupKeys {
		if store.IsNudgeableDedupKey(k) {
			wanted[k] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	nudged := 0
	for _, r := range m.rows {
		if r.Kind != kind {
			continue
		}
		if _, ok := wanted[r.DedupKey]; !ok {
			continue
		}
		if r.LeaseUntil != nil && !r.LeaseUntil.Before(now) {
			continue
		}
		if r.NextAttemptAt.After(now) {
			r.NextAttemptAt = now
			nudged++
		}
	}
	return nudged, nil
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

	stats, err := st.DurableQueueStats(ctx, "test.echo", "", now)
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

func TestQueueWakeAndRunShutdownInHandOnly(t *testing.T) {
	t.Run("wake_and_no_claim_after_ctx_done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

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
			PollInterval: time.Hour, // relies on Wake
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

		// Wait for the initial startup pass so Run is parked waiting on Wake/ctx.
		require.Eventually(t, func() bool {
			return st.claimCount() >= 1
		}, 2*time.Second, 5*time.Millisecond)

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
		require.Eventually(t, func() bool {
			stats, serr := st.DurableQueueStats(context.Background(), "test.echo", "", time.Now().UTC())
			return serr == nil && stats.Pending == 0
		}, 2*time.Second, 5*time.Millisecond)

		// Enqueue an unclaimed item WITHOUT waking, then cancel ctx: Run must
		// NOT execute any new ClaimDurableItems call on shutdown.
		claimsBeforeCancel := st.claimCount()
		_, err = q.Enqueue(context.Background(), nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: "unclaimed-on-shutdown",
			Payload:  []byte(`{}`),
		})
		require.NoError(t, err)

		cancel()
		select {
		case <-doneRun:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Run shutdown")
		}

		assert.Equal(t, claimsBeforeCancel, st.claimCount(), "Run must claim nothing new after ctx is done")
		select {
		case unexpected := <-handled:
			t.Fatalf("unexpected post-shutdown handling of %q", unexpected)
		default:
		}
		stats, err := st.DurableQueueStats(context.Background(), "test.echo", "", time.Now().UTC())
		require.NoError(t, err)
		assert.Equal(t, int64(1), stats.Pending, "unclaimed item must remain queued for next start or another replica")
		assert.Zero(t, stats.Leased)

		// Starting Run with an already-cancelled ctx also claims nothing.
		dCancelled, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:  st,
			Kind:   "test.echo",
			Logger: quietLogger(),
			Handler: func(_ context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				t.Error("handler must not be invoked when Run starts with cancelled ctx")
				return durablequeue.Result{Done: true}
			},
		})
		require.NoError(t, err)
		dCancelled.Run(ctx)
		assert.Equal(t, claimsBeforeCancel, st.claimCount())
	})

	t.Run("in_hand_finishes_within_budget_and_releases_unstarted_tail", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		st := newMemQueueStore()
		q := durablequeue.NewQueue(st)

		for _, key := range []string{"in-hand-1", "in-hand-2"} {
			_, err := q.Enqueue(context.Background(), nil, durablequeue.Item{
				Kind:     "test.echo",
				DedupKey: key,
				Payload:  []byte(`{}`),
			})
			require.NoError(t, err)
		}

		started1 := make(chan struct{})
		proceed1 := make(chan struct{})
		var (
			mu                  sync.Mutex
			handledKeys         []string
			handlerCtxCancelled bool
		)

		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:          st,
			Queue:          q,
			Kind:           "test.echo",
			BatchSize:      2,
			PollInterval:   time.Hour,
			LeaseDuration:  5 * time.Second,
			HandlerTimeout: 4 * time.Second,
			ShutdownBudget: 500 * time.Millisecond,
			Logger:         quietLogger(),
			Handler: func(hctx context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				if item.DedupKey == "in-hand-1" {
					close(started1)
					<-proceed1
					mu.Lock()
					handlerCtxCancelled = hctx.Err() != nil
					handledKeys = append(handledKeys, item.DedupKey)
					mu.Unlock()
					return durablequeue.Result{Done: true}
				}
				mu.Lock()
				handledKeys = append(handledKeys, item.DedupKey)
				mu.Unlock()
				return durablequeue.Result{Done: true}
			},
		})
		require.NoError(t, err)

		doneRun := make(chan struct{})
		go func() {
			defer close(doneRun)
			d.Run(ctx)
		}()

		// Wait until the 2-item batch is claimed and in-hand-1 is mid-handler.
		<-started1
		assert.Equal(t, 1, st.claimCount())

		// Enqueue a 3rd item and buffer a Wake before cancelling ctx: Run must
		// still not claim a second batch once ctx is done.
		_, err = q.Enqueue(context.Background(), nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: "queued-after-claim",
			Payload:  []byte(`{}`),
		})
		require.NoError(t, err)
		q.Wake("test.echo")

		cancel()
		// Give AfterFunc a moment to arm the ShutdownBudget timer while in-hand-1
		// is still running; its hctx must stay live within the 500ms budget.
		time.Sleep(25 * time.Millisecond)
		close(proceed1)

		select {
		case <-doneRun:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Run to finish in-hand work")
		}

		assert.Equal(t, 1, st.claimCount(), "no new claim may happen after ctx is cancelled")
		mu.Lock()
		assert.False(t, handlerCtxCancelled, "in-flight handler context must stay live within ShutdownBudget")
		assert.Equal(t, []string{"in-hand-1"}, handledKeys, "unstarted batch tail must be released rather than started under a shutting-down drainer")
		mu.Unlock()

		stats, err := st.DurableQueueStats(context.Background(), "test.echo", "", time.Now().UTC())
		require.NoError(t, err)
		assert.Equal(t, int64(2), stats.Pending, "in-hand-2 and queued-after-claim must remain pending")
		assert.Zero(t, stats.Leased, "in-hand-2 lease must be released before Run returns")

		st.mu.Lock()
		for _, r := range st.rows {
			assert.Zero(t, r.Attempts, "released shutdown row %q must not burn an attempt", r.DedupKey)
			assert.Empty(t, r.LeaseOwner)
			assert.Nil(t, r.LeaseUntil)
		}
		st.mu.Unlock()
	})

	t.Run("in_hand_exceeding_shutdown_budget_is_cancelled_and_released", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		st := newMemQueueStore()
		q := durablequeue.NewQueue(st)
		metrics := newRecordingMetrics()

		for _, key := range []string{"slow-1", "slow-2"} {
			_, err := q.Enqueue(context.Background(), nil, durablequeue.Item{
				Kind:     "test.echo",
				DedupKey: key,
				Payload:  []byte(`{}`),
			})
			require.NoError(t, err)
		}

		startedSlow := make(chan struct{})
		var (
			handlerCalls atomic.Int32
			sawCtxErr    atomic.Value
		)

		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:          st,
			Queue:          q,
			Kind:           "test.echo",
			BatchSize:      2,
			PollInterval:   time.Hour,
			LeaseDuration:  10 * time.Second,
			HandlerTimeout: 5 * time.Second, // much longer than ShutdownBudget
			ShutdownBudget: 60 * time.Millisecond,
			Metrics:        metrics,
			Logger:         quietLogger(),
			Handler: func(hctx context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				if handlerCalls.Add(1) == 1 && item.DedupKey == "slow-1" {
					close(startedSlow)
				}
				<-hctx.Done()
				sawCtxErr.Store(hctx.Err())
				return durablequeue.Result{Err: hctx.Err()}
			},
		})
		require.NoError(t, err)

		doneRun := make(chan struct{})
		go func() {
			defer close(doneRun)
			d.Run(ctx)
		}()

		<-startedSlow
		leasedDuring, err := st.DurableQueueStats(context.Background(), "test.echo", "", time.Now().UTC())
		require.NoError(t, err)
		assert.Equal(t, int64(2), leasedDuring.Leased)

		tCancel := time.Now()
		cancel()

		select {
		case <-doneRun:
		case <-time.After(time.Second):
			t.Fatal("Run did not exit within ShutdownBudget")
		}
		elapsed := time.Since(tCancel)
		assert.GreaterOrEqual(t, elapsed, 45*time.Millisecond, "in-flight handler should receive ShutdownBudget grace")
		assert.Less(t, elapsed, 750*time.Millisecond, "ShutdownBudget (not 5s HandlerTimeout) must bound shutdown")

		assert.Equal(t, 1, st.claimCount())
		assert.Equal(t, int32(1), handlerCalls.Load(), "second item in batch must be released without starting")
		assert.ErrorIs(t, sawCtxErr.Load().(error), context.Canceled)

		stats, err := st.DurableQueueStats(context.Background(), "test.echo", "", time.Now().UTC())
		require.NoError(t, err)
		assert.Equal(t, int64(2), stats.Pending)
		assert.Zero(t, stats.Leased, "both cut-off and unstarted items must have their leases released")

		st.mu.Lock()
		for _, r := range st.rows {
			assert.Zero(t, r.Attempts, "shutdown cut-off must release %q without counting an attempt", r.DedupKey)
			assert.Zero(t, r.PermanentFailures)
			assert.Empty(t, r.LeaseOwner)
			assert.Nil(t, r.LeaseUntil)
		}
		st.mu.Unlock()

		metrics.mu.Lock()
		assert.Zero(t, metrics.done)
		assert.Zero(t, metrics.retried)
		assert.Zero(t, metrics.permanent)
		metrics.mu.Unlock()
	})
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

func TestPreHandleRenewSkipsReclaimedBatchTail(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()

	cur := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
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
	for _, key := range []string{"batch-1", "batch-2"} {
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: key,
			Payload:  []byte(`{}`),
		})
		require.NoError(t, err)
	}

	d2ClaimedItem2 := make(chan struct{})
	releaseD2 := make(chan struct{})
	var (
		mu        sync.Mutex
		d1Handled []string
		d2Handled []string
		r2        durablequeue.DrainResult
		d2Wg      sync.WaitGroup
	)

	d2, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st,
		Kind:           "test.echo",
		Owner:          "replica-2",
		BatchSize:      2,
		LeaseDuration:  5 * time.Second,
		HandlerTimeout: 4 * time.Second,
		Now:            nowFn,
		Logger:         quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			mu.Lock()
			d2Handled = append(d2Handled, item.DedupKey)
			mu.Unlock()
			close(d2ClaimedItem2)
			<-releaseD2
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	d1, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          st,
		Kind:           "test.echo",
		Owner:          "replica-1",
		BatchSize:      2,
		LeaseDuration:  5 * time.Second,
		HandlerTimeout: 4 * time.Second,
		Now:            nowFn,
		Logger:         quietLogger(),
		Handler: func(hctx context.Context, item durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
			mu.Lock()
			d1Handled = append(d1Handled, item.DedupKey)
			mu.Unlock()
			if item.DedupKey == "batch-1" {
				// Extend batch-1's lease at +4s (to +9s) while batch-2's unrenewed
				// claim lease remains at +5s, then advance to +6s so batch-2's
				// lease expires and replica-2 re-claims batch-2 before batch-1 returns.
				advance(4 * time.Second)
				held, rerr := lease.Renew(hctx)
				require.NoError(t, rerr)
				require.True(t, held)
				advance(2 * time.Second)

				d2Wg.Add(1)
				go func() {
					defer d2Wg.Done()
					var derr error
					r2, derr = d2.DrainOnce(ctx)
					require.NoError(t, derr)
				}()
				<-d2ClaimedItem2
			}
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	r1, err := d1.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, r1.Claimed)
	assert.Equal(t, 1, r1.Done)

	close(releaseD2)
	d2Wg.Wait()
	assert.Equal(t, 1, r2.Claimed)
	assert.Equal(t, 1, r2.Done)

	mu.Lock()
	assert.Equal(t, []string{"batch-1"}, d1Handled, "d1 must skip invoking Handler on batch-2 once its pre-handle lease renew fails")
	assert.Equal(t, []string{"batch-2"}, d2Handled)
	mu.Unlock()
}

type recordedLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type recordingLogHandler struct {
	mu      sync.Mutex
	records []recordedLog
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	rec := recordedLog{
		level: r.Level,
		msg:   r.Message,
		attrs: make(map[string]any, r.NumAttrs()),
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Resolve().Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingLogHandler) snapshot() []recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedLog, len(h.records))
	copy(out, h.records)
	return out
}

func TestLeaseAutoRenewLogsErrorsWithoutCancelingAndSkipsContextErrors(t *testing.T) {
	ctx := context.Background()
	const secretToken = "do-not-log-secret-token-xyz"
	secretPayload := []byte(`{"secret":"` + secretToken + `"}`)

	t.Run("transient errors logged once per failure without canceling handler", func(t *testing.T) {
		st := newMemQueueStore()
		errReset := errors.New("db connection reset")
		errTimeout := errors.New("db write timeout")
		ticksDone := make(chan struct{})
		st.renewHook = func(_ context.Context, call int) error {
			switch call {
			case 1:
				return nil // pre-handle renew succeeds
			case 2:
				return errReset
			case 3:
				close(ticksDone)
				return errTimeout
			default:
				return nil
			}
		}

		q := durablequeue.NewQueue(st)
		seq, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: "renew-err-transient",
			Payload:  secretPayload,
		})
		require.NoError(t, err)

		lh := &recordingLogHandler{}
		var handlerCtxErr error
		// LeaseDuration=600ms -> auto-renew every 200ms; 2 ticks fire around 400ms (< 550ms HandlerTimeout).
		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:          st,
			Kind:           "test.echo",
			LeaseDuration:  600 * time.Millisecond,
			HandlerTimeout: 550 * time.Millisecond,
			Logger:         slog.New(lh),
			Handler: func(hctx context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				select {
				case <-ticksDone:
				case <-hctx.Done():
				}
				handlerCtxErr = hctx.Err()
				return durablequeue.Result{Done: true}
			},
		})
		require.NoError(t, err)

		res, err := d.DrainOnce(ctx)
		require.NoError(t, err)
		assert.NoError(t, handlerCtxErr, "transient renew errors must not cancel the handler")
		assert.Equal(t, 1, res.Done)

		logs := lh.snapshot()
		require.Len(t, logs, 2, "expected exactly one Warn log per transient renew failure")
		assert.Equal(t, slog.LevelWarn, logs[0].level)
		assert.Equal(t, "durable queue lease auto-renew failed", logs[0].msg)
		assert.Equal(t, map[string]any{"kind": "test.echo", "seq": seq, "error": errReset}, logs[0].attrs)

		assert.Equal(t, slog.LevelWarn, logs[1].level)
		assert.Equal(t, "durable queue lease auto-renew failed", logs[1].msg)
		assert.Equal(t, map[string]any{"kind": "test.echo", "seq": seq, "error": errTimeout}, logs[1].attrs)

		assert.NotContains(t, fmt.Sprintf("%+v", logs), secretToken, "payload secrets must never appear in logs")
	})

	t.Run("context canceled and deadline exceeded renew errors are not logged", func(t *testing.T) {
		st := newMemQueueStore()
		ticksDone := make(chan struct{})
		st.renewHook = func(_ context.Context, call int) error {
			switch call {
			case 1:
				return nil
			case 2:
				return fmt.Errorf("driver: %w", context.Canceled)
			case 3:
				close(ticksDone)
				return fmt.Errorf("driver: %w", context.DeadlineExceeded)
			default:
				return nil
			}
		}

		q := durablequeue.NewQueue(st)
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: "renew-err-ctx",
			Payload:  secretPayload,
		})
		require.NoError(t, err)

		lh := &recordingLogHandler{}
		var handlerCtxErr error
		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:          st,
			Kind:           "test.echo",
			LeaseDuration:  600 * time.Millisecond,
			HandlerTimeout: 550 * time.Millisecond,
			Logger:         slog.New(lh),
			Handler: func(hctx context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				select {
				case <-ticksDone:
				case <-hctx.Done():
				}
				handlerCtxErr = hctx.Err()
				return durablequeue.Result{Done: true}
			},
		})
		require.NoError(t, err)

		res, err := d.DrainOnce(ctx)
		require.NoError(t, err)
		assert.NoError(t, handlerCtxErr)
		assert.Equal(t, 1, res.Done)
		assert.Empty(t, lh.snapshot(), "context.Canceled and context.DeadlineExceeded renew errors must not be logged")
	})

	t.Run("in-flight renew canceled on handler return is not logged", func(t *testing.T) {
		st := newMemQueueStore()
		inRenew := make(chan struct{})
		st.renewHook = func(rctx context.Context, call int) error {
			if call == 2 {
				close(inRenew)
				<-rctx.Done()
				return rctx.Err()
			}
			return nil
		}

		q := durablequeue.NewQueue(st)
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:     "test.echo",
			DedupKey: "renew-inflight-cancel",
			Payload:  secretPayload,
		})
		require.NoError(t, err)

		lh := &recordingLogHandler{}
		var handlerCtxErr error
		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:          st,
			Kind:           "test.echo",
			LeaseDuration:  600 * time.Millisecond,
			HandlerTimeout: 550 * time.Millisecond,
			Logger:         slog.New(lh),
			Handler: func(hctx context.Context, _ durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
				select {
				case <-inRenew:
				case <-hctx.Done():
				}
				handlerCtxErr = hctx.Err()
				return durablequeue.Result{Done: true}
			},
		})
		require.NoError(t, err)

		res, err := d.DrainOnce(ctx)
		require.NoError(t, err)
		assert.NoError(t, handlerCtxErr)
		assert.Equal(t, 1, res.Done)
		assert.True(t, !strings.Contains(fmt.Sprintf("%+v", lh.snapshot()), secretToken))
		assert.Empty(t, lh.snapshot(), "in-flight renew aborted by stopRenew must not be logged")
	})
}

func TestQueueNudge_SemanticsAndLocalWake(t *testing.T) {
	ctx := context.Background()

	// Nil receiver / nil store guards.
	var nilQueue *durablequeue.StoreQueue
	_, err := nilQueue.Nudge(ctx, "test.echo", []string{"k1"})
	require.Error(t, err)
	_, err = durablequeue.NewQueue(nil).Nudge(ctx, "test.echo", []string{"k1"})
	require.Error(t, err)

	st := newMemQueueStore()
	cur := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	setNow := func(ts time.Time) {
		clockMu.Lock()
		cur = ts
		clockMu.Unlock()
	}

	q := durablequeue.NewQueue(st, durablequeue.WithQueueClock(nowFn))

	// Validation: bad kind and oversized list fail with store.ErrInvalidInput.
	_, err = q.Nudge(ctx, "Bad-Kind", []string{"k1"})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	tooMany := make([]string, store.MaxDurableQueueNudgeKeys+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("k-%d", i)
	}
	_, err = q.Nudge(ctx, "test.echo", tooMany)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// Empty key list is a no-op.
	n, err := q.Nudge(ctx, "test.echo", nil)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Seed:
	// - leased item of test.echo (must stay untouched)
	// - two future items of test.echo (nudge-a seq lower @ +10m, nudge-b seq higher @ +5m)
	// - one already-due-earlier item of test.echo (@ +10s when nudged at +20s -> must stay ahead of nudge-a/b)
	// - one unselected key of test.echo (@ +10m -> untouched)
	// - one item of other.echo with key "nudge-a" (@ +10m -> untouched)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "leased",
		Payload:   []byte("leased"),
		NotBefore: cur,
	})
	require.NoError(t, err)
	leasedRows, err := st.ClaimDurableItems(ctx, "test.echo", "holder", cur, cur.Add(time.Hour), 1)
	require.NoError(t, err)
	require.Len(t, leasedRows, 1)

	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "nudge-a",
		Payload:   []byte("nudge-a"),
		NotBefore: cur.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "nudge-b",
		Payload:   []byte("nudge-b"),
		NotBefore: cur.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "already-due",
		Payload:   []byte("already-due"),
		NotBefore: cur.Add(10 * time.Second),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "other-key",
		Payload:   []byte("other-key"),
		NotBefore: cur.Add(10 * time.Minute),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "other.echo",
		DedupKey:  "nudge-a",
		Payload:   []byte("other-kind"),
		NotBefore: cur.Add(10 * time.Minute),
	})
	require.NoError(t, err)

	// Start a live drainer with a 1-hour PollInterval so only Wake from q.Nudge
	// can trigger a poll after the initial startup pass (at t=cur, where nothing unleased is due).
	initialClaims := st.claimCount()
	var (
		handledMu sync.Mutex
		handled   []string
	)
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        st,
		Queue:        q,
		Kind:         "test.echo",
		PollInterval: time.Hour,
		Now:          nowFn,
		Logger:       quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handledMu.Lock()
			handled = append(handled, item.DedupKey)
			handledMu.Unlock()
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		d.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-doneCh
	})

	// Wait for initial pass at t=cur (claims nothing because leased is held and others are > cur).
	require.Eventually(t, func() bool {
		return st.claimCount() > initialClaims
	}, 2*time.Second, 5*time.Millisecond)
	handledMu.Lock()
	assert.Empty(t, handled)
	handledMu.Unlock()

	// Advance clock to cur + 20s ("already-due" @ +10s is now past due; nudge-a/b are still +10m/+5m).
	// Call q.Nudge: must update only nudge-a and nudge-b (n=2), leave already-due at +10s, ignore
	// leased and unknown keys, and Wake the local drainer so all 3 drain immediately in
	// (next_attempt_at, seq) order: already-due, nudge-a, nudge-b!
	setNow(cur.Add(20 * time.Second))
	nudged, err := q.Nudge(ctx, "test.echo", []string{"leased", "nudge-b", "nudge-a", "already-due", "unknown-key"})
	require.NoError(t, err)
	assert.Equal(t, 2, nudged)

	require.Eventually(t, func() bool {
		handledMu.Lock()
		defer handledMu.Unlock()
		return len(handled) == 3
	}, 2*time.Second, 5*time.Millisecond)

	handledMu.Lock()
	assert.Equal(t, []string{"already-due", "nudge-a", "nudge-b"}, handled)
	handledMu.Unlock()

	// Peer-replica wake check: when another replica already lowered next_attempt_at <= now
	// (so NudgeDurableItems returns n=0), q.Nudge with non-empty keys still wakes the local drainer!
	_, err = st.NudgeDurableItems(ctx, "test.echo", []string{"other-key"}, nowFn())
	require.NoError(t, err)
	nudgedZero, err := q.Nudge(ctx, "test.echo", []string{"other-key"})
	require.NoError(t, err)
	assert.Zero(t, nudgedZero)

	require.Eventually(t, func() bool {
		handledMu.Lock()
		defer handledMu.Unlock()
		return len(handled) == 4
	}, 2*time.Second, 5*time.Millisecond)

	handledMu.Lock()
	assert.Equal(t, []string{"already-due", "nudge-a", "nudge-b", "other-key"}, handled)
	handledMu.Unlock()

	// Verify other.echo item and leased item were untouched.
	otherStats, err := st.DurableQueueStats(ctx, "other.echo", "", nowFn())
	require.NoError(t, err)
	assert.Equal(t, int64(1), otherStats.Pending)

	echoStats, err := st.DurableQueueStats(ctx, "test.echo", "", nowFn())
	require.NoError(t, err)
	assert.Equal(t, int64(1), echoStats.Pending)
	assert.Equal(t, int64(1), echoStats.Leased)
}
