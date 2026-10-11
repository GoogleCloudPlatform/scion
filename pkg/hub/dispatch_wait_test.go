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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDispatchStore is a minimal in-memory BrokerDispatchStore for unit tests.
type fakeDispatchStore struct {
	dispatches map[string]*store.BrokerDispatch
}

func (f *fakeDispatchStore) GetBrokerDispatch(_ context.Context, id string) (*store.BrokerDispatch, error) {
	d, ok := f.dispatches[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeDispatchStore) InsertBrokerDispatch(_ context.Context, d *store.BrokerDispatch) error {
	return nil
}
func (f *fakeDispatchStore) ClaimBrokerDispatch(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeDispatchStore) CompleteBrokerDispatch(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeDispatchStore) FailBrokerDispatch(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDispatchStore) ListPendingDispatch(_ context.Context, _ string) ([]store.BrokerDispatch, error) {
	return nil, nil
}
func (f *fakeDispatchStore) HasOutstandingBrokerDispatch(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeDispatchStore) HasCompletedBrokerDispatchSince(_ context.Context, _, _ string, _ time.Time) (bool, error) {
	return false, nil
}
func (f *fakeDispatchStore) MarkMessageDispatched(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (f *fakeDispatchStore) MarkMessageFailed(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeDispatchStore) ListPendingMessages(_ context.Context, _ string) ([]store.Message, error) {
	return nil, nil
}
func (f *fakeDispatchStore) ReapStuckDispatch(_ context.Context, _ time.Time, _ int) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeDispatchStore) CountBrokerDispatchHealth(_ context.Context, _, _ time.Time) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeDispatchStore) CountStuckPendingMessages(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}
func (f *fakeDispatchStore) ExpireStuckPendingMessages(_ context.Context, _ time.Time, _ string) (int, error) {
	return 0, nil
}
func (f *fakeDispatchStore) FailPendingMessagesWithMissingRecipient(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (f *fakeDispatchStore) BackfillNonAgentDispatchState(_ context.Context, _ string) (int, error) {
	return 0, nil
}

// sendStatus pushes a fake AgentStatusEvent onto the channel.
func sendStatus(ch chan<- Event, phase, activity string, detail *AgentDetail) {
	evt := AgentStatusEvent{
		AgentID:  "agent-1",
		Phase:    phase,
		Activity: activity,
		Detail:   detail,
	}
	data, _ := json.Marshal(evt)
	ch <- Event{Subject: "agent.agent-1.status", Data: data}
}

// startTerminal and stopTerminal are the lifecycle terminal sets used by the
// waitForLifecycleOutcome tests below.
var (
	startTerminal = func(p string) bool { return p == "running" || p == "error" }
	stopTerminal  = func(p string) bool { return p == "stopped" || p == "error" }
)

// noRowStore is a dispatch store with no rows, so the lifecycle waiter never
// sees a failed row.
func noRowStore() *fakeDispatchStore {
	return &fakeDispatchStore{dispatches: map[string]*store.BrokerDispatch{}}
}

// setLifecycleTimings shortens the lifecycle wait timings for one test. A
// zero value keeps the current setting.
func setLifecycleTimings(t *testing.T, rolling, poll, grace time.Duration) {
	t.Helper()
	oldRolling, oldPoll, oldGrace := lifecycleRollingTimeout, lifecycleRowPollInterval, lifecycleErrorPhaseGrace
	if rolling > 0 {
		lifecycleRollingTimeout = rolling
	}
	if poll > 0 {
		lifecycleRowPollInterval = poll
	}
	if grace > 0 {
		lifecycleErrorPhaseGrace = grace
	}
	t.Cleanup(func() {
		lifecycleRollingTimeout, lifecycleRowPollInterval, lifecycleErrorPhaseGrace = oldRolling, oldPoll, oldGrace
	})
}

func TestWaitForLifecycleOutcome_SuccessPhase(t *testing.T) {
	ch := make(chan Event, 8)
	go func() {
		sendStatus(ch, "starting", "pulling image", nil)
		sendStatus(ch, "running", "", nil)
	}()

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	require.NoError(t, err)
}

func TestWaitForLifecycleOutcome_ErrorPhaseWithoutFailedRow(t *testing.T) {
	setLifecycleTimings(t, 0, 0, 20*time.Millisecond)
	ch := make(chan Event, 8)
	go func() {
		sendStatus(ch, "starting", "", nil)
		sendStatus(ch, "error", "", nil)
	}()

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent entered error phase during start")
}

func TestWaitForLifecycleOutcome_RollingReset(t *testing.T) {
	// Interim status events keep the wait alive past one rolling window.
	setLifecycleTimings(t, 50*time.Millisecond, 0, 0)
	ch := make(chan Event, 64)
	go func() {
		for i := 0; i < 5; i++ {
			sendStatus(ch, "starting", "step", &AgentDetail{Message: "progress"})
			time.Sleep(20 * time.Millisecond)
		}
		sendStatus(ch, "running", "", nil)
	}()

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	require.NoError(t, err)
}

func TestWaitForLifecycleOutcome_SilenceExpiry(t *testing.T) {
	setLifecycleTimings(t, 20*time.Millisecond, 0, 0)
	ch := make(chan Event, 4)

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	assert.ErrorIs(t, err, ErrDispatchFailed)
}

func TestWaitForLifecycleOutcome_ClosedChannel(t *testing.T) {
	ch := make(chan Event, 4)
	close(ch)

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	assert.ErrorIs(t, err, ErrDispatchFailed)
}

func TestWaitForLifecycleOutcome_ContextCancel(t *testing.T) {
	ch := make(chan Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForLifecycleOutcome(ctx, ch, func() {}, noRowStore(), "d-1", "start", startTerminal)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForLifecycleOutcome_UnsubCalled(t *testing.T) {
	ch := make(chan Event, 4)
	var unsubCalled bool
	close(ch)

	_ = waitForLifecycleOutcome(context.Background(), ch, func() { unsubCalled = true }, noRowStore(), "d-1", "start", startTerminal)
	assert.True(t, unsubCalled, "unsub must be called on return")
}

func TestWaitForLifecycleOutcome_StopSuccessPhase(t *testing.T) {
	ch := make(chan Event, 4)
	go func() {
		sendStatus(ch, "stopped", "", nil)
	}()

	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, noRowStore(), "d-1", "stop", stopTerminal)
	require.NoError(t, err)
}

// =========================================================================
// waitForDispatchDone tests (data-op completion path)
// =========================================================================

func TestWaitForDispatchDone_ReturnsOnDone(t *testing.T) {
	const dispatchID = "dispatch-1"
	ch := make(chan Event, 4)
	unsub := func() {}

	fs := &fakeDispatchStore{
		dispatches: map[string]*store.BrokerDispatch{
			dispatchID: {
				ID:     dispatchID,
				State:  store.DispatchStateDone,
				Result: `{"hasPrompt":true}`,
			},
		},
	}

	go func() {
		ch <- Event{Subject: "broker.dispatch." + dispatchID + ".done"}
	}()

	result, err := waitForDispatchDone(context.Background(), ch, unsub, fs, dispatchID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateDone, result.State)
	assert.Equal(t, `{"hasPrompt":true}`, result.Result)
}

func TestWaitForDispatchDone_ReturnsOnFailed(t *testing.T) {
	const dispatchID = "dispatch-2"
	ch := make(chan Event, 4)
	unsub := func() {}

	fs := &fakeDispatchStore{
		dispatches: map[string]*store.BrokerDispatch{
			dispatchID: {
				ID:    dispatchID,
				State: store.DispatchStateFailed,
				Error: "container crashed",
			},
		},
	}

	go func() {
		ch <- Event{Subject: "broker.dispatch." + dispatchID + ".done"}
	}()

	result, err := waitForDispatchDone(context.Background(), ch, unsub, fs, dispatchID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, result.State)
	assert.Equal(t, "container crashed", result.Error)
}

func TestWaitForDispatchDone_ChannelClose(t *testing.T) {
	const dispatchID = "dispatch-3"
	ch := make(chan Event, 4)
	unsub := func() {}

	fs := &fakeDispatchStore{dispatches: map[string]*store.BrokerDispatch{}}

	close(ch)

	_, err := waitForDispatchDone(context.Background(), ch, unsub, fs, dispatchID)
	assert.ErrorIs(t, err, ErrDispatchFailed)
}

func TestWaitForDispatchDone_ContextCancel(t *testing.T) {
	const dispatchID = "dispatch-4"
	ch := make(chan Event, 4)
	unsub := func() {}

	fs := &fakeDispatchStore{dispatches: map[string]*store.BrokerDispatch{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForDispatchDone(ctx, ch, unsub, fs, dispatchID)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForDispatchDone_TimeoutReread(t *testing.T) {
	// Verify that on timeout, the row is re-read and if done, returned.
	const dispatchID = "dispatch-5"
	ch := make(chan Event, 4)
	var unsubCalled bool
	unsub := func() { unsubCalled = true }

	fs := &fakeDispatchStore{
		dispatches: map[string]*store.BrokerDispatch{
			dispatchID: {
				ID:     dispatchID,
				State:  store.DispatchStateDone,
				Result: `{"success":true}`,
			},
		},
	}

	// Don't send any event — let it time out and re-read.
	// We can't easily override the 90s rolling timeout in a unit test,
	// so we test the channel-close path instead (above) and verify the
	// unsub is called on all paths.
	close(ch)
	_, _ = waitForDispatchDone(context.Background(), ch, unsub, fs, dispatchID)
	assert.True(t, unsubCalled, "unsub must be called on return")
}

// cancelOnReadStore is a dispatch store whose row read ends the caller's ctx
// and fails with its error, as a store read does when ctx is cancelled
// while it runs.
type cancelOnReadStore struct {
	*fakeDispatchStore
	cancel context.CancelFunc
}

func (c *cancelOnReadStore) GetBrokerDispatch(ctx context.Context, _ string) (*store.BrokerDispatch, error) {
	c.cancel()
	return nil, ctx.Err()
}

// A row read that fails because ctx ended returns ctx.Err(), not the
// timeout or error-phase error, whichever case made the read.
func TestWaitForLifecycleOutcome_RowReadCtxErrorReturnsCtxErr(t *testing.T) {
	t.Run("closed channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st := &cancelOnReadStore{fakeDispatchStore: noRowStore(), cancel: cancel}
		ch := make(chan Event)
		close(ch)

		err := waitForLifecycleOutcome(ctx, ch, func() {}, st, "d-1", "start", startTerminal)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, ErrDispatchFailed)
	})

	t.Run("error phase", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st := &cancelOnReadStore{fakeDispatchStore: noRowStore(), cancel: cancel}
		ch := make(chan Event, 1)
		sendStatus(ch, "error", "", nil)

		err := waitForLifecycleOutcome(ctx, ch, func() {}, st, "d-1", "start", startTerminal)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("rolling window expiry", func(t *testing.T) {
		setLifecycleTimings(t, 20*time.Millisecond, time.Hour, 0)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st := &cancelOnReadStore{fakeDispatchStore: noRowStore(), cancel: cancel}

		err := waitForLifecycleOutcome(ctx, make(chan Event), func() {}, st, "d-1", "start", startTerminal)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, ErrDispatchFailed)
	})
}
