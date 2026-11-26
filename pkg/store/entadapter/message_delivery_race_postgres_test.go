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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Two-connection Postgres concurrency matrix for the deliver-message row
// protocol (bind, fail, finish, sweep, unbind, MarkMessageDispatched).
// Always compiled under !no_sqlite; skips cleanly unless built with
// -tags integration and SCION_TEST_POSTGRES_URL is set, as in CI's
// entadapter Postgres job:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -count=50 -run 'TestMessageDeliveryRace_' ./pkg/store/entadapter/

// deliveryRaceBlockedFor is how long connection 2 must stay blocked to prove
// it is waiting on connection 1's Postgres row lock.
const deliveryRaceBlockedFor = 100 * time.Millisecond

const (
	outcomeDelivered     = `{"outcome":"delivered"}`
	outcomePartialSubmit = `{"outcome":"partial","stage":"submit"}`
)

type deliveryRaceFixture struct {
	c1    *ent.Client
	c2    *ent.Client
	ms1   *MessageStore
	ds1   *BrokerDispatchStore
	ds2   *BrokerDispatchStore
	agent string
}

func newDeliveryRaceFixture(t *testing.T) *deliveryRaceFixture {
	t.Helper()
	requirePostgres(t)
	schemaURL := enttest.NewSchemaURL(t)

	c1, err := entc.OpenPostgres(schemaURL, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, Diag: txdiag.QuietConfig()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c1.Close() })

	c2, err := entc.OpenPostgres(schemaURL, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, Diag: txdiag.QuietConfig()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })

	return &deliveryRaceFixture{
		c1:    c1,
		c2:    c2,
		ms1:   NewMessageStore(c1),
		ds1:   NewBrokerDispatchStore(c1),
		ds2:   NewBrokerDispatchStore(c2),
		agent: uuid.NewString(),
	}
}

func (f *deliveryRaceFixture) msg(t *testing.T, state string) string {
	t.Helper()
	m := newTestMessage(uuid.NewString(), f.agent)
	m.DispatchState = state
	require.NoError(t, f.ms1.CreateMessage(context.Background(), m))
	return m.ID
}

func (f *deliveryRaceFixture) row(t *testing.T, id string) *ent.Message {
	t.Helper()
	uid, err := parseUUID(id)
	require.NoError(t, err)
	row, err := f.c1.Message.Get(context.Background(), uid)
	require.NoError(t, err)
	return row
}

func newTxBrokerDispatchStore(tx *ent.Tx) *BrokerDispatchStore {
	return &BrokerDispatchStore{client: tx.Client(), inTx: true}
}

// installBindBarrier installs a one-shot barrier inside bindOnce right after
// SELECT ... FOR UPDATE acquires the row locks. The first BindMessageDelivery
// call closes locked and waits until release is closed.
func installBindBarrier(t *testing.T) (locked <-chan struct{}, release chan struct{}) {
	t.Helper()
	l := make(chan struct{})
	r := make(chan struct{})
	var once sync.Once
	bindAfterLockHook = func() {
		once.Do(func() {
			close(l)
			<-r
		})
	}
	t.Cleanup(func() {
		bindAfterLockHook = nil
		select {
		case <-r:
		default:
			close(r)
		}
	})
	return l, r
}

// assertAtMostOneDeliveryID checks invariant 1: every row has at most one
// delivery_id matching wantID (or nil when wantID == "").
func (f *deliveryRaceFixture) assertAtMostOneDeliveryID(t *testing.T, ids []string, wantID string) {
	t.Helper()
	seen := make(map[string]struct{})
	for _, id := range ids {
		r := f.row(t, id)
		if wantID == "" {
			assert.Nil(t, r.DeliveryID, "invariant 1: row %s must have no delivery_id", id)
			continue
		}
		require.NotNil(t, r.DeliveryID, "invariant 1: row %s must carry delivery_id %q", id, wantID)
		assert.Equal(t, wantID, *r.DeliveryID, "invariant 1: row %s must carry single delivery_id %q", id, wantID)
		seen[*r.DeliveryID] = struct{}{}
	}
	assert.LessOrEqual(t, len(seen), 1, "invariant 1: batch must have at most one delivery_id")
}

// assertNoRowLeavesDelivered checks invariant 2: if a row reached delivered,
// it stays delivered even against follow-up fail, sweep, and unbind attempts.
func (f *deliveryRaceFixture) assertNoRowLeavesDelivered(t *testing.T, ids []string, mustBeDelivered bool) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		r := f.row(t, id)
		if mustBeDelivered {
			assert.Equal(t, store.MessageDispatchDelivered, r.DispatchState, "invariant 2: row %s must remain delivered", id)
		}
		if r.DispatchState == store.MessageDispatchDelivered {
			won, err := f.ds2.FailUndeliveredMessage(ctx, id, "late fail after delivered")
			require.NoError(t, err)
			assert.False(t, won, "invariant 2: FailUndeliveredMessage must not move delivered row %s", id)

			swept, err := f.ds2.SweepStaleDeliveries(ctx, time.Now().Add(time.Hour), store.DeliveryOutcomeUnknownPartial)
			require.NoError(t, err)
			assert.Zero(t, swept, "invariant 2: SweepStaleDeliveries must not move delivered row %s", id)

			if r.DeliveryID != nil {
				require.NoError(t, f.ds2.UnbindMessageDelivery(ctx, *r.DeliveryID, []string{id}))
			}
			assert.Equal(t, store.MessageDispatchDelivered, f.row(t, id).DispatchState, "invariant 2: row %s must never leave delivered", id)
		}
	}
}

// assertOutcomePreserved checks invariant 3: stored delivery_outcome matches
// wantOutcome, and for final rows with a delivery_id a same-id replay returns
// that exact outcome.
func (f *deliveryRaceFixture) assertOutcomePreserved(t *testing.T, ids []string, wantDeliveryID, wantOutcome string) {
	t.Helper()
	ctx := context.Background()
	allFinal := true
	for _, id := range ids {
		r := f.row(t, id)
		if wantOutcome == "" {
			assert.Nil(t, r.DeliveryOutcome, "invariant 3: row %s should have no delivery_outcome", id)
		} else {
			require.NotNil(t, r.DeliveryOutcome, "invariant 3: row %s must preserve delivery_outcome", id)
			assert.JSONEq(t, wantOutcome, *r.DeliveryOutcome, "invariant 3: row %s delivery_outcome mismatch", id)
		}
		if !finalDeliveryStates[r.DispatchState] {
			allFinal = false
		}
	}
	if allFinal && wantDeliveryID != "" && wantOutcome != "" {
		res, err := f.ds2.BindMessageDelivery(ctx, f.agent, wantDeliveryID, ids, time.Now())
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryReplay, res.Result, "invariant 3: same-id bind on final rows must replay")
		assert.JSONEq(t, wantOutcome, res.Outcome, "invariant 3: replay must return preserved outcome")
	}
}

// TestMessageDeliveryRace_BindVsBindDifferentID covers pair 1 in both orders:
// bind(id1) vs bind(id2) on the same rows. Exactly one wins; the other gets
// DeliveryConflict (409 bound).
func TestMessageDeliveryRace_BindVsBindDifferentID(t *testing.T) {
	ctx := context.Background()
	orders := []struct {
		name     string
		firstID  string
		secondID string
	}{
		{name: "id1_holds_lock_then_id2", firstID: "d1", secondID: "d2"},
		{name: "id2_holds_lock_then_id1", firstID: "d2", secondID: "d1"},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryRaceFixture(t)
			a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchPending)
			ids := []string{a, b}
			now := time.Now().UTC().Truncate(time.Microsecond)

			locked, release := installBindBarrier(t)
			type bindRes struct {
				res store.DeliveryBind
				err error
			}
			c1Done := make(chan bindRes, 1)
			go func() {
				res, err := f.ds1.BindMessageDelivery(ctx, f.agent, tc.firstID, ids, now)
				c1Done <- bindRes{res, err}
			}()
			<-locked

			c2Done := make(chan bindRes, 1)
			go func() {
				res, err := f.ds2.BindMessageDelivery(ctx, f.agent, tc.secondID, ids, now)
				c2Done <- bindRes{res, err}
			}()
			select {
			case r := <-c2Done:
				t.Fatalf("conn2 bind(%s) did not wait for conn1's FOR UPDATE lock: %+v", tc.secondID, r)
			case <-time.After(deliveryRaceBlockedFor):
			}

			close(release)
			r1 := <-c1Done
			r2 := <-c2Done
			require.NoError(t, r1.err)
			require.NoError(t, r2.err)
			assert.Equal(t, store.DeliveryBound, r1.res.Result, "first binder wins")
			assert.Equal(t, store.DeliveryConflict, r2.res.Result, "second binder gets 409 bound")

			for _, id := range ids {
				assert.Equal(t, store.MessageDispatchDelivering, f.row(t, id).DispatchState)
			}
			f.assertAtMostOneDeliveryID(t, ids, tc.firstID)
			f.assertNoRowLeavesDelivered(t, ids, false)
			f.assertOutcomePreserved(t, ids, tc.firstID, "")
		})
	}
}

// TestMessageDeliveryRace_BindVsBindSameID covers pair 2 in both orders:
// bind(id1) vs bind(id1) replay — both when racing an in-flight bind and when
// replaying already-final rows.
func TestMessageDeliveryRace_BindVsBindSameID(t *testing.T) {
	ctx := context.Background()

	t.Run("inflight_c1_holds_lock_then_c2", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a, b}
		now := time.Now().UTC().Truncate(time.Microsecond)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c2Done <- bindRes{res, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 same-id bind did not wait for conn1's FOR UPDATE lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		r2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryBound, r1.res.Result)
		assert.Equal(t, store.DeliveryInFlight, r2.res.Result)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", "")
	})

	t.Run("inflight_c2_holds_lock_then_c1", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a, b}
		now := time.Now().UTC().Truncate(time.Microsecond)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c2Done <- bindRes{res, err}
		}()
		<-locked

		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		select {
		case r := <-c1Done:
			t.Fatalf("conn1 same-id bind did not wait for conn2's FOR UPDATE lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r2 := <-c2Done
		r1 := <-c1Done
		require.NoError(t, r2.err)
		require.NoError(t, r1.err)
		assert.Equal(t, store.DeliveryBound, r2.res.Result)
		assert.Equal(t, store.DeliveryInFlight, r1.res.Result)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", "")
	})

	t.Run("final_replay_concurrent_same_id", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a, b}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)
		n, err := f.ds1.FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchDelivered, outcomeDelivered)
		require.NoError(t, err)
		require.Equal(t, 2, n)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c2Done <- bindRes{res, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 replay did not wait for conn1's FOR UPDATE lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		r2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryReplay, r1.res.Result)
		assert.JSONEq(t, outcomeDelivered, r1.res.Outcome)
		assert.Equal(t, store.DeliveryReplay, r2.res.Result)
		assert.JSONEq(t, outcomeDelivered, r2.res.Outcome)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, true)
		f.assertOutcomePreserved(t, ids, "d1", outcomeDelivered)
	})
}

// TestMessageDeliveryRace_BindVsFail covers pair 3 in both orders:
// bind vs FailUndeliveredMessage (/message-failures path).
func TestMessageDeliveryRace_BindVsFail(t *testing.T) {
	ctx := context.Background()

	t.Run("bind_holds_lock_then_fail", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		type failRes struct {
			won bool
			err error
		}
		c2Done := make(chan failRes, 1)
		go func() {
			won, err := f.ds2.FailUndeliveredMessage(ctx, a, "broker failure")
			c2Done <- failRes{won, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 fail did not wait for conn1's bind FOR UPDATE lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		r2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryBound, r1.res.Result)
		assert.True(t, r2.won, "fail waiting on bind lock must transition delivering -> failed")
		assert.Equal(t, store.MessageDispatchFailed, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", store.DeliveryOutcomeUnknownPartial)
	})

	t.Run("bind_holds_lock_then_mark_failed", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchPending)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		c2Done := make(chan error, 1)
		go func() {
			c2Done <- f.ds2.MarkMessageFailed(ctx, a, "ExecuteAgentDM finalization")
		}()
		select {
		case err := <-c2Done:
			t.Fatalf("conn2 MarkMessageFailed did not wait for conn1's bind FOR UPDATE lock: %v", err)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		err2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, err2)
		assert.Equal(t, store.DeliveryBound, r1.res.Result)
		assert.Equal(t, store.MessageDispatchFailed, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", store.DeliveryOutcomeUnknownPartial)
	})

	t.Run("fail_holds_lock_then_bind", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		won, err := newTxBrokerDispatchStore(tx).FailUndeliveredMessage(ctx, a, "broker failure")
		require.NoError(t, err)
		require.True(t, won)

		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c2Done <- bindRes{res, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 bind did not wait for conn1's fail row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryConflict, r2.res.Result, "bind after fail must be refused")
		assert.Equal(t, store.MessageDispatchFailed, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "", "")
	})
}

// TestMessageDeliveryRace_FinishVsSweep covers pair 4 in both orders:
// FinishMessageDelivery vs SweepStaleDeliveries. Exactly one terminal state.
func TestMessageDeliveryRace_FinishVsSweep(t *testing.T) {
	ctx := context.Background()

	t.Run("finish_holds_lock_then_sweep", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		staleAt := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
		cutoff := time.Now().UTC().Add(-220 * time.Second).Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, staleAt)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		n, err := newTxBrokerDispatchStore(tx).FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchDelivered, outcomeDelivered)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		type sweepRes struct {
			n   int
			err error
		}
		c2Done := make(chan sweepRes, 1)
		go func() {
			swept, err := f.ds2.SweepStaleDeliveries(ctx, cutoff, store.DeliveryOutcomeUnknownPartial)
			c2Done <- sweepRes{swept, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 sweep did not wait for conn1's finish row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Zero(t, r2.n, "sweep after finish must update 0 rows")

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, true)
		f.assertOutcomePreserved(t, ids, "d1", outcomeDelivered)
	})

	t.Run("sweep_holds_lock_then_finish", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		staleAt := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
		cutoff := time.Now().UTC().Add(-220 * time.Second).Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, staleAt)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		swept, err := newTxBrokerDispatchStore(tx).SweepStaleDeliveries(ctx, cutoff, store.DeliveryOutcomeUnknownPartial)
		require.NoError(t, err)
		require.Equal(t, 1, swept)

		type finishRes struct {
			n   int
			err error
		}
		c2Done := make(chan finishRes, 1)
		go func() {
			n, err := f.ds2.FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchDelivered, outcomeDelivered)
			c2Done <- finishRes{n, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 finish did not wait for conn1's sweep row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Zero(t, r2.n, "finish after sweep must update 0 rows")
		assert.Equal(t, store.MessageDispatchPartial, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", store.DeliveryOutcomeUnknownPartial)
	})
}

// TestMessageDeliveryRace_FinishVsFail covers pair 5 in both orders:
// finish (partial, G5, and delivered) vs FailUndeliveredMessage.
func TestMessageDeliveryRace_FinishVsFail(t *testing.T) {
	ctx := context.Background()

	t.Run("finish_partial_holds_lock_then_fail", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		n, err := newTxBrokerDispatchStore(tx).FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchPartial, outcomePartialSubmit)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		type failRes struct {
			won bool
			err error
		}
		c2Done := make(chan failRes, 1)
		go func() {
			won, err := f.ds2.FailUndeliveredMessage(ctx, a, "partial delivery failed")
			c2Done <- failRes{won, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 fail did not wait for conn1's finish(partial) row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.True(t, r2.won, "G5: fail on partial row transitions partial -> failed")
		assert.Equal(t, store.MessageDispatchFailed, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", outcomePartialSubmit)
	})

	t.Run("fail_holds_lock_then_finish_partial", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		won, err := newTxBrokerDispatchStore(tx).FailUndeliveredMessage(ctx, a, "broker failure")
		require.NoError(t, err)
		require.True(t, won)

		type finishRes struct {
			n   int
			err error
		}
		c2Done := make(chan finishRes, 1)
		go func() {
			n, err := f.ds2.FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchPartial, outcomePartialSubmit)
			c2Done <- finishRes{n, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 finish did not wait for conn1's fail row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Zero(t, r2.n, "finish after fail must update 0 rows")
		assert.Equal(t, store.MessageDispatchFailed, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", store.DeliveryOutcomeUnknownPartial)
	})

	t.Run("finish_delivered_holds_lock_then_fail", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		n, err := newTxBrokerDispatchStore(tx).FinishMessageDelivery(ctx, "d1", ids, store.MessageDispatchDelivered, outcomeDelivered)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		type failRes struct {
			won bool
			err error
		}
		c2Done := make(chan failRes, 1)
		go func() {
			won, err := f.ds2.FailUndeliveredMessage(ctx, a, "late broker failure")
			c2Done <- failRes{won, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 fail did not wait for conn1's finish(delivered) row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.False(t, r2.won, "fail after finish(delivered) must not overwrite delivered")

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, true)
		f.assertOutcomePreserved(t, ids, "d1", outcomeDelivered)
	})
}

// TestMessageDeliveryRace_UnbindVsBind covers pair 6 in both orders:
// UnbindMessageDelivery vs BindMessageDelivery.
func TestMessageDeliveryRace_UnbindVsBind(t *testing.T) {
	ctx := context.Background()

	t.Run("unbind_holds_lock_then_bind", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		require.NoError(t, newTxBrokerDispatchStore(tx).UnbindMessageDelivery(ctx, "d1", ids))

		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d2", ids, now)
			c2Done <- bindRes{res, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 bind did not wait for conn1's unbind row lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryBound, r2.res.Result, "bind after unbind succeeds under new id")
		assert.Equal(t, store.MessageDispatchDelivering, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d2")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d2", "")
	})

	t.Run("bind_holds_lock_then_unbind", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
		require.NoError(t, err)
		require.Equal(t, store.DeliveryBound, res.Result)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d2", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		c2Done := make(chan error, 1)
		go func() {
			c2Done <- f.ds2.UnbindMessageDelivery(ctx, "d1", ids)
		}()
		select {
		case err := <-c2Done:
			t.Fatalf("conn2 unbind did not wait for conn1's bind FOR UPDATE lock: %v", err)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		err2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, err2)
		assert.Equal(t, store.DeliveryConflict, r1.res.Result, "bind(d2) saw d1 delivering and conflicted")
		assert.Equal(t, store.MessageDispatchDispatched, f.row(t, a).DispatchState, "unbind released d1 back to dispatched")

		f.assertAtMostOneDeliveryID(t, ids, "")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "", "")
	})
}

// TestMessageDeliveryRace_MarkDispatchedVsBind covers pair 7 in both orders:
// MarkMessageDispatched (pending -> dispatched CAS) vs BindMessageDelivery
// (pending OR dispatched, G1). CAS miss is benign.
func TestMessageDeliveryRace_MarkDispatchedVsBind(t *testing.T) {
	ctx := context.Background()

	t.Run("mark_dispatched_holds_lock_then_bind", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchPending)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		tx, err := f.c1.Tx(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		dispatched, err := newTxBrokerDispatchStore(tx).MarkMessageDispatched(ctx, a)
		require.NoError(t, err)
		require.True(t, dispatched)

		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c2Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds2.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c2Done <- bindRes{res, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 bind did not wait for conn1's MarkMessageDispatched lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		require.NoError(t, tx.Commit())
		r2 := <-c2Done
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryBound, r2.res.Result, "bind accepts dispatched row")
		assert.Equal(t, store.MessageDispatchDelivering, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", "")
	})

	t.Run("bind_holds_lock_then_mark_dispatched", func(t *testing.T) {
		f := newDeliveryRaceFixture(t)
		a := f.msg(t, store.MessageDispatchPending)
		ids := []string{a}
		now := time.Now().UTC().Truncate(time.Microsecond)

		locked, release := installBindBarrier(t)
		type bindRes struct {
			res store.DeliveryBind
			err error
		}
		c1Done := make(chan bindRes, 1)
		go func() {
			res, err := f.ds1.BindMessageDelivery(ctx, f.agent, "d1", ids, now)
			c1Done <- bindRes{res, err}
		}()
		<-locked

		type dispRes struct {
			dispatched bool
			err        error
		}
		c2Done := make(chan dispRes, 1)
		go func() {
			d, err := f.ds2.MarkMessageDispatched(ctx, a)
			c2Done <- dispRes{d, err}
		}()
		select {
		case r := <-c2Done:
			t.Fatalf("conn2 MarkMessageDispatched did not wait for conn1's bind FOR UPDATE lock: %+v", r)
		case <-time.After(deliveryRaceBlockedFor):
		}

		close(release)
		r1 := <-c1Done
		r2 := <-c2Done
		require.NoError(t, r1.err)
		require.NoError(t, r2.err)
		assert.Equal(t, store.DeliveryBound, r1.res.Result, "G1: bind accepts pending row")
		assert.False(t, r2.dispatched, "CAS miss on already-delivering row is benign")
		assert.Equal(t, store.MessageDispatchDelivering, f.row(t, a).DispatchState)

		f.assertAtMostOneDeliveryID(t, ids, "d1")
		f.assertNoRowLeavesDelivered(t, ids, false)
		f.assertOutcomePreserved(t, ids, "d1", "")
	})
}
