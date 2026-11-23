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
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
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

func enableExperimentForReadyzTest(t *testing.T, srv *Server, name string) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == name {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

// TestReadyz_ArtifactStoreStartupLifecycle verifies ptone/scion#5712:
//   - /healthz remains 200 OK (liveness) while the artifact store is initializing.
//   - /readyz returns 503 ("artifact store initializing") while ExpectArtifactStore
//     is pending and artifacts are enabled, and turns 200 once SetArtifactStore
//     completes.
//   - During initialization, artifact routes answer 503 with a starting-up
//     message and Retry-After instead of "artifact storage is not configured".
//   - When artifacts are disabled, /readyz is not blocked on artifact store init.
//   - When ArtifactStoreInitFailed is called while artifacts are enabled,
//     /readyz stays 503 ("artifact store not available") and artifact routes
//     answer 503 without Retry-After.
func TestReadyz_ArtifactStoreStartupLifecycle(t *testing.T) {
	t.Run("artifacts disabled does not block readyz", func(t *testing.T) {
		srv, _ := testServer(t)
		require.False(t, srv.experimentEnabled(experiments.Artifacts))
		require.False(t, srv.experimentEnabled(experiments.AttachmentArtifacts))

		srv.ExpectArtifactStore()
		rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"status":"ready"`)
	})

	t.Run("artifacts enabled gates readyz and sets Retry-After until initialized", func(t *testing.T) {
		srv, _ := testServer(t)
		enableExperimentForReadyzTest(t, srv, experiments.Artifacts)
		blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "hub", LocalPath: t.TempDir()})
		require.NoError(t, err)
		srv.SetStorage(blobs)

		// Before ExpectArtifactStore, /readyz is 200 (unconfigured test server).
		rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		// Startup marks artifact store as initializing before listener binds.
		srv.ExpectArtifactStore()

		// /healthz stays 200 OK (liveness).
		healthRec := doRequest(t, srv, http.MethodGet, "/healthz", nil)
		require.Equal(t, http.StatusOK, healthRec.Code)
		assert.True(t, strings.HasPrefix(healthRec.Body.String(), `{"status":"healthy"`))

		// /readyz reports 503 not_ready while initializing.
		readyRec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusServiceUnavailable, readyRec.Code)
		var readyBody map[string]string
		require.NoError(t, json.Unmarshal(readyRec.Body.Bytes(), &readyBody))
		assert.Equal(t, "not_ready", readyBody["status"])
		assert.Equal(t, "artifact store initializing", readyBody["reason"])

		// Artifact route returns 503 with starting-up message and Retry-After.
		artRec := doRequest(t, srv, http.MethodGet, "/api/v1/artifacts?mine=1", nil)
		require.Equal(t, http.StatusServiceUnavailable, artRec.Code)
		assert.NotEmpty(t, artRec.Header().Get("Retry-After"))
		assert.Contains(t, artRec.Body.String(), "starting up")
		assert.NotContains(t, artRec.Body.String(), "not configured")

		// Once SetArtifactStore completes, /readyz turns 200 and artifact routes serve.
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifacts.db")+"?_pragma=busy_timeout(5000)")
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		st := artifacts.NewStore(db, "sqlite")
		require.NoError(t, st.Init(context.Background()))
		srv.SetArtifactStore(st)

		readyRec = doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusOK, readyRec.Code)
		assert.Contains(t, readyRec.Body.String(), `"status":"ready"`)

		artRec = doRequest(t, srv, http.MethodGet, "/api/v1/artifacts?mine=1", nil)
		require.Equal(t, http.StatusOK, artRec.Code)
	})

	t.Run("init failure keeps readyz 503 and drops Retry-After", func(t *testing.T) {
		srv, _ := testServer(t)
		enableExperimentForReadyzTest(t, srv, experiments.Artifacts)
		srv.ExpectArtifactStore()
		srv.ArtifactStoreInitFailed()

		readyRec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusServiceUnavailable, readyRec.Code)
		var readyBody map[string]string
		require.NoError(t, json.Unmarshal(readyRec.Body.Bytes(), &readyBody))
		assert.Equal(t, "not_ready", readyBody["status"])
		assert.Equal(t, "artifact store not available", readyBody["reason"])

		artRec := doRequest(t, srv, http.MethodGet, "/api/v1/artifacts?mine=1", nil)
		require.Equal(t, http.StatusServiceUnavailable, artRec.Code)
		assert.Empty(t, artRec.Header().Get("Retry-After"))
		assert.Contains(t, artRec.Body.String(), "not configured")
	})

	t.Run("attachment_artifacts experiment also gates readyz while initializing", func(t *testing.T) {
		srv, _ := testServer(t)
		enableExperimentForReadyzTest(t, srv, experiments.AttachmentArtifacts)
		srv.ExpectArtifactStore()

		readyRec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusServiceUnavailable, readyRec.Code)
		var readyBody map[string]string
		require.NoError(t, json.Unmarshal(readyRec.Body.Bytes(), &readyBody))
		assert.Equal(t, "not_ready", readyBody["status"])
		assert.Equal(t, "artifact store initializing", readyBody["reason"])
	})
}
