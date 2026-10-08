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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthSummaryPluginDouble is a test double for the plugin manager with
// per-plugin health. A plugin listed in stopped fails its info query, as a
// plugin whose process has exited does.
type healthSummaryPluginDouble struct {
	*mockIntegrationManager
	health  map[string]string
	message map[string]string
	details map[string]map[string]string
	stopped map[string]bool
}

func newHealthSummaryPluginDouble(names ...string) *healthSummaryPluginDouble {
	d := &healthSummaryPluginDouble{
		mockIntegrationManager: newMockIntegrationManager(),
		health:                 map[string]string{},
		message:                map[string]string{},
		details:                map[string]map[string]string{},
		stopped:                map[string]bool{},
	}
	for _, n := range names {
		d.plugins[n] = map[string]string{}
		d.health[n] = "healthy"
	}
	return d
}

func (d *healthSummaryPluginDouble) BrokerInfo(name string) (string, string, []string, error) {
	if d.stopped[name] {
		return "", "", nil, errors.New("plugin process exited: connection refused")
	}
	return "v1.2.3", "chan-secret-id", []string{"send"}, nil
}

func (d *healthSummaryPluginDouble) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	if d.stopped[name] {
		return "", "", nil, errors.New("plugin process exited")
	}
	return d.health[name], d.message[name], d.details[name], nil
}

func getHealthSummaryIntegrations(t *testing.T, srv *Server) ([]HealthSummaryIntegration, []byte) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Integrations, "integrations must be a list, never null")
	return resp.Integrations, rr.Body.Bytes()
}

func findHealthSummaryIntegration(t *testing.T, list []HealthSummaryIntegration, name string) HealthSummaryIntegration {
	t.Helper()
	for _, it := range list {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("integration %q not in %+v", name, list)
	return HealthSummaryIntegration{}
}

func createHealthSummaryPluginRecord(t *testing.T, s store.Store, name string) {
	t.Helper()
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID:              tid("plugin-record-" + name),
		Name:            "plugin-" + name,
		Slug:            "plugin-" + name,
		Status:          store.BrokerStatusOnline,
		ConnectionState: "embedded",
		Labels:          map[string]string{pluginBrokerLabel: name},
		Created:         time.Now(),
		Updated:         time.Now(),
	}))
}

func TestHandleHealthSummary_IntegrationsEmptyWithoutPlugins(t *testing.T) {
	srv, _ := testServer(t)
	list, body := getHealthSummaryIntegrations(t, srv)
	assert.Empty(t, list)
	assert.Contains(t, string(body), `"integrations":[]`)
}

func TestHandleHealthSummary_IntegrationHealthy(t *testing.T) {
	srv, s := testServer(t)
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	// The plugin's own record must not appear as a runtime broker.
	createHealthSummaryPluginRecord(t, s, "telegram")

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	require.Len(t, resp.Integrations, 1, "a managed plugin with a record is listed once")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "telegram", Platform: "telegram", Health: "healthy", Connected: true, Version: "v1.2.3",
	}, resp.Integrations[0])
	for _, b := range resp.Brokers.Items {
		assert.NotEqual(t, "plugin-telegram", b.Name, "plugin must appear only under integrations")
	}
}

func TestHandleHealthSummary_IntegrationStoppedChangesNextRefresh(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("chat-app")
	srv.SetPluginManager(mgr)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "healthy", got.Health)
	assert.Equal(t, "gchat", got.Platform)
	assert.True(t, got.Connected)

	mgr.stopped["chat-app"] = true
	list, body := getHealthSummaryIntegrations(t, srv)
	got = findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "unknown", got.Health)
	assert.False(t, got.Connected)
	assert.Empty(t, got.Reason, "a managed plugin gets no not-managed reason")
	assert.NotContains(t, string(body), "connection refused", "raw errors must not leak")
}

func TestHandleHealthSummary_IntegrationUnhealthyNotConnected(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("slack")
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "slack")
	assert.Equal(t, "unhealthy", got.Health)
	assert.False(t, got.Connected)
}

func TestHandleHealthSummary_IntegrationNotManaged(t *testing.T) {
	srv, s := testServer(t)
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	createHealthSummaryPluginRecord(t, s, "discord")

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 2)
	assert.Equal(t, "discord", list[0].Name, "sorted by name")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "discord", Platform: "discord", Health: "unknown", Reason: "not managed by this hub instance",
	}, list[0])
	assert.Equal(t, "healthy", list[1].Health)
}

func TestHandleHealthSummary_IntegrationNotManagedWithoutManager(t *testing.T) {
	srv, s := testServer(t)
	createHealthSummaryPluginRecord(t, s, "teams")

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 1)
	assert.Equal(t, "unknown", list[0].Health)
	assert.Equal(t, "not managed by this hub instance", list[0].Reason)
}

func TestHandleHealthSummary_IntegrationsOmitMessageAndDetails(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("telegram")
	mgr.health["telegram"] = "degraded"
	mgr.message["telegram"] = "token sk-live-SECRETVALUE rejected"
	mgr.details["telegram"] = map[string]string{"bot_token": "SECRETDETAIL"}
	srv.SetPluginManager(mgr)

	_, body := getHealthSummaryIntegrations(t, srv)
	var raw struct {
		Integrations []map[string]json.RawMessage `json:"integrations"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw.Integrations, 1)
	allowed := map[string]bool{"name": true, "platform": true, "health": true, "connected": true, "version": true, "reason": true}
	for k := range raw.Integrations[0] {
		assert.True(t, allowed[k], "unexpected integration field %q", k)
	}
	assert.NotContains(t, raw.Integrations[0], "message")
	assert.NotContains(t, raw.Integrations[0], "details")
	for _, leak := range []string{"SECRETVALUE", "SECRETDETAIL", "chan-secret-id"} {
		assert.False(t, strings.Contains(string(body), leak), "response leaks %q", leak)
	}
	assert.Contains(t, string(body), `"health":"degraded"`)
}
