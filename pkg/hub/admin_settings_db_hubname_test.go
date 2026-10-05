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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubNameDBServer boots a DB-mode server whose bootstrap sets
// server.hub.hub_name and whose endpoints row is seeded from that bootstrap,
// the way syncHubSettings seeds it on every boot. "Prod.Hub" deliberately
// does not match the schema pattern: bootstrap accepts it (ptone/scion#2073
// review finding 12).
func newHubNameDBServer(t *testing.T, bootstrapHubName string) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.hub_name":   bootstrapHubName,
		"server.hub.public_url": "https://boot.example.com",
	})
	doc, err := opsettings.ExtractSectionFromKoanf(bootstrapK, "endpoints")
	require.NoError(t, err)
	fakeStore.seedWithOrigin("endpoints", doc, "seeded")

	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	return srv, fakeStore, ops
}

func putHubNameServerConfigDB(t *testing.T, srv *Server, ops *OperationalSettings, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	if rr.Code == http.StatusOK {
		_, err := ops.Refresh(context.Background())
		require.NoError(t, err)
	}
	return rr
}

func getServerConfigDB(t *testing.T, srv *Server, ops *OperationalSettings) ServerConfigDBResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

func endpointsRow(t *testing.T, fakeStore *fakeHubSettingStore) (opsettings.EndpointsSettings, string) {
	t.Helper()
	fakeStore.mu.Lock()
	row := fakeStore.settings["endpoints"]
	fakeStore.mu.Unlock()
	require.NotNil(t, row)
	var d opsettings.EndpointsSettings
	require.NoError(t, json.Unmarshal(row.Value, &d))
	return d, row.Origin
}

func supersededKeyNames(resp ServerConfigDBResponse, section string) []string {
	var keys []string
	for _, sk := range resp.SupersededKeys[section] {
		keys = append(keys, sk.Key)
	}
	return keys
}

// Review finding 1: making the endpoints row managed (PUT public_url) must
// not report the bootstrap hub_name as superseded. Nothing overrides it:
// the managed row has no hub_name and ApplySnapshot keeps the bootstrap
// value.
func TestServerConfigDB_HubName_ManagedEndpointsNotSuperseded(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "Prod.Hub")

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://admin.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row, origin := endpointsRow(t, fakeStore)
	assert.Equal(t, "managed", origin)
	assert.Empty(t, row.HubName, "the seeded bootstrap hub_name is not written into the managed row")

	resp := getServerConfigDB(t, srv, ops)
	assert.NotContains(t, supersededKeyNames(resp, "endpoints"), "server.hub.hub_name")
	assert.Contains(t, supersededKeyNames(resp, "endpoints"), "server.hub.public_url",
		"a value the managed row really overrides is still reported")
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Hub)
	assert.Equal(t, "Prod.Hub", resp.Server.Hub.HubName, "GET returns the effective hub_name")
}

// Review findings 1 and 12: a PUT that echoes the effective hub_name back
// (as a client sending the GET body does) neither fails, even when the
// bootstrap value does not match the schema pattern, nor writes hub_name.
func TestServerConfigDB_HubName_EchoNeitherFailsNorWrites(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "Prod.Hub")

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://admin.example.com","hub_name":"Prod.Hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ := endpointsRow(t, fakeStore)
	assert.Empty(t, row.HubName)
	assert.Equal(t, "https://admin.example.com", row.PublicURL)

	// hub_name alone, echoed: nothing to write, still 200.
	before, _ := endpointsRow(t, fakeStore)
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"Prod.Hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, before, after)

	// The GET body echoed back verbatim is accepted for the endpoints part.
	resp := getServerConfigDB(t, srv, ops)
	echo, err := json.Marshal(map[string]interface{}{"server": map[string]interface{}{"hub": map[string]interface{}{
		"public_url": resp.Server.Hub.PublicURL,
		"hub_name":   resp.Server.Hub.HubName,
	}}})
	require.NoError(t, err)
	rr = putHubNameServerConfigDB(t, srv, ops, string(echo))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

// Review finding 1: a PUT that changes hub_name persists it, ApplySnapshot
// applies it, and later endpoints writes that omit hub_name keep it.
func TestServerConfigDB_HubName_ChangePersistsAndApplies(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "boot-hub")

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://boot.example.com","hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, origin := endpointsRow(t, fakeStore)
	assert.Equal(t, "managed", origin)
	assert.Equal(t, "new-hub", row.HubName)

	// One running server, applied after every step, as each replica does
	// on refresh.
	running := &Server{}
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, "new-hub", ops.Snapshot().HubName)
	assert.Equal(t, "new-hub", running.HubName())

	resp := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "new-hub", resp.Server.Hub.HubName)
	assert.Contains(t, supersededKeyNames(resp, "endpoints"), "server.hub.hub_name",
		"a managed hub_name that differs from bootstrap is a real override")

	// A later endpoints write without hub_name, and one echoing it, keep it.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://other.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ = endpointsRow(t, fakeStore)
	assert.Equal(t, "new-hub", row.HubName)
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://other.example.com","hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ = endpointsRow(t, fakeStore)
	assert.Equal(t, "new-hub", row.HubName)

	// An explicit "" clears the managed value; the bootstrap value applies.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://other.example.com","hub_name":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ = endpointsRow(t, fakeStore)
	assert.Empty(t, row.HubName)
	assert.Equal(t, "boot-hub", getServerConfigDB(t, srv, ops).Server.Hub.HubName)
	// The running hub switches back too (round-2 finding 1): Snapshot
	// resolves the bootstrap name, so ApplySnapshot does not keep the stale
	// managed name. The GCP secret backend label follows the same
	// snap.HubName in ApplySnapshot.
	assert.Equal(t, "boot-hub", ops.Snapshot().HubName)
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, "boot-hub", running.HubName())
}

// Round-2 finding 5: endpoints PUTs are built on the current row, so a
// hub_name-only PUT keeps the managed public_url and image_registry, and an
// image_registry-only PUT keeps public_url and hub_name.
func TestServerConfigDB_Endpoints_PutChangesOnlyItsFields(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "boot-hub")
	fakeStore.seedWithOrigin("endpoints", json.RawMessage(`{"public_url":"https://admin.example.com","image_registry":"reg.example.com"}`), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, opsettings.EndpointsSettings{PublicURL: "https://admin.example.com", HubName: "new-hub", ImageRegistry: "reg.example.com"}, row)

	rr = putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"other.example.com"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ = endpointsRow(t, fakeStore)
	assert.Equal(t, opsettings.EndpointsSettings{PublicURL: "https://admin.example.com", HubName: "new-hub", ImageRegistry: "other.example.com"}, row)

	// No-op echo of everything: row unchanged.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"other.example.com","server":{"hub":{"public_url":"https://admin.example.com","hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, row, after)
}

// A changed hub_name is validated against the schema pattern.
func TestServerConfigDB_HubName_InvalidChangeRejected(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "boot-hub")
	before, _ := endpointsRow(t, fakeStore)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"Bad.Name"}}}`)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "endpoints")
	after, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, before, after)
}
