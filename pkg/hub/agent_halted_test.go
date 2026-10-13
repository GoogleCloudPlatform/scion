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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Halted phase hub tests (ptone/scion#4853). The store rules themselves are
// covered by pkg/store/entadapter's TestAgentHalted_* on both backends;
// these cover the hub's evidence plumbing, guards and lifecycle writes.

// haltFixture is a running kubernetes agent of run "run-1", meant to be
// running, on an online broker the dev user may heartbeat for.
type haltFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	broker  *store.RuntimeBroker
	agent   *store.Agent
}

func newHaltFixture(t *testing.T) haltFixture {
	t.Helper()
	ctx := context.Background()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	grantDevUserRuntimeBrokerAccess(t, s)
	a := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Runtime = "kubernetes"
		a.Activity = "working"
	})
	_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	_, err = s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	return haltFixture{srv: srv, s: s, project: project, broker: broker, agent: a}
}

func (f haltFixture) get(t *testing.T) *store.Agent {
	t.Helper()
	a, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	return a
}

// heartbeat sends one broker heartbeat entry for the fixture's agent.
func (f haltFixture) heartbeat(t *testing.T, hb brokerAgentHeartbeat) {
	t.Helper()
	hb.Slug = f.agent.Slug
	require.Equal(t, http.StatusOK, sendHeartbeat(t, f.srv, f.broker.ID, f.project.ID, hb))
}

// selfReport posts body to the agent's status endpoint as the agent, with
// an agent token for run runID, and returns the response code.
func (f haltFixture) selfReport(t *testing.T, runID string, body map[string]any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(raw))
	req = req.WithContext(agentClaimsCtx(req.Context(), f.agent, runID, ScopeAgentStatusUpdate))
	rec := httptest.NewRecorder()
	f.srv.updateAgentStatus(rec, req, f.agent.ID)
	return rec.Code
}

// halt puts the fixture's agent in phase halted directly, with a platform
// reason (a restore write marked as the runtime's, so the reason is kept).
func (f haltFixture) halt(t *testing.T, reason string) {
	t.Helper()
	setHalted(t, f.s, f.agent.ID, reason)
}

// setHalted writes phase halted and reason onto an agent row directly.
func setHalted(t *testing.T, s store.Store, id, reason string) {
	t.Helper()
	require.NoError(t, s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{
		Phase: string(state.PhaseHalted), Restore: true, ExitReason: reason,
		ReportSource: store.StatusReportSourceRuntime,
	}))
}

func TestHalted_HeartbeatPreemptHalts(t *testing.T) {
	f := newHaltFixture(t)
	code := 143
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", ExitCode: &code, RunID: "run-1"})
	got := f.get(t)
	assert.Equal(t, "halted", got.Phase)
	assert.Equal(t, "preempted", got.ExitReason)
	assert.Empty(t, got.Activity)

	// Later reports of the run do not move it.
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", RunID: "run-1"})
	assert.Equal(t, "halted", f.get(t).Phase)
}

func TestHalted_HeartbeatWithoutRunIDHalts(t *testing.T) {
	// An older broker sends no run ID: there is no run check, and its
	// stopped + reason report is the evidence.
	f := newHaltFixture(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "evicted"})
	assert.Equal(t, "halted", f.get(t).Phase)
}

func TestHalted_HeartbeatOldRunIgnored(t *testing.T) {
	f := newHaltFixture(t)
	before := f.get(t)
	code := 143
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", ExitCode: &code,
		ContainerStatus: "exited (143)", Message: "old run", RunID: "run-0"})
	got := f.get(t)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Empty(t, got.ExitReason)
}

func TestHalted_HeartbeatWireDrop(t *testing.T) {
	f := newHaltFixture(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "halted", RunID: "run-1"})
	assert.Equal(t, "running", f.get(t).Phase, "a broker cannot report halted")
}

func TestHalted_HeartbeatOnHaltedRowIsSticky(t *testing.T) {
	f := newHaltFixture(t)
	f.halt(t, "preempted")
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "working", RunID: "run-1"})
	assert.Equal(t, "halted", f.get(t).Phase)
}

func TestHalted_SelfReportSticky(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"error":   {"phase": "error", "exitReason": "crashed"},
		"stopped": {"phase": "stopped"},
		"running": {"phase": "running", "activity": "working"},
		"working": {"activity": "working"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHaltFixture(t)
			f.halt(t, "preempted")
			require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", body))
			got := f.get(t)
			assert.Equal(t, "halted", got.Phase)
			assert.Empty(t, got.Activity)
		})
	}
}

func TestHalted_SelfReportUnderClaimApplies(t *testing.T) {
	// The in-doubt start: the start's rollback restored halted, the claim is
	// still held, and the new run reports running.
	f := newHaltFixture(t)
	f.halt(t, "preempted")
	a := f.get(t)
	a.StartClaimID = "claim-1"
	status := store.AgentStatusUpdate{Phase: "running", Activity: "working"}
	guardAgentPhaseTransition(a, &status)
	assert.Equal(t, "running", status.Phase, "Guard 0e lets a claimed start's running through")
	assert.Equal(t, "working", status.Activity)

	status = store.AgentStatusUpdate{Phase: "stopped"}
	guardAgentPhaseTransition(a, &status)
	assert.Empty(t, status.Phase, "a terminal report under the claim is still dropped")
}

func TestHalted_Guard0e(t *testing.T) {
	halted := &store.Agent{Phase: string(state.PhaseHalted), ExitReason: "preempted"}
	for _, tc := range []struct {
		in        store.AgentStatusUpdate
		wantPhase string
	}{
		{store.AgentStatusUpdate{Phase: "running", Activity: "working"}, ""},
		{store.AgentStatusUpdate{Phase: "starting"}, ""},
		{store.AgentStatusUpdate{Phase: "stopped", Message: "Agent stopped"}, ""},
		{store.AgentStatusUpdate{Phase: "error", ExitReason: "oom_killed"}, ""},
		{store.AgentStatusUpdate{Activity: "working"}, ""},
	} {
		status := tc.in
		guardAgentPhaseTransition(halted, &status)
		assert.Equal(t, tc.wantPhase, status.Phase, "%+v", tc.in)
		assert.Empty(t, status.Activity, "%+v", tc.in)
		assert.Empty(t, status.ExitReason, "the agent's own report never changes a halted reason")
		assert.Nil(t, status.ExitCode)
		assert.Equal(t, tc.in.Message, status.Message, "the message is kept")
	}
}

func TestHalted_SelfReportOldRunIgnored(t *testing.T) {
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-0", map[string]any{"phase": "stopped", "exitReason": "oom_killed"}))
	got := f.get(t)
	assert.Equal(t, "running", got.Phase)
	assert.Empty(t, got.ExitReason)
}

func TestHalted_SelfReportWireDrop(t *testing.T) {
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "halted"}))
	assert.Equal(t, "running", f.get(t).Phase)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "halted", "restore": true}))
	assert.Equal(t, "running", f.get(t).Phase, "restore is not on the wire")
}

func TestHalted_SelfReportNeverHalts(t *testing.T) {
	// A self-report carrying every halt-related field, including the
	// heartbeat flag sciontool sends: the agent's own report is never
	// evidence of a halt, and the platform reason is not recorded.
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{
		"phase": "error", "exitReason": "oom_killed", "exitCode": 137, "heartbeat": true,
		"reportSource": "runtime", "restore": true, "containerStatus": "exited (137)",
	}))
	got := f.get(t)
	assert.Equal(t, "error", got.Phase)
	assert.Empty(t, got.ExitReason)
}

func TestHalted_SelfReportedStopThenRuntimeReasonHalts(t *testing.T) {
	// A self-report never suppresses the runtime's later halt: the plain
	// stop sciontool reports on SIGTERM, then the broker's disruption.
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped", "exitCode": 0}))
	require.Equal(t, "stopped", f.get(t).Phase)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", RunID: "run-1"})
	assert.Equal(t, "halted", f.get(t).Phase)
}

func TestHalted_HubInternalWritesNeverHalt(t *testing.T) {
	ctx := context.Background()

	t.Run("restart stop leg", func(t *testing.T) {
		f := newHaltFixture(t)
		// A disruption reason recorded while the pod still ran.
		f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ExitReason: "preempted", RunID: "run-1"})
		require.Equal(t, "preempted", f.get(t).ExitReason)
		require.True(t, f.srv.recordRestartStopped(ctx, f.agent.ID, "run-1"))
		assert.Equal(t, "stopped", f.get(t).Phase)
	})

	t.Run("recorded stop carrying a reason", func(t *testing.T) {
		f := newHaltFixture(t)
		ok, err := f.srv.recordStopStatus(ctx, f.agent.ID, "run-1", "stop", store.AgentStatusUpdate{
			Phase: "stopped", ExitReason: "preempted",
		})
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "stopped", f.get(t).Phase)
	})

	t.Run("start rollback restores halted", func(t *testing.T) {
		f := newHaltFixture(t)
		f.halt(t, "preempted")
		a := f.get(t)
		d, err := f.srv.beginStartDispatch(ctx, a)
		require.NoError(t, err)
		require.True(t, d.marked, "a start from halted writes starting")
		assert.Equal(t, "starting", f.get(t).Phase)
		d.rollback(ctx)
		got := f.get(t)
		assert.Equal(t, "halted", got.Phase, "the rollback restores halted")
		assert.Equal(t, "preempted", got.ExitReason)
	})

	t.Run("offline stop", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetDispatcher(&runIntentDispatcher{})
		srv.commandBus = &recordingCommandBus{}
		_, _, agent := setupOfflineBrokerAgent(t, s, "halt-q")
		_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
		require.NoError(t, err)
		setHalted(t, s, agent.ID, "preempted")

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "stopped", got.Phase, "an offline stop of a halted agent ends stopped")
		assert.Equal(t, containerStatusStopQueued, got.ContainerStatus)
	})
}

func TestHalted_BeginStartDispatchRefusesMovedRow(t *testing.T) {
	// The provisional starting write is conditional on the phase the start
	// read (halted): a row that moved on since fails it with
	// errStartingWrite, and nothing is written. The full scenario, through
	// the start handler with a claim held, is
	// TestHaltedStart_SelfReportBeforeProvisionalWrite.
	ctx := context.Background()
	f := newHaltFixture(t)
	f.halt(t, "preempted")
	stale := f.get(t)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, f.agent.ID, store.AgentStatusUpdate{Phase: "running", Restore: true}))
	_, err := f.srv.beginStartDispatch(ctx, stale)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errStartingWrite))
	assert.True(t, errors.Is(err, store.ErrPhaseMismatch))
	assert.Equal(t, "running", f.get(t).Phase)
}

func TestHalted_WriteStartedStatusFromHalted(t *testing.T) {
	ctx := context.Background()
	f := newHaltFixture(t)
	f.halt(t, "preempted")
	a := f.get(t)
	require.NoError(t, f.srv.writeStartedStatus(ctx, a, true))
	got := f.get(t)
	assert.Equal(t, "running", got.Phase)
	assert.Empty(t, got.ExitReason, "the old run's reason is cleared")
}

func TestHalted_UserStartAndStop(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetDispatcher(&runIntentDispatcher{})
	_, _, agent := setupOnlineBrokerAgent(t, s, "halt-lc")
	setHalted(t, s, agent.ID, "preempted")
	path := "/api/v1/agents/" + agent.ID

	rec := doRequest(t, srv, http.MethodPost, path+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase)
	assert.Empty(t, got.ExitReason)

	setHalted(t, s, agent.ID, "preempted")
	rec = doRequest(t, srv, http.MethodPost, path+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
}

func TestHalted_ReconcileMissing(t *testing.T) {
	inventory := func(runtime string) *brokerInventory {
		inv := completeInventory()
		inv.Targets[0].Runtime = runtime
		return inv
	}
	for _, tc := range []struct {
		name      string
		runtime   string
		intent    store.RunIntent
		wantPhase string
	}{
		{"docker target halts", "docker", store.RunIntentRunning, "halted"},
		{"Cloud Run target errors", "cloudrun", store.RunIntentRunning, "error"},
		{"no runtime in the inventory errors", "", store.RunIntentRunning, "error"},
		{"intent stopped errors", "docker", store.RunIntentStopped, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			omitted := f.addAgent("omitted", "running", "working")
			_, err := f.s.SetRunIntent(context.Background(), omitted.ID, tc.intent)
			require.NoError(t, err)

			f.heartbeat(inventory(tc.runtime))
			f.expireClock(omitted.ID)
			f.heartbeat(inventory(tc.runtime))

			got := f.get(omitted.ID)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, "container_missing", got.ExitReason)
		})
	}
}

func TestHalted_TargetRuntimes(t *testing.T) {
	hb := &brokerHeartbeatRequest{Inventory: &brokerInventory{Targets: []brokerInventoryTarget{
		{ID: "docker", Runtime: "docker", Complete: true},
		{ID: "k8s", Runtime: "kubernetes", Complete: true},
		{ID: "dup", Runtime: "docker", Complete: true},
		{ID: "dup", Runtime: "cloudrun", Complete: true},
	}}}
	got := hb.targetRuntimes()
	assert.Equal(t, "docker", got["docker"])
	assert.Equal(t, "kubernetes", got["k8s"])
	assert.Equal(t, "", got["dup"], "conflicting runtimes for one target are not trusted")
	assert.Nil(t, (&brokerHeartbeatRequest{}).targetRuntimes())
}

func TestHalted_PhaseSites(t *testing.T) {
	assert.False(t, isBrokerQuotaCountedPhase("halted"), "a halted agent holds no broker slot")
	assert.True(t, isTerminalAgentPhase("halted"))
	assert.True(t, isStartTerminal("halted"))
	assert.True(t, isStopTerminal("halted"))
	assert.True(t, configPatchPhase("halted"))
	assert.True(t, isConfirmedNonRunningPhase("halted"))
	inPlace, forced := resumeInPlaceDecision("halted", true, false)
	assert.True(t, inPlace)
	assert.False(t, forced)
	assert.NotNil(t, validateAgentDeliverable(&store.Agent{Phase: "halted", Slug: "a"}))
}

// createdCapturingPublisher records the agents PublishAgentCreated and
// PublishAgentRestored were called with.
type createdCapturingPublisher struct {
	noopEventPublisher
	mu      sync.Mutex
	created []*store.Agent
}

func (p *createdCapturingPublisher) PublishAgentCreated(_ context.Context, a *store.Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := *a
	p.created = append(p.created, &cp)
}

func TestHalted_CreatedPublishEnrichesRuntime(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	broker := &store.RuntimeBroker{
		ID:       tid("halt-enrich-broker"),
		Name:     "Halt Enrich Broker",
		Slug:     "halt-enrich-broker",
		Status:   store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{{Name: "k8s-prod", Type: "kubernetes", Available: true}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("halt-enrich-project"), Name: "Halt Enrich", Slug: "halt-enrich"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("halt-enrich-agent"), Slug: "halt-enrich-agent", Name: "Halt Enrich Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: "running",
		AppliedConfig: &store.AgentAppliedConfig{Profile: "k8s-prod"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	pub := &createdCapturingPublisher{}
	srv.events = pub
	require.True(t, srv.publishAgentCreatedIfLive(ctx, agent))
	require.Len(t, pub.created, 1)
	assert.Equal(t, "kubernetes", pub.created[0].Runtime, "the created event carries the display runtime")

	stored, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.Runtime, "the display runtime is never persisted")
}
