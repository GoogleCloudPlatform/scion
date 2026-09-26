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

// Admission and release coverage for every site that reserves or releases a
// max_agents_per_broker slot (ptone/scion#1978).

package hub

import (
	"context"
	"encoding/json"
	"fmt"
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

// Two brokers, cap 1 each, N concurrent starts per broker -> exactly one
// success per broker (scopes independent, each scope serialized).
func TestBrokerQuota_TwoBrokersConcurrentStartsIndependent(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 1)
	const N = 6
	type bk struct {
		b   *store.RuntimeBroker
		ids []string
	}
	var bks []bk
	for _, sfx := range []string{"bq-two-a", "bq-two-b"} {
		b, p := newQuotaTestBrokerAndProject(t, s, sfx)
		var ids []string
		for i := 0; i < N; i++ {
			ids = append(ids, newQuotaTestAgent(t, s, b, p, fmt.Sprintf("%s-%d", sfx, i), state.PhaseStopped).ID)
		}
		bks = append(bks, bk{b, ids})
	}
	var ok [2]atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for bi := range bks {
		for _, id := range bks[bi].ids {
			wg.Add(1)
			go func(bi int, id string) {
				defer wg.Done()
				<-start
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/start", nil)
				if rec.Code == http.StatusOK {
					ok[bi].Add(1)
				}
			}(bi, id)
		}
	}
	close(start)
	wg.Wait()
	for bi := range bks {
		assert.EqualValues(t, 1, ok[bi].Load(), "broker %d: exactly one start may succeed", bi)
		assert.EqualValues(t, 1, brokerReservationCount(t, s, bks[bi].b.ID), "broker %d count", bi)
	}
}

// Legacy/mixed state -> reconcile converges to the true
// running set; twice idempotent; reservations for missing/hard-deleted/soft-
// deleted IDs released without panic; empty broker OK; start after backfill
// respects the corrected count.
func TestBrokerQuota_ReconcileConvergesOnMixedState(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	ctx := context.Background()
	setBrokerAgentCeiling(t, s, 3)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-mixed")
	_, _ = newQuotaTestBrokerAndProject(t, s, "bq-mixed-empty") // empty broker, no agents/reservations

	r1 := newQuotaTestAgent(t, s, broker, project, "mixed-run1", state.PhaseRunning) // legacy: no reservation
	r2 := newQuotaTestAgent(t, s, broker, project, "mixed-run2", state.PhaseRunning) // legacy: no reservation
	st := newQuotaTestAgent(t, s, broker, project, "mixed-stopped", state.PhaseStopped)
	reserveBrokerSlot(t, s, broker, st.ID)                                  // stale counted
	reserveBrokerSlot(t, s, broker, "00000000-0000-0000-0000-00000000dead") // missing agent ID
	hd := newQuotaTestAgent(t, s, broker, project, "mixed-harddel", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, hd.ID)
	require.NoError(t, s.DeleteAgent(ctx, hd.ID))
	sd := newQuotaTestAgent(t, s, broker, project, "mixed-softdel", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, sd.ID)
	sd.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, sd))
	stoppedCandidate := newQuotaTestAgent(t, s, broker, project, "mixed-cand", state.PhaseStopped)
	stoppedCandidate2 := newQuotaTestAgent(t, s, broker, project, "mixed-cand2", state.PhaseStopped)
	require.EqualValues(t, 4, brokerReservationCount(t, s, broker.ID), "seed: 4 stale rows, 0 for the 2 running")

	require.NotPanics(t, func() { srv.ReconcileStaleBrokerQuotaReservations(ctx) })
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID), "converge to the 2 running agents")
	def, _ := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	for _, id := range []string{r1.ID, r2.ID} {
		has, _ := s.HasActiveReservation(ctx, def.ID, id)
		assert.True(t, has, "running %s reserved", id)
	}
	require.NotPanics(t, func() { srv.ReconcileStaleBrokerQuotaReservations(ctx) })
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID), "idempotent second pass")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stoppedCandidate.ID+"/start", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stoppedCandidate2.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "cap 3 reached after backfill: %s", rec.Body.String())
	assert.EqualValues(t, 3, brokerReservationCount(t, s, broker.ID))
}

// Soft-delete via the HTTP delete handler releases the slot; reconcile
// afterwards does not double-release (no underflow).
func TestBrokerQuota_SoftDeleteReleasesThenReconcileNoUnderflow(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	srv.config.SoftDeleteRetention = 72 * time.Hour
	ctx := context.Background()
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-softdel")
	a := newQuotaTestAgent(t, s, broker, project, "softdel-a", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	b := newQuotaTestAgent(t, s, broker, project, "softdel-b", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, b.ID)
	c := newQuotaTestAgent(t, s, broker, project, "softdel-c", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+c.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, err := s.GetAgent(ctx, a.ID)
	if err == nil {
		assert.False(t, got.DeletedAt.IsZero(), "expected a SOFT delete")
	}
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "soft-delete released")
	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "no double release / underflow")
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+c.ID+"/start", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Restart of a STOPPED agent
// on a full broker must be refused with 429 before any start dispatch.
func TestBrokerQuota_RestartAtCapRejected_NoDispatch(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-restart")
	held := newQuotaTestAgent(t, s, broker, project, "bq-restart-held", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, held.ID)
	cand := newQuotaTestAgent(t, s, broker, project, "bq-restart-cand", state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/restart", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.EqualValues(t, 0, disp.startCount.Load(), "restart at cap must not dispatch a start")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// Restart with a free slot succeeds and
// leaves the restarted agent reserved (count == running set).
func TestBrokerQuota_RestartWithFreeSlotReserves(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-restart-ok")
	held := newQuotaTestAgent(t, s, broker, project, "bq-restart-ok-held", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, held.ID)
	cand := newQuotaTestAgent(t, s, broker, project, "bq-restart-ok-cand", state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/restart", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, disp.startCount.Load())
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID))
}

// createExistingAgentQuotaSetup: 2 agents created via POST (cap 2), the second is moved to
// phaseB via the lifecycle endpoint, a third create fills the cap again.
func createExistingAgentQuotaSetup(t *testing.T, action string) (srv *Server, s store.Store, disp *quotaLifecycleDispatcher, project *store.Project, targetName, fillerID string) {
	t.Helper()
	disp = &quotaLifecycleDispatcher{}
	srv, s, project = setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 2)
	brokerID := project.DefaultRuntimeBrokerID
	ids := map[string]string{}
	for _, n := range []string{"bqc-a", "bqc-b", "bqc-c"} {
		if n == "bqc-c" {
			// POST-created agents sit in "provisioning" on the fake dispatcher; mark b running first.
			require.NoError(t, s.UpdateAgentStatus(context.Background(), ids["bqc-b"], store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+ids["bqc-b"]+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, brokerReservationCount(t, s, brokerID), action+" released b")
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: n, ProjectID: project.ID})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var cr CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cr))
		ids[n] = cr.Agent.ID
	}
	require.EqualValues(t, 2, brokerReservationCount(t, s, brokerID), "cap full (a, c)")
	return srv, s, disp, project, "bqc-b", ids["bqc-c"]
}

func assertCreateExistingAgentAtCap(t *testing.T, action string) {
	srv, s, disp, project, name, filler := createExistingAgentQuotaSetup(t, action)
	brokerID := project.DefaultRuntimeBrokerID
	before := disp.startCount.Load()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: name, ProjectID: project.ID, Resume: true})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "POST create of %s agent at cap: %s", action, rec.Body.String())
	assert.Equal(t, before, disp.startCount.Load(), "no start dispatch at cap")
	assert.EqualValues(t, 2, brokerReservationCount(t, s, brokerID))
	// positive control: free a slot, the same create now resumes/starts and reserves
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+filler+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: name, ProjectID: project.ID, Resume: true})
	assert.Less(t, rec.Code, 300, "control: create of %s agent with a free slot: %s", action, rec.Body.String())
	assert.EqualValues(t, 2, brokerReservationCount(t, s, brokerID), "control: re-reserved")
}

// Create of an existing suspended agent (resume) is refused at the cap.
func TestBrokerQuota_CreateResumeSuspendedAtCapRejected(t *testing.T) {
	assertCreateExistingAgentAtCap(t, "suspend")
}

// Create of an existing stopped agent (start) is refused at the cap.
func TestBrokerQuota_CreateStartStoppedAtCapRejected(t *testing.T) {
	assertCreateExistingAgentAtCap(t, "stop")
}

// Waking a suspended agent for a DM is refused at the cap, and succeeds once
// a slot is free.
func TestBrokerQuota_WakeAtCapRejected(t *testing.T) {
	srv, s := testServer(t)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-wake")
	held := newQuotaTestAgent(t, s, broker, project, "bq-wake-held", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, held.ID)
	target := newQuotaTestAgent(t, s, broker, project, "bq-wake-target", state.PhaseSuspended)

	_, dmErr := srv.wakeAgentForDM(context.Background(), target)
	require.NotNil(t, dmErr, "wake at cap must fail")
	assert.Equal(t, http.StatusTooManyRequests, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeQuotaExceeded, dmErr.Code)
	assert.Len(t, disp.getStartCalls(), 0, "no resume dispatch at cap")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	// positive control: free the slot, the wake dispatches and reserves.
	srv.releaseBrokerQuota(context.Background(), held)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning), Activity: "idle"})
	}()
	_, dmErr = srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, dmErr)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// Stop-all frees the broker
// slots of the agents it stops; a start at the former cap then succeeds.
func TestBrokerQuota_StopAllReleasesSlots(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-stopall")
	for _, n := range []string{"bq-stopall-a", "bq-stopall-b"} {
		a := newQuotaTestAgent(t, s, broker, project, n, state.PhaseRunning)
		reserveBrokerSlot(t, s, broker, a.ID)
	}
	cand := newQuotaTestAgent(t, s, broker, project, "bq-stopall-cand", state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "control: full before stop-all: %s", rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "stop-all must release the stopped agents' slots")
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/start", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Auto-suspend releases the slot; a failed auto-suspend stop keeps it, since
// the container is still up.
func TestBrokerQuota_AutoSuspendReleasesSlot(t *testing.T) {
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 3)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-autosuspend")
	a := newQuotaTestAgent(t, s, broker, project, "bq-autosuspend-a", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	b := newQuotaTestAgent(t, s, broker, project, "bq-autosuspend-b", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, b.ID)

	srv.SetDispatcher(&failingStopStartDispatcher{})
	srv.autoSuspendStalledAgents(ctx, []store.Agent{*b})
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID), "control: failed auto-suspend keeps the slot")

	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	srv.autoSuspendStalledAgents(ctx, []store.Agent{*a})
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "auto-suspend must release the slot")
}

// Delete releases the broker slot itself, not only the project reservation.
func TestBrokerQuota_HardDeleteReleasesBrokerSlot(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-delete")
	a := newQuotaTestAgent(t, s, broker, project, "bq-delete-a", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	b := newQuotaTestAgent(t, s, broker, project, "bq-delete-b", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, b.ID)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "delete must release the broker slot immediately")
}

// "start" on an already-running,
// already-reserved agent at a FULL cap is a no-op success (200), not 429/500,
// and never double-counts. Positive control: a different stopped agent at
// the same full cap IS rejected with 429.
func TestBrokerQuota_StartOnRunningAtFullCapIsIdempotent(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-idem")
	a := newQuotaTestAgent(t, s, broker, project, "bq-idem-a", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	for i := 0; i < 2; i++ {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		assert.Equal(t, http.StatusOK, rec.Code, "start #%d on running agent at full cap: %s", i+1, rec.Body.String())
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "no double count")
	}
	other := newQuotaTestAgent(t, s, broker, project, "bq-idem-other", state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+other.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "control: %s", rec.Body.String())
}

// A self-reported phase
// change to a non-counted phase (error) via POST /agents/{id}/status releases
// the slot. Positive control: a counted-phase report (running, activity idle)
// keeps it.
func TestBrokerQuota_StatusReportErrorReleasesSlot(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 3)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-status")
	a := newQuotaTestAgent(t, s, broker, project, "bq-status-a", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	b := newQuotaTestAgent(t, s, broker, project, "bq-status-b", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, b.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+b.ID+"/status", map[string]string{"phase": string(state.PhaseRunning), "activity": "idle"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID), "control: counted report keeps the slot")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/status", map[string]string{"phase": string(state.PhaseError)})
	require.Less(t, rec.Code, 300, rec.Body.String())
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseError), got.Phase, "status persisted")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "self-reported error must release the slot")
}
