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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4211: a single-agent stop or suspend no longer follows the
// client's request. A client that gives up mid-dispatch (the CLI's 30s
// timeout) must not cancel the stop dispatch or the stopped (or suspended)
// status write after it. The dispatch is bounded by syncDispatch instead.
// These tests are not parallel: they share package-level timeouts.

func TestLifecycleStop_ClientCancelDuringDispatch_StopCompletes(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "cancel-stop", state.PhaseRunning, store.RunIntentRunning)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	disp.cancelRequest = cancel
	rec := serve()

	assert.Equal(t, 1, disp.stopCalls)
	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "the stopped status write must land despite the cancel")
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
}

func TestLifecycleSuspend_ClientCancelDuringDispatch_SuspendCompletes(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "cancel-suspend", state.PhaseRunning, store.RunIntentRunning)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	disp.cancelRequest = cancel
	rec := serve()

	assert.Equal(t, 1, disp.stopCalls)
	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the suspended status write must land despite the cancel")
	assert.Equal(t, "stopped", got.ContainerStatus)
}

// The detached stop is still bounded: a stop dispatch that never answers
// ends at syncDispatchTimeout, and the stop answers with the failure rather
// than hanging.
func TestLifecycleStop_DispatchHonoursOwnTimeout(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			shortenSyncDispatchTimeout(t, 50*time.Millisecond)
			disp := &launchProbeDispatcher{mode: probeBlock}
			srv, s, project := setupCreateAgentServer(t, disp)
			agent := createSiteAgent(t, s, project, "timeout-"+action, state.PhaseRunning, store.RunIntentRunning)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Len(t, disp.ctxErrs, 1)
			require.ErrorIs(t, disp.ctxErrs[0], context.DeadlineExceeded, "the stop dispatch ctx must be done at its deadline")
			assertSyncDispatchDeadline(t, disp, 0)
			assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
		})
	}
}

// The whole detached stop is bounded by the stop's write budget, so its
// work ends no later than the response's write deadline.
func TestDetachStopFromClient_KeepsValuesDropsCancelBounded(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	ctx, stop := detachStopFromClient(parent, time.Minute)
	defer stop()
	cancel()
	assert.NoError(t, ctx.Err(), "the stop must not follow the canceled request")
	assert.Equal(t, "v", ctx.Value(key{}), "the stop keeps the request's values")
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "the stop is bounded")
	assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, 5*time.Second)
}

// cancelOnDispatchInsertStore cancels the client's request when the
// offline-broker stop queues its durable dispatch, then answers as a
// ctx-aware store does: a canceled ctx fails the write.
type cancelOnDispatchInsertStore struct {
	store.Store
	cancelRequest context.CancelFunc
	insertCtxErr  error
}

func (c *cancelOnDispatchInsertStore) InsertBrokerDispatch(ctx context.Context, d *store.BrokerDispatch) error {
	c.cancelRequest()
	c.insertCtxErr = ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Store.InsertBrokerDispatch(ctx, d)
}

// The broker is offline: the stop is queued for its reconnect. A client
// that gives up while the stop is being queued must not cancel the durable
// dispatch or the stop_queued status write.
func TestLifecycleStop_OfflineBroker_ClientCancelWhileQueueing_StopQueued(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetDispatcher(&runIntentDispatcher{})
	srv.commandBus = &recordingCommandBus{}
	_, broker, agent := setupOfflineBrokerAgent(t, s, "cancel-q")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	hook := &cancelOnDispatchInsertStore{Store: srv.store, cancelRequest: cancel}
	srv.store = hook
	rec := serve()

	assert.NoError(t, hook.insertCtxErr, "the queued dispatch must not follow the canceled request")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	pending, err := s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the stop is queued for the broker's reconnect")
	assert.Equal(t, "stop", pending[0].Op)
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "the stop_queued status write must land despite the cancel")
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
}
