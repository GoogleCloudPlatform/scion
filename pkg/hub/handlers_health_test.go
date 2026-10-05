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
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckColocatedBrokerHealth_NotExpected covers a purely distributed Hub
// that never calls ExpectEmbeddedBroker: there is no co-located broker to
// check, so no key is added (same "no-op when not configured" shape as
// checkWorkspaceStorageHealth).
func TestCheckColocatedBrokerHealth_NotExpected(t *testing.T) {
	srv := &Server{}
	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	_, ok := checks["colocated_broker"]
	assert.False(t, ok, "expected no colocated_broker check when no embedded broker is expected")
}

// TestCheckColocatedBrokerHealth_Registered covers the happy path: the
// co-located broker registered successfully.
func TestCheckColocatedBrokerHealth_Registered(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()
	srv.SetEmbeddedBrokerID(tid("broker-1"))

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	assert.Equal(t, "healthy", checks["colocated_broker"])
}

// TestCheckColocatedBrokerHealth_RegistrationFailed is the ptone/scion#2154
// regression case: a co-located broker that failed to register (e.g. a
// non-UUID broker_id rejected by the store) must surface as unhealthy
// instead of being silently absent from the checks map.
func TestCheckColocatedBrokerHealth_RegistrationFailed(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New(`invalid UUID "not-a-broker-id"`))

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	// Fixed string, not the raw store/driver error: /healthz is
	// unauthenticated, and regErr can carry backend detail (table/constraint
	// names, values). The full error goes to the server log instead (see
	// EmbeddedBrokerRegistrationFailed's callers).
	assert.Equal(t, "unhealthy: registration failed", checks["colocated_broker"])
	assert.NotContains(t, checks["colocated_broker"], "invalid UUID",
		"the raw registration error must not reach the public health check value")
}

// TestCheckColocatedBrokerHealth_Pending covers the narrow startup race
// window: ExpectEmbeddedBroker has run but neither SetEmbeddedBrokerID nor
// EmbeddedBrokerRegistrationFailed has resolved it yet. The snapshot must
// not block (unlike waitForEmbeddedBroker) since /healthz is polled
// frequently (often with short client timeouts) and must not stall on this
// window.
func TestCheckColocatedBrokerHealth_Pending(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	assert.Contains(t, checks["colocated_broker"], "unhealthy")
	assert.Contains(t, checks["colocated_broker"], "pending")
}

// TestGetHealthInfo_DegradedWhenColocatedBrokerRegistrationFailed is the
// end-to-end regression test for ptone/scion#2154: previously, a Hub with a
// co-located broker that failed to register still reported top-level status
// "healthy" (with zero connected brokers) via GetHealthInfo/handleHealthz,
// and the health summary derived from it. The first symptom an operator saw
// was agent creation failing 422 (no runtime broker available). Now the
// status must go "degraded" with a reason as soon as registration fails.
func TestGetHealthInfo_DegradedWhenColocatedBrokerRegistrationFailed(t *testing.T) {
	srv, _ := testServer(t)

	// Sanity: healthy before anything embedded-broker related happens.
	info := srv.GetHealthInfo(context.Background())
	require.Equal(t, "healthy", info.Status)
	_, ok := info.Checks["colocated_broker"]
	assert.False(t, ok)

	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New(`invalid UUID "not-a-broker-id"`))

	info = srv.GetHealthInfo(context.Background())
	assert.Equal(t, "degraded", info.Status)
	assert.Equal(t, "unhealthy: registration failed", info.Checks["colocated_broker"])
	assert.NotContains(t, info.Checks["colocated_broker"], "invalid UUID",
		"the raw registration error must not reach the public health check value")
}

// pingFailStore wraps a real store but reports the database as unreachable,
// so GetHealthInfo's critical database check fails while everything else
// (stats queries, summary aggregation) still works.
type pingFailStore struct {
	store.Store
}

func (pingFailStore) Ping(context.Context) error { return errors.New("database is down") }

// TestDeriveHealthStatus pins the severity distinction from
// ptone/scion#1094: only a critical check (database, workspace_storage) makes the composite
// status unhealthy; any other non-healthy key only degrades it, so
// informational keys no longer read as "down" to consumers.
func TestDeriveHealthStatus(t *testing.T) {
	tests := []struct {
		name   string
		checks map[string]string
		want   string
	}{
		{"no checks", nil, HealthStatusHealthy},
		{"all healthy", map[string]string{"database": "healthy", "colocated_broker": "healthy"}, HealthStatusHealthy},
		{"non-critical only", map[string]string{"database": "healthy", "colocated_broker": "unhealthy: registration failed"}, HealthStatusDegraded},
		{"informational key", map[string]string{"database": "healthy", "workspace_storage": "healthy", "workspace_storage_mount_verification": "unavailable: could not compare filesystem device IDs"}, HealthStatusDegraded},
		{"database down", map[string]string{"database": "unhealthy"}, HealthStatusUnhealthy},
		{"workspace storage down", map[string]string{"database": "healthy", "workspace_storage": "unhealthy: mount not available"}, HealthStatusUnhealthy},
		{"workspace storage down wins over non-critical", map[string]string{"database": "healthy", "workspace_storage": "unhealthy: mount check timed out", "colocated_broker": "unhealthy: registration pending"}, HealthStatusUnhealthy},
		{"database down wins over non-critical", map[string]string{"database": "unhealthy", "colocated_broker": "unhealthy: registration pending"}, HealthStatusUnhealthy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Map iteration order is random; repeat to catch an
			// order-dependent early return.
			for i := 0; i < 20; i++ {
				assert.Equal(t, tt.want, deriveHealthStatus(tt.checks))
			}
		})
	}
}

func TestWorseHealthStatus(t *testing.T) {
	assert.Equal(t, HealthStatusHealthy, worseHealthStatus("healthy", "healthy"))
	assert.Equal(t, HealthStatusDegraded, worseHealthStatus("healthy", "degraded"))
	assert.Equal(t, HealthStatusDegraded, worseHealthStatus("degraded", "healthy"))
	assert.Equal(t, HealthStatusUnhealthy, worseHealthStatus("degraded", "unhealthy"))
	assert.Equal(t, HealthStatusUnhealthy, worseHealthStatus("unhealthy", "degraded"))
	assert.Equal(t, HealthStatusUnhealthy, worseHealthStatus("unhealthy", "healthy"))
	// Unknown values are a problem we cannot classify: degraded, not down.
	assert.Equal(t, HealthStatusDegraded, worseHealthStatus("healthy", "something-else"))
	assert.Equal(t, HealthStatusDegraded, worseHealthStatus("healthy", ""))
}

// TestGetHealthInfo_UnhealthyWhenDatabaseDown: a failed critical check must
// make the composite status "unhealthy", distinct from "degraded".
func TestGetHealthInfo_UnhealthyWhenDatabaseDown(t *testing.T) {
	srv, _ := testServer(t)
	srv.store = pingFailStore{srv.store}

	info := srv.GetHealthInfo(context.Background())
	assert.Equal(t, HealthStatusUnhealthy, info.Status)
	assert.Equal(t, "unhealthy", info.Checks["database"])

	// Still unhealthy (not merely degraded) with a non-critical failure too.
	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New("boom"))
	info = srv.GetHealthInfo(context.Background())
	assert.Equal(t, HealthStatusUnhealthy, info.Status)
}

// TestHealthz_StatusIsFirstField: gce-start-hub.sh and
// single-node-vm/deploy.sh read the top-level status by matching the body
// prefix {"status":"...", so reordering HealthResponse fields would make
// both scripts treat every hub as unknown and fail the deploy.
func TestHealthz_StatusIsFirstField(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Body.String(), `{"status":"healthy"`), "got %s", rec.Body.String())

	srv.store = pingFailStore{srv.store}
	rec = doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Body.String(), `{"status":"unhealthy"`), "got %s", rec.Body.String())
}
