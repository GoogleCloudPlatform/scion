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

// ptone/scion#2014: a start-type dispatch holds its broker reservation for
// the whole dispatch leg (beginStartDispatch), and a heartbeat that reports
// the old container mid-dispatch does not undo that (heartbeatPhaseGuarded).

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookedStartDispatcher is a quotaLifecycleDispatcher that calls onStart /
// onStop from inside the dispatch, before it returns, and can fail the
// start.
type hookedStartDispatcher struct {
	quotaLifecycleDispatcher
	onStart   func(agent *store.Agent)
	onStop    func(agent *store.Agent)
	onDelete  func(agent *store.Agent)
	failStart bool
	// startErr, when set, is the error a failing start returns.
	startErr error
}

func (d *hookedStartDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	if d.onStart != nil {
		d.onStart(agent)
	}
	if d.failStart {
		if d.startErr != nil {
			return d.startErr
		}
		return errors.New("simulated broker start failure")
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

func (d *hookedStartDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, soft bool, startedAt time.Time) error {
	if d.onDelete != nil {
		d.onDelete(agent)
	}
	return d.quotaLifecycleDispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch, soft, startedAt)
}

func (d *hookedStartDispatcher) DispatchAgentStop(_ context.Context, agent *store.Agent) error {
	d.stopCount.Add(1)
	if d.onStop != nil {
		d.onStop(agent)
	}
	agent.Phase = string(state.PhaseStopped)
	agent.ContainerStatus = "stopped"
	return nil
}

// startSiteCase drives one start-type dispatch site against an agent the
// setup put in the site's starting phase, with no broker reservation.
type startSiteCase struct {
	name string
	// setup returns the server, store, the agent's broker and agent ID, and
	// run, which performs the start and reports whether it succeeded.
	setup func(t *testing.T, disp *hookedStartDispatcher, async bool) (srv *Server, s store.Store, broker *store.RuntimeBroker, agentID string, run func() bool)
	// affectedByAsync is true for the sites that sit in the create handler.
	affectedByAsync bool
}

// directStartSite is a site driven against an agent created in the store:
// the HTTP start and restart actions and the DM wake.
func directStartSite(name string, phase state.Phase, run func(t *testing.T, srv *Server, a *store.Agent) bool) startSiteCase {
	return startSiteCase{name: name, setup: func(t *testing.T, disp *hookedStartDispatcher, async bool) (*Server, store.Store, *store.RuntimeBroker, string, func() bool) {
		srv, s := testServer(t)
		srv.config.AsyncAgentLaunch = async
		srv.SetDispatcher(disp)
		setBrokerAgentCeiling(t, s, 3)
		sfx := fmt.Sprintf("sd-%s-%v", name, async)
		broker, project := newQuotaTestBrokerAndProject(t, s, sfx)
		a := newQuotaTestAgent(t, s, broker, project, sfx, phase)
		return srv, s, broker, a.ID, func() bool { return run(t, srv, a) }
	}}
}

// createExistingStartSite is the create handler's existing-agent branch:
// an agent created over HTTP, moved to phase by action ("suspend" or
// "stop"), then created again with resume.
func createExistingStartSite(name, action string) startSiteCase {
	return startSiteCase{name: name, affectedByAsync: true, setup: func(t *testing.T, disp *hookedStartDispatcher, async bool) (*Server, store.Store, *store.RuntimeBroker, string, func() bool) {
		srv, s, project := setupCreateAgentServer(t, disp)
		srv.config.AsyncAgentLaunch = async
		srv.SetDispatcher(disp)
		setBrokerAgentCeiling(t, s, 3)
		ctx := context.Background()
		agentName := "sd-" + name
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: agentName, ProjectID: project.ID})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var cr CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cr))
		require.NoError(t, s.UpdateAgentStatus(ctx, cr.Agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cr.Agent.ID+"/"+action, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
		require.NoError(t, err)
		require.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), action+" released the slot")
		disp.startCount.Store(0)
		return srv, s, broker, cr.Agent.ID, func() bool {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: agentName, ProjectID: project.ID, Resume: true})
			return rec.Code < 300
		}
	}}
}

func startSiteCases() []startSiteCase {
	httpAction := func(action string) func(t *testing.T, srv *Server, a *store.Agent) bool {
		return func(t *testing.T, srv *Server, a *store.Agent) bool {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)
			return rec.Code == http.StatusOK
		}
	}
	wake := func(t *testing.T, srv *Server, a *store.Agent) bool {
		_, dmErr := srv.wakeAgentForDM(context.Background(), a)
		return dmErr == nil
	}
	return []startSiteCase{
		directStartSite("start", state.PhaseStopped, httpAction("start")),
		directStartSite("start-error", state.PhaseError, httpAction("start")),
		directStartSite("restart", state.PhaseStopped, httpAction("restart")),
		directStartSite("wake", state.PhaseSuspended, wake),
		createExistingStartSite("create-resume-suspended", "suspend"),
		createExistingStartSite("create-start-stopped", "stop"),
	}
}

// forEachStartSite runs f for every site, and for the create sites with
// async agent launch both off and on.
func forEachStartSite(t *testing.T, f func(t *testing.T, site startSiteCase, async bool)) {
	for _, site := range startSiteCases() {
		asyncModes := []bool{false}
		if site.affectedByAsync {
			asyncModes = []bool{false, true}
		}
		for _, async := range asyncModes {
			t.Run(fmt.Sprintf("%s/async=%v", site.name, async), func(t *testing.T) { f(t, site, async) })
		}
	}
}

// markReady makes the wake's readiness wait succeed: it waits for the agent
// to report an activity.
func markReady(t *testing.T, s store.Store, agentID string) {
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{Activity: string(state.ActivityWorking)}))
}

// A reconcile tick in the middle of the dispatch keeps the reservation, even
// one the start reuses that is far older than reconcileMinReservationAge
// (the reconcile's only time input is the reservation's created_at, so this
// is also a dispatch that has run past the grace window), and sees phase
// starting. The dispatcher still gets the pre-dispatch phase in memory,
// which its revoke-on-failure decision reads.
func TestStartDispatch_ReconcileMidDispatchKeepsReservation(t *testing.T) {
	forEachStartSite(t, func(t *testing.T, site startSiteCase, async bool) {
		disp := &hookedStartDispatcher{}
		srv, s, broker, agentID, run := site.setup(t, disp, async)
		ctx := context.Background()
		before, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		reserveStaleBrokerSlot(t, s, broker, agentID)
		staleIDs := brokerReservationIDs(t, s, broker.ID)

		var held bool
		var midPhase, memPhase string
		disp.onStart = func(a *store.Agent) {
			memPhase = a.Phase
			srv.ReconcileStaleBrokerQuotaReservations(ctx)
			held = hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID)
			got, err := s.GetAgent(ctx, agentID)
			require.NoError(t, err)
			midPhase = got.Phase
			markReady(t, s, agentID)
		}
		require.True(t, run(), "start succeeds")
		require.EqualValues(t, 1, disp.startCount.Load())
		assert.True(t, held, "reconcile mid-dispatch must keep the reservation")
		assert.Equal(t, string(state.PhaseStarting), midPhase, "the row reads starting during the dispatch")
		assert.Equal(t, before.Phase, memPhase, "the dispatcher sees the pre-dispatch phase in memory (revoke decision unchanged)")
		assert.Equal(t, staleIDs, brokerReservationIDs(t, s, broker.ID), "the existing reservation is reused, not replaced")
	})
}

// A failed dispatch restores the prior phase and releases the reservation
// the start created, once: the other agent's reservation on the broker is
// untouched, and the broker count drops back to it.
func TestStartDispatch_FailedDispatchRestoresPhaseAndReleases(t *testing.T) {
	forEachStartSite(t, func(t *testing.T, site startSiteCase, async bool) {
		disp := &hookedStartDispatcher{failStart: true}
		srv, s, broker, agentID, run := site.setup(t, disp, async)
		ctx := context.Background()
		_ = srv
		before, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		other := newQuotaTestAgent(t, s, broker, mustProject(t, s, before.ProjectID), "sd-other-"+site.name, state.PhaseRunning)
		reserveBrokerSlot(t, s, broker, other.ID)

		var midReserved bool
		disp.onStart = func(*store.Agent) {
			midReserved = hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID)
		}
		require.False(t, run(), "start fails")
		require.EqualValues(t, 1, disp.startCount.Load())
		assert.True(t, midReserved, "the start reserved before dispatching")

		got, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		want := before.Phase
		if site.name == "restart" {
			// The stop leg succeeded: a restart records the agent stopped,
			// as before ptone/scion#2014.
			want = string(state.PhaseStopped)
		}
		assert.Equal(t, want, got.Phase, "phase restored")
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID), "the start's reservation is released")
		assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, other.ID), "another agent's reservation is untouched")
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
	})
}

func mustProject(t *testing.T, s store.Store, id string) *store.Project {
	t.Helper()
	p, err := s.GetProject(context.Background(), id)
	require.NoError(t, err)
	return p
}

// A failed start that reused an existing reservation keeps it (it did not
// create it), and restores the phase.
func TestStartDispatch_FailedDispatchKeepsReusedReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-reuse-fail")
	a := newQuotaTestAgent(t, s, broker, project, "sd-reuse-fail", state.PhaseStopped)
	reserveStaleBrokerSlot(t, s, broker, a.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// rollback restores the prior phase only over its own "starting": a phase
// written by someone else during the dispatch is kept.
func TestStartDispatch_RollbackKeepsNewerPhase(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-newer")
	a := newQuotaTestAgent(t, s, broker, project, "sd-newer", state.PhaseStopped)
	ctx := context.Background()
	disp.onStart = func(*store.Agent) {
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseError)}))
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseError), got.Phase, "a newer phase is not overwritten by the restore")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
}

// An agent already in a counted phase is not marked starting: restart of a
// running agent keeps phase running through both legs, as before.
func TestStartDispatch_CountedPhaseNotMarked(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-counted")
	a := newQuotaTestAgent(t, s, broker, project, "sd-counted", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	ctx := context.Background()
	var midPhase string
	disp.onStart = func(*store.Agent) {
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		midPhase = got.Phase
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseRunning), midPhase)
}

// The store clears the stale stop/crash message on a stopped/error ->
// running write. With "starting" written in between, the start's final
// write clears it explicitly (ClearTerminalRemnants).
func TestStartDispatch_StartClearsPriorStopMessage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		phase      state.Phase
		newMessage string
	}{
		{"stopped", state.PhaseStopped, ""},
		{"error", state.PhaseError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &hookedStartDispatcher{}
			srv.SetDispatcher(disp)
			setBrokerAgentCeiling(t, s, 2)
			ctx := context.Background()
			broker, project := newQuotaTestBrokerAndProject(t, s, "sd-msg-"+tc.name)
			a := newQuotaTestAgent(t, s, broker, project, "sd-msg-"+tc.name, tc.phase)
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Message: "Agent crashed with exit code 1"}))
			if tc.newMessage != "" {
				disp.onStart = func(*store.Agent) {
					require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Message: tc.newMessage}))
				}
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
			assert.Equal(t, tc.newMessage, got.Message)
		})
	}
}

// postStoppedHeartbeat posts a broker heartbeat reporting agent's container
// exited (phase stopped, exit code 137, reason crashed).
func postStoppedHeartbeat(t *testing.T, srv *Server, brokerID string, a *store.Agent) {
	t.Helper()
	code := 137
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:  a.ProjectID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:            a.Slug,
				Phase:           string(state.PhaseStopped),
				ContainerStatus: "Exited (137)",
				ExitCode:        &code,
				ExitReason:      string(state.ExitReasonCrashed),
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A heartbeat reporting the old container stopped while a start of a
// stopped agent is dispatching leaves phase starting and the reservation
// held; its exit code and reason are still recorded.
func TestHeartbeatPhaseGuard_StoppedReportMidStart(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "hbg-start")
	a := newQuotaTestAgent(t, s, broker, project, "hbg-start", state.PhaseStopped)

	var mid *store.Agent
	var held bool
	disp.onStart = func(*store.Agent) {
		postStoppedHeartbeat(t, srv, broker.ID, a)
		var err error
		mid, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		held = hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, mid)
	assert.Equal(t, string(state.PhaseStarting), mid.Phase, "the heartbeat must not move the row off starting mid-dispatch")
	assert.True(t, held, "the heartbeat must not release the reservation mid-dispatch")
	assert.Equal(t, string(state.ExitReasonCrashed), mid.ExitReason, "the exit reason is still recorded")
	require.NotNil(t, mid.ExitCode)
	assert.Equal(t, 137, *mid.ExitCode)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.Empty(t, got.Message, "the old container's exit message does not survive the start")
	assert.Empty(t, got.ExitReason)
	assert.Nil(t, got.ExitCode)
}

// Restart window: between the stop and start legs the row still reads
// running; a heartbeat reporting the stopped container does not apply and
// does not release the slot.
func TestHeartbeatPhaseGuard_StoppedReportDuringRestart(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "hbg-restart")
	a := newQuotaTestAgent(t, s, broker, project, "hbg-restart", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)

	var midPhase string
	var held bool
	disp.onStop = func(*store.Agent) {
		postStoppedHeartbeat(t, srv, broker.ID, a)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		midPhase = got.Phase
		held = hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseRunning), midPhase)
	assert.True(t, held)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// With no lifecycle op in flight a stopped heartbeat applies as before: the
// phase moves and the slot is released.
func TestHeartbeatPhaseGuard_NoOpAppliesAsBefore(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseStarting} {
		t.Run(string(phase), func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			setBrokerAgentCeiling(t, s, 2)
			ctx := context.Background()
			broker, project := newQuotaTestBrokerAndProject(t, s, "hbg-noop-"+string(phase))
			a := newQuotaTestAgent(t, s, broker, project, "hbg-noop-"+string(phase), phase)
			reserveBrokerSlot(t, s, broker, a.ID)

			postStoppedHeartbeat(t, srv, broker.ID, a)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseError), got.Phase, "exit code 137 is recorded as a crash")
			assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
		})
	}
}

// heartbeatPhaseGuarded's decision table.
func TestHeartbeatPhaseGuarded(t *testing.T) {
	srv, _ := testServer(t)
	a := &store.Agent{ID: "agent-guard"}
	cases := []struct {
		stored, hb string
		op         bool
		want       bool
	}{
		{"starting", "stopped", true, true},
		{"starting", "error", true, true},
		{"running", "stopped", true, true},
		{"running", "suspended", true, true},
		{"starting", "stopped", false, false},
		{"starting", "running", true, false},
		{"starting", "", true, false},
		// The heartbeat's snapshot may predate the starting write: an
		// uncounted stored phase is guarded too while an op is active.
		{"stopped", "stopped", true, true},
		{"suspended", "stopped", true, true},
		{"error", "stopped", true, true},
		{"stopped", "stopped", false, false},
		{"stopped", "running", true, false},
	}
	for _, tc := range cases {
		a.Phase = tc.stored
		var end func()
		if tc.op {
			end = srv.beginLifecycleOp(a.ID)
		}
		assert.Equal(t, tc.want, srv.heartbeatPhaseGuarded(a, tc.hb), "stored=%s hb=%s op=%v", tc.stored, tc.hb, tc.op)
		if end != nil {
			end()
		}
	}
}

// ptone/scion#2014: at every start-type site, a heartbeat
// guarded mid-dispatch stores the old container's exit message, code and
// reason; the start's final write clears them, and the stalled marker,
// however the row reads by then.
func TestStartDispatch_FinalWriteClearsTerminalRemnants(t *testing.T) {
	forEachStartSite(t, func(t *testing.T, site startSiteCase, async bool) {
		disp := &hookedStartDispatcher{}
		srv, s, broker, agentID, run := site.setup(t, disp, async)
		grantDevUserRuntimeBrokerAccess(t, s)
		ctx := context.Background()
		row, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		row.StalledFromActivity = string(state.ActivityWorking)
		row.Message = "Agent stopped"
		require.NoError(t, s.UpdateAgent(ctx, row))

		var midPhase string
		disp.onStart = func(*store.Agent) {
			cur, err := s.GetAgent(ctx, agentID)
			require.NoError(t, err)
			postStoppedHeartbeat(t, srv, broker.ID, cur)
			cur, err = s.GetAgent(ctx, agentID)
			require.NoError(t, err)
			midPhase = cur.Phase
			require.NotEmpty(t, cur.Message, "the guarded heartbeat stored its exit message")
			markReady(t, s, agentID)
		}
		require.True(t, run(), "start succeeds")
		assert.Equal(t, string(state.PhaseStarting), midPhase, "the heartbeat's phase was guarded")

		got, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Empty(t, got.Message, "no stale exit message on the running agent")
		assert.Empty(t, got.ExitReason)
		assert.Nil(t, got.ExitCode)
		assert.Empty(t, got.StalledFromActivity, "the stalled marker is cleared")
	})
}

// ptone/scion#2014: restart of a running and of a stopped agent with a
// stopped heartbeat guarded mid-dispatch ends with message "".
func TestStartDispatch_RestartGuardedHeartbeatLeavesNoStaleMessage(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseStopped} {
		t.Run(string(phase), func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			disp := &hookedStartDispatcher{}
			srv.SetDispatcher(disp)
			setBrokerAgentCeiling(t, s, 2)
			ctx := context.Background()
			sfx := "sd-f1-" + string(phase)
			broker, project := newQuotaTestBrokerAndProject(t, s, sfx)
			a := newQuotaTestAgent(t, s, broker, project, sfx, phase)
			if phase == state.PhaseRunning {
				reserveBrokerSlot(t, s, broker, a.ID)
			}
			disp.onStart = func(*store.Agent) { postStoppedHeartbeat(t, srv, broker.ID, a) }

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
			assert.Equal(t, "", got.Message)
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
		})
	}
}

// ptone/scion#2014: a caller whose snapshot is stale (it read
// stopped/suspended, the row is running now) does not write starting over the
// running row: the start fails as a conflict before dispatch and releases the
// reservation it created; the phase is unchanged.
func TestStartDispatch_StaleSnapshotConflicts(t *testing.T) {
	setup := func(t *testing.T, name string, snapshot state.Phase) (*Server, store.Store, *store.RuntimeBroker, *store.Agent, *hookedStartDispatcher) {
		srv, s := testServer(t)
		disp := &hookedStartDispatcher{}
		srv.SetDispatcher(disp)
		setBrokerAgentCeiling(t, s, 2)
		broker, project := newQuotaTestBrokerAndProject(t, s, "sd-stale-"+name)
		a := newQuotaTestAgent(t, s, broker, project, "sd-stale-"+name, snapshot)
		require.NoError(t, s.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
		return srv, s, broker, a, disp
	}
	assertUnchanged := func(t *testing.T, s store.Store, broker *store.RuntimeBroker, a *store.Agent, disp *hookedStartDispatcher) {
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase, "the running row is not overwritten")
		assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "the reservation the call created is released")
		assert.EqualValues(t, 0, disp.startCount.Load(), "nothing dispatched")
	}

	t.Run("helper", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "helper", state.PhaseStopped)
		sd, err := srv.beginStartDispatch(context.Background(), a)
		require.Error(t, err)
		assert.Nil(t, sd)
		assert.ErrorIs(t, err, errStartingWrite)
		assert.ErrorIs(t, err, store.ErrPhaseMismatch)
		assertUnchanged(t, s, broker, a, disp)
	})
	t.Run("http", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "http", state.PhaseStopped)
		w := httptest.NewRecorder()
		_, ok := srv.beginStartDispatchHTTP(context.Background(), w, a)
		require.False(t, ok)
		assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assertUnchanged(t, s, broker, a, disp)
	})
	t.Run("wake", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "wake", state.PhaseSuspended)
		_, dmErr := srv.wakeAgentForDM(context.Background(), a)
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assertUnchanged(t, s, broker, a, disp)
	})
}

// ptone/scion#2014: the heartbeat guard also holds during a non-start op. A
// stopped heartbeat that lands mid-stop is not applied over running; the stop
// writes stopped itself and releases the slot.
func TestHeartbeatPhaseGuard_DuringStop(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "hbg-stop")
	a := newQuotaTestAgent(t, s, broker, project, "hbg-stop", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	var midPhase string
	disp.onStop = func(*store.Agent) {
		postStoppedHeartbeat(t, srv, broker.ID, a)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		midPhase = got.Phase
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseRunning), midPhase, "guarded during the stop op")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// claimDeleteInHook claims a delete of agentID (as a racing DELETE would),
// from inside the start dispatch; the claim moves the starting row to
// stopping and records prior=starting.
func claimDeleteInHook(t *testing.T, srv *Server, agentID string) *agentDeletionPlan {
	t.Helper()
	plan, err := srv.claimAgentDeletion(context.Background(), agentID, agentDeleteParams{requestedBy: "test"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.Equal(t, string(state.PhaseStarting), plan.prior.Phase)
	return plan
}

// ptone/scion#2014: a start that fails under a delete claim, then the delete
// fails: the delete rollback restores stopped (no start in flight any more),
// not the starting the claim captured, with the failed marker set.
func TestStartDispatch_FailedStartThenFailedDeleteEndsStopped(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true}
	disp.deleteErr = errors.New("simulated broker delete failure")
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f3")
	a := newQuotaTestAgent(t, s, broker, project, "sd-f3", state.PhaseStopped)

	var plan *agentDeletionPlan
	disp.onStart = func(*store.Agent) { plan = claimDeleteInHook(t, srv, a.ID) }
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	mid, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopping), mid.Phase, "the claim holds the row; the start's restore was dropped")

	out := <-srv.runAgentDeletion(ctx, plan)
	assert.Equal(t, deletionOutcomeFailed, out.kind)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "not stuck in a counted phase with no container")
	assert.Empty(t, got.Activity)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// ptone/scion#2014: while the start dispatch is still in flight (lifecycle op
// held), a failed delete restores the captured starting as before; the
// start's own rollback then restores the prior phase once the delete has
// failed.
func TestStartDispatch_FailedDeleteDuringStartRestoresStarting(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true}
	disp.deleteErr = errors.New("simulated broker delete failure")
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f3-guard")
	a := newQuotaTestAgent(t, s, broker, project, "sd-f3-guard", state.PhaseStopped)

	var midPhase string
	disp.onStart = func(*store.Agent) {
		plan := claimDeleteInHook(t, srv, a.ID)
		out := <-srv.runAgentDeletion(ctx, plan)
		require.Equal(t, deletionOutcomeFailed, out.kind)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		midPhase = got.Phase
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Equal(t, string(state.PhaseStarting), midPhase, "a start in flight: the delete rollback restores starting")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "the start's rollback restores the prior phase")
}

// ptone/scion#2014, ptone/scion#2550: a running intent cannot be recorded
// after the claim (the store refuses it while the delete holds the row,
// ptone/scion#2550), so the failed delete restores stopped.
func TestStartDispatch_FailedDeleteIntentRefusedUnderClaim(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true}
	disp.deleteErr = errors.New("simulated broker delete failure")
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f3-intent")
	a := newQuotaTestAgent(t, s, broker, project, "sd-f3-intent", state.PhaseStopped)
	var plan *agentDeletionPlan
	disp.onStart = func(*store.Agent) { plan = claimDeleteInHook(t, srv, a.ID) }
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	var swapErr error
	disp.onDelete = func(*store.Agent) {
		_, _, swapErr = s.SwapRunIntent(ctx, a.ID, store.RunIntentRunning)
	}
	out := <-srv.runAgentDeletion(ctx, plan)
	require.Equal(t, deletionOutcomeFailed, out.kind)
	assert.ErrorIs(t, swapErr, store.ErrDeleteInProgress)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}

// ptone/scion#2014: a live launch keeps today's behaviour; the captured
// starting is restored.
func TestStartDispatch_FailedDeleteKeepsStartingUnderLiveLaunch(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	disp.deleteErr = errors.New("simulated broker delete failure")
	srv.SetDispatcher(disp)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f3-launch")
	a := newQuotaTestAgent(t, s, broker, project, "sd-f3-launch", state.PhaseProvisioning)
	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	plan := claimDeleteInHook(t, srv, a.ID)
	require.NotEmpty(t, plan.prior.LaunchID)
	out := <-srv.runAgentDeletion(ctx, plan)
	require.Equal(t, deletionOutcomeFailed, out.kind)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStarting), got.Phase, "a live launch keeps the prior restore")
}

// ptone/scion#2014: the wake clears the previous generation's leftovers on
// its post-dispatch starting write, so a status the new container posts
// during the readiness wait ("Agent started") survives the running write.
func TestStartDispatch_WakeKeepsNewGenerationStatus(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-wake-newgen")
	a := newQuotaTestAgent(t, s, broker, project, "sd-wake-newgen", state.PhaseSuspended)

	var wg sync.WaitGroup
	var oldCleared bool
	disp.onStart = func(*store.Agent) {
		// What a guarded heartbeat would leave from the old container.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Message: "Agent crashed with exit code 137"}))
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Once the starting write has cleared it, post the new
			// generation's first status (the readiness signal).
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				got, err := s.GetAgent(ctx, a.ID)
				if err == nil && got.Message == "" {
					oldCleared = true
					_ = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
						Activity: string(state.ActivityWorking), Message: "Agent started",
					})
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	_, dmErr := srv.wakeAgentForDM(ctx, a)
	wg.Wait()
	require.Nil(t, dmErr)
	assert.True(t, oldCleared, "the starting write cleared the old container's message")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Equal(t, "Agent started", got.Message, "the new generation's status survives the running write")
}

// ptone/scion#2014, ptone/scion#2550: a delete that claimed the row after the
// caller read it (the claim moved it to stopping) is answered as #2415's
// delete_in_progress, not as a generic phase conflict, by the helper, the
// HTTP helper and the wake; nothing is dispatched and the reservation is
// released.
func TestStartDispatch_DeleteClaimBeforeStartingWrite(t *testing.T) {
	setup := func(t *testing.T, name string, snapshot state.Phase) (*Server, store.Store, *store.RuntimeBroker, *store.Agent, *hookedStartDispatcher) {
		srv, s := testServer(t)
		disp := &hookedStartDispatcher{}
		srv.SetDispatcher(disp)
		setBrokerAgentCeiling(t, s, 2)
		ctx := context.Background()
		broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f2-"+name)
		a := newQuotaTestAgent(t, s, broker, project, "sd-f2-"+name, snapshot)
		// The row moves on (running) after the caller's snapshot, and a
		// delete claims it, moving it to stopping.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
		plan, err := srv.claimAgentDeletion(ctx, a.ID, agentDeleteParams{requestedBy: "test"})
		require.NoError(t, err)
		require.NotNil(t, plan)
		return srv, s, broker, a, disp
	}
	assertNothingDone := func(t *testing.T, s store.Store, broker *store.RuntimeBroker, disp *hookedStartDispatcher) {
		assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
		assert.EqualValues(t, 0, disp.startCount.Load())
	}
	t.Run("helper", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "helper", state.PhaseStopped)
		_, err := srv.beginStartDispatch(context.Background(), a)
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrDeleteInProgress)
		assert.ErrorIs(t, err, store.ErrPhaseMismatch)
		assertNothingDone(t, s, broker, disp)
	})
	t.Run("http", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "http", state.PhaseStopped)
		w := httptest.NewRecorder()
		_, ok := srv.beginStartDispatchHTTP(context.Background(), w, a)
		require.False(t, ok)
		assert.Equal(t, http.StatusConflict, w.Code)
		var body struct {
			Error struct {
				Code    string                 `json:"code"`
				Details map[string]interface{} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
		assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
		assert.Equal(t, a.ID, body.Error.Details["agentId"])
		assertNothingDone(t, s, broker, disp)
	})
	t.Run("wake", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "wake", state.PhaseSuspended)
		_, dmErr := srv.wakeAgentForDM(context.Background(), a)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assertNothingDone(t, s, broker, disp)
	})
}

// failingStartingStore fails UpdateAgentStatus writes of phase starting, as a
// store error (not a phase mismatch) would.
type failingStartingStore struct {
	store.Store
}

func (f *failingStartingStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if su.Phase == string(state.PhaseStarting) {
		return errors.New("simulated store failure")
	}
	return f.Store.UpdateAgentStatus(ctx, id, su)
}

// ptone/scion#2014: a starting write that fails for a reason other than
// a phase mismatch answers 500 over HTTP and ErrCodeRuntimeError from the
// wake, releases the reservation the call created, leaves the phase as it
// was, and dispatches nothing.
func TestStartDispatch_StartingWriteFailure(t *testing.T) {
	setup := func(t *testing.T, name string, phase state.Phase) (*Server, store.Store, *store.RuntimeBroker, *store.Agent, *hookedStartDispatcher) {
		srv, s := testServer(t)
		// Swap the wrapper in after construction: server start-up needs the
		// concrete store's optional interfaces; the start path reads
		// srv.store.
		srv.store = &failingStartingStore{Store: srv.store}
		disp := &hookedStartDispatcher{}
		srv.SetDispatcher(disp)
		setBrokerAgentCeiling(t, s, 2)
		broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f6a-"+name)
		a := newQuotaTestAgent(t, s, broker, project, "sd-f6a-"+name, phase)
		return srv, s, broker, a, disp
	}
	assertUnchanged := func(t *testing.T, s store.Store, broker *store.RuntimeBroker, a *store.Agent, phase state.Phase, disp *hookedStartDispatcher) {
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(phase), got.Phase)
		assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "the created reservation is released")
		assert.EqualValues(t, 0, disp.startCount.Load())
	}
	t.Run("http-start", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "http", state.PhaseStopped)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		assertUnchanged(t, s, broker, a, state.PhaseStopped, disp)
	})
	t.Run("wake", func(t *testing.T) {
		srv, s, broker, a, disp := setup(t, "wake", state.PhaseSuspended)
		_, dmErr := srv.wakeAgentForDM(context.Background(), a)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeRuntimeError, dmErr.Code)
		assert.Equal(t, http.StatusInternalServerError, dmErr.HTTPStatus)
		assertUnchanged(t, s, broker, a, state.PhaseSuspended, disp)
	})
}

// ptone/scion#2014: the delete rollback's stopped-for-starting branch through
// a restart. The restart's start leg fails because a delete claimed the row
// mid-dispatch (ErrDeleteInProgress branch); the delete then fails, and the
// row ends stopped, not starting.
func TestStartDispatch_RestartFailedUnderClaimThenFailedDeleteEndsStopped(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{failStart: true, startErr: fmt.Errorf("start refused: %w", store.ErrDeleteInProgress)}
	disp.deleteErr = errors.New("simulated broker delete failure")
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-f6b")
	a := newQuotaTestAgent(t, s, broker, project, "sd-f6b", state.PhaseStopped)

	var plan *agentDeletionPlan
	disp.onStart = func(*store.Agent) { plan = claimDeleteInHook(t, srv, a.ID) }
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeDeleteInProgress)
	assert.EqualValues(t, 1, disp.stopCount.Load())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the restart's reservation is released")

	out := <-srv.runAgentDeletion(ctx, plan)
	assert.Equal(t, deletionOutcomeFailed, out.kind)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
}

// ptone/scion#2014: a start on an agent that is already running is not a
// new generation (no starting write, no stop leg): the final write keeps the
// live harness's message, activity and stalled marker.
func TestStartDispatch_StartOnRunningAgentKeepsLiveStatus(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "sd-r3-running")
	a := newQuotaTestAgent(t, s, broker, project, "sd-r3-running", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	row, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	row.Activity = string(state.ActivityStalled)
	row.StalledFromActivity = string(state.ActivityWorking)
	row.Message = "Processing: hello"
	require.NoError(t, s.UpdateAgent(ctx, row))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Equal(t, "Processing: hello", got.Message)
	assert.Equal(t, string(state.ActivityStalled), got.Activity)
	assert.Equal(t, string(state.ActivityWorking), got.StalledFromActivity)
}

// recordingStatusStore records the heartbeat status updates it is given.
type recordingStatusStore struct {
	store.Store
	mu         sync.Mutex
	heartbeats []store.AgentStatusUpdate
}

func (r *recordingStatusStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if su.Heartbeat {
		r.mu.Lock()
		r.heartbeats = append(r.heartbeats, su)
		r.mu.Unlock()
	}
	return r.Store.UpdateAgentStatus(ctx, id, su)
}

func (r *recordingStatusStore) lastHeartbeat(t *testing.T) store.AgentStatusUpdate {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.heartbeats)
	return r.heartbeats[len(r.heartbeats)-1]
}

// ptone/scion#2014: the heartbeat wiring of the guard. While an op is
// held on an agent whose stored phase is uncounted (the heartbeat's snapshot
// predating a starting write), a stopped report's phase is dropped before
// the store write and the quota reconcile, while its exit code and reason
// are still recorded. Without an op the same report keeps its phase.
func TestHeartbeatPhaseGuard_UncountedStoredPhaseEndToEnd(t *testing.T) {
	for _, op := range []bool{true, false} {
		t.Run(fmt.Sprintf("op=%v", op), func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			rec := &recordingStatusStore{Store: srv.store}
			srv.store = rec
			setBrokerAgentCeiling(t, s, 2)
			ctx := context.Background()
			sfx := fmt.Sprintf("hbg-e2e-%v", op)
			broker, project := newQuotaTestBrokerAndProject(t, s, sfx)
			a := newQuotaTestAgent(t, s, broker, project, sfx, state.PhaseStopped)
			reserveBrokerSlot(t, s, broker, a.ID)
			if op {
				defer srv.beginLifecycleOp(a.ID)()
			}

			code := 137
			hb := brokerHeartbeatRequest{
				Status: "online",
				Projects: []brokerProjectHeartbeat{{
					ProjectID:  a.ProjectID,
					AgentCount: 1,
					Agents: []brokerAgentHeartbeat{{
						Slug:            a.Slug,
						Phase:           string(state.PhaseStopped),
						ContainerStatus: "Exited (137)",
						ExitCode:        &code,
						ExitReason:      string(state.ExitReasonPreempted),
					}},
				}},
			}
			resp := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
			require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

			su := rec.lastHeartbeat(t)
			if op {
				assert.Empty(t, su.Phase, "the guard drops the phase before the store write")
			} else {
				assert.Equal(t, string(state.PhaseStopped), su.Phase, "no op: the report's phase applies as before")
			}
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
			assert.Equal(t, string(state.ExitReasonPreempted), got.ExitReason, "the exit reason is recorded")
			require.NotNil(t, got.ExitCode, "the exit code is recorded")
			assert.Equal(t, 137, *got.ExitCode)
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "no reconcile release")
		})
	}
}

// postDyingStoppedReport posts the dying container's own status report
// (phase stopped), as sciontool does on a clean SIGTERM, through the agent
// status endpoint with the agent's identity.
func postDyingStoppedReport(t *testing.T, srv *Server, a *store.Agent) {
	t.Helper()
	body, err := json.Marshal(store.AgentStatusUpdate{Phase: string(state.PhaseStopped), Message: "Agent stopped"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", bytes.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(a.ID, a.ProjectID, ScopeAgentStatusUpdate)))
	rec := httptest.NewRecorder()
	srv.updateAgentStatus(rec, req, a.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ptone/scion#2010, ptone/scion#2014: the dying container's own stopped
// report during a restart's stop leg releases the reservation through the
// status endpoint; the restart re-asserts it after the stop leg, so a
// successful restart ends running with the slot held.
func TestRestart_DyingContainerStoppedReportKeepsReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-dying")
	a := newQuotaTestAgent(t, s, broker, project, "rs-dying", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	var releasedDuringStop bool
	disp.onStop = func(*store.Agent) {
		postDyingStoppedReport(t, srv, a)
		releasedDuringStop = !hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, releasedDuringStop, "precondition: the self-report released the slot")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the restart keeps its slot")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// ptone/scion#2010, ptone/scion#2014: when the restart's start leg then
// fails, the re-asserted reservation is released exactly once, whichever
// failure branch runs (stop leg succeeded: release and record stopped; both
// legs failed: rollback; a delete claimed the row: rollback, answered
// delete_in_progress), and another agent's slot is untouched.
func TestRestart_DyingContainerStoppedReportFailedStartReleasesOnce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stopFails     bool
		deleteClaimed bool
	}{
		{name: "stop-ok"},
		{name: "stop-failed", stopFails: true},
		{name: "delete-in-progress", deleteClaimed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopFails := tc.stopFails
			srv, s := testServer(t)
			disp := &hookedStartDispatcher{failStart: true}
			if tc.deleteClaimed {
				disp.startErr = fmt.Errorf("start refused: %w", store.ErrDeleteInProgress)
			}
			srv.SetDispatcher(disp)
			setBrokerAgentCeiling(t, s, 3)
			ctx := context.Background()
			sfx := "rs-dying-fail-" + tc.name
			broker, project := newQuotaTestBrokerAndProject(t, s, sfx)
			a := newQuotaTestAgent(t, s, broker, project, sfx, state.PhaseRunning)
			reserveBrokerSlot(t, s, broker, a.ID)
			other := newQuotaTestAgent(t, s, broker, project, sfx+"-other", state.PhaseRunning)
			reserveBrokerSlot(t, s, broker, other.ID)
			disp.onStop = func(*store.Agent) { postDyingStoppedReport(t, srv, a) }
			if stopFails {
				srv.SetDispatcher(&stopFailingHooked{hookedStartDispatcher: disp})
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
			if tc.deleteClaimed {
				require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), ErrCodeDeleteInProgress)
			} else {
				require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
			}
			assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, other.ID))
			assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
		})
	}
}

// stopFailingHooked is a hookedStartDispatcher whose stop leg runs its hook
// and then fails.
type stopFailingHooked struct {
	*hookedStartDispatcher
}

func (d *stopFailingHooked) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	_ = d.hookedStartDispatcher.DispatchAgentStop(ctx, agent)
	return errors.New("simulated broker stop failure")
}

// ptone/scion#2014: a create-of-an-existing-agent resume that retries its
// full-row write after a version conflict carries the cleared message and
// stalled marker (empty values included) onto the re-read row.
func TestMergeDispatchedAgent_RunningCarriesClearedMessageAndStalled(t *testing.T) {
	dst := &store.Agent{
		Phase:               string(state.PhaseStarting),
		Message:             "Agent crashed with exit code 137",
		StalledFromActivity: string(state.ActivityWorking),
	}
	src := &store.Agent{Phase: string(state.PhaseRunning)}
	mergeDispatchedAgent(dst, src)
	assert.Equal(t, string(state.PhaseRunning), dst.Phase)
	assert.Empty(t, dst.Message)
	assert.Empty(t, dst.StalledFromActivity)
}

// ptone/scion#2010, ptone/scion#2014: a stopped report about the old
// container that lands during the start leg (after the post-stop re-assert)
// releases the slot; the restart re-asserts once more after its final write,
// so it ends running with the slot held.
func TestRestart_StoppedReportDuringStartLegKeepsReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-startleg")
	a := newQuotaTestAgent(t, s, broker, project, "rs-startleg", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	var releasedDuringStart bool
	disp.onStart = func(*store.Agent) {
		postDyingStoppedReport(t, srv, a)
		releasedDuringStart = !hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, releasedDuringStart, "precondition: the report released the slot")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the restart keeps its slot")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}
