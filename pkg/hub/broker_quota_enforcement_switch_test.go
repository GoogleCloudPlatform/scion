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

// Tests for the "Enforce broker agent quotas" hub-settings switch
// (design ptone/scion#2061 P1-D4/P1-D5): pkg/hub/server.go
// (Server.brokerQuotasEnforced, ServerConfig.EnforceBrokerQuotas) and
// pkg/hub/quota.go (QuotaService.enforced/isEnforced, Reserve).

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

// setProjectAgentCeiling overrides the seeded max_agents_per_project limit
// (default 0 = unlimited, see seed.go) to a small value so tests can hit it.
func setProjectAgentCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err, "max_agents_per_project must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// Regression test for review finding F1 (ptone/scion#2270 round 1):
// brokerQuotasEnforced() must read s.config.EnforceBrokerQuotas under
// s.mu.RLock(), matching the s.mu.Lock() ApplySnapshot writes it under. Run
// with -race, this reproduces the reviewer's confirmed data race before the
// fix (concurrent ApplySnapshot writers and brokerQuotasEnforced() readers on
// the same field) and passes clean after it.
func TestBrokerQuotasEnforced_ConcurrentApplySnapshotIsRaceFree(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}

	const iterations = 200
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			v := i%2 == 0
			ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: &v})
		}
	}()

	for i := 0; i < iterations; i++ {
		_ = srv.brokerQuotasEnforced()
	}
	<-done
}

// Test 1 (design 4.7 P1b): the switch defaults to unset, which must mean
// enforced (fail-safe default) — an over-cap create is rejected exactly as
// before this feature existed.
func TestBrokerQuotaSwitch_UnsetIsEnforced(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	require.Nil(t, srv.config.EnforceBrokerQuotas, "switch must default to unset")
	setBrokerAgentCeiling(t, s, 1)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-unset-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-unset-2", ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
}

// Test 2 (design 4.7 P1b): with the switch off, a create beyond the cap
// returns 201, and a reservation row exists for it (the count stays
// accurate while enforcement is off).
func TestBrokerQuotaSwitch_OffAllowsOverCapAndCounts(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	srv.config.EnforceBrokerQuotas = boolPtr(false)
	setBrokerAgentCeiling(t, s, 1)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-off-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))

	// At the cap, but enforcement is off: the create must still succeed, and
	// still create a reservation (design P1-D5: "keep counting").
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-off-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
	assert.EqualValues(t, 2, brokerReservationCount(t, s, brokerID),
		"a reservation row must exist for the over-cap create")
}

// Test 3 (design 4.7 P1b): switching enforcement back on makes the very next
// over-cap create return 429 immediately, with no reconcile pass needed —
// because counting never stopped while the switch was off.
func TestBrokerQuotaSwitch_OffThenOnRejectsImmediately(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	srv.config.EnforceBrokerQuotas = boolPtr(false)
	setBrokerAgentCeiling(t, s, 1)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-flip-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-flip-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String(), "over cap while off must succeed")

	// Flip the switch back on.
	srv.config.EnforceBrokerQuotas = boolPtr(true)

	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-flip-3", ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusTooManyRequests, rec3.Code, rec3.Body.String(),
		"re-enabling enforcement must reject the next over-cap create immediately")
}

// Test 4 (design 4.7 P1b): with the switch off, lock contention on the
// broker scope must let the request proceed (Reserve returns (false, nil))
// instead of surfacing ErrQuotaLockContention.
func TestBrokerQuotaSwitch_OffLockContentionProceeds(t *testing.T) {
	qs, s := newTestQuotaService(t)
	qs.enforced = func(limitName string) bool { return false }
	ctx := context.Background()

	seedLimit(t, s, "max_test_lock_off", 10)

	// Pre-acquire the advisory lock to force contention, exactly as
	// TestCheckAndReserve_LockContentionReturnsRetryableError does.
	wrapper := qs.store.(*lockingStoreWrapper)
	objID := store.StableProjectHash("p1")
	acquired, release, err := wrapper.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, objID)
	require.NoError(t, err)
	require.True(t, acquired, "should acquire lock in test setup")
	defer func() { _ = release() }()

	created, err := qs.Reserve(ctx, "max_test_lock_off", "user-1", "project", "p1", "r1")
	require.NoError(t, err, "lock contention while enforcement is off must not error")
	assert.False(t, created, "no reservation is made on the contended path")
}

// TestBrokerQuotaSwitch_OffLockContentionProceedsEndToEnd is the
// review-requested (F5) end-to-end variant of
// TestBrokerQuotaSwitch_OffLockContentionProceeds: it goes through the real
// server wiring (srv.quotaService, store.LimitMaxAgentsPerBroker,
// srv.config.EnforceBrokerQuotas) and a real HTTP create request, rather than
// a synthetic limit and a stubbed enforced func.
func TestBrokerQuotaSwitch_OffLockContentionProceedsEndToEnd(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	srv.config.EnforceBrokerQuotas = boolPtr(false)
	setBrokerAgentCeiling(t, s, 10) // high enough that only lock contention is in play
	brokerID := project.DefaultRuntimeBrokerID

	// Wrap the store so TryAdvisoryLockObject behaves like a real lock (on
	// SQLite it is normally a documented no-op) — the same trick
	// newTestQuotaService uses — without disturbing any other store method
	// the create path relies on.
	wrapper := newLockingStoreWrapper(s)
	srv.quotaService.store = wrapper

	objID := store.StableProjectHash(brokerID)
	acquired, release, err := wrapper.TryAdvisoryLockObject(context.Background(), store.LockQuotaEnforcement, objID)
	require.NoError(t, err)
	require.True(t, acquired, "should acquire the broker's advisory lock in test setup")
	defer func() { _ = release() }()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "switch-off-lock-e2e", ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String(),
		"create must proceed through real lock contention on max_agents_per_broker while enforcement is off")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID),
		"no reservation is made on the contended path, same as the direct QuotaService test")
}

// Test 5 (design 4.7 P1b): a DM wake (the non-HTTP enforcement site,
// wake_dm.go) over the broker cap succeeds when the switch is off, since it
// goes through the same Reserve() as every other site.
func TestBrokerQuotaSwitch_OffAllowsWakeOverCap(t *testing.T) {
	srv, s := testServer(t)
	srv.config.EnforceBrokerQuotas = boolPtr(false)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "bq-switch-wake")
	held := newQuotaTestAgent(t, s, broker, project, "bq-switch-wake-held", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, held.ID)
	target := newQuotaTestAgent(t, s, broker, project, "bq-switch-wake-target", state.PhaseSuspended)

	// wakeAgentForDM waits for the broker to report the agent ready; simulate
	// that confirmation the same way TestBrokerQuota_WakeAtCapRejected does.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning), Activity: "idle"})
	}()

	_, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, dmErr, "wake over cap must succeed while enforcement is off")
	assert.Len(t, disp.getStartCalls(), 1, "wake must still dispatch the resume")
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID),
		"a reservation row must exist for the woken agent too")
}

// Test 6 (design 4.7 P1b): the switch only exempts max_agents_per_broker.
// max_agents_per_project is still enforced while the broker switch is off.
func TestBrokerQuotaSwitch_OffProjectCapStillEnforced(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	srv.config.EnforceBrokerQuotas = boolPtr(false)
	setProjectAgentCeiling(t, s, 1)
	// Leave the broker cap generously high so only the project cap can bind.
	setBrokerAgentCeiling(t, s, 1000)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "project-cap-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "project-cap-2", ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String(),
		"max_agents_per_project must still be enforced when only the broker switch is off")
}
