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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deliveryFixture struct {
	ms    *MessageStore
	ds    *BrokerDispatchStore
	agent string
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	client := enttest.NewClient(t)
	return &deliveryFixture{ms: NewMessageStore(client), ds: NewBrokerDispatchStore(client), agent: uuid.NewString()}
}

func (f *deliveryFixture) msg(t *testing.T, state string) string {
	t.Helper()
	m := newTestMessage(uuid.NewString(), f.agent)
	m.DispatchState = state
	require.NoError(t, f.ms.CreateMessage(context.Background(), m))
	return m.ID
}

func (f *deliveryFixture) state(t *testing.T, id string) string {
	t.Helper()
	m, err := f.ms.GetMessage(context.Background(), id)
	require.NoError(t, err)
	return m.DispatchState
}

// TestBindMessageDelivery_Rules covers the §2.6 bind rules: all-or-nothing,
// same-id forward and replay, another id refused, recipient checked.
func TestBindMessageDelivery_Rules(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	t.Run("binds dispatched and pending rows (G1)", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchPending)
		res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a, b}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryBound, res.Result)
		assert.Equal(t, store.MessageDispatchDelivering, f.state(t, a))
		assert.Equal(t, store.MessageDispatchDelivering, f.state(t, b))

		res, err = f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a, b}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryInFlight, res.Result, "same-id call while delivering: owned by the binder")

		res, err = f.ds.BindMessageDelivery(ctx, f.agent, "d2", []string{a}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryConflict, res.Result, "another id")
	})
	t.Run("mixed set rolls back", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchFailed)
		res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a, b}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryConflict, res.Result)
		assert.Equal(t, store.MessageDispatchDispatched, f.state(t, a), "no row changed")
		assert.Equal(t, store.MessageDispatchFailed, f.state(t, b))
	})
	t.Run("unbindable states", func(t *testing.T) {
		for _, st := range []string{store.MessageDispatchFailed, store.MessageDispatchDeferred, store.MessageDispatchNoRecipient} {
			f := newDeliveryFixture(t)
			a := f.msg(t, st)
			res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, now)
			require.NoError(t, err)
			assert.Equal(t, store.DeliveryConflict, res.Result, st)
			assert.Equal(t, st, f.state(t, a))
		}
	})
	t.Run("other recipient or missing row", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		_, err := f.ds.BindMessageDelivery(ctx, uuid.NewString(), "d1", []string{a}, now)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a, uuid.NewString()}, now)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, store.MessageDispatchDispatched, f.state(t, a))
	})
	t.Run("final rows replay under the same id only", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		_, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, now)
		require.NoError(t, err)
		n, err := f.ds.FinishMessageDelivery(ctx, "d1", []string{a}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryReplay, res.Result)
		assert.JSONEq(t, `{"outcome":"delivered"}`, res.Outcome)
		res, err = f.ds.BindMessageDelivery(ctx, f.agent, "d9", []string{a}, now)
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryConflict, res.Result)
	})
}

func TestUnbindAndFinishAreCompareAndSet(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	a := f.msg(t, store.MessageDispatchDispatched)
	_, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
	require.NoError(t, err)

	require.NoError(t, f.ds.UnbindMessageDelivery(ctx, "other", []string{a}))
	assert.Equal(t, store.MessageDispatchDelivering, f.state(t, a), "unbind under another id is a no-op")
	require.NoError(t, f.ds.UnbindMessageDelivery(ctx, "d1", []string{a}))
	assert.Equal(t, store.MessageDispatchDispatched, f.state(t, a))

	// After an unbind the row binds again, under a new id (exec may also
	// leave it dispatched).
	res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d2", []string{a}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, store.DeliveryBound, res.Result)

	n, err := f.ds.FinishMessageDelivery(ctx, "d1", []string{a}, store.MessageDispatchDelivered, `{}`)
	require.NoError(t, err)
	assert.Zero(t, n, "a finish under a stale id changes nothing")
}

func TestSweepStaleDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	old, fresh := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
	now := time.Now()
	_, err := f.ds.BindMessageDelivery(ctx, f.agent, "old", []string{old}, now.Add(-221*time.Second))
	require.NoError(t, err)
	_, err = f.ds.BindMessageDelivery(ctx, f.agent, "fresh", []string{fresh}, now)
	require.NoError(t, err)

	n, err := f.ds.SweepStaleDeliveries(ctx, now.Add(-220*time.Second), `{"outcome":"partial","stage":"unknown"}`)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, store.MessageDispatchPartial, f.state(t, old), "stale delivering becomes partial, never dispatched")
	assert.Equal(t, store.MessageDispatchDelivering, f.state(t, fresh))

	// A late answer after the sweep does not overwrite it.
	n, err = f.ds.FinishMessageDelivery(ctx, "old", []string{old}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, store.MessageDispatchPartial, f.state(t, old))
}

// TestFailureVersusFinish (review R4): a failure write and a /deliver
// finish race on a delivering row, and exactly one wins in either order.
// A delivered row is never failed; a row failed while delivering keeps a
// replayable outcome.
func TestFailureVersusFinish(t *testing.T) {
	ctx := context.Background()
	t.Run("finish first", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a := f.msg(t, store.MessageDispatchPending)
		_, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
		require.NoError(t, err)
		n, err := f.ds.FinishMessageDelivery(ctx, "d1", []string{a}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		won, err := f.ds.FailUndeliveredMessage(ctx, a, "late report")
		require.NoError(t, err)
		assert.False(t, won, "a delivered row is never failed")
		require.NoError(t, f.ds.MarkMessageFailed(ctx, a, "ExecuteAgentDM finalization"))
		assert.Equal(t, store.MessageDispatchDelivered, f.state(t, a), "MarkMessageFailed keeps delivered too")
	})
	t.Run("failure first", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a := f.msg(t, store.MessageDispatchDispatched)
		_, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
		require.NoError(t, err)
		won, err := f.ds.FailUndeliveredMessage(ctx, a, "broker failure")
		require.NoError(t, err)
		assert.True(t, won)
		assert.Equal(t, store.MessageDispatchFailed, f.state(t, a))
		n, err := f.ds.FinishMessageDelivery(ctx, "d1", []string{a}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
		require.NoError(t, err)
		assert.Zero(t, n, "the late finish loses")
		res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryReplay, res.Result, "same id replays the stored outcome")
		assert.JSONEq(t, store.DeliveryOutcomeUnknownPartial, res.Outcome)
		won, err = f.ds.FailUndeliveredMessage(ctx, a, "again")
		require.NoError(t, err)
		assert.False(t, won, "a second report does not win again")
	})
	t.Run("MarkMessageFailed on delivering stores the outcome", func(t *testing.T) {
		f := newDeliveryFixture(t)
		a := f.msg(t, store.MessageDispatchPending)
		_, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
		require.NoError(t, err)
		require.NoError(t, f.ds.MarkMessageFailed(ctx, a, "ExecuteAgentDM finalization"))
		res, err := f.ds.BindMessageDelivery(ctx, f.agent, "d1", []string{a}, time.Now())
		require.NoError(t, err)
		assert.Equal(t, store.DeliveryReplay, res.Result)
	})
	t.Run("states FailUndeliveredMessage leaves alone", func(t *testing.T) {
		for _, st := range []string{store.MessageDispatchDeferred, store.MessageDispatchNoRecipient, store.MessageDispatchFailed} {
			f := newDeliveryFixture(t)
			a := f.msg(t, st)
			won, err := f.ds.FailUndeliveredMessage(ctx, a, "x")
			require.NoError(t, err)
			assert.False(t, won, st)
			assert.Equal(t, st, f.state(t, a))
		}
	})
}
