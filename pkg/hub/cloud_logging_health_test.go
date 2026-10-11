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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// P2-5: the cloud_logging /healthz key follows the injected provider's
// state (healthy, circuit open, recent write failures), keeps the composite
// at degraded (never unhealthy), leaves readiness alone, and is absent
// without a provider. Production constructor; the provider is the real
// logging.CloudWriteStats.HealthStatus over standalone counters, injected
// the way cmd does (pkg/hub never sees the Cloud handler type).
func TestCloudLoggingHealth_P2_5_FollowsProviderState(t *testing.T) {
	srv := newTestServerFromStore(t, newBareTestStore(t), nil)
	ctx := context.Background()

	info := srv.GetHealthInfo(ctx)
	_, present := info.Checks[cloudLoggingHealthKey]
	require.False(t, present, "cloud_logging present without a Cloud handler: %v", info.Checks)
	baseline := info.Status
	require.False(t, criticalHealthChecks[cloudLoggingHealthKey])

	stats := &logging.CloudWriteStats{}
	var open atomic.Bool
	stats.RegisterCircuitSource(open.Load)
	now := time.Now()
	srv.SetCloudLoggingHealth(func() string { return stats.HealthStatus(now) })

	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, "healthy", info.Checks[cloudLoggingHealthKey])
	assert.Equal(t, baseline, info.Status, "a healthy cloud_logging key leaves the composite unchanged")

	open.Store(true)
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, "degraded: circuit open", info.Checks[cloudLoggingHealthKey])
	assert.Equal(t, HealthStatusDegraded, info.Status)
	health := httptest.NewRecorder()
	srv.handleHealthz(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, health.Code)
	assert.Contains(t, health.Body.String(), `"cloud_logging":"degraded: circuit open"`)
	ready := httptest.NewRecorder()
	srv.handleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, ready.Code, "readiness is independent of cloud_logging")

	open.Store(false)
	stats.RecordFailure(logging.CloudReasonError)
	now = time.Now() // inside the 5-minute window of the failure
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, "degraded: recent write failures", info.Checks[cloudLoggingHealthKey])
	assert.Equal(t, HealthStatusDegraded, info.Status)

	now = time.Now().Add(10 * time.Minute) // window passed
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, "healthy", info.Checks[cloudLoggingHealthKey])

	// A value outside the contract can only degrade.
	srv.SetCloudLoggingHealth(func() string { return "unhealthy: anything" })
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, "degraded: unknown", info.Checks[cloudLoggingHealthKey])
	assert.Equal(t, HealthStatusDegraded, info.Status)

	srv.SetCloudLoggingHealth(nil)
	_, present = srv.GetHealthInfo(ctx).Checks[cloudLoggingHealthKey]
	assert.False(t, present, "cloud_logging still present after removing the provider")
}
