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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubNameNoRowDBServer boots a DB-mode server whose bootstrap sets
// server.hub.hub_name and that has no endpoints row yet.
func newHubNameNoRowDBServer(t *testing.T, bootstrapHubName string) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.public_url": "https://boot.example.com",
		"server.hub.hub_name":   bootstrapHubName,
	})
	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), store: &settingsAuditStore{}}
	srv.SetOperationalSettings(ops)
	return srv, fakeStore, ops
}

// A save that echoes the hub_name in effect together with a sibling
// endpoints field, with no endpoints row, does not report hub_name as
// changed: the bootstrap value stays in effect (ptone/scion#4699).
func TestPutServerConfigDB_EchoedHubNameNoEndpointsRowNotReported(t *testing.T) {
	srv, fakeStore, ops := newHubNameNoRowDBServer(t, "prod-hub")

	rr := putHubNameServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"hub_name":"prod-hub","monitoring_dashboard_url":"https://dash.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	applied, pending := savedKeys(t, rr.Body.Bytes())
	assert.NotContains(t, applied, "server.hub.hub_name")
	assert.NotContains(t, pending, "server.hub.hub_name")
	assert.Contains(t, applied, "server.hub.monitoring_dashboard_url", "the sibling change is still reported")

	row, _ := endpointsRow(t, fakeStore)
	assert.Empty(t, row.HubName, "the echoed hub_name is not written")
	assert.Equal(t, "prod-hub", ops.Snapshot().HubName)
}

// The same holds for a sibling save from a seeded endpoints row that holds
// the bootstrap hub_name (ptone/scion#4699).
func TestPutServerConfigDB_SiblingSaveFromSeededRowDoesNotReportHubName(t *testing.T) {
	srv, _, ops := newHubNameDBServer(t, "prod-hub")

	rr := putHubNameServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	applied, pending := savedKeys(t, rr.Body.Bytes())
	assert.NotContains(t, applied, "server.hub.hub_name")
	assert.NotContains(t, pending, "server.hub.hub_name")
	assert.Contains(t, applied, "server.hub.monitoring_dashboard_url")
}

// A real hub_name change is still reported (ptone/scion#4699).
func TestPutServerConfigDB_HubNameChangeStillReported(t *testing.T) {
	srv, _, ops := newHubNameNoRowDBServer(t, "prod-hub")

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"other-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	applied, pending := savedKeys(t, rr.Body.Bytes())
	assert.Contains(t, append(applied, pending...), "server.hub.hub_name")
}
