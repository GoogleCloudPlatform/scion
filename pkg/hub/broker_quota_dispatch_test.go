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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
}

func (d *hookedStartDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	if d.onStart != nil {
		d.onStart(agent)
	}
	if d.failStart {
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
		{"stopped", "stopped", true, false},
		{"suspended", "stopped", true, false},
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

// ptone/scion#2014 review F1/F2: at every start-type site, a heartbeat
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

// The reviewer's reproduction (F1): restart of a running and of a stopped
// agent with a stopped heartbeat guarded mid-dispatch ends with message "".
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

// F5(i)/F9(a): a caller whose snapshot is stale (it read stopped/suspended,
// the row is running now) does not write starting over the running row:
// the start fails as a conflict before dispatch and releases the
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

// F6/F9(e): the guard also holds during a non-start op. A stopped heartbeat
// that lands mid-stop is not applied over running; the stop writes stopped
// itself and releases the slot.
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

// F3: a start that fails under a delete claim, then the delete fails: the
// delete rollback restores stopped (no start in flight any more), not the
// starting the claim captured, with the failed marker set.
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

// F3 guard: while the start dispatch is still in flight (lifecycle op held),
// a failed delete restores the captured starting as before; the start's own
// rollback then restores the prior phase once the delete has failed.
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

// F3 controls: a live launch keeps today's behaviour (the captured starting
// is restored); a running intent cannot be recorded under the claim.
func TestStartDispatch_FailedDeleteKeepsStartingWhenStartOutstanding(t *testing.T) {
	t.Run("running-intent-refused-under-claim", func(t *testing.T) {
		// A running intent cannot be recorded after the claim: the store
		// refuses it while the delete holds the row (ptone/scion#2550), so
		// startNotInFlight's intent check is defensive, and the failed
		// delete restores stopped.
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
	})
	t.Run("live-launch", func(t *testing.T) {
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
	})
}
