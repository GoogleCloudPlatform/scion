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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each broker row carries the broker's self-reported health: null when it
// never reported one, the stored report otherwise, including the last one
// of an offline broker.
func TestHealthSummaryBrokers_Health(t *testing.T) {
	srv, s := testServer(t)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-unreported"), Name: "hs-health-unreported", LastHeartbeat: time.Now(),
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-degraded"), Name: "hs-health-degraded", LastHeartbeat: time.Now(),
		Health: &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": "unavailable"}},
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-ok"), Name: "hs-health-ok", LastHeartbeat: time.Now(),
		Health: &api.BrokerHealthReport{Status: "healthy", Checks: map[string]string{"docker": "available"}},
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-offline"), Name: "hs-health-offline", Status: store.BrokerStatusOffline,
		LastHeartbeat: time.Now().Add(-time.Hour),
		Health:        &api.BrokerHealthReport{Status: "healthy"},
	})

	list, rows := getHealthSummaryBrokers(t, srv)
	require.Len(t, rows, 4)

	assert.Equal(t, "null", string(rows[tid("hs-health-unreported")]["health"]),
		"an older broker's health is null (not reported), never healthy")
	assert.JSONEq(t, `{"status":"degraded","checks":{"runtime":"unavailable"}}`,
		string(rows[tid("hs-health-degraded")]["health"]))
	assert.JSONEq(t, `{"status":"healthy","checks":{"docker":"available"}}`,
		string(rows[tid("hs-health-ok")]["health"]))
	assert.JSONEq(t, `{"status":"healthy","checks":{}}`, string(rows[tid("hs-health-offline")]["health"]),
		"an offline broker keeps its last report; checks is never null")

	byID := map[string]HealthSummaryBroker{}
	for _, b := range list.Items {
		byID[b.ID] = b
	}
	assert.Equal(t, store.BrokerStatusOnline, byID[tid("hs-health-degraded")].Status,
		"self-health never changes liveness")
	assert.Equal(t, store.BrokerStatusOffline, byID[tid("hs-health-offline")].Status)
}

// End to end: a broker whose default runtime failed sends a degraded
// report on its heartbeat; the summary shows it degraded with runtime
// unavailable while it stays online.
func TestHealthSummaryBrokers_HealthFromHeartbeat(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	createSummaryBroker(t, s, &store.RuntimeBroker{ID: tid("hs-health-hb"), Name: "hs-health-hb"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+tid("hs-health-hb")+"/heartbeat",
		brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline,
			Health: &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": "unavailable"}},
		})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	list, _ := getHealthSummaryBrokers(t, srv)
	require.Len(t, list.Items, 1)
	b := list.Items[0]
	assert.Equal(t, store.BrokerStatusOnline, b.Status)
	require.NotNil(t, b.Health)
	assert.Equal(t, "degraded", b.Health.Status)
	assert.Equal(t, map[string]string{"runtime": "unavailable"}, b.Health.Checks)
	assert.NotNil(t, b.LastHeartbeat, "the report's freshness is the last heartbeat")
}

// A degraded online broker is a problem row: it is ordered ahead of
// healthy rows, so capping the list never hides it.
func TestHealthSummaryBrokers_DegradedHealthKeptWhenCapped(t *testing.T) {
	prev := healthSummaryBrokerLimit
	healthSummaryBrokerLimit = 1
	t.Cleanup(func() { healthSummaryBrokerLimit = prev })

	srv, s := testServer(t)
	// Store order is newest first, so the degraded broker is created first
	// and would fall past the cap without problem-first ordering.
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-cap-bad"), Name: "hs-health-cap-bad", LastHeartbeat: time.Now(),
		Health: &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": "unavailable"}},
	})
	time.Sleep(2 * time.Millisecond)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-cap-ok"), Name: "hs-health-cap-ok", LastHeartbeat: time.Now(),
		Health: &api.BrokerHealthReport{Status: "healthy"},
	})

	list, _ := getHealthSummaryBrokers(t, srv)
	require.Len(t, list.Items, 1)
	assert.Equal(t, tid("hs-health-cap-bad"), list.Items[0].ID)
	assert.True(t, list.Truncated)
}

func TestHealthSummaryBrokerSelfIsProblem(t *testing.T) {
	for _, tc := range []struct {
		health *HealthBrokerSelf
		want   bool
	}{
		{nil, false},
		{&HealthBrokerSelf{Status: "healthy"}, false},
		{&HealthBrokerSelf{Status: "degraded"}, true},
		{&HealthBrokerSelf{Status: "unhealthy"}, true},
	} {
		got := healthSummaryBrokerSelfIsProblem(tc.health)
		assert.Equal(t, tc.want, got, "%+v", tc.health)
	}
	// Self-health is one problem signal next to liveness and NFS.
	assert.True(t, healthSummaryBrokerHasProblem(HealthSummaryBroker{
		Status: store.BrokerStatusOnline, Health: &HealthBrokerSelf{Status: "degraded"},
	}))
	assert.False(t, healthSummaryBrokerHasProblem(HealthSummaryBroker{
		Status: store.BrokerStatusOnline, Health: &HealthBrokerSelf{Status: "healthy"},
	}))
}

// The summary normalises the stored report again, so a row holding free
// text (written by any path other than the heartbeat) still only yields
// fixed values in the response.
func TestHealthSummaryBrokers_HealthNormalisedFromStoredRow(t *testing.T) {
	srv, s := testServer(t)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-health-raw"), Name: "hs-health-raw", LastHeartbeat: time.Now(),
		Health: &api.BrokerHealthReport{
			Status: "degraded: see /var/log/broker.log",
			Checks: map[string]string{"nfs_mounts": rawNFSHealthCheck, "../etc": "healthy"},
		},
	})

	_, rows := getHealthSummaryBrokers(t, srv)
	assert.JSONEq(t, `{"status":"degraded","checks":{"nfs_mounts":"unhealthy"}}`,
		string(rows[tid("hs-health-raw")]["health"]))
}
