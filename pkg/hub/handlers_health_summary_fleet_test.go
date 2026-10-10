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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// probeCountingStore wraps a real store and counts the calls a live probe
// of this process would make: store Ping, and DB() (the only way to reach
// sql.DB.Stats() from the hub).
type probeCountingStore struct {
	store.Store
	db    *sql.DB
	pings atomic.Int32
	dbs   atomic.Int32
}

func (p *probeCountingStore) Ping(ctx context.Context) error {
	p.pings.Add(1)
	return p.Store.Ping(ctx)
}

func (p *probeCountingStore) DB() *sql.DB {
	p.dbs.Add(1)
	return p.db
}

// The summary handler runs no probe of its own: no store Ping, no plugin
// call and no sql.DB.Stats() read. The hub section is still filled, from
// the registry row the tick wrote.
func TestHandleHealthSummary_NoPingPluginOrPoolCall(t *testing.T) {
	srv, s, counting, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *probeCountingStore {
		return &probeCountingStore{Store: inner}
	})
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store exposes its *sql.DB")
	counting.db = dbp.DB()
	mgr := &pluginCallCountingManager{healthSummaryPluginDouble: newHealthSummaryPluginDouble("telegram")}
	srv.SetPluginManager(mgr)

	tickHubInstance(t, srv)
	require.Positive(t, counting.pings.Load(), "the registry tick pings the store")
	require.Positive(t, counting.dbs.Load(), "the registry tick reads the pool")
	require.Positive(t, mgr.calls.Load(), "the registry tick queries the plugins")
	counting.pings.Store(0)
	counting.dbs.Store(0)
	mgr.calls.Store(0)

	rr := httptest.NewRecorder()
	srv.handleHealthSummary(rr, httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Zero(t, counting.pings.Load(), "the summary handler must not ping the store")
	assert.Zero(t, counting.dbs.Load(), "the summary handler must not read the pool (no sql.DB.Stats call)")
	assert.Zero(t, mgr.calls.Load(), "the summary handler must make no plugin call")

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, HealthStatusHealthy, resp.Hub.Status)
	require.NotNil(t, resp.Hub.Instances)
	assert.Equal(t, 1, resp.Hub.Instances.Live)
}

// pinnedClockStore wraps a store and returns its registry rows with a
// fixed store clock, so two requests read the rows at the same instant.
type pinnedClockStore struct {
	store.Store
	now time.Time
}

func (p *pinnedClockStore) ListHubInstances(ctx context.Context, window time.Duration) ([]store.HubInstance, time.Time, error) {
	rows, _, err := p.Store.ListHubInstances(ctx, window)
	return rows, p.now, err
}

// normalizeServingFields removes the fields that name the serving replica
// (generated_at, hub.instance_id and each hub instance's serving flag)
// from a decoded summary.
func normalizeServingFields(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	delete(m, "generated_at")
	hub, ok := m["hub"].(map[string]any)
	require.True(t, ok)
	delete(hub, "instance_id")
	section, ok := m["hub_instances"].(map[string]any)
	require.True(t, ok)
	items, ok := section["items"].([]any)
	require.True(t, ok)
	for _, it := range items {
		item, ok := it.(map[string]any)
		require.True(t, ok)
		delete(item, "serving")
	}
	return m
}

// Two replicas on one store return the same summary apart from the serving
// marker and generated_at: the hub section, the hub instance list, its
// order and the attention items come from the database only. Replica B is
// degraded (a failed co-located broker check), so the fleet section and the
// attention list are not empty.
func TestHandleHealthSummary_TwoReplicasSameResponse(t *testing.T) {
	a, s := testServer(t)
	b := newHubReplica(t, s)
	require.NotEqual(t, a.InstanceID(), b.InstanceID())
	b.ExpectEmbeddedBroker()
	b.EmbeddedBrokerRegistrationFailed(errors.New("registration failed"))

	tickHubInstance(t, a)
	tickHubInstance(t, b)
	// One store-clock instant for both reads; as_of is that instant.
	pinned := time.Now().UTC()
	a.store = &pinnedClockStore{Store: a.store, now: pinned}
	b.store = &pinnedClockStore{Store: b.store, now: pinned}

	bodies := map[string][]byte{}
	for _, serving := range []*Server{a, b} {
		rr := doRequest(t, serving, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.Equal(t, serving.InstanceID(), resp.Hub.InstanceID)
		require.NotNil(t, resp.HubInstances)
		require.Len(t, resp.HubInstances.Items, 2)
		for _, it := range resp.HubInstances.Items {
			assert.Equal(t, it.ID == serving.InstanceID(), it.Serving, "only the serving instance is marked")
		}
		assert.Equal(t, HealthStatusDegraded, resp.Status, "1 of 2 live instances degraded")
		assert.Equal(t, HealthStatusDegraded, resp.Hub.Status)
		require.Len(t, resp.Hub.UnhealthyChecks, 1)
		assert.Equal(t, b.InstanceID(), resp.Hub.UnhealthyChecks[0].InstanceID)
		bodies[serving.InstanceID()] = rr.Body.Bytes()
	}

	assert.Equal(t,
		normalizeServingFields(t, bodies[a.InstanceID()]),
		normalizeServingFields(t, bodies[b.InstanceID()]),
		"the responses differ only in the serving marker and generated_at")
}

// errDBUnreachable is returned by dbUnreachableStore for every read the
// summary makes.
var errDBUnreachable = errors.New("dial tcp 10.0.0.9:5432: connection refused")

// dbUnreachableStore fails every store read of the health summary, as a
// replica that cannot reach the database at all would.
type dbUnreachableStore struct{ store.Store }

func (dbUnreachableStore) AggregateAgentHealth(context.Context) (*store.AgentHealthAggregate, error) {
	return nil, errDBUnreachable
}

func (dbUnreachableStore) ListRuntimeBrokers(context.Context, store.RuntimeBrokerFilter, store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	return nil, errDBUnreachable
}

func (dbUnreachableStore) CountStuckPendingMessages(context.Context, time.Time) (int, error) {
	return 0, errDBUnreachable
}

func (dbUnreachableStore) CountBrokerDispatchHealth(context.Context, time.Time, time.Time) (int, int, error) {
	return 0, 0, errDBUnreachable
}

func (dbUnreachableStore) ListHubInstances(context.Context, time.Duration) ([]store.HubInstance, time.Time, error) {
	return nil, time.Time{}, errDBUnreachable
}

// When the serving replica cannot read the database at all, the summary is
// a 503 with a fixed body: no partial data and no store error text.
func TestHandleHealthSummary_DatabaseUnreadableIs503(t *testing.T) {
	srv, _ := testServer(t)
	tickHubInstance(t, srv)
	srv.store = dbUnreachableStore{srv.store}

	rr := httptest.NewRecorder()
	srv.handleHealthSummary(rr, httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), healthSummaryUnavailableMessage)
	assert.Contains(t, rr.Body.String(), ErrCodeUnavailable)
	for _, frag := range []string{"dial tcp", "10.0.0.9", "connection refused", `"hub"`, `"attention"`, `"status"`} {
		assert.NotContains(t, rr.Body.String(), frag)
	}
}

// When only the registry cannot be read, the summary is still served: the
// hub status is unknown, a warning explains it, and the overall status
// does not change.
func TestHandleHealthSummary_RegistryReadFailureIsWarningOnly(t *testing.T) {
	srv, _, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, err: errors.New("registry read failed")}
	})

	resp, raw := getHubInstancesSummary(t, srv)
	assert.Equal(t, "null", string(raw["hub_instances"]))
	assert.Equal(t, HealthStatusHealthy, resp.Status)
	assert.Equal(t, HubStatusUnknown, resp.Hub.Status)
	assert.Nil(t, resp.Hub.Instances)
	assert.Empty(t, resp.Hub.UnhealthyChecks)
	assert.Equal(t, []HealthAttentionItem{{
		Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance,
		Subject: HealthAttentionSubject{Type: HealthSubjectHub},
		Message: "Hub instance data not available",
	}}, resp.Attention)
}

// Before any instance has written its row, no hub instance is live: the
// fleet is unhealthy with one critical item.
func TestHandleHealthSummary_NoLiveInstanceIsUnhealthy(t *testing.T) {
	srv, _ := testServer(t)

	resp, _ := getHubInstancesSummary(t, srv)
	assert.Equal(t, HealthStatusUnhealthy, resp.Status)
	assert.Equal(t, HealthStatusUnhealthy, resp.Hub.Status)
	require.NotNil(t, resp.Hub.Instances)
	assert.Equal(t, HealthSummaryHubFleet{}, *resp.Hub.Instances)
	assert.Equal(t, []HealthAttentionItem{{
		Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance,
		Subject: HealthAttentionSubject{Type: HealthSubjectHub},
		Message: "No hub instance is reporting",
	}}, resp.Attention)
}

// Only replica B has recorded that the service account assignment check
// cannot run. The diagnostic reaches the summary through B's registry row,
// so both replicas return the same response (apart from the serving
// marker and generated_at), the section names B, and the status follows
// the fleet rule: one of two live instances degraded is degraded.
func TestHandleHealthSummary_SACheckOnOneReplicaSameResponse(t *testing.T) {
	a, s := testServer(t)
	b := newHubReplica(t, s)
	b.mu.Lock()
	b.saAssignCheckMode = SAAssignCheckEnforce
	b.mu.Unlock()
	b.saAssignCheckDiag.Store(&saAssignCheckDiagnostic{})
	require.True(t, b.saAssignCheckCannotRun())
	require.False(t, a.saAssignCheckCannotRun())

	tickHubInstance(t, a)
	tickHubInstance(t, b)
	pinned := time.Now().UTC()
	a.store = &pinnedClockStore{Store: a.store, now: pinned}
	b.store = &pinnedClockStore{Store: b.store, now: pinned}

	bodies := map[string][]byte{}
	for _, serving := range []*Server{a, b} {
		rr := doRequest(t, serving, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.NotNil(t, resp.HubInstances)
		var bLabel string
		for _, it := range resp.HubInstances.Items {
			if it.ID == b.InstanceID() {
				bLabel = it.Label
			}
		}
		require.NotNil(t, resp.ServiceAccountCheck, "the section is present whichever replica serves")
		assert.Equal(t, []string{bLabel}, resp.ServiceAccountCheck.Instances)
		assert.Equal(t, HealthStatusDegraded, resp.Status)
		assert.Equal(t, HealthStatusDegraded, resp.Hub.Status)
		require.NotNil(t, resp.Hub.Instances)
		assert.Equal(t, HealthSummaryHubFleet{Live: 2, Healthy: 1, Degraded: 1}, *resp.Hub.Instances)
		assert.Equal(t, []HealthSummaryHubCheck{{
			InstanceID: b.InstanceID(), InstanceLabel: bLabel, Name: saAssignCheckName, Value: "degraded",
		}}, resp.Hub.UnhealthyChecks)
		bodies[serving.InstanceID()] = rr.Body.Bytes()
	}

	assert.Equal(t,
		normalizeServingFields(t, bodies[a.InstanceID()]),
		normalizeServingFields(t, bodies[b.InstanceID()]),
		"the responses differ only in the serving marker and generated_at")
}
