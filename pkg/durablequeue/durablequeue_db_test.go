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

package durablequeue_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

type echoMessage struct {
	ID   string `json:"id"`
	Body string `json:"body"`
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

func newEchoDBStore(t *testing.T) *entadapter.CompositeStore {
	t.Helper()
	return entadapter.NewCompositeStore(enttest.NewClient(t))
}

func TestEchoConsumer_EnqueueInTxCommitRollbackAndDedup(t *testing.T) {
	ctx := context.Background()
	cs := newEchoDBStore(t)
	t0 := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))

	// 1. Producer rollback: neither project nor test.echo item survives.
	rbProjID := uuid.NewString()
	err := cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateProject(ctx, &store.Project{
			ID:   rbProjID,
			Name: "rb",
			Slug: "rb-" + rbProjID[:8],
		}); err != nil {
			return err
		}
		if _, err := q.Enqueue(ctx, tx, durablequeue.Item{
			Kind:      "test.echo",
			DedupKey:  "tx-rb",
			ProjectID: rbProjID,
			Payload:   []byte(`{"id":"rb","body":"never"}`),
		}); err != nil {
			return err
		}
		return errors.New("abort producer tx")
	})
	require.Error(t, err)

	st, err := cs.DurableQueueStats(ctx, "test.echo", "")
	require.NoError(t, err)
	assert.Zero(t, st.Pending)

	// 2. Producer commit + duplicate (kind, dedup_key) rejection.
	okProjID := uuid.NewString()
	var seq int64
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateProject(ctx, &store.Project{
			ID:   okProjID,
			Name: "ok",
			Slug: "ok-" + okProjID[:8],
		}); err != nil {
			return err
		}
		var enqErr error
		seq, enqErr = q.Enqueue(ctx, tx, durablequeue.Item{
			Kind:      "test.echo",
			DedupKey:  "echo-1",
			ProjectID: okProjID,
			Payload:   []byte(`{"id":"1","body":"hello"}`),
		})
		return enqErr
	}))
	assert.Positive(t, seq)

	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "echo-1",
		ProjectID: okProjID,
		Payload:   []byte(`{"id":"1","body":"duplicate"}`),
	})
	require.ErrorIs(t, err, durablequeue.ErrDuplicate)

	// Drain with test.echo consumer and verify payload integrity.
	var received []echoMessage
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:  cs,
		Queue:  q,
		Kind:   "test.echo",
		Now:    func() time.Time { return t0 },
		Logger: quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			if uerr := json.Unmarshal(item.Payload, &msg); uerr != nil {
				return durablequeue.Result{Permanent: true, Err: uerr}
			}
			received = append(received, msg)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Done)
	assert.Zero(t, res.Remaining)
	assert.Equal(t, []echoMessage{{ID: "1", Body: "hello"}}, received)
}

func TestEchoConsumer_ClaimOrderingAndNotBefore(t *testing.T) {
	ctx := context.Background()
	cs := newEchoDBStore(t)
	cur := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))

	// Enqueue out of NotBefore order:
	// - "later" has lowest seq (1) but NotBefore = cur + 30s
	// - "soon-a" has seq 2 and NotBefore = cur + 5s
	// - "soon-b" has seq 3 and NotBefore = cur + 5s
	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "later",
		Payload:   []byte(`{"id":"later","body":"3"}`),
		NotBefore: cur.Add(30 * time.Second),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "soon-a",
		Payload:   []byte(`{"id":"soon-a","body":"1"}`),
		NotBefore: cur.Add(5 * time.Second),
	})
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "soon-b",
		Payload:   []byte(`{"id":"soon-b","body":"2"}`),
		NotBefore: cur.Add(5 * time.Second),
	})
	require.NoError(t, err)

	var order []string
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:  cs,
		Kind:   "test.echo",
		Now:    nowFn,
		Logger: quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			require.NoError(t, json.Unmarshal(item.Payload, &msg))
			order = append(order, msg.ID)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	// At t=cur, nothing is due.
	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)

	// At t=cur+40s, all three are due and must be handled in (next_attempt_at, seq) order!
	cur = cur.Add(40 * time.Second)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Done)
	assert.Equal(t, []string{"soon-a", "soon-b", "later"}, order)
}

func TestEchoConsumer_LeaseRenewalAndLostLeaseReclaim(t *testing.T) {
	ctx := context.Background()
	cs := newEchoDBStore(t)
	cur := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		mu.Lock()
		cur = cur.Add(d)
		mu.Unlock()
	}

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))
	exactPayload := []byte(`{"id":"reclaim-me","body":"exact-bytes"}`)
	seq, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "reclaim-1",
		Payload:  exactPayload,
	})
	require.NoError(t, err)

	// Simulate drainer A claiming the item directly and stalling beyond LeaseDuration (30 s).
	claimedA, err := cs.ClaimDurableItems(ctx, "test.echo", "drainer-a", nowFn(), nowFn().Add(30*time.Second), 1)
	require.NoError(t, err)
	require.Len(t, claimedA, 1)
	assert.Equal(t, seq, claimedA[0].Seq)

	// Advance clock past drainer A's lease; drainer B runs with real auto-renewal and completes it!
	advance(35 * time.Second)

	var rehandledPayload []byte
	drainerB, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          cs,
		Kind:           "test.echo",
		Owner:          "drainer-b",
		LeaseDuration:  90 * time.Millisecond,
		HandlerTimeout: 80 * time.Millisecond,
		Now:            nowFn,
		Logger:         quietLogger(),
		Handler: func(hctx context.Context, item durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
			// Exercise both manual Renew and background LeaseDuration/3 (30ms) auto-renew.
			held, rerr := lease.Renew(hctx)
			if rerr != nil || !held {
				return durablequeue.Result{Permanent: true, Err: errors.New("lost lease")}
			}
			time.Sleep(45 * time.Millisecond)
			rehandledPayload = append([]byte(nil), item.Payload...)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	resB, err := drainerB.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, resB.Done)
	assert.Equal(t, exactPayload, rehandledPayload)

	// Stale drainer A waking up afterwards cannot Complete a reclaimed/completed item.
	staleDone, err := cs.CompleteDurableItem(ctx, seq, "drainer-a")
	require.NoError(t, err)
	assert.False(t, staleDone)
}

func TestEchoConsumer_BackoffAndRetryAt(t *testing.T) {
	ctx := context.Background()
	cs := newEchoDBStore(t)
	cur := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))

	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "retry-item",
		Payload:  []byte(`{"id":"r1","body":"retry"}`),
	})
	require.NoError(t, err)

	var seenAttempts []int
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:  cs,
		Kind:   "test.echo",
		Now:    nowFn,
		Logger: quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			seenAttempts = append(seenAttempts, item.Attempts)
			switch item.Attempts {
			case 0:
				// Default backoff from Attempts=0 -> +1 s
				return durablequeue.Result{Err: errors.New("transient 1")}
			case 1:
				// Default backoff from Attempts=1 -> +2 s
				return durablequeue.Result{Err: errors.New("transient 2")}
			case 2:
				// Explicit custom RetryAt -> +10 s
				return durablequeue.Result{RetryAt: cur.Add(10 * time.Second), Err: errors.New("custom retry")}
			default:
				return durablequeue.Result{Done: true}
			}
		},
	})
	require.NoError(t, err)

	// Attempt 0 fails -> next attempt at cur + 1s
	res, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Retried)

	// At +500ms: not yet due!
	cur = cur.Add(500 * time.Millisecond)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)

	// At +1s: Attempt 1 runs and fails -> next attempt at +2s from new cur
	cur = cur.Add(500 * time.Millisecond)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Retried)

	// At +1.5s: not yet due!
	cur = cur.Add(1500 * time.Millisecond)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)

	// At +2s: Attempt 2 runs and schedules custom RetryAt = cur + 10s
	cur = cur.Add(500 * time.Millisecond)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Retried)

	// At +9s: still waiting for custom RetryAt!
	cur = cur.Add(9 * time.Second)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed)

	// At +10s: Attempt 3 succeeds!
	cur = cur.Add(time.Second)
	res, err = d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Done)
	assert.Equal(t, []int{0, 1, 2, 3}, seenAttempts)
}

func TestEchoConsumer_TrimByAgeRowsAndPoisonWithMetrics(t *testing.T) {
	ctx := context.Background()
	cs := newEchoDBStore(t)
	cur := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))

	// 1. Aged item (older than 72h).
	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "aged-1",
		ProjectID: "proj-small",
		Payload:   []byte(`{"id":"aged"}`),
		CreatedAt: cur.Add(-75 * time.Hour),
		NotBefore: cur.Add(-75 * time.Hour),
	})
	require.NoError(t, err)

	// 2. Overflow items: 6 in proj-flood, 2 in proj-small, plus 1 poison item in proj-small.
	// With MaxRows=6 (after age drop, 9 items remain -> excess 3), all 3 overflow drops
	// must come from proj-flood (the largest project), never from proj-small!
	for i := 0; i < 6; i++ {
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      "test.echo",
			DedupKey:  fmt.Sprintf("flood-%d", i),
			ProjectID: "proj-flood",
			Payload:   []byte(fmt.Sprintf(`{"id":"flood-%d"}`, i)),
		})
		require.NoError(t, err)
	}
	for i := 0; i < 2; i++ {
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      "test.echo",
			DedupKey:  fmt.Sprintf("small-%d", i),
			ProjectID: "proj-small",
			Payload:   []byte(fmt.Sprintf(`{"id":"small-%d"}`, i)),
		})
		require.NoError(t, err)
	}
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      "test.echo",
		DedupKey:  "poison-1",
		ProjectID: "proj-small",
		Payload:   []byte(`not-valid-json`),
	})
	require.NoError(t, err)

	metrics := newRecordingMetrics()
	dropsByReason := make(map[string][]durablequeue.Item)

	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Kind:         "test.echo",
		MaxAge:       72 * time.Hour,
		MaxRows:      6,
		MaxPermanent: 2,
		Now:          nowFn,
		Metrics:      metrics,
		Logger:       quietLogger(),
		OnDrop: func(_ context.Context, dropped []durablequeue.Item, reason string) {
			dropsByReason[reason] = append(dropsByReason[reason], dropped...)
		},
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			if uerr := json.Unmarshal(item.Payload, &msg); uerr != nil {
				return durablequeue.Result{Permanent: true, Err: errors.New("invalid payload")}
			}
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	// Pass 1: trims 1 aged + 3 overflow (from proj-flood), claims remaining 6 (5 valid + 1 poison);
	// poison fails permanently 1st time (PermanentFailures=1 < MaxPermanent=2).
	r1, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, r1.Dropped)
	assert.Equal(t, 5, r1.Done)
	assert.Equal(t, 1, r1.Permanent)
	assert.Equal(t, int64(1), r1.Remaining)

	require.Len(t, dropsByReason[durablequeue.DropReasonAge], 1)
	assert.Equal(t, "aged-1", dropsByReason[durablequeue.DropReasonAge][0].DedupKey)

	require.Len(t, dropsByReason[durablequeue.DropReasonOverflow], 3)
	for i, it := range dropsByReason[durablequeue.DropReasonOverflow] {
		assert.Equal(t, "proj-flood", it.ProjectID)
		assert.Equal(t, fmt.Sprintf("flood-%d", i), it.DedupKey)
	}

	// Pass 2 after 2s backoff: poison item fails permanently 2nd time -> hits MaxPermanent=2 -> immediately trimmed!
	cur = cur.Add(2 * time.Second)
	r2, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, r2.Permanent)
	assert.Equal(t, 1, r2.Dropped)
	assert.Zero(t, r2.Remaining)

	require.Len(t, dropsByReason[durablequeue.DropReasonPoison], 1)
	assert.Equal(t, "poison-1", dropsByReason[durablequeue.DropReasonPoison][0].DedupKey)
	assert.Equal(t, []byte(`not-valid-json`), dropsByReason[durablequeue.DropReasonPoison][0].Payload)

	metrics.mu.Lock()
	assert.Equal(t, 5, metrics.done)
	assert.Equal(t, 2, metrics.permanent)
	assert.Equal(t, 1, metrics.dropped[durablequeue.DropReasonAge])
	assert.Equal(t, 3, metrics.dropped[durablequeue.DropReasonOverflow])
	assert.Equal(t, 1, metrics.dropped[durablequeue.DropReasonPoison])
	assert.Zero(t, metrics.lastPending)
	metrics.mu.Unlock()
}

func TestEchoConsumer_TwoConcurrentDrainers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := newEchoDBStore(t)
	q := durablequeue.NewQueue(cs)

	const totalItems = 60
	var (
		mu       sync.Mutex
		handled  = make(map[string]string, totalItems)
		retried1 = make(map[string]bool)
	)

	makeHandler := func() durablequeue.Handler {
		return func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			if err := json.Unmarshal(item.Payload, &msg); err != nil {
				return durablequeue.Result{Permanent: true, Err: err}
			}
			mu.Lock()
			// Inject one fast retry on every 7th item to exercise concurrent re-claim.
			if item.Seq%7 == 0 && !retried1[msg.ID] {
				retried1[msg.ID] = true
				mu.Unlock()
				return durablequeue.Result{
					RetryAt: time.Now().UTC().Add(5 * time.Millisecond),
					Err:     errors.New("transient hiccup"),
				}
			}
			handled[msg.ID] = msg.Body
			mu.Unlock()
			return durablequeue.Result{Done: true}
		}
	}

	d1, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Queue:        q,
		Kind:         "test.echo",
		Owner:        "replica-1",
		PollInterval: 15 * time.Millisecond,
		BatchSize:    7,
		Logger:       quietLogger(),
		Handler:      makeHandler(),
	})
	require.NoError(t, err)

	d2, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Queue:        q,
		Kind:         "test.echo",
		Owner:        "replica-2",
		PollInterval: 15 * time.Millisecond,
		BatchSize:    7,
		Logger:       quietLogger(),
		Handler:      makeHandler(),
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); d1.Run(ctx) }()
	go func() { defer wg.Done(); d2.Run(ctx) }()

	// Enqueue 60 items concurrently with the two running drainers, nudging Wake periodically.
	for i := 0; i < totalItems; i++ {
		id := fmt.Sprintf("msg-%03d", i)
		payload, merr := json.Marshal(echoMessage{ID: id, Body: "echo:" + id})
		require.NoError(t, merr)
		require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
			_, eerr := q.Enqueue(ctx, tx, durablequeue.Item{
				Kind:      "test.echo",
				DedupKey:  id,
				ProjectID: fmt.Sprintf("proj-%d", i%3),
				Payload:   payload,
			})
			return eerr
		}))
		if i%10 == 0 {
			q.Wake("test.echo")
		}
	}
	q.Wake("test.echo")

	require.Eventually(t, func() bool {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n < totalItems {
			return false
		}
		st, serr := cs.DurableQueueStats(ctx, "test.echo", "")
		return serr == nil && st.Pending == 0
	}, 10*time.Second, 20*time.Millisecond)

	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, handled, totalItems)
	for i := 0; i < totalItems; i++ {
		id := fmt.Sprintf("msg-%03d", i)
		assert.Equal(t, "echo:"+id, handled[id])
	}
}
