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
	"errors"
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
