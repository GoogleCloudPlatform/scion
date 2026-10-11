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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

var hubInstanceT0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// fakeClockHubInstanceStore wraps a real store and serves ListHubInstances
// from fixed rows and a fixed store clock.
type fakeClockHubInstanceStore struct {
	store.Store
	rows   []store.HubInstance
	now    time.Time
	err    error
	window time.Duration
}

func getHubInstancesSummary(t *testing.T, srv *Server) (HealthSummaryResponse, map[string]json.RawMessage) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	return resp, raw
}

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

// tickHubInstance runs one registry tick for srv, writing its row, with its
// plugins' health, as the registry loop does every 15 s.
func tickHubInstance(t *testing.T, srv *Server) {
	t.Helper()
	srv.newHubInstanceRegistry().tick(context.Background())
}

// newHubReplica returns a second hub server on the same store s, as a
// second replica behind a load balancer. Its own instance ID and plugin
// manager are independent of the first server's.
func newHubReplica(t *testing.T, s store.Store) *Server {
	t.Helper()
	srv, err := newTestHubServer(t, testServerConfig(), s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	return srv
}

// pluginCallCountingManager counts every plugin manager call that reaches
// a plugin or lists plugins.
type pluginCallCountingManager struct {
	*healthSummaryPluginDouble
	calls atomic.Int32
}

func (f *fakeClockHubInstanceStore) ListHubInstances(_ context.Context, window time.Duration) ([]store.HubInstance, time.Time, error) {
	f.window = window
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return f.rows, f.now, nil
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

func (m *pluginCallCountingManager) ListPlugins() []string {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.ListPlugins()
}

func (m *pluginCallCountingManager) BrokerInfo(name string) (string, string, []string, error) {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.BrokerInfo(name)
}

func (m *pluginCallCountingManager) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.BrokerHealthCheck(name)
}
