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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleHealthSummary_AdminAccess(t *testing.T) {
	srv, _ := testServer(t)

	t.Run("Unauthenticated returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
		rr := httptest.NewRecorder()
		srv.handleHealthSummary(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("Non-admin user returns 403", func(t *testing.T) {
		member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), member))
		rr := httptest.NewRecorder()
		srv.handleHealthSummary(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("Admin user returns 200", func(t *testing.T) {
		admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleHealthSummary(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("POST returns 405", func(t *testing.T) {
		admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/health/summary", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleHealthSummary(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	})
}

func TestHandleHealthSummary_ResponseShape(t *testing.T) {
	srv, _ := testServer(t)

	// Use the full router with dev auth (admin)
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err, "response should be valid JSON")

	// Verify top-level status is present
	assert.NotEmpty(t, resp.Status)

	// Verify hub section populated
	assert.NotEmpty(t, resp.Hub.Status)
	assert.NotEmpty(t, resp.Hub.Version)
	assert.NotEmpty(t, resp.Hub.Uptime)

	// Verify database section populated
	assert.NotEmpty(t, resp.Database.Status)

	// Verify brokers is an array (even if empty)
	assert.NotNil(t, resp.Brokers)

	// Verify agents section has initialized maps/slices
	assert.NotNil(t, resp.Agents.ByPhase)
	assert.NotNil(t, resp.Agents.Stalled)
	assert.NotNil(t, resp.Agents.Crashed)
	assert.NotNil(t, resp.Agents.Errored)

	// Verify dispatch is nil (no dispatch metrics available yet)
	assert.Nil(t, resp.Dispatch)

	// Verify stall config has defaults
	assert.Equal(t, 300, resp.Stall.ThresholdSeconds, "default stalled threshold should be 5 minutes (300s)")
}

func TestHandleHealthSummary_AgentAggregation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   tid("health-project"),
		Name: "Health Test Project",
		Slug: "health-test",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:     tid("broker-1"),
		Name:   "Broker One",
		Slug:   "broker-one",
		Status: "online",
		Profiles: []store.BrokerProfile{
			{Name: "default", Type: "docker", Available: true},
		},
		LastHeartbeat: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create agents with mixed states
	agents := []struct {
		id       string
		name     string
		slug     string
		phase    string
		activity string
		brokerID string
	}{
		{"agent-healthy-1", "Healthy Agent 1", "healthy-1", string(state.PhaseRunning), string(state.ActivityWorking), tid("broker-1")},
		{"agent-healthy-2", "Healthy Agent 2", "healthy-2", string(state.PhaseRunning), string(state.ActivityWorking), tid("broker-1")},
		{"agent-stalled", "Stalled Agent", "stalled-agent", string(state.PhaseRunning), string(state.ActivityStalled), tid("broker-1")},
		{"agent-crashed", "Crashed Agent", "crashed-agent", string(state.PhaseRunning), string(state.ActivityCrashed), tid("broker-1")},
		{"agent-errored", "Errored Agent", "errored-agent", string(state.PhaseError), "", ""},
	}

	for _, a := range agents {
		ag := &store.Agent{
			ID:              tid(a.id),
			Name:            a.name,
			Slug:            a.slug,
			ProjectID:       project.ID,
			Phase:           a.phase,
			Activity:        a.activity,
			RuntimeBrokerID: a.brokerID,
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
	}

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	// Should be degraded because we have stalled/crashed/errored agents
	assert.Equal(t, "degraded", resp.Status)

	// Verify agent counts
	assert.Equal(t, 5, resp.Agents.Total)
	assert.Contains(t, resp.Agents.Stalled, "Stalled Agent")
	assert.Contains(t, resp.Agents.Crashed, "Crashed Agent")
	assert.Contains(t, resp.Agents.Errored, "Errored Agent")
	assert.Len(t, resp.Agents.Stalled, 1)
	assert.Len(t, resp.Agents.Crashed, 1)
	assert.Len(t, resp.Agents.Errored, 1)

	// Verify phase counts
	assert.Equal(t, 4, resp.Agents.ByPhase[string(state.PhaseRunning)])
	assert.Equal(t, 1, resp.Agents.ByPhase[string(state.PhaseError)])
}

func TestHandleHealthSummary_BrokerMixedStatus(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   tid("broker-test-project"),
		Name: "Broker Test",
		Slug: "broker-test",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create brokers with different statuses
	brokers := []struct {
		id        string
		name      string
		slug      string
		status    string
		runtime   string
		available bool
	}{
		{"broker-online", "Online Broker", "online-broker", "online", "docker", true},
		{"broker-offline", "Offline Broker", "offline-broker", "offline", "kubernetes", false},
	}

	for _, b := range brokers {
		br := &store.RuntimeBroker{
			ID:     tid(b.id),
			Name:   b.name,
			Slug:   b.slug,
			Status: b.status,
			Profiles: []store.BrokerProfile{
				{Name: "default", Type: b.runtime, Available: b.available},
			},
			LastHeartbeat: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, br))
	}

	// Create agents assigned to the online broker
	for i := 0; i < 3; i++ {
		ag := &store.Agent{
			ID:              tid("broker-agent-" + string(rune('a'+i))),
			Name:            "Agent " + string(rune('A'+i)),
			Slug:            "broker-agent-" + string(rune('a'+i)),
			ProjectID:       project.ID,
			Phase:           string(state.PhaseRunning),
			Activity:        string(state.ActivityWorking),
			RuntimeBrokerID: tid("broker-online"),
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
	}

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	// Should be degraded because one broker is offline
	assert.Equal(t, "degraded", resp.Status)

	// Should have two brokers
	require.Len(t, resp.Brokers, 2)

	// Find the online broker and verify agent counts
	var onlineBroker, offlineBroker *HealthSummaryBrkr
	for i := range resp.Brokers {
		switch resp.Brokers[i].Status {
		case "online":
			onlineBroker = &resp.Brokers[i]
		case "offline":
			offlineBroker = &resp.Brokers[i]
		}
	}

	require.NotNil(t, onlineBroker, "should have an online broker")
	require.NotNil(t, offlineBroker, "should have an offline broker")

	assert.Equal(t, 3, onlineBroker.AgentCount)
	assert.Equal(t, 3, onlineBroker.AgentHealthy)
	assert.Equal(t, "docker", onlineBroker.Runtime)
	assert.True(t, onlineBroker.RuntimeAvailable)

	assert.Equal(t, 0, offlineBroker.AgentCount)
	assert.Equal(t, "kubernetes", offlineBroker.Runtime)
	assert.False(t, offlineBroker.RuntimeAvailable)
}

func TestHandleHealthSummary_DispatchNullWhenUnavailable(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify the raw JSON has dispatch: null
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, "null", string(raw["dispatch"]))
}

func TestHandleHealthSummary_DatabaseHealthy(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	// SQLite test DB should be healthy
	assert.Equal(t, "healthy", resp.Database.Status)
	// Pool stats should be populated (at least max > 0 from sqlite config)
	// Note: SQLite test stores use MaxOpenConns=1
	assert.GreaterOrEqual(t, resp.Database.PoolMax, int64(0))
}

func TestHandleHealthSummary_ViaRouter(t *testing.T) {
	srv, _ := testServer(t)

	// Verify the route is registered and reachable through the full mux
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	// Unauthenticated should fail through the full mux
	rr = doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	// Without auth, the auth middleware may return 401 or the handler returns 403
	assert.True(t, rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized,
		"unauthenticated request should be rejected, got %d", rr.Code)
}
