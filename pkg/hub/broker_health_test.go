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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

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
	hb := brokerHeartbeatRequest{
		Status: "online",
		Health: &api.BrokerHealthReport{
			Status: "degraded",
			Checks: map[string]string{"runtime": "unavailable", "nfs_mounts": rawNFSHealthCheck},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, counting.updateRuntimeBrokerCalls, "the first report is written")

	// The repeat is compared after normalisation, so a free-text value
	// that was stored as a fixed word does not count as a change on every
	// heartbeat.
	rec = doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, counting.updateRuntimeBrokerCalls, "a repeated report must not write the row")
}

// rawNFSHealthCheck is an nfs_mounts value in the form /healthz shows it:
// share ID, NFS server and export, mount path and mount command output.
const rawNFSHealthCheck = "unhealthy: ws1: mount failed: mount 10.0.0.2:/export on /mnt/nfs/ws1 failed: exit status 32 (output: mount.nfs: access denied by server)"

// Check values are stored as fixed words only: free text from the broker
// (server, export, mount path, command output) is never stored nor
// returned by the health summary.
func TestBrokerHeartbeat_HealthCheckValuesNormalised(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	broker := newBrokerHealthTestBroker(t, s, "broker-health-normalise")
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status: "online",
		Health: &api.BrokerHealthReport{
			Status: "degraded",
			Checks: map[string]string{
				"nfs_mounts":   rawNFSHealthCheck,
				"runtime":      "unavailable",
				"/mnt/nfs/ws1": "healthy", // a name that is not a plain identifier is dropped
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, &api.BrokerHealthReport{
		Status: "degraded",
		Checks: map[string]string{"nfs_mounts": "unhealthy", "runtime": "unavailable"},
	}, got.Health)

	summary := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, summary.Code)
	body := summary.Body.String()
	for _, fragment := range []string{"10.0.0.2", "/export", "/mnt/nfs", "ws1", "exit status", "output:", "access denied"} {
		assert.NotContains(t, body, fragment, "the summary must not carry broker free text")
	}
	assert.Contains(t, body, `"health":{"status":"degraded","checks":{"nfs_mounts":"unhealthy","runtime":"unavailable"}}`)
}

// At most api.BrokerHealthMaxChecks checks are stored, the first in sorted
// name order; the selection is the same on every heartbeat, so a repeat
// causes no write.
func TestBrokerHeartbeat_HealthChecksCapped(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	broker := newBrokerHealthTestBroker(t, s, "broker-health-cap")

	counting := &countingBrokerLoadStore{Store: s}
	srv.store = counting
	defer func() { srv.store = s }()

	checks := map[string]string{}
	for i := 0; i < api.BrokerHealthMaxChecks+20; i++ {
		checks[fmt.Sprintf("check_%02d", i)] = "healthy"
	}
	// Names that are not plain identifiers or are over-long are dropped
	// rather than truncated, so no two names can collide.
	checks[strings.Repeat("a", api.BrokerHealthMaxNameChars+1)] = "unhealthy"
	checks[strings.Repeat("a", api.BrokerHealthMaxNameChars+2)] = "healthy"
	checks["a b"] = "unhealthy"
	hb := brokerHeartbeatRequest{Status: "online", Health: &api.BrokerHealthReport{Status: "healthy", Checks: checks}}
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	for i := 0; i < 5; i++ {
		rec := doRequest(t, srv, http.MethodPost, path, hb)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.Equal(t, 1, counting.updateRuntimeBrokerCalls, "only the first report is written")

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.Len(t, got.Health.Checks, api.BrokerHealthMaxChecks)
	for i := 0; i < api.BrokerHealthMaxChecks; i++ {
		assert.Contains(t, got.Health.Checks, fmt.Sprintf("check_%02d", i))
	}
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

func TestCountBrokerHealthNormalization(t *testing.T) {
	assert.False(t, countBrokerHealthNormalization(nil, nil).any())

	clean := &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": "unavailable", "x": "unknown"}}
	assert.False(t, countBrokerHealthNormalization(clean, api.NormalizeBrokerHealthReport(clean)).any(),
		"fixed words, including a value the broker sent as unknown, are not counted")

	checks := map[string]string{"nfs_mounts": rawNFSHealthCheck, "odd": "pending", "bad name": "healthy"}
	for i := 0; i < api.BrokerHealthMaxChecks; i++ {
		checks[fmt.Sprintf("z_%02d", i)] = "healthy"
	}
	in := &api.BrokerHealthReport{Status: "on fire", Checks: checks}
	got := countBrokerHealthNormalization(in, api.NormalizeBrokerHealthReport(in))
	assert.Equal(t, brokerHealthNormalization{
		// "bad name" is invalid; of the 18 valid names, the last 2 in
		// sorted order are past the cap.
		DroppedChecks:      3,
		UnrecognisedValues: 1, // "pending"; the NFS value keeps its leading word
		UnrecognisedStatus: true,
	}, got)
}

// What normalisation discarded is logged once per distinct report (only
// when the stored report changes), as counts without the broker's text.
// A report that needed no normalisation is not logged.
func TestBrokerHeartbeat_HealthNormalisationLogged(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	broker := newBrokerHealthTestBroker(t, s, "broker-health-log")
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"
	const msg = "broker health report normalised"

	send := func(report *api.BrokerHealthReport) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", Health: report})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	send(degradedRuntimeReport())
	assert.NotContains(t, logs.String(), msg, "a report of fixed words is not logged")

	odd := &api.BrokerHealthReport{
		Status: "degraded",
		Checks: map[string]string{"runtime": "pending: /var/run/x", "/mnt/nfs/ws1": "healthy"},
	}
	for i := 0; i < 3; i++ {
		send(odd)
	}
	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, msg), "logged once, when the stored report changed")
	assert.Contains(t, out, "dropped_checks=1")
	assert.Contains(t, out, "unrecognised_values=1")
	assert.Contains(t, out, "unrecognised_status=false")
	assert.NotContains(t, out, "/var/run/x")
	assert.NotContains(t, out, "/mnt/nfs")
}
