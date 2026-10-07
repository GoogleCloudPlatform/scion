//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func degradedRuntimeReport() *api.BrokerHealthReport {
	return &api.BrokerHealthReport{
		Status: "degraded",
		Checks: map[string]string{"runtime": "unavailable"},
	}
}

func newBrokerHealthTestBroker(t *testing.T, s store.Store, name string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:     tid(name),
		Name:   name,
		Slug:   name,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// An older broker sends no health report: the stored health stays null,
// and a broker that reported one before keeps it.
func TestBrokerHeartbeat_HealthAbsentLeavesStoredValue(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	broker := newBrokerHealthTestBroker(t, s, "broker-health-absent")
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Health, "a heartbeat without a health report leaves health null")

	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", Health: degradedRuntimeReport()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, degradedRuntimeReport(), got.Health, "an omitted report never clears the stored one")
}

// A changed report is persisted and replaces the stored one.
func TestBrokerHeartbeat_HealthChangePersists(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	broker := newBrokerHealthTestBroker(t, s, "broker-health-change")
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", Health: degradedRuntimeReport()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, degradedRuntimeReport(), got.Health)

	healthy := &api.BrokerHealthReport{
		Status: "healthy",
		Checks: map[string]string{"docker": "available", "nfs_mounts": "healthy"},
	}
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", Health: healthy})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, healthy, got.Health, "a recovered broker's report replaces the degraded one")
}

// A heartbeat repeating the stored report causes no broker write.
func TestBrokerHeartbeat_HealthUnchangedNoWrite(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	broker := newBrokerHealthTestBroker(t, s, "broker-health-nowrite")

	counting := &countingBrokerLoadStore{Store: s}
	srv.store = counting
	defer func() { srv.store = s }()

	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"
	long := strings.Repeat("x", brokerHealthValueMaxChars+30)
	hb := brokerHeartbeatRequest{
		Status: "online",
		Health: &api.BrokerHealthReport{
			Status: "degraded",
			Checks: map[string]string{"runtime": "unavailable", "nfs_mounts": long},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, counting.updateRuntimeBrokerCalls, "the first report is written")

	// The repeat is compared after truncation, so an over-long value that
	// was stored cut does not count as a change on every heartbeat.
	rec = doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, counting.updateRuntimeBrokerCalls, "a repeated report must not write the row")
}

// Self-health never changes the broker's liveness status: a degraded
// broker stays online, and the status the heartbeat states is kept.
func TestBrokerHeartbeat_HealthLeavesStatusUnchanged(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	broker := newBrokerHealthTestBroker(t, s, "broker-health-status")
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	for _, report := range []*api.BrokerHealthReport{
		degradedRuntimeReport(),
		{Status: "unhealthy", Checks: map[string]string{"runtime": "unavailable"}},
	} {
		rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", Health: report})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetRuntimeBroker(ctx, broker.ID)
		require.NoError(t, err)
		assert.Equal(t, store.BrokerStatusOnline, got.Status, "a %s report must not change status", report.Status)
		assert.Equal(t, report, got.Health)
	}
}

// A degraded broker keeps reconciling: the agents in its heartbeat are
// still updated alongside the stored health report.
func TestBrokerHeartbeat_DegradedBrokerKeepsReconciling(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("hb-degraded-agent", "starting", "")

	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Health:    degradedRuntimeReport(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "running", Activity: "working", RuntimeTarget: "docker"},
		}}},
	})

	ctx := context.Background()
	b, err := f.s.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, b.Status)
	assert.Equal(t, degradedRuntimeReport(), b.Health)

	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase, "the degraded broker's agent report is still applied")
	assert.True(t, got.LastSeen.After(a.LastSeen), "the agent's last seen is refreshed")
}

// The broker's wire type (hubclient) and the hub's request type decode the
// same JSON, in both directions of a version skew.
func TestBrokerHeartbeat_HealthWireCompatibility(t *testing.T) {
	// New broker to new hub: the field round-trips.
	raw, err := json.Marshal(hubclient.BrokerHeartbeat{Status: "online", Health: degradedRuntimeReport()})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"health":{"status":"degraded","checks":{"runtime":"unavailable"}}`)
	var req brokerHeartbeatRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	assert.Equal(t, degradedRuntimeReport(), req.Health)

	// Old broker to new hub: no field, decoded as nil (keep stored value).
	req = brokerHeartbeatRequest{}
	require.NoError(t, json.Unmarshal([]byte(`{"status":"online"}`), &req))
	assert.Nil(t, req.Health)

	// New broker to old hub: an old hub's request type has no Health field
	// and the hub decodes without DisallowUnknownFields, so the extra key
	// is ignored.
	var old struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(raw, &old))
	assert.Equal(t, "online", old.Status)

	// A broker without a report omits the key entirely.
	raw, err = json.Marshal(hubclient.BrokerHeartbeat{Status: "online"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"health"`)
}

func TestBoundBrokerHealthReport(t *testing.T) {
	assert.Nil(t, boundBrokerHealthReport(nil))

	long := strings.Repeat("a", brokerHealthValueMaxChars+1)
	got := boundBrokerHealthReport(&api.BrokerHealthReport{
		Status: long,
		Checks: map[string]string{"nfs_mounts": long, long: "healthy"},
	})
	assert.Len(t, got.Status, brokerHealthValueMaxChars)
	assert.Len(t, got.Checks["nfs_mounts"], brokerHealthValueMaxChars)
	assert.Equal(t, "healthy", got.Checks[long[:brokerHealthValueMaxChars]], "check names are bounded too")

	// Truncation counts characters, never splitting a multi-byte one.
	wide := strings.Repeat("é", brokerHealthValueMaxChars+5)
	got = boundBrokerHealthReport(&api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"nfs_mounts": wide}})
	v := got.Checks["nfs_mounts"]
	assert.True(t, utf8.ValidString(v))
	assert.Equal(t, brokerHealthValueMaxChars, utf8.RuneCountInString(v))

	// An empty check map normalizes to nil, matching what the store
	// reads back, so it is not seen as a change on every heartbeat.
	got = boundBrokerHealthReport(&api.BrokerHealthReport{Status: "healthy", Checks: map[string]string{}})
	assert.Nil(t, got.Checks)

	// The input is not modified.
	in := &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": long}}
	_ = boundBrokerHealthReport(in)
	assert.Equal(t, long, in.Checks["runtime"])
}
