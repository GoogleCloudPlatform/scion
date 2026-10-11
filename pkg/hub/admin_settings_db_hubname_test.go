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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubNameDBServer boots a DB-mode server whose bootstrap sets
// server.hub.hub_name and whose endpoints row is seeded from that bootstrap,
// the way syncHubSettings seeds it on every boot. "Prod.Hub" deliberately
// does not match the schema pattern: bootstrap accepts it (ptone/scion#2073).
func newHubNameDBServer(t *testing.T, bootstrapHubName string) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	flat := map[string]interface{}{"server.hub.public_url": "https://boot.example.com"}
	if bootstrapHubName != "" { // "" means no bootstrap hub_name at all
		flat["server.hub.hub_name"] = bootstrapHubName
	}
	bootstrapK := newFileKoanf(t, flat)
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

// Making the endpoints row managed (PUT public_url) must
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

// A PUT that echoes the effective hub_name back
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

// A PUT that changes hub_name persists it, ApplySnapshot
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
	// The running hub switches back too: Snapshot
	// resolves the bootstrap name, so ApplySnapshot does not keep the stale
	// managed name. The GCP secret backend label follows the name
	// ApplySnapshot resolves (snap.HubName, else the startup name).
	assert.Equal(t, "boot-hub", ops.Snapshot().HubName)
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, "boot-hub", running.HubName())
}

// Endpoints PUTs are built on the current row, so a
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

// With no hub_name configured anywhere, GET returns ""
// (unset), not this replica's hostname, so an echoed GET body sent to any
// replica is a no-op; a hostname-shaped name in a PUT is a real change; and
// set, clear, ApplySnapshot leaves the running hub on its own startup
// default (config.ResolveHubNameOrDefault), as a restart would.
func TestServerConfigDB_HubName_UnsetStaysUnsetAndRunsStartupDefault(t *testing.T) {
	srv, fakeStore, ops := newHubNameDBServer(t, "")
	startupDefault := config.ResolveHubNameOrDefault("")
	require.NotEmpty(t, startupDefault)

	resp := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "", resp.Server.Hub.HubName, "GET must not present a replica hostname as the configured hub_name")
	running := &Server{}
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, startupDefault, running.HubName())

	// Echo of the GET view: no-op.
	before, beforeOrigin := endpointsRow(t, fakeStore)
	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after, afterOrigin := endpointsRow(t, fakeStore)
	assert.Equal(t, before, after)
	assert.Equal(t, beforeOrigin, afterOrigin, "an echo must not make the row managed")

	// Another replica's hostname is a real change, written cluster-wide.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"scion-hub-7d9f8-abcde"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, "scion-hub-7d9f8-abcde", row.HubName)
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, "scion-hub-7d9f8-abcde", running.HubName())

	// Clear: GET is unset again and the running hub returns to its default.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "", ops.Snapshot().HubName)
	resp = getServerConfigDB(t, srv, ops)
	assert.Equal(t, "", resp.Server.Hub.HubName)
	assert.NotContains(t, supersededKeyNames(resp, "endpoints"), "server.hub.hub_name")
	ApplySnapshot(running, ops.Snapshot())
	assert.Equal(t, startupDefault, running.HubName())
}

// ApplySnapshot reports hub_name as applied only when the
// running name changes, including the reset to the startup default.
func TestApplySnapshot_HubNameAppliedOnlyOnChange(t *testing.T) {
	appliedKeys := func(res map[string]interface{}) []string {
		keys, _ := res["applied"].([]string)
		return keys
	}
	running := &Server{}
	assert.Contains(t, appliedKeys(ApplySnapshot(running, Layer1Snapshot{HubName: "a-hub"})), "hub_name")
	assert.NotContains(t, appliedKeys(ApplySnapshot(running, Layer1Snapshot{HubName: "a-hub"})), "hub_name")
	assert.Contains(t, appliedKeys(ApplySnapshot(running, Layer1Snapshot{})), "hub_name", "reset to the startup default")
	assert.Equal(t, config.ResolveHubNameOrDefault(""), running.HubName())
	assert.NotContains(t, appliedKeys(ApplySnapshot(running, Layer1Snapshot{})), "hub_name")
}

// On a seeded (non-managed) endpoints row, fields this
// node overrides by env are not carried into the shared row; without the
// env override they are carried.
func TestServerConfigDB_Endpoints_SeededBaseEnvGuard(t *testing.T) {
	seeded := json.RawMessage(`{"public_url":"https://seed.example.com","image_registry":"seed.example.com","hub_name":"seed-hub"}`)
	newSrv := func(envK map[string]interface{}) (*Server, *fakeHubSettingStore, *OperationalSettings) {
		fakeStore := newFakeHubSettingStore()
		fakeStore.seedWithOrigin("endpoints", seeded, "seeded")
		env := emptyKoanf()
		if envK != nil {
			env = newEnvKoanf(t, envK)
		}
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), env)
		_, err := ops.Refresh(context.Background())
		require.NoError(t, err)
		srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
		srv.SetOperationalSettings(ops)
		return srv, fakeStore, ops
	}

	// No env override: public_url and image_registry carry forward; the
	// seeded hub_name does not (it is bootstrap material).
	srv, fakeStore, ops := newSrv(nil)
	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ := endpointsRow(t, fakeStore)
	assert.Equal(t, opsettings.EndpointsSettings{PublicURL: "https://seed.example.com", ImageRegistry: "seed.example.com", HubName: "new-hub"}, row)

	// Both keys env-overridden on this node: neither is carried.
	srv, fakeStore, ops = newSrv(map[string]interface{}{
		"server.hub.public_url": "https://env.example.com",
		"image_registry":        "env.example.com",
	})
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, _ = endpointsRow(t, fakeStore)
	assert.Equal(t, opsettings.EndpointsSettings{HubName: "new-hub"}, row)
}

// With no endpoints row, the base is the effective
// (bootstrap) public_url and image_registry.
func TestServerConfigDB_Endpoints_NoRowBaseFromSnapshot(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.public_url": "https://boot.example.com",
		"image_registry":        "boot.example.com",
		"server.hub.hub_name":   "boot-hub",
	})
	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"admin.example.com"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row, origin := endpointsRow(t, fakeStore)
	assert.Equal(t, "managed", origin)
	assert.Equal(t, opsettings.EndpointsSettings{PublicURL: "https://boot.example.com", ImageRegistry: "admin.example.com"}, row,
		"bootstrap public_url carries forward; the bootstrap hub_name is not written")
}

// endpointsRaceStore simulates another replica writing the endpoints row
// between the PUT handler's read of the current row and its write.
type endpointsRaceStore struct {
	*fakeHubSettingStore
	reads int
}

func (c *endpointsRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	row, err := c.fakeHubSettingStore.GetHubSetting(ctx, section)
	if err == nil && section == "endpoints" {
		snapshot := *row
		c.reads++
		if c.reads == 1 {
			_, _ = c.UpsertHubSetting(ctx, "endpoints",
				json.RawMessage(`{"public_url":"https://other.example.com"}`), "other-replica", -1, "managed")
		}
		return &snapshot, nil
	}
	return row, err
}

// The endpoints carry-forward write is CAS-guarded on the
// revision it read, so a concurrent write yields 409 and nothing is written.
func TestServerConfigDB_Endpoints_ConcurrentWrite409(t *testing.T) {
	fake := newFakeHubSettingStore()
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"public_url":"https://admin.example.com"}`), "managed")
	raceStore := &endpointsRaceStore{fakeHubSettingStore: fake}
	ops := NewOperationalSettings(raceStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"admin.example.com"}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	row, _ := endpointsRow(t, fake)
	assert.Equal(t, opsettings.EndpointsSettings{PublicURL: "https://other.example.com"}, row, "the concurrent writer's row must stand")
}
