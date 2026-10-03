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
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runIntentDispatcher counts start and stop dispatches and can fail stops.
type runIntentDispatcher struct {
	createAgentDispatcher
	starts  atomic.Int32
	stops   atomic.Int32
	stopErr error
}

// DispatchAgentStart applies a running phase to agent, as a broker's start
// response does.
func (d *runIntentDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.starts.Add(1)
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *runIntentDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stops.Add(1)
	return d.stopErr
}

func requireRunIntent(t *testing.T, s store.Store, agentID string, want store.RunIntent) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.Equal(t, want, a.RunIntent)
	require.NotNil(t, a.RunIntentAt)
	return a
}

func TestRunIntent_LifecycleActionsRecordIntent(t *testing.T) {
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-lc")
	path := "/api/v1/agents/" + agent.ID

	rec := doRequest(t, srv, http.MethodPost, path+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stopped := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)

	rec = doRequest(t, srv, http.MethodPost, path+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	started := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.True(t, started.RunIntentAt.After(*stopped.RunIntentAt))

	rec = doRequest(t, srv, http.MethodPost, path+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	restarted := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.True(t, restarted.RunIntentAt.After(*started.RunIntentAt), "restart records a new intent")

	rec = doRequest(t, srv, http.MethodPost, path+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
}

// A user stop keeps its intent when the dispatch fails.
func TestRunIntent_UserStopKeepsIntentOnDispatchFailure(t *testing.T) {
	srv, s := testServer(t)
	disp := &runIntentDispatcher{stopErr: errors.New("broker refused")}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-stopfail")
	_, err := s.SetRunIntent(context.Background(), agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
}

func TestRunIntent_DeleteRecordsStoppedBeforeDispatch(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&deleteDispatcher{deleteErr: errors.New("broker refused")})
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-del")
	_, err := s.SetRunIntent(context.Background(), agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
}

func TestRunIntent_OfflineStopIsQueued(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	bus := &recordingCommandBus{}
	srv.commandBus = bus
	_, broker, agent := setupOfflineBrokerAgent(t, s, "ri-q")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, []string{broker.ID}, bus.signaled(), "the queued stop wakes the broker's drain")
	var resp struct {
		Phase           string   `json:"phase"`
		ContainerStatus string   `json:"containerStatus"`
		Warnings        []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, string(state.PhaseStopped), resp.Phase)
	assert.Equal(t, []string{offlineStopMessage}, resp.Warnings)
	assert.Zero(t, disp.stops.Load(), "nothing is dispatched while the broker is offline")

	got := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus)
	assert.Equal(t, offlineStopMessage, got.Message)

	pending, err := s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "stop", pending[0].Op)
	args, err := UnmarshalStopArgs(pending[0].Args)
	require.NoError(t, err)
	require.NotNil(t, args.IntentAt)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, *args.IntentAt), "row carries the stop's run_intent_at")

	// The broker reconnects and the drain applies the stop.
	srv.reconcileBroker(ctx, broker.ID)
	assert.Equal(t, int32(1), disp.stops.Load())
	pending, err = s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, pending)
	got = requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.Equal(t, "stopped", got.ContainerStatus)
}

// A queued stop is drained locally when this node holds the broker's
// control channel, with no command bus to carry the signal.
func TestRunIntent_OfflineStopDrainsLocallyWhenBrokerConnected(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	srv.commandBus = nil
	_, broker, agent := setupOfflineBrokerAgent(t, s, "ri-local")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	pending, err := s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the stop is queued while the broker is not connected")

	// The broker connects to this node just after the stop was queued.
	require.NotNil(t, srv.controlChannel)
	srv.controlChannel.mu.Lock()
	srv.controlChannel.connections[broker.ID] = &BrokerConnection{brokerID: broker.ID, streams: map[string]*StreamProxy{}}
	srv.controlChannel.mu.Unlock()
	t.Cleanup(func() {
		srv.controlChannel.mu.Lock()
		delete(srv.controlChannel.connections, broker.ID)
		srv.controlChannel.mu.Unlock()
	})

	srv.wakeBrokerDrain(ctx, broker.ID)
	require.Eventually(t, func() bool {
		p, err := s.ListPendingDispatch(ctx, broker.ID)
		return err == nil && len(p) == 0 && disp.stops.Load() == 1
	}, 10*time.Second, 20*time.Millisecond, "the local drain applies the queued stop")
	got := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.Equal(t, "stopped", got.ContainerStatus)
}

// A start recorded after an offline stop, before the broker's reconnect
// drain runs, wins: the queued stop is not applied.
func TestRunIntent_OfflineStopSupersededByStartBeforeDrain(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, broker, agent := setupOfflineBrokerAgent(t, s, "ri-sup")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Reconnect: the broker is online, and the user starts the agent before
	// the drain runs.
	b, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	b.Status = store.BrokerStatusOnline
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(1), disp.starts.Load())

	srv.reconcileBroker(ctx, broker.ID)

	assert.Zero(t, disp.stops.Load(), "the superseded stop must not be dispatched")
	got := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	pending, err := s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, pending, "the superseded row completes")
}

// A queued stop row without an intent time (an online cross-node stop) is
// applied as before.
func TestRunIntent_QueuedStopWithoutIntentAtIsApplied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-plain")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	args, err := MarshalDispatchArgs(StopDispatchArgs{})
	require.NoError(t, err)
	result, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: agent.ID, Op: "stop", Args: args})
	require.NoError(t, err)
	assert.Empty(t, result)
	assert.Equal(t, int32(1), disp.stops.Load())
}

func TestRunIntent_StopAllCoversIntentOnlyAgents(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	project, broker, running := setupOnlineBrokerAgent(t, s, "ri-sa")
	_, err := s.SetRunIntent(ctx, running.ID, store.RunIntentRunning)
	require.NoError(t, err)

	// An agent in phase error whose intent is still running.
	errored := &store.Agent{
		ID:              tid("agent-ri-sa-err"),
		Slug:            "agent-ri-sa-err",
		Name:            "agent-ri-sa-err",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, errored))
	_, err = s.SetRunIntent(ctx, errored.ID, store.RunIntentRunning)
	require.NoError(t, err)

	// An agent whose start is in flight, intent running.
	starting := &store.Agent{
		ID:              tid("agent-ri-sa-starting"),
		Slug:            "agent-ri-sa-starting",
		Name:            "agent-ri-sa-starting",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStarting),
	}
	require.NoError(t, s.CreateAgent(ctx, starting))
	_, err = s.SetRunIntent(ctx, starting.ID, store.RunIntentRunning)
	require.NoError(t, err)

	// A stopped agent whose intent is stopped is not selected.
	idle := &store.Agent{
		ID:              tid("agent-ri-sa-idle"),
		Slug:            "agent-ri-sa-idle",
		Name:            "agent-ri-sa-idle",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, idle))
	idleAt, err := s.SetRunIntent(ctx, idle.ID, store.RunIntentStopped)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Total)
	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 1, resp.StopRecorded)
	assert.Zero(t, resp.Failed)
	statusByID := map[string]string{}
	for _, r := range resp.Results {
		statusByID[r.ID] = r.Status
	}
	assert.Equal(t, "stopped", statusByID[running.ID])
	assert.Equal(t, "stopped", statusByID[errored.ID])
	assert.Equal(t, stopAllStatusStopRecorded, statusByID[starting.ID], "an in-flight start is reported as stop_recorded")

	assert.Equal(t, int32(1), disp.stops.Load(), "only the running agent is dispatched a stop")
	requireRunIntent(t, s, running.ID, store.RunIntentStopped)
	got := requireRunIntent(t, s, errored.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseError), got.Phase, "an intent-only stop leaves the phase alone")
	gotStarting := requireRunIntent(t, s, starting.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseStarting), gotStarting.Phase, "the in-flight start is not interrupted")
	gotIdle := requireRunIntent(t, s, idle.ID, store.RunIntentStopped)
	assert.True(t, gotIdle.RunIntentMatches(store.RunIntentStopped, idleAt), "an unselected agent is not written")
}

func TestRunIntent_AutoSuspendRevertsIntentOnDispatchFailure(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{stopErr: errors.New("broker refused")}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-as")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Equal(t, int32(1), disp.stops.Load())
	got := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.True(t, got.RunIntentAt.After(*loaded.RunIntentAt), "the revert keeps the stop's run_intent_at")
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}

// A user stop whose dispatch failed leaves phase running and intent stopped.
// If auto-suspend then also fails to stop the agent, the intent it puts back
// is the prior stopped one, not running.
func TestRunIntent_AutoSuspendKeepsPriorStoppedIntentOnDispatchFailure(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{stopErr: errors.New("broker refused")}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-as-prior")
	userStopAt, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseRunning), loaded.Phase)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Equal(t, int32(1), disp.stops.Load())
	got := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.True(t, got.RunIntentAt.After(userStopAt), "auto-suspend wrote its own stop")
}

func TestRunIntent_AutoSuspendRecordsStopped(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "ri-as-ok")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	got := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// recordingCommandBus records the brokers SignalBrokerCmd was called for.
type recordingCommandBus struct {
	NoopCommandBus
	mu      sync.Mutex
	signals []string
}

func (b *recordingCommandBus) SignalBrokerCmd(_ context.Context, brokerID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signals = append(b.signals, brokerID)
	return nil
}

func (b *recordingCommandBus) signaled() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.signals...)
}
