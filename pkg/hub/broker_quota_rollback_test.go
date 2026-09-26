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
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingStartDispatcher is a quotaLifecycleDispatcher whose start dispatch
// always fails without touching the agent, as a broker that cannot launch
// the container would.
type failingStartDispatcher struct {
	quotaLifecycleDispatcher
}

func (d *failingStartDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	return errors.New("simulated broker start failure")
}

// ptone/scion#1978: a failed start on an agent that is already running (and
// so already holds its max_agents_per_broker reservation) must keep that
// reservation, and the broker must still reject a start beyond its cap.
func TestBrokerQuota_FailedStartOnRunningAgentKeepsReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &failingStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "rollback-running")
	running := newQuotaTestAgent(t, s, broker, project, "rollback-running", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, running.ID)
	require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+running.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	require.EqualValues(t, 1, disp.startCount.Load(), "the start must have reached dispatch")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID),
		"a failed start must not release a reservation it did not create")

	// The broker is still full: another agent's start is rejected at the cap
	// and never dispatched.
	candidate := newQuotaTestAgent(t, s, broker, project, "rollback-candidate", state.PhaseStopped)
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+candidate.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, disp.startCount.Load(), "a start at the cap must not be dispatched")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// A failed start that took a new reservation still rolls it back, so a
// stopped agent that fails to start does not strand a slot.
func TestBrokerQuota_FailedStartOnStoppedAgentReleasesNewReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &failingStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "rollback-stopped")
	stopped := newQuotaTestAgent(t, s, broker, project, "rollback-stopped", state.PhaseStopped)
	require.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stopped.ID+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	require.EqualValues(t, 1, disp.startCount.Load())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID),
		"a failed start must roll back the reservation it created")
}

// Restart tears the container down in its stop leg (releasing the slot), so
// the start leg's reservation is always new and a failed start leg releases
// it: the agent has no container behind it any more.
func TestBrokerQuota_FailedRestartReleasesReservation(t *testing.T) {
	srv, s := testServer(t)
	disp := &failingStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "rollback-restart")
	running := newQuotaTestAgent(t, s, broker, project, "rollback-restart", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, running.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+running.ID+"/restart", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
}

// failingStopStartDispatcher fails both legs of a restart, as a broker that
// cannot reach a still-running container would.
type failingStopStartDispatcher struct {
	failingStartDispatcher
}

func (d *failingStopStartDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return errors.New("simulated broker stop failure")
}

// ptone/scion#1978: when the stop leg of a restart fails, the container may
// still be running, so its reservation is kept whether or not the start leg
// then succeeds, and the broker cap still holds.
func TestBrokerQuota_FailedStopLegKeepsReservation(t *testing.T) {
	for _, startFails := range []bool{true, false} {
		t.Run(fmt.Sprintf("startFails=%v", startFails), func(t *testing.T) {
			srv, s := testServer(t)
			if startFails {
				srv.SetDispatcher(&failingStopStartDispatcher{})
			} else {
				srv.SetDispatcher(&failingStopDispatcher{})
			}
			setBrokerAgentCeiling(t, s, 1)

			broker, project := newQuotaTestBrokerAndProject(t, s, "restart-stopfail")
			running := newQuotaTestAgent(t, s, broker, project, "restart-stopfail", state.PhaseRunning)
			reserveBrokerSlot(t, s, broker, running.ID)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+running.ID+"/restart", nil)
			if startFails {
				require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
			} else {
				require.Less(t, rec.Code, 300, rec.Body.String())
			}
			assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, running.ID))

			other := newQuotaTestAgent(t, s, broker, project, "restart-stopfail-other", state.PhaseStopped)
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+other.ID+"/start", nil)
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		})
	}
}

// failingStopDispatcher fails only the stop leg.
type failingStopDispatcher struct {
	quotaLifecycleDispatcher
}

func (d *failingStopDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return errors.New("simulated broker stop failure")
}

// Suspended-agent wake via DM: a failed wake rolls back only a reservation
// it created.
func TestBrokerQuota_RollbackOnlyWhenCreated(t *testing.T) {
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 2)
	ctx := context.Background()

	broker, project := newQuotaTestBrokerAndProject(t, s, "rollback-unit")
	held := newQuotaTestAgent(t, s, broker, project, "rollback-held", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, held.ID)
	fresh := newQuotaTestAgent(t, s, broker, project, "rollback-fresh", state.PhaseSuspended)

	created, err := srv.checkAndReserveBrokerQuota(ctx, held)
	require.NoError(t, err)
	assert.False(t, created, "an existing reservation is not a new one")
	srv.rollbackBrokerQuota(ctx, held, created)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))

	created, err = srv.checkAndReserveBrokerQuota(ctx, fresh)
	require.NoError(t, err)
	assert.True(t, created)
	require.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID))
	srv.rollbackBrokerQuota(ctx, fresh, created)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

func TestQuotaService_ReserveReportsCreated(t *testing.T) {
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 1)
	ctx := context.Background()
	broker, _ := newQuotaTestBrokerAndProject(t, s, "reserve-created")
	qs := srv.quotaService

	created, err := qs.Reserve(ctx, store.LimitMaxAgentsPerBroker, broker.ID, store.QuotaScopeBroker, broker.ID, "res-a")
	require.NoError(t, err)
	assert.True(t, created)

	created, err = qs.Reserve(ctx, store.LimitMaxAgentsPerBroker, broker.ID, store.QuotaScopeBroker, broker.ID, "res-a")
	require.NoError(t, err)
	assert.False(t, created, "idempotent re-reserve")

	created, err = qs.Reserve(ctx, store.LimitMaxAgentsPerBroker, broker.ID, store.QuotaScopeBroker, broker.ID, "res-b")
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)
	assert.False(t, created)

	created, err = qs.Reserve(ctx, "no-such-limit", broker.ID, store.QuotaScopeBroker, broker.ID, "res-c")
	require.NoError(t, err)
	assert.False(t, created, "no limit defined: nothing reserved")
}

// swappableStartDispatcher lets a test flip start dispatch between success
// and failure mid-test.
type swappableStartDispatcher struct {
	quotaLifecycleDispatcher
	failStart bool
}

func (d *swappableStartDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	if d.failStart {
		d.startCount.Add(1)
		return errors.New("simulated broker start failure")
	}
	return d.quotaLifecycleDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

func hasReservation(t *testing.T, s store.Store, limitName, resourceID string) bool {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), limitName)
	require.NoError(t, err)
	held, err := s.HasActiveReservation(context.Background(), def.ID, resourceID)
	require.NoError(t, err)
	return held
}

// ptone/scion#1978: the per-project agent reservation lasts as long as the
// agent exists. Releasing the broker slot (stop, suspend, failed-start
// rollback) must not release it; delete releases both.
func TestProjectQuota_KeptAcrossBrokerReleases_ReleasedOnDelete(t *testing.T) {
	disp := &swappableStartDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 5)
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	def.DefaultValue = 5
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "project-quota-keep", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	id := created.Agent.ID
	require.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id))
	require.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id))

	post := func(action string) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/"+action, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", action, rec.Body.String())
	}

	post("stop")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id), "stop releases the broker slot")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id), "stop keeps the project reservation")

	post("start")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id))
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id))

	post("suspend")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id))
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id), "suspend keeps the project reservation")

	disp.failStart = true
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/start", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id), "failed start rolls back the new broker slot")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id), "rollback keeps the project reservation")

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+id, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id))
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id), "delete releases the project reservation")
}

// brokerReservationIDs returns the IDs of broker's active
// max_agents_per_broker reservations.
func brokerReservationIDs(t *testing.T, s store.Store, brokerID string) []string {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	rows, err := s.ListActiveReservations(ctx, def.ID, store.QuotaScopeBroker, brokerID)
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// ptone/scion#1978: a restart holds the agent's broker reservation across
// the stop and start legs instead of releasing it and reserving again, so
// there is no point between the legs where another start could take the
// slot. The reservation row itself must survive the restart.
func TestBrokerQuota_RestartHoldsReservationAcrossLegs(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "restart-hold")
	running := newQuotaTestAgent(t, s, broker, project, "restart-hold", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, running.ID)
	before := brokerReservationIDs(t, s, broker.ID)
	require.Len(t, before, 1)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+running.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, before, brokerReservationIDs(t, s, broker.ID),
		"restart must keep the existing reservation, not release and re-create it")
}
