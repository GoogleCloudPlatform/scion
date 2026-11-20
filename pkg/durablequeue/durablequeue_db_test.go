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

type passSignalMetrics struct {
	durablequeue.NopMetrics
	onObserve func()
}

func (m *passSignalMetrics) ObserveQueue(string, int64, int64, time.Duration) {
	if m.onObserve != nil {
		m.onObserve()
	}
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

	st, err := cs.DurableQueueStats(ctx, "test.echo", "", t0)
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
	assert.Equal(t, "proj-small", dropsByReason[durablequeue.DropReasonAge][0].ProjectID)
	assert.Positive(t, dropsByReason[durablequeue.DropReasonAge][0].Seq)
	assert.Nil(t, dropsByReason[durablequeue.DropReasonAge][0].Payload, "trim must not load payload bytes")

	require.Len(t, dropsByReason[durablequeue.DropReasonOverflow], 3)
	for i, it := range dropsByReason[durablequeue.DropReasonOverflow] {
		assert.Equal(t, "proj-flood", it.ProjectID)
		assert.Equal(t, fmt.Sprintf("flood-%d", i), it.DedupKey)
		assert.Positive(t, it.Seq)
		assert.Nil(t, it.Payload, "trim must not load payload bytes")
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
	assert.Equal(t, "proj-small", dropsByReason[durablequeue.DropReasonPoison][0].ProjectID)
	assert.Positive(t, dropsByReason[durablequeue.DropReasonPoison][0].Seq)
	assert.False(t, dropsByReason[durablequeue.DropReasonPoison][0].CreatedAt.IsZero())
	assert.Nil(t, dropsByReason[durablequeue.DropReasonPoison][0].Payload, "trim must not load payload bytes")

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

	const (
		totalItems     = 60
		initialBacklog = 40
	)
	var (
		mu               sync.Mutex
		invocations      = make(map[string]int, totalItems)
		successes        = make(map[string]int, totalItems)
		seqByID          = make(map[string]int64, totalItems)
		bodies           = make(map[string]string, totalItems)
		handledByDrainer = make(map[string]int, 2)
		retriedOnce      = make(map[string]bool)
		totalDone        int
	)

	enqueueOne := func(i int) {
		t.Helper()
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
	}

	// Seed an initial backlog before starting the two drainers so both replicas
	// contend immediately on claim batches, then enqueue the remainder live.
	for i := 0; i < initialBacklog; i++ {
		enqueueOne(i)
	}

	makeHandler := func(replica string) durablequeue.Handler {
		return func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			if err := json.Unmarshal(item.Payload, &msg); err != nil {
				return durablequeue.Result{Permanent: true, Err: err}
			}
			mu.Lock()
			invocations[msg.ID]++
			if prev, ok := seqByID[msg.ID]; ok {
				assert.Equal(t, prev, item.Seq)
			} else {
				seqByID[msg.ID] = item.Seq
			}
			// Inject one fast retry on every 7th item to exercise concurrent re-claim.
			if item.Seq%7 == 0 && !retriedOnce[msg.ID] {
				retriedOnce[msg.ID] = true
				mu.Unlock()
				return durablequeue.Result{
					RetryAt: time.Now().UTC().Add(5 * time.Millisecond),
					Err:     errors.New("transient hiccup"),
				}
			}
			mu.Unlock()

			// Hold the lease briefly while doing simulated work so the sibling
			// drainer concurrently polls/claims while this batch is in flight.
			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			successes[msg.ID]++
			bodies[msg.ID] = msg.Body
			handledByDrainer[replica]++
			totalDone++
			mu.Unlock()
			return durablequeue.Result{Done: true}
		}
	}

	d1, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Queue:        q,
		Kind:         "test.echo",
		Owner:        "replica-1",
		PollInterval: 10 * time.Millisecond,
		BatchSize:    5,
		Logger:       quietLogger(),
		Handler:      makeHandler("replica-1"),
	})
	require.NoError(t, err)

	d2, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Queue:        q,
		Kind:         "test.echo",
		Owner:        "replica-2",
		PollInterval: 10 * time.Millisecond,
		BatchSize:    5,
		Logger:       quietLogger(),
		Handler:      makeHandler("replica-2"),
	})
	require.NoError(t, err)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		d1.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		<-start
		d2.Run(ctx)
	}()
	close(start)

	for i := initialBacklog; i < totalItems; i++ {
		enqueueOne(i)
		if i%5 == 0 {
			q.Wake("test.echo")
		}
	}
	q.Wake("test.echo")

	require.Eventually(t, func() bool {
		mu.Lock()
		done := totalDone
		mu.Unlock()
		if done < totalItems {
			return false
		}
		st, serr := cs.DurableQueueStats(ctx, "test.echo", "", time.Now().UTC())
		return serr == nil && st.Pending == 0
	}, 10*time.Second, 20*time.Millisecond)

	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, handledByDrainer["replica-1"], 1, "replica-1 must handle work")
	assert.GreaterOrEqual(t, handledByDrainer["replica-2"], 1, "replica-2 must handle work")
	assert.Equal(t, totalItems, handledByDrainer["replica-1"]+handledByDrainer["replica-2"],
		"no item may be completed more than once across drainers")

	require.Len(t, successes, totalItems)
	for i := 0; i < totalItems; i++ {
		id := fmt.Sprintf("msg-%03d", i)
		assert.Equal(t, 1, successes[id], "item %s must succeed exactly once", id)
		expectedCalls := 1
		if seqByID[id]%7 == 0 {
			expectedCalls = 2
		}
		assert.Equal(t, expectedCalls, invocations[id], "item %s (seq %d) invocation count", id, seqByID[id])
		assert.Equal(t, "echo:"+id, bodies[id])
	}
}

func TestEchoConsumer_NudgeUnleasedOnlyAndClaimOrder(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	t0 := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	cur := t0
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

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))

	// Validation & empty-list behavior through StoreQueue.Nudge against the real DB store.
	nudged, err := q.Nudge(ctx, "test.echo", nil)
	require.NoError(t, err)
	assert.Zero(t, nudged)

	oversized := make([]string, store.MaxDurableQueueNudgeKeys+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("k-%d", i)
	}
	_, err = q.Nudge(ctx, "test.echo", oversized)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	enq := func(kind, id string, notBefore time.Time) int64 {
		t.Helper()
		payload, merr := json.Marshal(echoMessage{ID: id, Body: "body:" + id})
		require.NoError(t, merr)
		seq, eerr := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      kind,
			DedupKey:  id,
			ProjectID: "proj-nudge",
			Payload:   payload,
			NotBefore: notBefore,
		})
		require.NoError(t, eerr)
		return seq
	}

	// 1. Leased item: claimed by another replica, lease active through t0 + 5m,
	// with next_attempt_at pushed into the future so only queueItemUnleased(now)
	// prevents NudgeDurableItems from updating it.
	seqLeased := enq("test.echo", "leased-item", t0)
	leasedClaimed, err := cs.ClaimDurableItems(ctx, "test.echo", "other-replica", t0, t0.Add(5*time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, leasedClaimed, 1)
	assert.Equal(t, seqLeased, leasedClaimed[0].Seq)
	leasedFuture := t0.Add(10 * time.Minute)
	require.NoError(t, entClient.DurableQueueItem.UpdateOneID(seqLeased).
		SetNextAttemptAt(leasedFuture).
		Exec(ctx))

	// 2. Two future unleased items: nudge-1 has lower seq but later NotBefore (+10m) than nudge-2 (+4m).
	// Nudging both to cur = t0 + 30s gives both next_attempt_at = cur, so (next_attempt_at, seq)
	// claims nudge-1 before nudge-2.
	enq("test.echo", "nudge-1", t0.Add(10*time.Minute))
	enq("test.echo", "nudge-2", t0.Add(4*time.Minute))

	// 3. Item already due at t0 + 10s (earlier than nudge time t0 + 30s):
	// Nudge must NOT move its next_attempt_at forward to t0 + 30s, so it claims FIRST ahead of nudge-1/2.
	enq("test.echo", "already-due", t0.Add(10*time.Second))

	// 4. Other key of test.echo and same key on other.echo: both remain in the future.
	enq("test.echo", "untouched-key", t0.Add(10*time.Minute))
	enq("other.echo", "nudge-1", t0.Add(10*time.Minute))

	// Advance clock to t0 + 30s and nudge via StoreQueue.Nudge while a drainer with a long
	// PollInterval is running, proving both DB semantics and local Wake delivery.
	var (
		gotMu         sync.Mutex
		got           []string
		firstPassOnce sync.Once
		firstPassDone = make(chan struct{})
	)
	passMetrics := &passSignalMetrics{onObserve: func() {
		firstPassOnce.Do(func() { close(firstPassDone) })
	}}
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:        cs,
		Queue:        q,
		Kind:         "test.echo",
		PollInterval: time.Hour,
		Now:          nowFn,
		Metrics:      passMetrics,
		Logger:       quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			var msg echoMessage
			if uerr := json.Unmarshal(item.Payload, &msg); uerr != nil {
				return durablequeue.Result{Permanent: true, Err: uerr}
			}
			gotMu.Lock()
			got = append(got, msg.ID)
			gotMu.Unlock()
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	// Start drainer Run loop; wait for its initial pass at t0 (where nothing unleased is due),
	// then advance to t0+30s and Nudge so all three due items are claimed in the single woken pass.
	runCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		d.Run(runCtx)
	}()
	defer func() {
		cancel()
		<-doneCh
	}()

	select {
	case <-firstPassDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial drainer pass")
	}

	setNow(t0.Add(30 * time.Second))
	nudged, err = q.Nudge(ctx, "test.echo", []string{
		"leased-item",
		"nudge-2",
		"nudge-1",
		"already-due",
		"unknown-1",
		"unknown-2",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, nudged, "only unleased future items nudge-1 and nudge-2 are updated")

	require.Eventually(t, func() bool {
		gotMu.Lock()
		defer gotMu.Unlock()
		return len(got) == 3
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	<-doneCh

	gotMu.Lock()
	assert.Equal(t, []string{"already-due", "nudge-1", "nudge-2"}, got)
	gotMu.Unlock()

	// Remaining in test.echo: leased-item (still leased) + untouched-key (still future).
	stEcho, err := cs.DurableQueueStats(ctx, "test.echo", "", nowFn())
	require.NoError(t, err)
	assert.Equal(t, int64(2), stEcho.Pending)
	assert.Equal(t, int64(1), stEcho.Leased)

	// Other kind untouched.
	stOther, err := cs.DurableQueueStats(ctx, "other.echo", "", nowFn())
	require.NoError(t, err)
	assert.Equal(t, int64(1), stOther.Pending)

	// Original lease holder still owns leased-item exclusively with its future next_attempt_at intact.
	leasedStored, err := entClient.DurableQueueItem.Get(ctx, seqLeased)
	require.NoError(t, err)
	assert.True(t, leasedStored.NextAttemptAt.Equal(leasedFuture), "nudge must not touch leased row next_attempt_at")
	doneLeased, err := cs.CompleteDurableItem(ctx, seqLeased, "other-replica")
	require.NoError(t, err)
	assert.True(t, doneLeased)
}

func TestDurableQueueDrainerFair_FloodDoesNotStarveAcrossTwoDrainers(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	now := t0.Add(5 * time.Minute)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))

	const floodCount = 40
	const victimCount = 4
	for i := 0; i < floodCount; i++ {
		ts := t0.Add(time.Duration(i) * time.Second)
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      "trigger.event",
			DedupKey:  fmt.Sprintf("flood-%02d", i),
			ProjectID: "proj-flood",
			Payload:   []byte(`{}`),
			CreatedAt: ts,
			NotBefore: ts,
		})
		require.NoError(t, err)
	}
	for i := 0; i < victimCount; i++ {
		ts := t0.Add(time.Duration(100+i) * time.Second)
		_, err := q.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      "trigger.event",
			DedupKey:  fmt.Sprintf("victim-%02d", i),
			ProjectID: "proj-victim",
			Payload:   []byte(`{}`),
			CreatedAt: ts,
			NotBefore: ts,
		})
		require.NoError(t, err)
	}

	var (
		mu          sync.Mutex
		order       []string
		handleCount = make(map[string]int)
	)
	handler := func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
		mu.Lock()
		order = append(order, item.DedupKey)
		handleCount[item.DedupKey]++
		mu.Unlock()
		return durablequeue.Result{Done: true}
	}

	makeDrainer := func(owner string) *durablequeue.Drainer {
		d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
			Store:         cs,
			Queue:         q,
			Kind:          "trigger.event",
			Owner:         owner,
			BatchSize:     4,
			FairByProject: true,
			PollInterval:  10 * time.Millisecond,
			Now:           func() time.Time { return now },
			Logger:        quietLogger(),
			Handler:       handler,
		})
		require.NoError(t, err)
		return d
	}

	d1 := makeDrainer("drainer-1")
	d2 := makeDrainer("drainer-2")

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, d := range []*durablequeue.Drainer{d1, d2} {
		wg.Add(1)
		go func(dr *durablequeue.Drainer) {
			defer wg.Done()
			dr.Run(runCtx)
		}(d)
	}

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == floodCount+victimCount
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, handleCount, floodCount+victimCount, "no item may be lost")
	for k, c := range handleCount {
		assert.Equal(t, 1, c, "item %s must be handled exactly once", k)
	}
	// All 4 victim items must complete well before the 40-item flood finishes.
	lastVictimIdx := -1
	for idx, key := range order {
		if len(key) >= 7 && key[:7] == "victim-" {
			lastVictimIdx = idx
		}
	}
	require.GreaterOrEqual(t, lastVictimIdx, 0)
	assert.Less(t, lastVictimIdx, floodCount, "victim items must not be starved behind the flooding project")
}

func TestDurableQueueDrainerFair_MoreProjectsThanBatchSizeRotation(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	now := t0.Add(time.Minute)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))

	const numProjects = 8
	for p := 0; p < numProjects; p++ {
		proj := fmt.Sprintf("proj-%02d", p)
		for i := 0; i < 2; i++ {
			ts := t0.Add(time.Duration(i) * time.Second)
			_, err := q.Enqueue(ctx, nil, durablequeue.Item{
				Kind:      "trigger.event",
				DedupKey:  fmt.Sprintf("%s-%d", proj, i),
				ProjectID: proj,
				Payload:   []byte(`{}`),
				CreatedAt: ts,
				NotBefore: ts,
			})
			require.NoError(t, err)
		}
	}

	var handled []string
	d, err := durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:         cs,
		Queue:         q,
		Kind:          "trigger.event",
		Owner:         "rot-drainer",
		BatchSize:     4,
		FairByProject: true,
		Now:           func() time.Time { return now },
		Logger:        quietLogger(),
		Handler: func(_ context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
			handled = append(handled, item.ProjectID)
			return durablequeue.Result{Done: true}
		},
	})
	require.NoError(t, err)

	// Pass 1 serves proj-00..proj-03; Pass 2 must advance cursor and serve proj-04..proj-07.
	res1, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, res1.Done)

	res2, err := d.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, res2.Done)

	assert.Equal(t, []string{
		"proj-00", "proj-01", "proj-02", "proj-03",
		"proj-04", "proj-05", "proj-06", "proj-07",
	}, handled)
}
