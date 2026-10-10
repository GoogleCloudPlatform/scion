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
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2661: the stops of a stop-all no longer follow the client's
// request. A client that disconnects or gives up while the stops are in
// flight must not cancel them; each agent's broker work is still bounded by
// stopAllAgentOpTimeout.

// stopAllProbeDispatcher holds each stop dispatch until all want stops are
// in flight, then cancels the client's request (in cancel mode) and records
// each stop's ctx. Safe for the stop-all's parallel stops.
type stopAllProbeDispatcher struct {
	createAgentDispatcher
	want          int
	block         bool
	cancelRequest context.CancelFunc

	mu          sync.Mutex
	arrived     int
	allIn       chan struct{}
	ctxErrs     []error
	deadlineIn  []time.Duration
	hadDeadline []bool
}

func newStopAllProbeDispatcher(want int, block bool) *stopAllProbeDispatcher {
	return &stopAllProbeDispatcher{want: want, block: block, allIn: make(chan struct{})}
}

func (d *stopAllProbeDispatcher) DispatchAgentStop(ctx context.Context, _ *store.Agent) error {
	deadline, ok := ctx.Deadline()
	d.mu.Lock()
	d.arrived++
	if d.arrived == d.want {
		if d.cancelRequest != nil {
			d.cancelRequest()
		}
		close(d.allIn)
	}
	d.mu.Unlock()

	var err error
	select {
	case <-d.allIn:
		if d.block {
			err = errProbeNeverDone
			if awaitCanceled(ctx) {
				err = ctx.Err()
			}
		} else {
			err = ctx.Err()
		}
	case <-time.After(requestCancelWait):
		err = errProbeNeverDone
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.ctxErrs = append(d.ctxErrs, err)
	d.hadDeadline = append(d.hadDeadline, ok)
	var in time.Duration
	if ok {
		in = time.Until(deadline)
	}
	d.deadlineIn = append(d.deadlineIn, in)
	return err
}

func TestStopAll_ClientCancelDuringStops_AllAgentsStop(t *testing.T) {
	const n = 3
	disp := newStopAllProbeDispatcher(n, false)
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	var ids []string
	for i := 0; i < n; i++ {
		a := createSiteAgent(t, s, project, "cancel-stop-all-"+string(rune('a'+i)), state.PhaseRunning, store.RunIntentRunning)
		ids = append(ids, a.ID)
	}

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	disp.cancelRequest = cancel
	rec := serve()

	require.Len(t, disp.ctxErrs, n, "every agent's stop is dispatched")
	for i, err := range disp.ctxErrs {
		assert.NoError(t, err, "stop %d must not follow the canceled request", i)
		assert.True(t, disp.hadDeadline[i], "stop %d must run under the per-agent op timeout", i)
		assert.LessOrEqual(t, disp.deadlineIn[i], stopAllAgentOpTimeout, "stop %d deadline is later than the per-agent op timeout", i)
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, n, resp.Stopped, rec.Body.String())
	assert.Equal(t, 0, resp.Failed, rec.Body.String())
	for _, id := range ids {
		got, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase, "the stopped status write must land despite the cancel")
		assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	}
}

// Detached, each agent's broker work is still bounded: a stop dispatch that
// never answers ends at stopAllAgentOpTimeout and is reported as failed.
func TestStopAll_DispatchHonoursPerAgentOpTimeout(t *testing.T) {
	const n = 2
	disp := newStopAllProbeDispatcher(n, true)
	srv, s, project := setupCreateAgentServer(t, disp)
	setStopAllAgentOpTimeout(t, 100*time.Millisecond)
	for i := 0; i < n; i++ {
		createSiteAgent(t, s, project, "timeout-stop-all-"+string(rune('a'+i)), state.PhaseRunning, store.RunIntentRunning)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Len(t, disp.ctxErrs, n)
	for i, err := range disp.ctxErrs {
		assert.ErrorIs(t, err, context.DeadlineExceeded, "stop %d must end at the per-agent op timeout", i)
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, n, resp.Failed, rec.Body.String())
}

// slowAgentStopDispatcher holds the stop dispatch of one agent until its
// ctx is done and stops every other agent at once.
type slowAgentStopDispatcher struct {
	createAgentDispatcher
	slowID string
}

func (d *slowAgentStopDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if agent.ID != d.slowID {
		return nil
	}
	if !awaitCanceled(ctx) {
		return errProbeNeverDone
	}
	return ctx.Err()
}

// One agent's stop outlasts the per-agent op timeout while the others stop:
// the slow one is reported as failed and left as it was, and the others are
// reported and recorded as stopped.
func TestStopAll_OneAgentTimesOut_OthersStop(t *testing.T) {
	disp := &slowAgentStopDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	setStopAllAgentOpTimeout(t, 150*time.Millisecond)
	setAgentQuotaLimits(t, s)
	slow := createSiteAgent(t, s, project, "mixed-stop-all-slow", state.PhaseRunning, store.RunIntentRunning)
	disp.slowID = slow.ID
	fast1 := createSiteAgent(t, s, project, "mixed-stop-all-fast-1", state.PhaseRunning, store.RunIntentRunning)
	fast2 := createSiteAgent(t, s, project, "mixed-stop-all-fast-2", state.PhaseRunning, store.RunIntentRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Total, rec.Body.String())
	assert.Equal(t, 2, resp.Stopped, rec.Body.String())
	assert.Equal(t, 1, resp.Failed, rec.Body.String())

	byID := map[string]stopAllResult{}
	for _, r := range resp.Results {
		byID[r.ID] = r
	}
	require.Contains(t, byID, slow.ID)
	assert.Equal(t, "error", byID[slow.ID].Status)
	assert.Contains(t, byID[slow.ID].Error, context.DeadlineExceeded.Error())
	for _, id := range []string{fast1.ID, fast2.ID} {
		require.Contains(t, byID, id)
		assert.Equal(t, "stopped", byID[id].Status)
		got, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase, "a stopped agent records stopped")
		assert.Equal(t, "stopped", got.ContainerStatus)
	}
	got, err := s.GetAgent(context.Background(), slow.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the timed-out agent keeps its phase")
}

// slowStopIntentStore delays the stop intent write, a pre-dispatch store
// write of each stop-all stop, once its fault switch is armed.
type slowStopIntentStore struct {
	store.Store
	fault *storeFaultSwitch
	delay time.Duration

	mu sync.Mutex
	// firstEntry is when the first stop intent write began.
	firstEntry time.Time
}

func (s *slowStopIntentStore) SwapRunIntent(ctx context.Context, agentID string, intent store.RunIntent) (store.RunIntent, time.Time, error) {
	if intent == store.RunIntentStopped && s.fault.Active() {
		now := time.Now()
		s.mu.Lock()
		if s.firstEntry.IsZero() || now.Before(s.firstEntry) {
			s.firstEntry = now
		}
		s.mu.Unlock()
		time.Sleep(s.delay)
	}
	return s.Store.SwapRunIntent(ctx, agentID, intent)
}

// deadlineProbeDispatcher records the deadline of each stop dispatch.
type deadlineProbeDispatcher struct {
	createAgentDispatcher
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineProbeDispatcher) DispatchAgentStop(ctx context.Context, _ *store.Agent) error {
	deadline, _ := ctx.Deadline()
	d.mu.Lock()
	d.deadlines = append(d.deadlines, deadline)
	d.mu.Unlock()
	return nil
}

// Every agent's broker work shares one deadline, stopAllAgentOpTimeout after
// the stops begin: a slow pre-dispatch store write takes broker time, not
// the slack the status write needs.
func TestStopAll_SharedOpDeadline_SlowPreDispatchWrite(t *testing.T) {
	const writeDelay = 300 * time.Millisecond
	disp := &deadlineProbeDispatcher{}
	srv, s, slow, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *slowStopIntentStore {
		return &slowStopIntentStore{Store: inner, fault: f, delay: writeDelay}
	})
	project := setupCreateAgentProject(t, s)
	srv.SetDispatcher(disp)
	setStopAllAgentOpTimeout(t, 2*time.Second)
	createSiteAgent(t, s, project, "shared-deadline-a", state.PhaseRunning, store.RunIntentRunning)
	createSiteAgent(t, s, project, "shared-deadline-b", state.PhaseRunning, store.RunIntentRunning)
	fault.Arm()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, disp.deadlines, 2)
	require.False(t, slow.firstEntry.IsZero(), "fixture check: the stop intent write ran")
	// The shared deadline is taken before any agent's stop intent write
	// begins, so it is at most stopAllAgentOpTimeout after the first one;
	// a per-agent timeout taken after the slow write would be later.
	limit := slow.firstEntry.Add(stopAllAgentOpTimeout)
	for i, d := range disp.deadlines {
		require.False(t, d.IsZero(), "stop %d must run under the op deadline", i)
		assert.False(t, d.After(limit),
			"stop %d deadline is %v past the first stop intent write plus the op timeout: the pre-dispatch write took slack time", i, d.Sub(limit))
	}
	assert.True(t, disp.deadlines[0].Equal(disp.deadlines[1]), "every agent shares one op deadline")
}
