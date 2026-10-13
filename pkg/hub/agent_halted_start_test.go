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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Starts from halted, and the report rules around a start
// (ptone/scion#4853).

// selfReportAs posts body to agent a's status endpoint as the agent, with a
// token for run runID, and returns the response code.
func selfReportAs(t *testing.T, srv *Server, a *store.Agent, runID string, body map[string]any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", bytes.NewReader(raw))
	req = req.WithContext(agentClaimsCtx(req.Context(), a, runID, ScopeAgentStatusUpdate))
	rec := httptest.NewRecorder()
	srv.updateAgentStatus(rec, req, a.ID)
	return rec.Code
}

// A cross-node start from halted (subscribe, intent, signal, wait) that
// the owner node lands ends running.
func TestHaltedCrossNodeStart_EndsRunning(t *testing.T) {
	setLifecycleTimings(t, 3*time.Second, 50*time.Millisecond, 50*time.Millisecond)
	srv, s := testServer(t)
	ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
	agent := setupBrokerAgentInPhase(t, s, "xn-halted-ok", state.PhaseStopped)
	setHalted(t, s, agent.ID, "preempted")
	ownerDisp.onLand = func(_ context.Context, a *store.Agent) {
		running := *a
		running.Phase = string(state.PhaseRunning)
		bus.PublishAgentStatus(context.Background(), &running)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase)
	assert.Empty(t, got.ExitReason, "the old run's reason is cleared")
}

// A cross-node start from halted whose new run is reported halted again:
// the wait reports a failure and the rollback restores halted.
func TestHaltedCrossNodeStart_FailureRestoresHalted(t *testing.T) {
	setLifecycleTimings(t, 3*time.Second, 50*time.Millisecond, 50*time.Millisecond)
	srv, s := testServer(t)
	ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
	agent := setupBrokerAgentInPhase(t, s, "xn-halted-fail", state.PhaseStopped)
	setHalted(t, s, agent.ID, "preempted")
	ownerDisp.onLand = func(_ context.Context, a *store.Agent) {
		halted := *a
		halted.Phase = string(state.PhaseHalted)
		bus.PublishAgentStatus(context.Background(), &halted)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "halted phase during start")
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase, "the start's rollback restores halted")
	assert.Equal(t, "preempted", got.ExitReason)
}

// The rare window between a start taking its claim and its provisional
// starting write: an agent report of running with no run (an older token)
// moves the halted row (the claim-held exception), the start's conditional write then fails
// and answers 409, and the claim stays unconfirmed for the reaper, so a
// retry is refused with start_in_progress until it is settled.
func TestHaltedStart_SelfReportBeforeProvisionalWrite(t *testing.T) {
	f, d, a := newClaimFixture(t)
	setHalted(t, f.s, a.ID, "preempted")
	d.start = func(context.Context, *store.Agent) error {
		t.Error("the start must not be dispatched")
		return nil
	}
	fired := false
	f.srv.startClaimTestHook = func(r *startClaimRun) {
		if fired {
			return
		}
		fired = true
		require.Equal(t, http.StatusOK, selfReportAs(t, f.srv, a, "", map[string]any{"phase": "running", "activity": "working"}))
	}

	code, _ := lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusConflict, code)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "running", got.Phase, "the report under the claim applied")
	assert.NotEmpty(t, got.StartClaimID)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "held unconfirmed until the reaper settles it")

	code, body := lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusConflict, code)
	errCode, _ := errorDetails(body)
	assert.Equal(t, ErrCodeStartInProgress, errCode)
}

// A start from halted whose outcome is in doubt: the rollback restores
// halted, the new run's own running report applies under the held claim,
// and the reaper releases the claim once an inventory lists the container.
func TestHaltedStart_InDoubtStartRecovers(t *testing.T) {
	f, d, a := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	setHalted(t, f.s, a.ID, "preempted")
	d.start = func(context.Context, *store.Agent) error { return errors.New("broker timed out") }

	cur := getAgent(t, f.s, a.ID)
	require.Error(t, f.srv.startAgentCore(ctx, cur, StartOpts{Kind: store.StartClaimUser}))
	got := getAgent(t, f.s, a.ID)
	require.Equal(t, "halted", got.Phase, "the rollback restores halted")
	require.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)

	require.Equal(t, http.StatusOK, selfReportAs(t, f.srv, a, "", map[string]any{"phase": "running", "activity": "working"}))
	require.Equal(t, "running", getAgent(t, f.s, a.ID).Phase, "the new run's report applies under the claim")

	f.heartbeat(completeInventory(), a.Slug)
	f.srv.reapStartClaims(ctx)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper releases the claim")
	assert.Equal(t, "running", getAgent(t, f.s, a.ID).Phase)
}

// A start from halted whose final status write fails leaves the row
// starting; the next running heartbeat moves it to running.
func TestHaltedStart_FailedFinalWriteThenRunningHeartbeat(t *testing.T) {
	f, _, a := newClaimFixture(t)
	setHalted(t, f.s, a.ID, "preempted")
	f.srv.store = failRunningStatusStore{Store: f.s, agentID: a.ID}
	code, _ := lifecycle(t, f, a.ID, "start")
	assert.NotEqual(t, http.StatusBadGateway, code, "not answered as a failed dispatch")
	f.srv.store = f.s
	require.Equal(t, "starting", getAgent(t, f.s, a.ID).Phase)

	f.heartbeat(completeInventory(), a.Slug)
	assert.Equal(t, "running", getAgent(t, f.s, a.ID).Phase)
}

// A self-report on a halted agent reserves no broker capacity.
func TestHaltedSelfReport_NoQuotaReserve(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	a := f.addAgent("halted-quota", "running", "working")
	setHalted(t, f.s, a.ID, "preempted")
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, selfReportAs(t, f.srv, a, "", map[string]any{"phase": "running", "activity": "working"}))
	assert.Equal(t, "halted", getAgent(t, f.s, a.ID).Phase)
	has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "the dropped running report reserves nothing")
}

// An old run's report leaves the broker reservation alone, as it leaves
// the row: the store applies nothing of it.
func TestOldRunReport_KeepsReservation(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	a := f.addAgent("old-run", "running", "working")
	_, err := f.s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	_, err = f.srv.checkAndReserveBrokerQuota(ctx, a)
	require.NoError(t, err)
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, selfReportAs(t, f.srv, a, "run-0", map[string]any{"phase": "stopped"}))
	f.send(brokerHeartbeatRequest{
		Status: store.BrokerStatusOnline,
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "stopped", RunID: "run-0", RuntimeTarget: "docker"},
		}}},
	})

	assert.Equal(t, "running", getAgent(t, f.s, a.ID).Phase)
	has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.True(t, has, "an old run's stopped report does not release the reservation")
}

// The restore event carries the display runtime, like the create event.
func TestRestoreAgent_EventCarriesDisplayRuntime(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("restore-rt-broker"), Name: "Restore RT Broker", Slug: "restore-rt-broker",
		Status:   store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{{Name: "k8s-prod", Type: "kubernetes", Available: true}},
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	softDeleteAgent(t, f)
	// Point the soft-deleted row at the broker (after the delete, so the
	// delete does not dispatch to it), with no runtime recorded.
	target, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	target.RuntimeBrokerID = broker.ID
	target.Runtime = ""
	if target.AppliedConfig == nil {
		target.AppliedConfig = &store.AgentAppliedConfig{}
	}
	target.AppliedConfig.Profile = "k8s-prod"
	require.NoError(t, f.store.UpdateAgent(ctx, target))

	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	f.srv.events = bus
	ch, unsub := bus.Subscribe("agent." + f.target.ID + ".created")
	defer unsub()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.targetPath()+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	data := nextCreatedEvent(t, ch)
	assert.Equal(t, "kubernetes", data.Runtime)

	stored, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.Runtime, "the display runtime is not persisted")
}
