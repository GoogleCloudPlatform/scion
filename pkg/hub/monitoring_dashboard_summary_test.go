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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getHealthSummaryRaw(t *testing.T, srv *Server) map[string]json.RawMessage {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	return raw
}

// Through the full router: the health summary carries
// links.monitoring_dashboard only while the setting is set, and a save
// changes it without a restart.
func TestHealthSummary_MonitoringDashboardLink(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)
	srv.GetOperationalSettings().server = srv

	raw := getHealthSummaryRaw(t, srv)
	assert.NotContains(t, raw, "links", "unset: links is omitted")

	rr := doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", map[string]interface{}{
		"server": map[string]interface{}{"hub": map[string]interface{}{"monitoring_dashboard_url": testDashboardURL}},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	raw = getHealthSummaryRaw(t, srv)
	require.Contains(t, raw, "links")
	var links map[string]string
	require.NoError(t, json.Unmarshal(raw["links"], &links))
	assert.Equal(t, map[string]string{"monitoring_dashboard": testDashboardURL}, links)

	rr = doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", map[string]interface{}{
		"server": map[string]interface{}{"hub": map[string]interface{}{"monitoring_dashboard_url": ""}},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	raw = getHealthSummaryRaw(t, srv)
	assert.NotContains(t, raw, "links", "cleared: links is omitted")
}

// The link is not part of the unauthenticated public settings.
func TestPublicSettings_NoMonitoringDashboardLink(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)
	srv.GetOperationalSettings().server = srv
	rr := doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", map[string]interface{}{
		"server": map[string]interface{}{"hub": map[string]interface{}{"monitoring_dashboard_url": testDashboardURL}},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/settings/public", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "monitoring")
	assert.NotContains(t, rr.Body.String(), testDashboardURL)
}
