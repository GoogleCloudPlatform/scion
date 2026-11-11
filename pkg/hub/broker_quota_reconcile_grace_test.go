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

// Coverage for ptone/scion#2011: both the DM wake path and the HTTP
// start/resume path reserve a broker slot before dispatch but only write the
// counted phase (e.g. "starting"/"running") after dispatch returns. A
// reconcile pass that lands in that window sees the agent's pre-dispatch
// (uncounted) stored phase and, without a grace period, releases the
// in-flight reservation out from under the dispatch that is still running.

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

// reconcileMidDispatchDispatcher runs a reconcile pass from inside
// DispatchAgentStart, simulating the periodic reconcile tick (or a startup
// sweep) landing while a broker dispatch is still in flight — after the
// caller reserved the slot but before it has written the counted phase back
// to the store.
type reconcileMidDispatchDispatcher struct {
	quotaLifecycleDispatcher
	srv *Server
}

func (d *reconcileMidDispatchDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	return d.quotaLifecycleDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

// TestBrokerQuota_ReconcileDuringDispatch is the regression test for
// ptone/scion#2011: a reconcile pass landing mid-dispatch must not release a
// reservation taken for a dispatch that is still running, for either the DM
// wake path or the HTTP start path. Without the fix, the reconcile drops the
// count to 0 while the dispatch is in flight and a second start at cap 1
// wrongly succeeds (200); with the fix, the reservation survives the
// mid-dispatch reconcile and the second start is correctly rejected (429).
func TestBrokerQuota_ReconcileDuringDispatch(t *testing.T) {
	t.Run("wake", func(t *testing.T) {
		srv, s := testServer(t)
		setBrokerAgentCeiling(t, s, 1)
		grantDevUserRuntimeBrokerAccess(t, s)
		broker, project := newQuotaTestBrokerAndProject(t, s, "grace-wake")
		target := newQuotaTestAgent(t, s, broker, project, "grace-wake-target", state.PhaseSuspended)
		srv.SetDispatcher(&reconcileMidDispatchDispatcher{srv: srv})

		// A short deadline so waitForAgentReady times out quickly: nothing in
		// this test ever reports harness activity for target.
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_, dmErr := srv.wakeAgentForDM(ctx, target)
		require.NotNil(t, dmErr, "wake must fail on readiness timeout")
		require.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus, dmErr.Message)

		require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID),
			"the reconcile pass that ran mid-dispatch must not have released the wake's reservation")

		srv.SetDispatcher(&quotaLifecycleDispatcher{})
		other := newQuotaTestAgent(t, s, broker, project, "grace-wake-other", state.PhaseStopped)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+other.ID+"/start", nil)
		assertBrokerQuotaExceeded(t, rec)
	})

	t.Run("http-start", func(t *testing.T) {
		srv, s := testServer(t)
		setBrokerAgentCeiling(t, s, 1)
		broker, project := newQuotaTestBrokerAndProject(t, s, "grace-http")
		a := newQuotaTestAgent(t, s, broker, project, "grace-http-a", state.PhaseStopped)
		srv.SetDispatcher(&reconcileMidDispatchDispatcher{srv: srv})

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID),
			"the reconcile pass that ran mid-dispatch must not have released the start's reservation")

		srv.SetDispatcher(&quotaLifecycleDispatcher{})
		b := newQuotaTestAgent(t, s, broker, project, "grace-http-b", state.PhaseStopped)
		rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+b.ID+"/start", nil)
		assertBrokerQuotaExceeded(t, rec2)
	})
}

// TestBrokerQuota_ReconcileGracePeriodBoundary exercises
// reconcileMinReservationAge directly: a reservation for an uncounted-phase
// agent younger than the grace period must survive a reconcile pass, one at
// least as old must be released, and the age check must not affect the
// always-release case of a missing/deleted agent.
func TestBrokerQuota_ReconcileGracePeriodBoundary(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "grace-boundary")

	fresh := newQuotaTestAgent(t, s, broker, project, "grace-boundary-fresh", state.PhaseStopped)
	reserveBrokerSlot(t, s, broker, fresh.ID) // age ~0

	old := newQuotaTestAgent(t, s, broker, project, "grace-boundary-old", state.PhaseStopped)
	reserveStaleBrokerSlot(t, s, broker, old.ID) // age well past the grace period

	// A missing agent's reservation must still be released regardless of age.
	reserveBrokerSlot(t, s, broker, "00000000-0000-0000-0000-00000000ac1d") // never a real agent

	require.EqualValues(t, 3, brokerReservationCount(t, s, broker.ID))

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	hasFresh, err := s.HasActiveReservation(context.Background(), def.ID, fresh.ID)
	require.NoError(t, err)
	assert.True(t, hasFresh, "a reservation younger than the grace period must survive reconcile")

	hasOld, err := s.HasActiveReservation(context.Background(), def.ID, old.ID)
	require.NoError(t, err)
	assert.False(t, hasOld, "a reservation at least as old as the grace period must still be released")

	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID),
		"only the fresh reservation should remain: the old one and the missing agent's are both released")
}
