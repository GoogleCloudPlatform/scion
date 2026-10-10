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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var hubInstanceT0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// fakeClockHubInstanceStore wraps a real store and serves ListHubInstances
// from fixed rows and a fixed store clock.
type fakeClockHubInstanceStore struct {
	store.Store
	rows   []store.HubInstance
	now    time.Time
	err    error
	window time.Duration
}

func (f *fakeClockHubInstanceStore) ListHubInstances(_ context.Context, window time.Duration) ([]store.HubInstance, time.Time, error) {
	f.window = window
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return f.rows, f.now, nil
}

func getHubInstancesSummary(t *testing.T, srv *Server) (HealthSummaryResponse, map[string]json.RawMessage) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	return resp, raw
}

// The state rule against a fake store clock: no write for 44 s is live, 46 s
// is stale (the threshold is 45 s, three ticks). A stopped row is stopped.
func TestHubInstanceState_FakeClockBoundary(t *testing.T) {
	now := hubInstanceT0
	row := func(age time.Duration) store.HubInstance {
		return store.HubInstance{ID: "hub-x", LastSeen: now.Add(-age)}
	}
	assert.Equal(t, 45*time.Second, hubInstanceStaleAfter)
	assert.Equal(t, HubInstanceStateLive, hubInstanceState(row(0), now))
	assert.Equal(t, HubInstanceStateLive, hubInstanceState(row(44*time.Second), now))
	assert.Equal(t, HubInstanceStateLive, hubInstanceState(row(45*time.Second), now))
	assert.Equal(t, HubInstanceStateStale, hubInstanceState(row(46*time.Second), now))

	stopped := row(time.Second)
	at := now.Add(-time.Second)
	stopped.StoppedAt = &at
	assert.Equal(t, HubInstanceStateStopped, hubInstanceState(stopped, now))
}

// The same rule through the summary handler: the state is computed against
// the store clock returned with the rows, not the serving replica's clock.
func TestHandleHealthSummary_HubInstancesFakeClockStale(t *testing.T) {
	srv, _, fake, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, now: hubInstanceT0}
	})
	fake.rows = []store.HubInstance{
		{ID: srv.InstanceID(), Label: "hub-a", Version: "v1", Status: "healthy", StartedAt: hubInstanceT0.Add(-time.Hour), LastSeen: hubInstanceT0.Add(-44 * time.Second)},
		{ID: "hub-b-1", Label: "hub-b", Version: "v1", Status: "healthy", StartedAt: hubInstanceT0.Add(-time.Hour), LastSeen: hubInstanceT0.Add(-46 * time.Second)},
	}

	resp, _ := getHubInstancesSummary(t, srv)
	require.NotNil(t, resp.HubInstances)
	require.Len(t, resp.HubInstances.Items, 2)
	assert.Equal(t, srv.InstanceID(), resp.HubInstances.Items[0].ID)
	assert.Equal(t, HubInstanceStateLive, resp.HubInstances.Items[0].State)
	assert.True(t, resp.HubInstances.Items[0].Serving)
	assert.Equal(t, "hub-b-1", resp.HubInstances.Items[1].ID)
	assert.Equal(t, HubInstanceStateStale, resp.HubInstances.Items[1].State)
	assert.False(t, resp.HubInstances.Items[1].Serving)
	assert.Equal(t, 1, resp.HubInstances.Live)
	assert.Equal(t, 2, resp.HubInstances.Total)

	// The store makes the one-hour cut at its own clock; the section
	// reports that clock as as_of.
	assert.Equal(t, hubInstanceDisplayWindow, fake.window)
	assert.True(t, resp.HubInstances.AsOf.Equal(hubInstanceT0))
}

// A serving hub whose clock is far from the database's still shows correct
// states: the state rule, the window and as_of all use the store clock, and
// the dashboard computes ages from as_of. Here the store clock is two hours
// behind the hub's, which on the hub's clock would put every row outside
// the one-hour window.
func TestHandleHealthSummary_HubInstancesSkewedStoreClock(t *testing.T) {
	storeNow := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	srv, _, fake, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, now: storeNow}
	})
	fake.rows = []store.HubInstance{
		{ID: srv.InstanceID(), Label: "hub-a", Version: "v1", Status: "healthy", StartedAt: storeNow.Add(-30 * time.Minute), LastSeen: storeNow.Add(-10 * time.Second)},
		{ID: "hub-b-1", Label: "hub-b", Version: "v1", Status: "healthy", StartedAt: storeNow.Add(-30 * time.Minute), LastSeen: storeNow.Add(-50 * time.Second)},
	}

	resp, raw := getHubInstancesSummary(t, srv)
	require.NotNil(t, resp.HubInstances)
	got := resp.HubInstances
	require.Len(t, got.Items, 2)
	assert.Equal(t, HubInstanceStateLive, got.Items[0].State, "10 s old on the store clock is live")
	assert.Equal(t, HubInstanceStateStale, got.Items[1].State, "50 s old on the store clock is stale")
	assert.True(t, got.AsOf.Equal(storeNow), "as_of is the store clock: got %v want %v", got.AsOf, storeNow)
	assert.Equal(t, hubInstanceDisplayWindow, fake.window, "the cut is a window applied by the store")
	assert.False(t, got.AsOf.Equal(resp.GeneratedAt), "as_of differs from generated_at when the clocks differ")

	var section map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["hub_instances"], &section))
	assert.Contains(t, section, "as_of")
}

// Two instances on one store: the summary lists both from the database,
// with each one's version and started_at, and marks the serving one.
func TestHandleHealthSummary_HubInstancesTwoInstances(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: srv.InstanceID(), Label: "hub-a", Version: "v1.0.0", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
		Stats:  json.RawMessage(`{"db":{"in_use":3,"idle":2,"max_open":25,"wait_count":4}}`),
	}))
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-b-0123", Label: "hub-b", Version: "v1.1.0", Status: "degraded",
		Checks: map[string]string{"database": "healthy", "colocated_broker": "unhealthy"},
		Stats:  json.RawMessage(`{"db":{"in_use":9,"idle":0,"max_open":10,"wait_count":17}}`),
	}))
	stored, _, err := s.ListHubInstances(ctx, time.Hour)
	require.NoError(t, err)
	byID := map[string]store.HubInstance{}
	for _, r := range stored {
		byID[r.ID] = r
	}

	resp, raw := getHubInstancesSummary(t, srv)
	require.Contains(t, raw, "hub_instances")
	require.NotNil(t, resp.HubInstances)
	got := resp.HubInstances
	assert.Equal(t, 2, got.Total)
	assert.Equal(t, 2, got.Live)
	assert.False(t, got.Truncated)
	require.Len(t, got.Items, 2)

	a, b := got.Items[0], got.Items[1]
	assert.Equal(t, srv.InstanceID(), a.ID, "hub-a sorts before hub-b")
	assert.True(t, a.Serving)
	assert.Equal(t, "hub-a", a.Label)
	assert.Equal(t, "v1.0.0", a.Version)
	assert.Equal(t, HubInstanceStateLive, a.State)
	assert.True(t, a.StartedAt.Equal(byID[a.ID].StartedAt))
	assert.Nil(t, a.StoppedAt)

	assert.Equal(t, "hub-b-0123", b.ID)
	assert.False(t, b.Serving)
	assert.Equal(t, "v1.1.0", b.Version)
	assert.Equal(t, HubInstanceStateLive, b.State)
	assert.Equal(t, "degraded", b.Status)
	assert.Equal(t, map[string]string{"database": "healthy", "colocated_broker": "unhealthy"}, b.Checks)
	assert.True(t, b.StartedAt.Equal(byID[b.ID].StartedAt))
	assert.True(t, b.LastSeen.Equal(byID[b.ID].LastSeen))

	// Each instance carries its own pool, not the serving instance's.
	assert.Equal(t, &HealthHubInstanceDB{PoolActive: 3, PoolIdle: 2, PoolMax: 25, PoolWaitCountTotal: 4}, a.Database)
	assert.Equal(t, &HealthHubInstanceDB{PoolActive: 9, PoolIdle: 0, PoolMax: 10, PoolWaitCountTotal: 17}, b.Database)
	// The pool is per instance only: there is no top-level database block.
	assert.NotContains(t, raw, "database")

	// The fleet hub section comes from the same rows: one of two live
	// instances is degraded, so the fleet is degraded, and its failing
	// check is tagged with its instance.
	assert.Equal(t, HealthStatusDegraded, resp.Hub.Status)
	assert.Equal(t, HubVersionMixed, resp.Hub.Version)
	require.NotNil(t, resp.Hub.Instances)
	assert.Equal(t, HealthSummaryHubFleet{Live: 2, Healthy: 1, Degraded: 1}, *resp.Hub.Instances)
	assert.Equal(t, []HealthSummaryHubCheck{
		{InstanceID: "hub-b-0123", InstanceLabel: "hub-b", Name: "colocated_broker", Value: "unhealthy"},
	}, resp.Hub.UnhealthyChecks)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
		Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: "hub-b-0123", Name: "hub-b"},
		Message: "Hub check colocated_broker is not healthy on instance hub-b",
	})
	assert.Contains(t, fmt.Sprint(resp.Attention), "1 of 2 hub instances healthy")
}

// A registry read failure reports hub_instances as null (not reported) and
// keeps the store error out of the response.
func TestHandleHealthSummary_HubInstancesNullWhenReadFails(t *testing.T) {
	srv, _, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, err: errors.New("registry read failed")}
	})

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, "null", string(raw["hub_instances"]))
	assert.NotContains(t, rr.Body.String(), "registry read failed")
}

// A row with no pool block, or with stats that do not decode, is listed
// with a null database.
func TestBuildHealthSummaryHubInstances_DatabaseNullWithoutPool(t *testing.T) {
	now := hubInstanceT0
	got := buildHealthSummaryHubInstances([]store.HubInstance{
		{ID: "hub-a", LastSeen: now},
		{ID: "hub-b", LastSeen: now, Stats: json.RawMessage(`{"integrations_truncated":true}`)},
		{ID: "hub-c", LastSeen: now, Stats: json.RawMessage(`not json`)},
		{ID: "hub-d", LastSeen: now, Stats: json.RawMessage(`{"db":{"in_use":1,"idle":1,"max_open":0,"wait_count":0}}`)},
	}, now, "hub-a")
	require.Len(t, got.Items, 4)
	byID := map[string]HealthHubInstance{}
	for _, it := range got.Items {
		byID[it.ID] = it
	}
	assert.Nil(t, byID["hub-a"].Database)
	assert.Nil(t, byID["hub-b"].Database)
	assert.Nil(t, byID["hub-c"].Database)
	assert.Equal(t, &HealthHubInstanceDB{PoolActive: 1, PoolIdle: 1}, byID["hub-d"].Database)

	b, err := json.Marshal(byID["hub-a"])
	require.NoError(t, err)
	assert.Contains(t, string(b), `"database":null`)
	assert.Contains(t, string(b), `"integration_counts":{"total":0,`)
	assert.NotContains(t, string(b), `"integrations"`)
	assert.True(t, byID["hub-b"].IntegrationsTruncated)
}

// Each instance's integrations come from its own row: counts by health,
// the list sorted and normalised again on read.
func TestBuildHealthSummaryHubInstances_Integrations(t *testing.T) {
	now := hubInstanceT0
	got := buildHealthSummaryHubInstances([]store.HubInstance{
		{ID: "hub-a", LastSeen: now, Stats: json.RawMessage(`{"integrations":[` +
			`{"name":"slack","health":"unhealthy","connected":false,"version":"2"},` +
			`{"name":"chat","health":"healthy","connected":true,"version":"1"},` +
			`{"name":"BAD NAME","health":"healthy"},` +
			`{"name":"mail","health":"weird"}]}`)},
	}, now, "hub-a")
	require.Len(t, got.Items, 1)
	it := got.Items[0]
	assert.Equal(t, HealthSummaryIntegrationCounts{Total: 3, Healthy: 1, Unhealthy: 1, Unknown: 1}, it.IntegrationCounts)
	assert.Equal(t, []HealthHubInstanceIntegration{
		{Name: "chat", Health: "healthy", Connected: true, Version: "1"},
		{Name: "mail", Health: "unknown"},
		{Name: "slack", Health: "unhealthy", Version: "2"},
	}, it.Integrations)
}

// poolCountingStore wraps a real store and counts DB() calls: the only way
// to reach sql.DB.Stats() from the hub is through the store's *sql.DB.
type poolCountingStore struct {
	store.Store
	db    *sql.DB
	calls atomic.Int32
}

func (p *poolCountingStore) DB() *sql.DB {
	p.calls.Add(1)
	return p.db
}

// The summary handler reads no pool counters itself (no sql.DB.Stats()
// call): every pool figure comes from the registry rows. The registry tick,
// by contrast, does read this process's pool.
func TestHandleHealthSummary_NoPoolStatsRead(t *testing.T) {
	srv, s, counting, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *poolCountingStore {
		return &poolCountingStore{Store: inner}
	})
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store exposes its *sql.DB")
	counting.db = dbp.DB()

	srv.newHubInstanceRegistry().tick(context.Background())
	require.Positive(t, counting.calls.Load(), "the registry tick reads this process's pool")
	counting.calls.Store(0)

	rr := httptest.NewRecorder()
	srv.handleHealthSummary(rr, httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Zero(t, counting.calls.Load(), "the summary handler must not read the pool (no sql.DB.Stats call)")

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.HubInstances)
	require.Len(t, resp.HubInstances.Items, 1)
	require.NotNil(t, resp.HubInstances.Items[0].Database, "the serving instance's pool comes from its row")
	assert.Equal(t, dbp.DB().Stats().MaxOpenConnections, resp.HubInstances.Items[0].Database.PoolMax)
}

func TestBuildHealthSummaryHubInstances_OrderAndCap(t *testing.T) {
	now := hubInstanceT0
	stoppedAt := now.Add(-10 * time.Minute)
	rows := []store.HubInstance{
		{ID: "id-stopped", Label: "a-stopped", LastSeen: stoppedAt, StoppedAt: &stoppedAt},
		{ID: "id-stale", Label: "a-stale", LastSeen: now.Add(-time.Minute)},
		{ID: "id-live-c", Label: "c", LastSeen: now},
		{ID: "id-live-b", Label: "b", LastSeen: now},
		{ID: "id-serving", Label: "z", LastSeen: now},
	}
	got := buildHealthSummaryHubInstances(rows, now, "id-serving")
	var ids []string
	for _, it := range got.Items {
		ids = append(ids, it.ID)
		assert.NotNil(t, it.Checks, "checks is never null")
	}
	// Live first, then stale, then stopped, each by label. The serving
	// instance is marked, not moved, so every replica lists the same
	// order.
	assert.Equal(t, []string{"id-live-b", "id-live-c", "id-serving", "id-stale", "id-stopped"}, ids)
	assert.Equal(t, 3, got.Live)
	assert.Equal(t, 5, got.Total)
	assert.False(t, got.Truncated)

	old := healthSummaryHubInstanceLimit
	healthSummaryHubInstanceLimit = 2
	t.Cleanup(func() { healthSummaryHubInstanceLimit = old })
	got = buildHealthSummaryHubInstances(rows, now, "id-serving")
	assert.Len(t, got.Items, 2)
	assert.True(t, got.Truncated)
	assert.Equal(t, 5, got.Total)
	assert.Equal(t, 3, got.Live, "live counts every row, including those cut from items")
}

func TestHealthSummaryHubInstanceLimitIsFifty(t *testing.T) {
	assert.Equal(t, 50, healthSummaryHubInstanceLimit)
	assert.Equal(t, time.Hour, hubInstanceDisplayWindow)
}
