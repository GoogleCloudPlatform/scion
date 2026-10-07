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

	// Authorization subtests (Unauthenticated returns 403, Non-admin returns 403)
	// were removed: authorization is now enforced by the routeGuard via
	// hub.health.read permission (PR-A4). The handler no longer performs inline
	// admin checks. Authorization is tested in TestRouteGuardOpsPermissions.

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
	assert.NotNil(t, resp.Brokers.Items)

	// Verify agents section has initialized slices
	assert.NotNil(t, resp.Agents.ByPhase)
	assert.NotNil(t, resp.Agents.Problems)

	// Verify dispatch is nil (no dispatch metrics available yet)
	assert.Nil(t, resp.Dispatch)

	// Stall settings are configuration, not health: they are edited on the
	// Server Config page and are not part of the health summary.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "stall_config", "health summary must not carry stall settings")
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

	// Degraded because of the crashed and errored agents (not the stalled one).
	assert.Equal(t, "degraded", resp.Status)

	assert.Equal(t, 5, resp.Agents.Total)
	assert.Equal(t, 4, resp.Agents.Active)
	assert.Equal(t, 2, resp.Agents.Errored)
	assert.Equal(t, 5, resp.Agents.Considered)
	assert.Equal(t, []HealthPhaseCount{
		{Phase: string(state.PhaseRunning), Count: 4},
		{Phase: string(state.PhaseError), Count: 1},
	}, resp.Agents.ByPhase)

	require.Len(t, resp.Agents.Problems, 2, "offline group is empty and omitted")
	assert.Equal(t, HealthAgentGroup{Kind: HealthAgentGroupErrored, Count: 1, Items: []HealthAgentRef{{
		ID: tid("agent-errored"), Name: "Errored Agent", ProjectID: project.ID, ProjectSlug: "health-test",
	}}}, resp.Agents.Problems[0])
	assert.Equal(t, HealthAgentGroup{Kind: HealthAgentGroupCrashed, Count: 1, Items: []HealthAgentRef{{
		ID: tid("agent-crashed"), Name: "Crashed Agent", ProjectID: project.ID, ProjectSlug: "health-test",
		BrokerID: tid("broker-1"),
	}}}, resp.Agents.Problems[1])

	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, HealthBrokerAgents{Running: 4, Attention: 1}, resp.Brokers.Items[0].Agents)

	assertNoStallData(t, rr.Body.Bytes())
}

// assertNoStallData fails if the summary body mentions stalled or suspended
// agents anywhere: stalls are not a health signal.
func assertNoStallData(t *testing.T, body []byte) {
	t.Helper()
	text := string(body)
	assert.NotContains(t, text, "stall")
	assert.NotContains(t, text, "Stall")
	var raw struct {
		Agents map[string]json.RawMessage `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	for _, k := range []string{"stalled", "suspended", "crashed"} {
		assert.NotContains(t, raw.Agents, k, "agents.%s must not be returned", k)
	}
	var problems struct {
		Agents struct {
			Problems []HealthAgentGroup `json:"problems"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &problems))
	for _, g := range problems.Agents.Problems {
		assert.Contains(t, []string{HealthAgentGroupErrored, HealthAgentGroupCrashed, HealthAgentGroupOffline}, g.Kind)
	}
}

// TestHandleHealthSummary_SameNamedAgentsDistinguishable: two agents with the
// same name in different projects come back with distinct IDs and project
// slugs, so the dashboard can label and link each one correctly.
func TestHandleHealthSummary_SameNamedAgentsDistinguishable(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	for _, p := range []string{"alpha", "beta"} {
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("proj-" + p), Name: p, Slug: p}))
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid("dup-" + p), Name: "worker", Slug: "worker", ProjectID: tid("proj-" + p),
			Phase: string(state.PhaseError),
		}))
	}

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	require.Len(t, resp.Agents.Problems, 1)
	g := resp.Agents.Problems[0]
	assert.Equal(t, HealthAgentGroupErrored, g.Kind)
	assert.Equal(t, 2, g.Count)
	got := map[string]string{}
	for _, it := range g.Items {
		assert.Equal(t, "worker", it.Name)
		got[it.ID] = it.ProjectSlug
	}
	assert.Equal(t, map[string]string{tid("dup-alpha"): "alpha", tid("dup-beta"): "beta"}, got)
}

// projectListCountingStore counts project list calls so a test can confirm
// slugs are resolved with one batched lookup.
type projectListCountingStore struct {
	store.Store
	summaryCalls, listCalls, getCalls int
	failSummaries                     bool
}

func (p *projectListCountingStore) ListProjectSummaries(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	p.summaryCalls++
	if p.failSummaries {
		return nil, errors.New("boom")
	}
	return p.Store.ListProjectSummaries(ctx, f, o)
}

func (p *projectListCountingStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	p.listCalls++
	return p.Store.ListProjects(ctx, f, o)
}

func (p *projectListCountingStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	p.getCalls++
	return p.Store.GetProject(ctx, id)
}

// TestHandleHealthSummary_ProjectSlugsOneLookup: refs across all groups and
// several projects resolve with a single batched project lookup, counts stay
// true above the reference cap, and a failed lookup keeps the section.
func TestHandleHealthSummary_ProjectSlugsOneLookup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	n := 0
	add := func(project, phase, activity string) {
		n++
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid(fmt.Sprintf("slug-agent-%d", n)), Name: fmt.Sprintf("a%d", n), Slug: fmt.Sprintf("a%d", n),
			ProjectID: tid(project), Phase: phase, Activity: activity,
		}))
	}
	for _, p := range []string{"p1", "p2", "p3"} {
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid(p), Name: p, Slug: "slug-" + p}))
	}
	for i := 0; i < store.AgentHealthRefCap+3; i++ {
		add("p1", string(state.PhaseError), "")
	}
	add("p2", string(state.PhaseRunning), string(state.ActivityCrashed))
	add("p3", string(state.PhaseRunning), string(state.ActivityOffline))

	counting := &projectListCountingStore{Store: srv.store}
	srv.store = counting

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	assert.Equal(t, 1, counting.summaryCalls, "one batched project lookup")
	assert.Zero(t, counting.getCalls, "no per-agent project lookup")

	require.Len(t, resp.Agents.Problems, 3)
	kinds := []string{}
	for _, g := range resp.Agents.Problems {
		kinds = append(kinds, g.Kind)
		for _, it := range g.Items {
			assert.NotEmpty(t, it.ProjectSlug, it.ID)
		}
	}
	assert.Equal(t, []string{HealthAgentGroupErrored, HealthAgentGroupCrashed, HealthAgentGroupOffline}, kinds)
	assert.Equal(t, store.AgentHealthRefCap+3, resp.Agents.Problems[0].Count)
	assert.Len(t, resp.Agents.Problems[0].Items, store.AgentHealthRefCap)
	assert.Equal(t, "slug-p2", resp.Agents.Problems[1].Items[0].ProjectSlug)
	assert.Equal(t, "slug-p3", resp.Agents.Problems[2].Items[0].ProjectSlug)

	// A failed slug lookup keeps the section, with empty slugs.
	counting.failSummaries = true
	rr = doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Agents.Problems, 3)
	assert.Empty(t, resp.Agents.Problems[1].Items[0].ProjectSlug)
	assert.Equal(t, tid("p2"), resp.Agents.Problems[1].Items[0].ProjectID)
}

// TestHandleHealthSummary_StalledOnlyStaysHealthy: a stalled or suspended
// agent is not a health signal and appears nowhere in the response.
func TestHandleHealthSummary_StalledOnlyStaysHealthy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("stall-proj"), Name: "stall", Slug: "stall"}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("stalled-1"), Name: "stalled-one", Slug: "stalled-one", ProjectID: tid("stall-proj"),
		Phase: string(state.PhaseRunning), Activity: string(state.ActivityStalled),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("suspended-1"), Name: "suspended-one", Slug: "suspended-one", ProjectID: tid("stall-proj"),
		Phase: string(state.PhaseSuspended), Activity: string(state.ActivityStalled),
	}))

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "healthy", resp.Status)
	assert.Empty(t, resp.Agents.Problems)
	assert.Equal(t, 0, resp.Agents.Errored)
	assert.Equal(t, 2, resp.Agents.Considered)
	assert.NotContains(t, rr.Body.String(), "stalled-one")
	assert.NotContains(t, rr.Body.String(), "suspended-one")
	assertNoStallData(t, rr.Body.Bytes())
}

func TestOrderedPhaseCounts(t *testing.T) {
	in := map[string]int{"error": 1, "running": 3, "zzz-legacy": 2, "created": 1, "stopped": 0, "aaa-legacy": 1}
	want := []HealthPhaseCount{
		{Phase: "created", Count: 1},
		{Phase: "running", Count: 3},
		{Phase: "error", Count: 1},
		{Phase: "aaa-legacy", Count: 1},
		{Phase: "zzz-legacy", Count: 2},
	}
	for i := 0; i < 20; i++ { // map iteration order must not leak into the result
		assert.Equal(t, want, orderedPhaseCounts(in))
	}
	assert.Equal(t, []HealthPhaseCount{}, orderedPhaseCounts(nil))
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
	require.Len(t, resp.Brokers.Items, 2)

	// Find the online broker and verify agent counts
	var onlineBroker, offlineBroker *HealthSummaryBroker
	for i := range resp.Brokers.Items {
		switch resp.Brokers.Items[i].Status {
		case "online":
			onlineBroker = &resp.Brokers.Items[i]
		case "offline":
			offlineBroker = &resp.Brokers.Items[i]
		}
	}

	require.NotNil(t, onlineBroker, "should have an online broker")
	require.NotNil(t, offlineBroker, "should have an offline broker")

	assert.Equal(t, HealthBrokerAgents{Running: 3}, onlineBroker.Agents)
	require.NotNil(t, onlineBroker.Runtime)
	assert.Equal(t, "docker", onlineBroker.Runtime.Type)

	assert.Equal(t, HealthBrokerAgents{}, offlineBroker.Agents)
	require.NotNil(t, offlineBroker.Runtime)
	assert.Equal(t, "kubernetes", offlineBroker.Runtime.Type)
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

// TestHandleHealthSummary_SurfacesNonHealthyChecks: a degraded hub must carry
// its cause in the summary, not only the database check (ptone/scion#1094).
func TestHandleHealthSummary_SurfacesNonHealthyChecks(t *testing.T) {
	srv, _ := testServer(t)
	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New("boom"))

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "degraded", resp.Status)
	assert.Equal(t, "degraded", resp.Hub.Status)
	assert.Equal(t, "healthy", resp.Database.Status, "database itself is fine; the cause is elsewhere")
	assert.Equal(t, "unhealthy: registration failed", resp.Hub.Checks["colocated_broker"])
	assert.Equal(t, []string{"colocated_broker: unhealthy: registration failed"}, resp.Hub.UnhealthyChecks)
}

// TestHandleHealthSummary_HealthyHasNoUnhealthyChecks: the cause list is
// omitted when everything is healthy.
func TestHandleHealthSummary_HealthyHasNoUnhealthyChecks(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "healthy", resp.Status)
	assert.Empty(t, resp.Hub.UnhealthyChecks)
	assert.Equal(t, "healthy", resp.Hub.Checks["database"])
}

// TestHandleHealthSummary_UnhealthyNotDowngraded: degrading signals (stalled
// agents, offline brokers, aggregation errors) only raise severity, so an
// unhealthy hub stays unhealthy in the summary.
func TestHandleHealthSummary_UnhealthyNotDowngraded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     tid("summary-offline-broker"),
		Name:   "offline-broker",
		Slug:   "offline-broker",
		Status: store.BrokerStatusOffline,
	}))
	srv.store = pingFailStore{srv.store}

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "unhealthy", resp.Status)
	assert.Equal(t, "unhealthy", resp.Database.Status)
	assert.Contains(t, resp.Hub.UnhealthyChecks, "database: unhealthy")
}

// Chat plugin broker records are always marked online; the connected broker
// count in /healthz and the health summary must only count runtime brokers.
func TestHealthStats_ConnectedBrokersExcludesPlugins(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:            tid("count-runtime-broker"),
		Name:          "Runtime Broker",
		Slug:          "runtime-broker",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
	}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:            tid("count-plugin-broker"),
		Name:          "plugin-broker-telegram",
		Slug:          "plugin-broker-telegram",
		Status:        store.BrokerStatusOnline,
		Labels:        map[string]string{"scion.io/plugin": "telegram"},
		LastHeartbeat: time.Now(),
	}))

	rr := doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var health HealthResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &health))
	require.NotNil(t, health.Stats)
	assert.Equal(t, 1, health.Stats.ConnectedBrokers, "/healthz stats")

	rr = doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var summary HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &summary))
	assert.Equal(t, 1, summary.Hub.ConnectedBrokers, "health summary")
}

// pageCountingStore counts ListRuntimeBrokers calls so a test can confirm
// that a listing spanned more than one page.
type pageCountingStore struct {
	store.Store
	calls int
}

func (p *pageCountingStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	p.calls++
	return p.Store.ListRuntimeBrokers(ctx, filter, opts)
}

// The connected broker count must follow the cursor across pages and still
// skip plugin records.
func TestHealthStats_ConnectedBrokersMultiPage(t *testing.T) {
	orig := connectedBrokerPageSize
	connectedBrokerPageSize = 1
	t.Cleanup(func() { connectedBrokerPageSize = orig })

	srv, s := testServer(t)
	ctx := context.Background()

	for _, name := range []string{"page-broker-a", "page-broker-b"} {
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:            tid(name),
			Name:          name,
			Slug:          name,
			Status:        store.BrokerStatusOnline,
			LastHeartbeat: time.Now(),
		}))
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:            tid("page-plugin-broker"),
		Name:          "plugin-broker-discord",
		Slug:          "plugin-broker-discord",
		Status:        store.BrokerStatusOnline,
		Labels:        map[string]string{"scion.io/plugin": "discord"},
		LastHeartbeat: time.Now(),
	}))

	counting := &pageCountingStore{Store: srv.store}
	srv.store = counting

	count, err := srv.countOnlineRuntimeBrokers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Greater(t, counting.calls, 1, "count should span more than one page")
}
