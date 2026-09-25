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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quotaLifecycleDispatcher extends createAgentDispatcher with atomic
// start/stop call counters and phase mutation that mimics a real broker's
// start/stop acknowledgment (handleAgentLifecycle reuses the broker-reported
// phase off the agent pointer it passed in). Used to test the
// max_agents_per_broker reservation lifecycle across stop/start/resume/crash
// (ptone/scion#1963).
type quotaLifecycleDispatcher struct {
	createAgentDispatcher
	startCount atomic.Int32
	stopCount  atomic.Int32
}

func (d *quotaLifecycleDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

func (d *quotaLifecycleDispatcher) DispatchAgentStop(_ context.Context, agent *store.Agent) error {
	d.stopCount.Add(1)
	agent.Phase = string(state.PhaseStopped)
	agent.ContainerStatus = "stopped"
	return nil
}

// brokerReservationCount returns the number of active (non-released)
// max_agents_per_broker reservations for broker.
func brokerReservationCount(t *testing.T, s store.Store, brokerID string) int64 {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	n, err := s.CountActiveReservations(context.Background(), def.ID, brokerID, store.QuotaScopeBroker, brokerID)
	require.NoError(t, err)
	return n
}

// newQuotaTestBrokerAndProject creates an online runtime broker and a project
// wired to it (project provider + DefaultRuntimeBrokerID), the same wiring
// setupCreateAgentServer uses, so both the full create-agent HTTP flow and
// directly store-created agents can share one broker.
func newQuotaTestBrokerAndProject(t *testing.T, s store.Store, suffix string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-quota-" + suffix),
		Name:   "Quota Broker " + suffix,
		Slug:   "quota-broker-" + suffix,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   tid("proj-quota-" + suffix),
		Name: "Quota Project " + suffix,
		Slug: "quota-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	return broker, project
}

// newQuotaTestAgent creates an agent directly via the store (bypassing the
// create-agent HTTP flow and its automatic reservation) in the given phase,
// assigned to broker/project.
func newQuotaTestAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// reserveBrokerSlot manually creates a max_agents_per_broker reservation for
// agentID against broker, mirroring what createAgentInProject would have done
// had the agent been created through the normal HTTP flow.
func reserveBrokerSlot(t *testing.T, s store.Store, broker *store.RuntimeBroker, agentID string) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	_, err = s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         broker.ID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           broker.ID,
		ResourceID:        agentID,
		Reserved:          1,
	})
	require.NoError(t, err)
}

// TestBrokerQuota_StopFreesSlot is the core regression test for
// ptone/scion#1963: a stopped agent must not continue consuming its broker's
// max_agents_per_broker slot.
func TestBrokerQuota_StopFreesSlot(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "quota-stop-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))

	// At the cap: a second create must be rejected outright.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "quota-stop-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())

	// Stop the first agent — its slot must be released.
	recStop := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, recStop.Code, recStop.Body.String())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID), "stop must release the reservation")

	// A new create should now succeed.
	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "quota-stop-3", ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
}

// TestBrokerQuota_SuspendFreesSlot mirrors the stop test for suspend.
func TestBrokerQuota_SuspendFreesSlot(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "suspend")
	agent := newQuotaTestAgent(t, s, broker, project, "suspend-agent", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, agent.ID)
	require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "suspend must release the reservation")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// TestBrokerQuota_StartAtCapRejected_NoDispatch verifies that a start which
// would exceed the broker's ceiling is rejected before the broker is ever
// dispatched to, and leaves the agent's phase untouched.
func TestBrokerQuota_StartAtCapRejected_NoDispatch(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "cap")

	// occupant holds the only slot.
	occupant := newQuotaTestAgent(t, s, broker, project, "cap-occupant", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, occupant.ID)

	// candidate is stopped (no reservation) and wants to start.
	candidate := newQuotaTestAgent(t, s, broker, project, "cap-candidate", state.PhaseStopped)

	startsBefore := disp.startCount.Load()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+candidate.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, startsBefore, disp.startCount.Load(), "rejected start must not reach the broker dispatch")

	got, err := s.GetAgent(context.Background(), candidate.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "rejected start must not change the agent's phase")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "the occupant's reservation must be undisturbed")
}

// TestBrokerQuota_ResumeReReserves verifies that resuming a suspended agent
// re-reserves its broker slot, and that the resume is itself gated by the
// cap like any other start.
func TestBrokerQuota_ResumeReReserves(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "resume")
	agent := newQuotaTestAgent(t, s, broker, project, "resume-agent", state.PhaseSuspended)
	// No reservation: suspend already released it.
	require.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))

	// Resuming into the free slot must succeed and re-reserve.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)

	// Suspend again, let another agent take the slot, then resuming the
	// first agent must be rejected because the slot is taken.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	other := newQuotaTestAgent(t, s, broker, project, "resume-other", state.PhaseSuspended)
	recOther := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+other.ID+"/start", nil)
	require.Equal(t, http.StatusOK, recOther.Code, recOther.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	recBlocked := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, recBlocked.Code, recBlocked.Body.String())
}

// TestBrokerQuota_CrashViaHeartbeatReleases verifies that when the hub
// observes (via broker heartbeat) that a container crashed/exited, the
// agent's max_agents_per_broker reservation is released.
func TestBrokerQuota_CrashViaHeartbeatReleases(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)

	broker, project := newQuotaTestBrokerAndProject(t, s, "crash")
	agent := newQuotaTestAgent(t, s, broker, project, "crash-agent", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, agent.ID)
	require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	ec := 1
	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:       agent.Slug,
		Phase:      "stopped",
		Activity:   "crashed",
		ExitCode:   &ec,
		ExitReason: "crashed",
	})
	require.Equal(t, http.StatusOK, code)

	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "an observed crash must release the reservation")

	got := getAgentState(t, s, agent.Slug, project.ID)
	assert.Equal(t, string(state.PhaseError), got.Phase, "non-zero exit code promotes stopped to error")
}

// TestBrokerQuota_CleanStopViaHeartbeatReleases mirrors the crash test for a
// clean exit (exit code 0) reported via heartbeat rather than an explicit
// stop action — e.g. the container exited on its own.
func TestBrokerQuota_CleanStopViaHeartbeatReleases(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)

	broker, project := newQuotaTestBrokerAndProject(t, s, "cleanexit")
	agent := newQuotaTestAgent(t, s, broker, project, "cleanexit-agent", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, agent.ID)
	require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	zero := 0
	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:     agent.Slug,
		Phase:    "stopped",
		ExitCode: &zero,
	})
	require.Equal(t, http.StatusOK, code)

	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "an observed clean exit must release the reservation")

	got := getAgentState(t, s, agent.Slug, project.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}

// TestBrokerQuota_StopStartCycleDoesNotLeak runs several stop/start cycles on
// the same agent against a ceiling of 1 and verifies the reservation count
// never exceeds 1 and never drops below 0 — i.e. cycling never leaks or
// double-books a slot.
func TestBrokerQuota_StopStartCycleDoesNotLeak(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "cycle-agent", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	agentID := created.Agent.ID
	require.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))

	for i := 0; i < 5; i++ {
		recStop := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/stop", nil)
		require.Equalf(t, http.StatusOK, recStop.Code, "stop #%d: %s", i, recStop.Body.String())
		assert.EqualValuesf(t, 0, brokerReservationCount(t, s, brokerID), "stop #%d must leave no active reservation", i)

		recStart := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/start", nil)
		require.Equalf(t, http.StatusOK, recStart.Code, "start #%d: %s", i, recStart.Body.String())
		assert.EqualValuesf(t, 1, brokerReservationCount(t, s, brokerID), "start #%d must hold exactly one reservation", i)
	}
}

// TestBrokerQuota_ReconcileFixesStaleRows verifies that
// ReconcileStaleBrokerQuotaReservations releases reservations left behind for
// agents that are stopped or suspended (the pre-fix drift ptone/scion#1963
// describes) while leaving a running agent's reservation untouched, and that
// running it again is a no-op (idempotent).
func TestBrokerQuota_ReconcileFixesStaleRows(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "stale")

	stoppedAgent := newQuotaTestAgent(t, s, broker, project, "stale-stopped", state.PhaseStopped)
	reserveBrokerSlot(t, s, broker, stoppedAgent.ID)

	suspendedAgent := newQuotaTestAgent(t, s, broker, project, "stale-suspended", state.PhaseSuspended)
	reserveBrokerSlot(t, s, broker, suspendedAgent.ID)

	erroredAgent := newQuotaTestAgent(t, s, broker, project, "stale-error", state.PhaseError)
	reserveBrokerSlot(t, s, broker, erroredAgent.ID)

	runningAgent := newQuotaTestAgent(t, s, broker, project, "stale-running", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, runningAgent.ID)

	require.EqualValues(t, 4, brokerReservationCount(t, s, broker.ID))

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID),
		"only the running agent's reservation should survive reconcile")

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	has, err := s.HasActiveReservation(context.Background(), def.ID, runningAgent.ID)
	require.NoError(t, err)
	assert.True(t, has, "running agent's reservation must survive reconcile")

	for _, id := range []string{stoppedAgent.ID, suspendedAgent.ID, erroredAgent.ID} {
		has, err := s.HasActiveReservation(context.Background(), def.ID, id)
		require.NoError(t, err)
		assert.False(t, has, "non-counted agent %s must have its reservation released", id)
	}

	// Idempotent: running again changes nothing further.
	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// TestBrokerQuota_ConcurrentStartsAtCapMinusOne_ExactlyOneSucceeds is the
// concurrency regression test for ptone/scion#1963: with exactly one free
// slot, two simultaneous start requests must not both succeed.
func TestBrokerQuota_ConcurrentStartsAtCapMinusOne_ExactlyOneSucceeds(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "race")
	a1 := newQuotaTestAgent(t, s, broker, project, "race-1", state.PhaseStopped)
	a2 := newQuotaTestAgent(t, s, broker, project, "race-2", state.PhaseStopped)
	ids := []string{a1.ID, a2.ID}

	var wg sync.WaitGroup
	codes := make([]int, len(ids))
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+ids[i]+"/start", nil)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	successCount, rejectCount := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			successCount++
		case http.StatusTooManyRequests:
			rejectCount++
		}
	}
	assert.Equal(t, 1, successCount, "exactly one concurrent start must succeed at cap-1: codes=%v", codes)
	assert.Equal(t, 1, rejectCount, "the other concurrent start must be rejected: codes=%v", codes)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// TestBrokerQuota_ReconcileMultiBroker is the multi-broker case for the
// gemini review of ptone/scion#1963's N+1 fix: ReconcileStaleBrokerQuotaReservations
// now batch-fetches reserved agents per broker via GetAgentsByIDs instead of
// one GetAgent call per reservation. Two brokers, each with one stale and one
// live reservation, must each be reconciled independently and correctly in a
// single pass — proving the per-broker agent map isn't accidentally shared or
// mixed up across brokers.
func TestBrokerQuota_ReconcileMultiBroker(t *testing.T) {
	srv, s := testServer(t)

	brokerA, projectA := newQuotaTestBrokerAndProject(t, s, "multi-a")
	staleA := newQuotaTestAgent(t, s, brokerA, projectA, "multi-a-stale", state.PhaseStopped)
	reserveBrokerSlot(t, s, brokerA, staleA.ID)
	liveA := newQuotaTestAgent(t, s, brokerA, projectA, "multi-a-live", state.PhaseRunning)
	reserveBrokerSlot(t, s, brokerA, liveA.ID)

	brokerB, projectB := newQuotaTestBrokerAndProject(t, s, "multi-b")
	staleB := newQuotaTestAgent(t, s, brokerB, projectB, "multi-b-stale", state.PhaseSuspended)
	reserveBrokerSlot(t, s, brokerB, staleB.ID)
	liveB := newQuotaTestAgent(t, s, brokerB, projectB, "multi-b-live", state.PhaseRunning)
	reserveBrokerSlot(t, s, brokerB, liveB.ID)

	require.EqualValues(t, 2, brokerReservationCount(t, s, brokerA.ID))
	require.EqualValues(t, 2, brokerReservationCount(t, s, brokerB.ID))

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerA.ID), "broker A: only the live agent's reservation should survive")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerB.ID), "broker B: only the live agent's reservation should survive")

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	for _, id := range []string{liveA.ID, liveB.ID} {
		has, err := s.HasActiveReservation(context.Background(), def.ID, id)
		require.NoError(t, err)
		assert.True(t, has, "live agent %s's reservation must survive reconcile", id)
	}
	for _, id := range []string{staleA.ID, staleB.ID} {
		has, err := s.HasActiveReservation(context.Background(), def.ID, id)
		require.NoError(t, err)
		assert.False(t, has, "stale agent %s's reservation must be released", id)
	}
}

// TestBrokerQuota_ReconcileBackfillsLegacyAgent is the ptone/scion#1963 item-7
// regression: an agent in a counted phase with no active reservation at all
// (e.g. created before max_agents_per_broker existed) must get one backfilled
// by the reconcile, with no cap check — this is accounting for an agent that
// already exists and is already running, not a new admission decision.
// Backfilling must be idempotent: running the reconcile again must not create
// a second reservation for the same agent.
func TestBrokerQuota_ReconcileBackfillsLegacyAgent(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "backfill")

	legacy := newQuotaTestAgent(t, s, broker, project, "backfill-legacy", state.PhaseRunning)
	require.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "legacy agent starts with no reservation")

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "reconcile must backfill a reservation for the legacy running agent")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	has, err := s.HasActiveReservation(context.Background(), def.ID, legacy.ID)
	require.NoError(t, err)
	assert.True(t, has)

	// Idempotent: running again must not create a second reservation.
	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "backfill must not double-reserve on a second pass")
}

// TestBrokerQuota_ReconcileReleasesSoftDeletedAgent is the ptone/scion#1963
// item-5 regression: a soft-deleted agent's phase is not guaranteed to be
// stopped/suspended/error (isBrokerQuotaCountedPhase's exclusion list), so
// checking phase alone is not enough to know the agent is gone. GetAgentsByIDs
// excludes soft-deleted rows, so a soft-deleted agent's reservation must be
// released via the same "missing from the batch-fetch" path as a hard-deleted
// agent's, regardless of its stored phase.
func TestBrokerQuota_ReconcileReleasesSoftDeletedAgent(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "softdel")

	agent := newQuotaTestAgent(t, s, broker, project, "softdel-agent", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, agent.ID)
	require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	// Soft-delete without touching phase, to isolate DeletedAt as the signal
	// under test (a real delete path also flips phase to stopped, which
	// would let isBrokerQuotaCountedPhase alone catch this).
	agent.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID),
		"a soft-deleted agent's reservation must be released even though its phase still reads running")
}

// TestBrokerQuota_StartAtCapRejected_NamesLimitInError is the ptone/scion#1963
// item-8 regression: a legacy agent with no reservation that loses the race
// for the last slot at start/resume must get a 429 whose message clearly
// names the exceeded limit (max_agents_per_broker), not a generic error.
func TestBrokerQuota_StartAtCapRejected_NamesLimitInError(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "namedlimit")

	occupant := newQuotaTestAgent(t, s, broker, project, "namedlimit-occupant", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, occupant.ID)

	// candidate is a legacy agent: stopped, and has never held a reservation.
	candidate := newQuotaTestAgent(t, s, broker, project, "namedlimit-candidate", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+candidate.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), store.LimitMaxAgentsPerBroker,
		"the rejection must name the exceeded limit; body: %s", rec.Body.String())
}
