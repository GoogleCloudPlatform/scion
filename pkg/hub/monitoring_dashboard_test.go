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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDashboardURL = "https://console.cloud.google.com/monitoring/dashboards/builder/hub?project=p"

// newMonitoringDBServer is a DB-mode server whose writes apply to the
// server synchronously, as on the writing replica in production.
func newMonitoringDBServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	srv, fakeStore, ops := newTestDBServer(t)
	ops.server = srv
	return srv, fakeStore, ops
}

func putServerConfig(t *testing.T, srv *Server, ops *OperationalSettings, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	return rr
}

func storedMonitoringURL(t *testing.T, fakeStore *fakeHubSettingStore) (string, bool) {
	t.Helper()
	fakeStore.mu.Lock()
	row := fakeStore.settings["endpoints"]
	fakeStore.mu.Unlock()
	if row == nil {
		return "", false
	}
	var d opsettings.EndpointsSettings
	require.NoError(t, json.Unmarshal(row.Value, &d))
	return d.MonitoringDashboardURL, true
}

func TestMonitoringDashboardURL_SaveAppliesLive(t *testing.T) {
	srv, fakeStore, ops := newMonitoringDBServer(t)
	assert.Empty(t, srv.monitoringDashboardURL())
	assert.Nil(t, srv.healthSummaryLinks(), "unset: no links block")

	rr := putServerConfig(t, srv, ops, `{"server":{"hub":{"monitoring_dashboard_url":"`+testDashboardURL+`"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	got, ok := storedMonitoringURL(t, fakeStore)
	require.True(t, ok)
	assert.Equal(t, testDashboardURL, got)
	assert.Equal(t, testDashboardURL, srv.monitoringDashboardURL(), "applied without a restart")
	require.NotNil(t, srv.healthSummaryLinks())
	assert.Equal(t, testDashboardURL, srv.healthSummaryLinks().MonitoringDashboard)

	resp := getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Hub)
	assert.Equal(t, testDashboardURL, resp.Server.Hub.MonitoringDashboardURL, "GET returns the stored value")
}

func TestMonitoringDashboardURL_WriteSemantics(t *testing.T) {
	srv, fakeStore, ops := newMonitoringDBServer(t)
	rr := putServerConfig(t, srv, ops, `{"server":{"hub":{"monitoring_dashboard_url":"`+testDashboardURL+`"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Omitted in a save of another endpoints key: kept.
	rr = putServerConfig(t, srv, ops, `{"server":{"hub":{"public_url":"https://hub.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ := storedMonitoringURL(t, fakeStore)
	assert.Equal(t, testDashboardURL, got, "omitted keeps")

	// A save of another section: kept.
	rr = putServerConfig(t, srv, ops, `{"server":{"hub":{"stalled_threshold":"10m"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfig(t, srv, ops, `{"image_registry":"ghcr.io/example"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ = storedMonitoringURL(t, fakeStore)
	assert.Equal(t, testDashboardURL, got, "unrelated save keeps")
	assert.Equal(t, testDashboardURL, srv.monitoringDashboardURL())

	// The GET body echoed back keeps it too.
	resp := getServerConfigDB(t, srv, ops)
	echo, err := json.Marshal(map[string]interface{}{"server": map[string]interface{}{"hub": map[string]interface{}{
		"monitoring_dashboard_url": resp.Server.Hub.MonitoringDashboardURL,
	}}})
	require.NoError(t, err)
	rr = putServerConfig(t, srv, ops, string(echo))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ = storedMonitoringURL(t, fakeStore)
	assert.Equal(t, testDashboardURL, got)

	// An explicit "" clears it, live.
	rr = putServerConfig(t, srv, ops, `{"server":{"hub":{"monitoring_dashboard_url":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ = storedMonitoringURL(t, fakeStore)
	assert.Empty(t, got, "\"\" clears")
	assert.Empty(t, srv.monitoringDashboardURL())
	assert.Nil(t, srv.healthSummaryLinks())
	assert.Equal(t, "https://hub.example.com", ops.Snapshot().PublicURL, "clearing keeps the other endpoints keys")
}

func TestMonitoringDashboardURL_InvalidRejected(t *testing.T) {
	for name, value := range map[string]string{
		"javascript scheme": "javascript:alert(1)",
		"relative":          "/monitoring",
		"no scheme":         "dash.example.com/d",
		"ftp scheme":        "ftp://dash.example.com/",
		"credentials":       "https://u:p@dash.example.com/",
		"too long":          "https://dash.example.com/" + strings.Repeat("a", config.MonitoringDashboardURLMaxLength),
	} {
		t.Run(name, func(t *testing.T) {
			srv, fakeStore, ops := newMonitoringDBServer(t)
			body, err := json.Marshal(map[string]interface{}{"server": map[string]interface{}{"hub": map[string]interface{}{
				"monitoring_dashboard_url": value,
				"public_url":               "https://hub.example.com",
			}}})
			require.NoError(t, err)
			rr := putServerConfig(t, srv, ops, string(body))
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

			var env struct {
				Error struct {
					Code    string                 `json:"code"`
					Message string                 `json:"message"`
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env), rr.Body.String())
			assert.Equal(t, ErrCodeValidationError, env.Error.Code)
			assert.Equal(t, config.MonitoringDashboardURLKey, env.Error.Details["field"])
			assert.NotContains(t, env.Error.Message, value, "the message does not echo the value")

			_, ok := storedMonitoringURL(t, fakeStore)
			assert.False(t, ok, "nothing is written, including the valid sibling")
			assert.Empty(t, srv.monitoringDashboardURL())
		})
	}
}

// With no endpoints row, a bootstrap value (settings.yaml or SCION_SEED_)
// applies; an unrelated endpoints save writes it into the new row.
func TestMonitoringDashboardURL_BootstrapAppliesAndIsKept(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{config.MonitoringDashboardURLKey: testDashboardURL})
	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	ops.server = srv
	ApplySnapshot(srv, ops.Snapshot())
	assert.Equal(t, testDashboardURL, srv.monitoringDashboardURL())

	rr := putServerConfig(t, srv, ops, `{"server":{"hub":{"public_url":"https://hub.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, ok := storedMonitoringURL(t, fakeStore)
	require.True(t, ok)
	assert.Equal(t, testDashboardURL, got)
	assert.Equal(t, testDashboardURL, srv.monitoringDashboardURL())
}

// A node-local SCION_SERVER_ value shows as an env override and is not
// written into the shared row by an unrelated endpoints save.
func TestMonitoringDashboardURL_EnvOverrideNotWrittenToRow(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	flat := map[string]interface{}{config.MonitoringDashboardURLKey: "https://env.example.com/d"}
	ops := NewOperationalSettings(fakeStore, newFileKoanf(t, flat), newEnvKoanf(t, flat))
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	ops.server = srv

	assert.Contains(t, ops.EnvOverriddenKeys(), config.MonitoringDashboardURLKey)

	rr := putServerConfig(t, srv, ops, `{"server":{"hub":{"public_url":"https://hub.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, ok := storedMonitoringURL(t, fakeStore)
	require.True(t, ok)
	assert.Empty(t, got, "the env value stays node-local")
}

// A stored value that fails validation (written by other tooling) is not
// applied, and a later valid snapshot replaces it.
func TestMonitoringDashboardURL_ApplySnapshotIgnoresInvalid(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ApplySnapshot(srv, Layer1Snapshot{MonitoringDashboardURL: "javascript:alert(1)"})
	assert.Empty(t, srv.monitoringDashboardURL())
	assert.Nil(t, srv.healthSummaryLinks())

	ApplySnapshot(srv, Layer1Snapshot{MonitoringDashboardURL: testDashboardURL})
	assert.Equal(t, testDashboardURL, srv.monitoringDashboardURL())
	ApplySnapshot(srv, Layer1Snapshot{})
	assert.Empty(t, srv.monitoringDashboardURL(), "an unset snapshot clears the link")
}

// File mode (no OperationalSettings) carries the value on reload.
func TestMonitoringDashboardURL_FileSnapshot(t *testing.T) {
	gc := &config.GlobalConfig{}
	gc.Hub.MonitoringDashboardURL = testDashboardURL
	assert.Equal(t, testDashboardURL, BuildLayer1SnapshotFromFile(gc).MonitoringDashboardURL)
}

// A user access token may not change the link.
func TestMonitoringDashboardURL_TokenRefused(t *testing.T) {
	assert.Equal(t, settingsTokenRefused, serverConfigTokenKeys[config.MonitoringDashboardURLKey])
}
