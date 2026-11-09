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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A cross-node stop or delete clears the recorded native Cloud Run
// decision only once it is confirmed: a deferred stop whose wait ends on
// the stopped phase, or a deferred delete the owning node completes. A
// deferred stop whose wait ends on the error or halted phase or on a failed
// row, and a deferred delete the owning node fails, leave it set, so the
// GCP token path stays closed.
func TestDispatch_CloudRunNativeDeferredStopOrDelete(t *testing.T) {
	// A claimed row's error or halted phase waits this long for the row to
	// fail.
	setLifecycleTimings(t, 0, 0, 20*time.Millisecond)

	record := func(t *testing.T, st store.Store, a *store.Agent) {
		t.Helper()
		require.NoError(t, st.SetAgentNativeCloudRun(context.Background(), a.ID, true))
		require.True(t, crRecorded(t, st, a.ID))
		a.AppliedConfig = &store.AgentAppliedConfig{NativeCloudRun: true}
	}
	publishPhase := func(f *crossNodeFixture, phase state.Phase) {
		ended := *f.agent
		ended.Phase = string(phase)
		f.events.PublishAgentStatus(context.Background(), &ended)
	}
	// requireRecorded checks the decision is still recorded, in the store
	// and on the caller's copy.
	requireRecorded := func(t *testing.T, f *crossNodeFixture) {
		t.Helper()
		assert.True(t, crRecorded(t, f.store, f.agent.ID), "fails closed: the decision stays recorded")
		assert.True(t, f.agent.AppliedConfig.NativeCloudRun)
	}
	requireCleared := func(t *testing.T, f *crossNodeFixture) {
		t.Helper()
		assert.False(t, crRecorded(t, f.store, f.agent.ID))
		assert.False(t, f.agent.AppliedConfig.NativeCloudRun)
	}

	// Each stop case claims the pending row (the owning node never runs)
	// and then drives the wait's end. wantErr is the text the stop's error
	// carries; empty means the stop is confirmed.
	stopCases := []struct {
		name    string
		grace   time.Duration // a longer grace for this case, or 0
		drive   func(t *testing.T, f *crossNodeFixture, row store.BrokerDispatch)
		wantErr string
	}{
		{
			name:    "deferred stop ending on error",
			drive:   func(t *testing.T, f *crossNodeFixture, _ store.BrokerDispatch) { publishPhase(f, state.PhaseError) },
			wantErr: "agent entered error phase during stop",
		},
		{
			name:    "deferred stop ending on halted",
			drive:   func(t *testing.T, f *crossNodeFixture, _ store.BrokerDispatch) { publishPhase(f, state.PhaseHalted) },
			wantErr: "agent entered halted phase during stop",
		},
		{
			name:  "deferred stop ending on stopped",
			drive: func(t *testing.T, f *crossNodeFixture, _ store.BrokerDispatch) { publishPhase(f, state.PhaseStopped) },
		},
		{
			// A stopped phase after a halted one, within the grace, ends
			// the wait as confirmed: the instance did stop.
			name:  "deferred stop halted then stopped within the grace",
			grace: 5 * time.Second,
			drive: func(t *testing.T, f *crossNodeFixture, _ store.BrokerDispatch) {
				publishPhase(f, state.PhaseHalted)
				publishPhase(f, state.PhaseStopped)
			},
		},
		{
			// The owning node fails the row with no phase event.
			name: "deferred stop whose row fails",
			drive: func(t *testing.T, f *crossNodeFixture, row store.BrokerDispatch) {
				if f.failRow(t, row.ID, errors.New("owner stop failed")) {
					f.events.PublishDispatchDone(context.Background(), row.ID)
				}
			},
			wantErr: "owner stop failed",
		},
		{
			// An error phase, then the row fails inside the grace: the
			// row's failure is the outcome.
			name:  "deferred stop error phase then row fails within the grace",
			grace: 5 * time.Second,
			drive: func(t *testing.T, f *crossNodeFixture, row store.BrokerDispatch) {
				publishPhase(f, state.PhaseError)
				if f.failRow(t, row.ID, errors.New("owner stop failed")) {
					f.events.PublishDispatchDone(context.Background(), row.ID)
				}
			},
			wantErr: "owner stop failed",
		},
	}
	for _, tc := range stopCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.grace > 0 {
				setLifecycleTimings(t, 0, 0, tc.grace)
			}
			f := newCrossNodeFixture(t, nil, false)
			record(t, f.store, f.agent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				row, ok := f.claimPending(t)
				if !ok {
					return
				}
				tc.drive(t, f, row)
			}()

			err := f.requester.DispatchAgentStop(crossNodeCtx(t), f.agent)
			<-done
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.NotErrorIs(t, err, context.DeadlineExceeded)
				assert.ErrorContains(t, err, tc.wantErr)
				requireRecorded(t, f)
				return
			}
			require.NoError(t, err)
			requireCleared(t, f)
		})
	}

	for _, tc := range []struct {
		name    string
		wantErr string // empty means the owning node completes the delete
	}{
		{name: "deferred delete the owner fails", wantErr: "owner delete failed"},
		{name: "deferred delete the owner completes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := entadapter.NewCompositeStore(enttest.NewClient(t))
			events := NewChannelEventPublisher()
			t.Cleanup(events.Close)
			requester := NewHTTPAgentDispatcherWithClient(cs, &deferredDataOpTestClient{localBroker: "local-broker"}, false, slog.Default())
			requester.SetCrossNodeDeps(events, NoopCommandBus{})
			f := &crossNodeFixture{store: cs, events: events, requester: requester, agent: seedAgentWithBrokerID(t, cs, uuid.NewString())}
			record(t, f.store, f.agent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				row, ok := f.claimPending(t)
				if !ok {
					return
				}
				if tc.wantErr == "" {
					if !assert.NoError(t, f.store.CompleteBrokerDispatch(context.Background(), row.ID, "")) {
						return
					}
				} else if !f.failRow(t, row.ID, errors.New(tc.wantErr)) {
					return
				}
				f.events.PublishDispatchDone(context.Background(), row.ID)
			}()

			err := f.requester.DispatchAgentDelete(crossNodeCtx(t), f.agent, false, false, false, time.Time{})
			<-done
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.NotErrorIs(t, err, context.DeadlineExceeded)
				assert.ErrorContains(t, err, tc.wantErr)
				requireRecorded(t, f)
				return
			}
			require.NoError(t, err)
			requireCleared(t, f)
		})
	}
}
