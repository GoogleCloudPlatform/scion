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
	rows      []store.HubInstance
	now       time.Time
	err       error
	seenSince time.Time
}

func (f *fakeClockHubInstanceStore) ListHubInstances(_ context.Context, seenSince time.Time) ([]store.HubInstance, time.Time, error) {
	f.seenSince = seenSince
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return f.rows, f.now, nil
}

func getHealthSummary(t *testing.T, srv *Server) (HealthSummaryResponse, map[string]json.RawMessage) {
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
	srv, s := testServer(t)
	fake := &fakeClockHubInstanceStore{
		Store: s,
		now:   hubInstanceT0,
		rows: []store.HubInstance{
			{ID: srv.InstanceID(), Label: "hub-a", Version: "v1", Status: "healthy", StartedAt: hubInstanceT0.Add(-time.Hour), LastSeen: hubInstanceT0.Add(-44 * time.Second)},
			{ID: "hub-b-1", Label: "hub-b", Version: "v1", Status: "healthy", StartedAt: hubInstanceT0.Add(-time.Hour), LastSeen: hubInstanceT0.Add(-46 * time.Second)},
		},
	}
	srv.store = fake

	resp, _ := getHealthSummary(t, srv)
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

	// The list is cut at one hour before the serving replica's clock.
	assert.WithinDuration(t, time.Now().Add(-hubInstanceDisplayWindow), fake.seenSince, time.Minute)
}

// Two instances on one store: the summary lists both from the database,
// with each one's version and started_at, and marks the serving one.
func TestHandleHealthSummary_HubInstancesTwoInstances(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: srv.InstanceID(), Label: "hub-a", Version: "v1.0.0", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
	}))
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-b-0123", Label: "hub-b", Version: "v1.1.0", Status: "degraded",
		Checks: map[string]string{"database": "healthy", "colocated_broker": "unhealthy"},
	}))
	stored, _, err := s.ListHubInstances(ctx, time.Time{})
	require.NoError(t, err)
	byID := map[string]store.HubInstance{}
	for _, r := range stored {
		byID[r.ID] = r
	}

	resp, raw := getHealthSummary(t, srv)
	require.Contains(t, raw, "hub_instances")
	require.NotNil(t, resp.HubInstances)
	got := resp.HubInstances
	assert.Equal(t, 2, got.Total)
	assert.Equal(t, 2, got.Live)
	assert.False(t, got.Truncated)
	require.Len(t, got.Items, 2)

	a, b := got.Items[0], got.Items[1]
	assert.Equal(t, srv.InstanceID(), a.ID, "the serving instance is listed first")
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

	// The registry list does not change the overall status in this slice.
	assert.NotContains(t, fmt.Sprint(resp.Attention), "hub-b")
}

// A registry read failure reports hub_instances as null (not reported) and
// keeps the store error out of the response.
func TestHandleHealthSummary_HubInstancesNullWhenReadFails(t *testing.T) {
	srv, s := testServer(t)
	srv.store = &fakeClockHubInstanceStore{Store: s, err: errors.New("registry read failed")}

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, "null", string(raw["hub_instances"]))
	assert.NotContains(t, rr.Body.String(), "registry read failed")
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
	// Live first (serving first, then by label), then stale, then stopped.
	assert.Equal(t, []string{"id-serving", "id-live-b", "id-live-c", "id-stale", "id-stopped"}, ids)
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
