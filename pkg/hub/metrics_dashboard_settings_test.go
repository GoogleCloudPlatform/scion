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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metricsDBSnap(s Layer1Snapshot) Layer1Snapshot {
	s.FromOperationalSettings = true
	return s
}

func requestNames(c *fakeMetricsClient) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, len(c.requests))
	for i, r := range c.requests {
		names[i] = r.Name
	}
	return names
}

func resetFakeRequests(c *fakeMetricsClient) {
	c.mu.Lock()
	c.requests = nil
	c.mu.Unlock()
}

// TestApplySnapshot_MetricsDashboardRetargetsFromDB verifies that a DB-saved
// telemetry.cloud.gcp_project_id retargets an existing metrics dashboard
// service, clears its cache, is idempotent on re-apply, and reverts to the
// startup project when cleared.
func TestApplySnapshot_MetricsDashboardRetargetsFromDB(t *testing.T) {
	client := newFakeMetricsClient()
	svc := &MetricsDashboardService{
		client:    client,
		projectID: "proj-startup",
		cache:     make(map[string]*cacheEntry),
	}
	srv := &Server{
		config: ServerConfig{
			TelemetryProjectID: "proj-startup",
		},
		metricsDashboard: svc,
	}
	srv.recordStartupLayer1Values()

	// Initial query hits proj-startup and populates the cache.
	rec := httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names := requestNames(client)
	require.NotEmpty(t, names)
	for _, n := range names {
		assert.Equal(t, "projects/proj-startup", n)
	}

	// Second query is served from cache (no new RPCs).
	resetFakeRequests(client)
	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, requestNames(client), "expected cached response before retarget")

	// DB snapshot updates telemetry.cloud.gcp_project_id -> retargets & clears cache.
	dbProj := "proj-db"
	res := ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{
		TelemetryConfig: &config.V1TelemetryConfig{
			Cloud: &config.V1TelemetryCloudConfig{
				GCPProjectID: &dbProj,
			},
		},
	}))
	assert.Contains(t, res["applied"], "telemetry_config")
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Equal(t, "proj-db", srv.currentMetricsDashboard().ProjectID())

	// Next query misses cache and targets projects/proj-db.
	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names = requestNames(client)
	require.NotEmpty(t, names)
	for _, n := range names {
		assert.Equal(t, "projects/proj-db", n)
	}

	// Re-applying the same project is a no-op and preserves the cache.
	resetFakeRequests(client)
	res = ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{
		TelemetryConfig: &config.V1TelemetryConfig{
			Cloud: &config.V1TelemetryCloudConfig{
				GCPProjectID: &dbProj,
			},
		},
	}))
	assert.NotContains(t, res["applied"], "metrics_dashboard")
	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, requestNames(client), "expected cache preserved on no-op apply")

	// Clearing telemetry in DB reverts to the startup project.
	res = ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{}))
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Equal(t, "proj-startup", srv.currentMetricsDashboard().ProjectID())

	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names = requestNames(client)
	require.NotEmpty(t, names)
	for _, n := range names {
		assert.Equal(t, "projects/proj-startup", n)
	}
}

// TestApplySnapshot_MetricsDashboardBuildsFromDBAndClears verifies that when
// no telemetry project ID is configured at startup (metricsDashboard == nil),
// loading or saving telemetry.cloud.gcp_project_id via OperationalSettings
// initializes the dashboard service, and clearing it disables the dashboard
// again.
func TestApplySnapshot_MetricsDashboardBuildsFromDBAndClears(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	client := newFakeMetricsClient()
	var builtProjects []string
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
		metricsDashboardFactory: func(_ context.Context, projectID string) (*MetricsDashboardService, error) {
			builtProjects = append(builtProjects, projectID)
			return &MetricsDashboardService{
				client:    client,
				projectID: projectID,
				cache:     make(map[string]*cacheEntry),
			}, nil
		},
	}
	srv.recordStartupLayer1Values()
	srv.SetOperationalSettings(ops)

	// Unconfigured at startup -> 503.
	rec := httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Save telemetry.cloud.gcp_project_id in DB and apply snapshot (mirrors
	// both startup DB load and runtime admin save).
	_, err := ops.Update(ctx, "telemetry", json.RawMessage(`{"cloud":{"gcp_project_id":"proj-db-only"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	res := ApplySnapshot(srv, ops.Snapshot())
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Equal(t, []string{"proj-db-only"}, builtProjects)
	require.NotNil(t, srv.currentMetricsDashboard())
	assert.Equal(t, "proj-db-only", srv.currentMetricsDashboard().ProjectID())

	// Dashboard now responds 200 and queries projects/proj-db-only.
	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	names := requestNames(client)
	require.NotEmpty(t, names)
	for _, n := range names {
		assert.Equal(t, "projects/proj-db-only", n)
	}

	// Clearing telemetry in DB disables the dashboard again -> 503.
	_, err = ops.Update(ctx, "telemetry", json.RawMessage(`{}`), "admin", -1, "managed")
	require.NoError(t, err)
	res = ApplySnapshot(srv, ops.Snapshot())
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Nil(t, srv.currentMetricsDashboard())

	rec = httptest.NewRecorder()
	srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestApplySnapshot_MetricsDashboardFallsBackToGCPProjectID verifies the
// priority chain when TelemetryProjectID is unset at startup and GCPProjectID
// is used as the fallback.
func TestApplySnapshot_MetricsDashboardFallsBackToGCPProjectID(t *testing.T) {
	client := newFakeMetricsClient()
	srv := &Server{
		config: ServerConfig{
			GCPProjectID: "proj-mint",
		},
		metricsDashboard: &MetricsDashboardService{
			client:    client,
			projectID: "proj-mint",
			cache:     make(map[string]*cacheEntry),
		},
	}
	srv.recordStartupLayer1Values()

	override := "proj-override"
	res := ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{
		TelemetryConfig: &config.V1TelemetryConfig{
			Cloud: &config.V1TelemetryCloudConfig{
				Provider:     "gcp",
				GCPProjectID: &override,
			},
		},
	}))
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Equal(t, "proj-override", srv.currentMetricsDashboard().ProjectID())

	// Removing gcp_project_id while keeping the telemetry section reverts to
	// GCPProjectID.
	res = ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{
		TelemetryConfig: &config.V1TelemetryConfig{
			Cloud: &config.V1TelemetryCloudConfig{
				Provider: "gcp",
			},
		},
	}))
	assert.Contains(t, res["applied"], "metrics_dashboard")
	assert.Equal(t, "proj-mint", srv.currentMetricsDashboard().ProjectID())
}

// TestApplySnapshot_MetricsDashboardFactoryErrorIsNonFatal ensures a failure
// to create a Cloud Monitoring client (for example missing ADC) does not fail
// ApplySnapshot or falsely report metrics_dashboard as applied.
func TestApplySnapshot_MetricsDashboardFactoryErrorIsNonFatal(t *testing.T) {
	srv := &Server{
		metricsDashboardFactory: func(_ context.Context, _ string) (*MetricsDashboardService, error) {
			return nil, errors.New("no default credentials")
		},
	}
	srv.recordStartupLayer1Values()

	proj := "proj-db"
	res := ApplySnapshot(srv, metricsDBSnap(Layer1Snapshot{
		TelemetryConfig: &config.V1TelemetryConfig{
			Cloud: &config.V1TelemetryCloudConfig{
				GCPProjectID: &proj,
			},
		},
	}))
	assert.Contains(t, res["applied"], "telemetry_config")
	assert.NotContains(t, res["applied"], "metrics_dashboard")
	assert.Nil(t, srv.currentMetricsDashboard())
}

// TestMetricsDashboard_SetProjectIDDiscardsInFlightStaleCache verifies that an
// in-flight query started before SetProjectID cannot write its result into the
// new project's cache.
func TestMetricsDashboard_SetProjectIDDiscardsInFlightStaleCache(t *testing.T) {
	svc := &MetricsDashboardService{
		client:    newFakeMetricsClient(),
		projectID: "proj-a",
		cache:     make(map[string]*cacheEntry),
	}

	assert.True(t, svc.setZonedCacheForProject("proj-a", "summary:7:tz=UTC", "UTC", "from-a"))
	_, ok := svc.getCached("summary:7:tz=UTC")
	assert.True(t, ok)

	// Retarget clears cache and rejects late writes for proj-a.
	svc.SetProjectID("proj-b")
	_, ok = svc.getCached("summary:7:tz=UTC")
	assert.False(t, ok, "SetProjectID must clear prior project cache")
	assert.False(t, svc.setZonedCacheForProject("proj-a", "summary:7:tz=UTC", "UTC", "stale-from-a"))
	_, ok = svc.getCached("summary:7:tz=UTC")
	assert.False(t, ok, "stale write for previous project must be ignored")

	// Writes for the active project succeed.
	assert.True(t, svc.setZonedCacheForProject("proj-b", "summary:7:tz=UTC", "UTC", "from-b"))
	got, ok := svc.getCached("summary:7:tz=UTC")
	require.True(t, ok)
	assert.Equal(t, "from-b", got)
}
