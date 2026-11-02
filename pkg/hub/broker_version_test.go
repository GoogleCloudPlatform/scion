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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Runtime Broker vs hub version comparison (ptone/scion#5972).

// setHubFleet stores one live hub-instance row per version, so the fleet
// hub version is that version (one entry) or "mixed" (several).
func setHubFleet(t *testing.T, s store.Store, versions ...string) {
	t.Helper()
	for i, v := range versions {
		require.NoError(t, s.UpsertHubInstance(context.Background(), store.HubInstance{
			ID: tid("ver-hub-" + string(rune('a'+i))), Label: "hub", Version: v, Status: "healthy",
			Checks: map[string]string{"database": "healthy"},
		}))
	}
}

func versionTestBroker(name, ver string) *store.RuntimeBroker {
	return &store.RuntimeBroker{ID: tid(name), Name: name, Version: ver, LastHeartbeat: time.Now()}
}

// versionItems returns the broker_version_mismatch items of a summary.
func versionItems(resp HealthSummaryResponse) []HealthAttentionItem {
	var out []HealthAttentionItem
	for _, it := range resp.Attention {
		if it.Kind == HealthAttentionBrokerVersionMismatch {
			out = append(out, it)
		}
	}
	return out
}

func summaryBrokerByID(resp HealthSummaryResponse) map[string]HealthSummaryBroker {
	out := map[string]HealthSummaryBroker{}
	for _, b := range resp.Brokers.Items {
		out[b.ID] = b
	}
	return out
}

func TestBrokerVersionStatus(t *testing.T) {
	assert.Equal(t, "match", brokerVersionStatus("v1.2.0", "v1.2.0"))
	assert.Equal(t, "mismatch", brokerVersionStatus("v1.3.0", "v1.2.0"))
	assert.Equal(t, "unknown", brokerVersionStatus("v1.2.0", ""))
	assert.Equal(t, "unknown", brokerVersionStatus("", "v1.2.0"), "no live hub instance")
	assert.Equal(t, "unknown", brokerVersionStatus(HubVersionMixed, "v1.2.0"), "no single hub version during a rolling upgrade")
	assert.Equal(t, "unknown", brokerVersionStatus(HubVersionMixed, HubVersionMixed))
	long := strings.Repeat("a", 200)
	assert.Equal(t, "match", brokerVersionStatus(long[:hubInstanceMaxVersionBytes], long),
		"a stored version is bounded on read before it is compared")
}

func TestBrokerVersionToken_Bounded(t *testing.T) {
	assert.Equal(t, strings.Repeat("v", hubInstanceMaxVersionBytes), brokerVersionToken(strings.Repeat("v", 200)))
	assert.Equal(t, "v1.2.0", brokerVersionToken("v1.\n2.\x000"))
	assert.Equal(t, "", brokerVersionToken(""))
	assert.Equal(t, hubBuildToken(), HubBuildToken())
}

// Mixed fleet during a broker rollout: an older untagged broker (stored "",
// no heartbeat version) shows unknown and gets no item, next to a matching
// broker and an older tagged broker that lags the hub upgrade. The overall
// status is unchanged by any of them.
func TestHealthSummary_BrokerVersionMixedFleet(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	createSummaryBroker(t, s, versionTestBroker("ver-old-untagged", ""))
	createSummaryBroker(t, s, versionTestBroker("ver-new", "v1.3.0"))
	createSummaryBroker(t, s, versionTestBroker("ver-old-tagged", "v1.2.0"))

	resp, raw := getHealthSummary(t, srv)
	rows := summaryBrokerByID(resp)
	assert.Equal(t, "unknown", rows[tid("ver-old-untagged")].VersionStatus)
	assert.Equal(t, "match", rows[tid("ver-new")].VersionStatus)
	assert.Equal(t, "mismatch", rows[tid("ver-old-tagged")].VersionStatus)

	items := versionItems(resp)
	require.Len(t, items, 1, "only the online mismatched broker gets an item: %+v", resp.Attention)
	assert.Equal(t, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerVersionMismatch,
		Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: tid("ver-old-tagged"), Name: "ver-old-tagged"},
		Message: "Runtime broker ver-old-tagged runs v1.2.0; the hub runs v1.3.0",
	}, items[0])
	assert.Equal(t, HealthStatusHealthy, resp.Status, "a version mismatch never changes the overall status")

	var brokers struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw["runtime_brokers"], &brokers))
	for _, row := range brokers.Items {
		assert.Contains(t, row, "version_status")
	}
}

// Exact token: the hub on "v1.2.0" matches a broker on "v1.2.0"; "1.2.0"
// (no leading v) is a mismatch.
func TestHealthSummary_BrokerVersionExactToken(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.2.0")
	createSummaryBroker(t, s, versionTestBroker("ver-exact", "v1.2.0"))
	createSummaryBroker(t, s, versionTestBroker("ver-no-v", "1.2.0"))

	resp, _ := getHealthSummary(t, srv)
	rows := summaryBrokerByID(resp)
	assert.Equal(t, "match", rows[tid("ver-exact")].VersionStatus)
	assert.Equal(t, "mismatch", rows[tid("ver-no-v")].VersionStatus)
}

// A rolling hub upgrade ("mixed") or no live hub instance: every broker is
// unknown and gets no item, on the summary and on the list, which reports
// hubVersion as is.
func TestBrokerVersion_NoSingleHubVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fleet    []string
		wantList string
	}{
		{"mixed", []string{"v1.2.0", "v1.3.0"}, HubVersionMixed},
		{"no live instance", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			setHubFleet(t, s, tc.fleet...)
			createSummaryBroker(t, s, versionTestBroker("ver-roll-a", "v1.2.0"))
			createSummaryBroker(t, s, versionTestBroker("ver-roll-b", "v1.3.0"))

			resp, _ := getHealthSummary(t, srv)
			for _, b := range resp.Brokers.Items {
				assert.Equal(t, "unknown", b.VersionStatus, b.ID)
			}
			assert.Empty(t, versionItems(resp))

			list := listBrokersForVersion(t, srv, "")
			assert.Equal(t, tc.wantList, list.HubVersion)
			for _, b := range list.Brokers {
				assert.Equal(t, "unknown", b.VersionStatus, b.ID)
			}
		})
	}
}

// hubInstancesProbeStore counts the hub-instance registry reads made while
// its switch is armed, and fails them when fail is set. Installed right after
// the server is built (installStoreFault), never by reassigning srv.store.
type hubInstancesProbeStore struct {
	store.Store
	fault *storeFaultSwitch
	fail  bool
	reads atomic.Int32
}

func (f *hubInstancesProbeStore) ListHubInstances(ctx context.Context, window time.Duration) ([]store.HubInstance, time.Time, error) {
	if !f.fault.Active() {
		return f.Store.ListHubInstances(ctx, window)
	}
	f.reads.Add(1)
	if f.fail {
		return nil, time.Time{}, errors.New("registry unavailable")
	}
	return f.Store.ListHubInstances(ctx, window)
}

func newHubInstancesProbe(t *testing.T, fail bool) (*Server, store.Store, *hubInstancesProbeStore, *storeFaultSwitch) {
	t.Helper()
	return testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *hubInstancesProbeStore {
		return &hubInstancesProbeStore{Store: inner, fault: f, fail: fail}
	})
}

// A failed registry read never fails the list or get (200, hubVersion "",
// every broker unknown) and leaves every summary broker unknown.
func TestBrokerVersion_RegistryReadFailure(t *testing.T) {
	srv, s, _, fault := newHubInstancesProbe(t, true)
	setHubFleet(t, s, "v1.2.0")
	createSummaryBroker(t, s, versionTestBroker("ver-regfail", "v1.2.0"))
	fault.Arm()

	list := listBrokersForVersion(t, srv, "")
	assert.Equal(t, "", list.HubVersion)
	require.Len(t, list.Brokers, 1)
	assert.Equal(t, "unknown", list.Brokers[0].VersionStatus)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+tid("ver-regfail"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got RuntimeBrokerWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "unknown", got.VersionStatus)

	resp, _ := getHealthSummary(t, srv)
	assert.Nil(t, resp.HubInstances)
	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, "unknown", resp.Brokers.Items[0].VersionStatus)
}

// The summary reads the hub-instance registry once: the brokers'
// version_status and hub.version come from the same rows.
func TestHealthSummary_OneRegistryRead(t *testing.T) {
	srv, s, probe, fault := newHubInstancesProbe(t, false)
	setHubFleet(t, s, "v1.2.0")
	createSummaryBroker(t, s, versionTestBroker("ver-one-read", "v1.2.0"))
	fault.Arm()

	resp, _ := getHealthSummary(t, srv)
	assert.Equal(t, int32(1), probe.reads.Load(), "one ListHubInstances per summary")
	assert.Equal(t, "v1.2.0", resp.Hub.Version)
	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, "match", resp.Brokers.Items[0].VersionStatus)
}

// An offline mismatched broker keeps version_status mismatch but gets only
// its broker_offline item.
func TestHealthSummary_BrokerVersionOfflineNoItem(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	b := versionTestBroker("ver-offline", "v1.2.0")
	b.Status = store.BrokerStatusOffline
	createSummaryBroker(t, s, b)

	resp, _ := getHealthSummary(t, srv)
	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, "mismatch", resp.Brokers.Items[0].VersionStatus)
	assert.Empty(t, versionItems(resp))
	var kinds []string
	for _, it := range resp.Attention {
		if it.Subject.ID == tid("ver-offline") {
			kinds = append(kinds, it.Kind)
		}
	}
	assert.Equal(t, []string{HealthAttentionBrokerOffline}, kinds)
}

// Capped list: an online mismatched, otherwise healthy broker is kept ahead
// of a healthy matching one, with its item, and the status stays healthy.
func TestHealthSummaryBrokers_VersionMismatchKeptWhenCapped(t *testing.T) {
	prev := healthSummaryBrokerLimit
	healthSummaryBrokerLimit = 1
	t.Cleanup(func() { healthSummaryBrokerLimit = prev })

	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	// Store order is newest first, so the mismatched broker is created
	// first and would fall past the cap without the keep-when-capped key.
	createSummaryBroker(t, s, versionTestBroker("ver-cap-old", "v1.2.0"))
	time.Sleep(2 * time.Millisecond)
	createSummaryBroker(t, s, versionTestBroker("ver-cap-ok", "v1.3.0"))

	resp, _ := getHealthSummary(t, srv)
	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, tid("ver-cap-old"), resp.Brokers.Items[0].ID)
	assert.True(t, resp.Brokers.Truncated)
	require.Len(t, versionItems(resp), 1)
	assert.Equal(t, HealthStatusHealthy, resp.Status)
}

// A layout-mismatched row (GoogleCloudPlatform/scion#4177) and a
// version-mismatched row are both kept when the broker list is capped; the
// healthy row is the one dropped. Store order is newest first, so both
// mismatched brokers are created before the healthy one and would fall past
// the cap without the keep-when-capped key.
func TestHealthSummaryBrokers_LayoutAndVersionMismatchKeptWhenCapped(t *testing.T) {
	prev := healthSummaryBrokerLimit
	healthSummaryBrokerLimit = 2
	t.Cleanup(func() { healthSummaryBrokerLimit = prev })

	ctx := context.Background()
	srv, s := testServer(t)
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig("/mnt/nfs"))
	nfsOK := func() *api.BrokerWorkspaceStorage {
		return &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local",
			NFS: &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion", SubPathRoot: "projects", Healthy: true}}
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("cap-layout"), Name: "cap-layout", Slug: "cap-layout", Endpoint: "http://localhost:9811",
		Status: store.BrokerStatusOnline, Version: "v1.3.0",
		WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "local", Homes: "local"},
	}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("cap-version"), Name: "cap-version", Slug: "cap-version", Endpoint: "http://localhost:9812",
		Status: store.BrokerStatusOnline, Version: "v1.2.0", WorkspaceStorage: nfsOK(),
	}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("cap-ok"), Name: "cap-ok", Slug: "cap-ok", Endpoint: "http://localhost:9813",
		Status: store.BrokerStatusOnline, Version: "v1.3.0", WorkspaceStorage: nfsOK(),
	}))

	list, _, err := srv.healthSummaryBrokers(ctx, nil, "v1.3.0")
	require.NoError(t, err)
	assert.True(t, list.Truncated)
	assert.Equal(t, 3, list.Total)
	ids := map[string]HealthSummaryBroker{}
	for _, r := range list.Items {
		ids[r.ID] = r
	}
	require.Len(t, ids, 2)
	require.Contains(t, ids, tid("cap-layout"), "the layout-mismatched row is kept")
	require.Contains(t, ids, tid("cap-version"), "the version-mismatched row is kept")
	assert.NotContains(t, ids, tid("cap-ok"), "the healthy row is the one dropped")
	assert.NotEmpty(t, ids[tid("cap-layout")].WorkspaceStorage.LayoutMismatch)
	assert.Equal(t, "mismatch", ids[tid("cap-version")].VersionStatus)
	assert.Empty(t, ids[tid("cap-version")].WorkspaceStorage.LayoutMismatch)
}

func TestHealthSummaryBrokerKeepWhenCapped(t *testing.T) {
	online := store.BrokerStatusOnline
	assert.True(t, healthSummaryBrokerKeepWhenCapped(HealthSummaryBroker{Status: online, VersionStatus: "mismatch"}))
	assert.False(t, healthSummaryBrokerKeepWhenCapped(HealthSummaryBroker{Status: online, VersionStatus: "unknown"}))
	assert.False(t, healthSummaryBrokerKeepWhenCapped(HealthSummaryBroker{Status: online, VersionStatus: "match"}))
	assert.True(t, healthSummaryBrokerKeepWhenCapped(HealthSummaryBroker{Status: store.BrokerStatusOffline, VersionStatus: "match"}),
		"problem rows are still kept")
	assert.False(t, healthSummaryBrokerHasProblem(HealthSummaryBroker{Status: online, VersionStatus: "mismatch"}),
		"a mismatch is not a problem row")
	assert.True(t, healthSummaryBrokerKeepWhenCapped(HealthSummaryBroker{Status: online, VersionStatus: "match",
		WorkspaceStorage: &HealthBrokerStorage{Backend: "local", LayoutMismatch: []string{"backend"}}}),
		"a layout-mismatched row (a problem row) is kept")
}

// Order: the version item is the last item of its broker, before the next
// broker's items.
func TestDeriveHealthSummaryStatus_BrokerVersionItemOrder(t *testing.T) {
	unhealthy := false
	resp := &HealthSummaryResponse{
		Hub:          HealthSummaryHub{Status: HealthStatusHealthy, Version: "v1.3.0", Instances: &HealthSummaryHubFleet{Live: 1, Healthy: 1}},
		HubInstances: &HealthSummaryHubInstances{},
		Brokers: HealthSummaryBrokers{Items: []HealthSummaryBroker{
			{ID: "b1", Name: "first", Version: "v1.2.0", VersionStatus: "mismatch", Status: store.BrokerStatusOnline,
				WorkspaceStorage: &HealthBrokerStorage{Backend: "nfs", NFSHealthy: &unhealthy}},
			{ID: "b2", Name: "second", Status: store.BrokerStatusOffline},
		}},
	}
	_, items := deriveHealthSummaryStatus(resp)
	var got []string
	for _, it := range items {
		if it.Subject.Type == HealthSubjectBroker {
			got = append(got, it.Subject.ID+":"+it.Kind)
		}
	}
	assert.Equal(t, []string{
		"b1:" + HealthAttentionBrokerNFS,
		"b1:" + HealthAttentionBrokerVersionMismatch,
		"b2:" + HealthAttentionBrokerOffline,
	}, got)
}

// Plugin records never get a version status: not in the summary (they are
// not listed) and not on GET by id.
func TestBrokerVersion_PluginRecordHasNoStatus(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	p := versionTestBroker("ver-plugin", "0.1.0")
	p.Labels = map[string]string{pluginBrokerLabel: "chat"}
	createSummaryBroker(t, s, p)

	resp, _ := getHealthSummary(t, srv)
	assert.Empty(t, resp.Brokers.Items)
	assert.Empty(t, versionItems(resp))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+tid("ver-plugin"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "versionStatus")
}

// listBrokersForVersion lists runtime brokers (optionally for a project)
// and decodes hubVersion and each broker's versionStatus.
func listBrokersForVersion(t *testing.T, srv *Server, projectID string) struct {
	HubVersion string `json:"hubVersion"`
	Brokers    []struct {
		ID            string `json:"id"`
		Version       string `json:"version"`
		VersionStatus string `json:"versionStatus"`
	} `json:"brokers"`
} {
	t.Helper()
	path := "/api/v1/runtime-brokers"
	if projectID != "" {
		path += "?projectId=" + projectID
	}
	rec := doRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		HubVersion string `json:"hubVersion"`
		Brokers    []struct {
			ID            string `json:"id"`
			Version       string `json:"version"`
			VersionStatus string `json:"versionStatus"`
		} `json:"brokers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// The list and get carry hubVersion and versionStatus; the displayed
// version of a row stored before the bound existed is bounded on read.
func TestRuntimeBrokerList_VersionStatus(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	createSummaryBroker(t, s, versionTestBroker("ver-list-match", "v1.3.0"))
	createSummaryBroker(t, s, versionTestBroker("ver-list-old", "v1.2.0"))
	createSummaryBroker(t, s, versionTestBroker("ver-list-long", strings.Repeat("x", 200)))

	list := listBrokersForVersion(t, srv, "")
	assert.Equal(t, "v1.3.0", list.HubVersion)
	got := map[string]string{}
	for _, b := range list.Brokers {
		got[b.ID] = b.VersionStatus
		if b.ID == tid("ver-list-long") {
			assert.Len(t, b.Version, hubInstanceMaxVersionBytes, "bounded on read")
		}
	}
	assert.Equal(t, map[string]string{
		tid("ver-list-match"): "match", tid("ver-list-old"): "mismatch", tid("ver-list-long"): "mismatch",
	}, got)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+tid("ver-list-old"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var one RuntimeBrokerWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
	assert.Equal(t, "mismatch", one.VersionStatus)
}

// A legacy row with a 200-byte stored version: the summary row and the
// attention message carry the bounded token.
func TestHealthSummary_BrokerVersionBoundedOnRead(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	createSummaryBroker(t, s, versionTestBroker("ver-long", strings.Repeat("y", 200)))

	resp, _ := getHealthSummary(t, srv)
	require.Len(t, resp.Brokers.Items, 1)
	assert.Equal(t, strings.Repeat("y", hubInstanceMaxVersionBytes), resp.Brokers.Items[0].Version)
	items := versionItems(resp)
	require.Len(t, items, 1)
	assert.Equal(t, "Runtime broker ver-long runs "+strings.Repeat("y", hubInstanceMaxVersionBytes)+"; the hub runs v1.3.0", items[0].Message)
}

// The heartbeat refreshes the stored version only from the broker itself:
// a self heartbeat replaces a stale join value, a version-only heartbeat is
// enough, an omitted or empty version keeps it, a user heartbeat never sets
// it, and the value is bounded.
func TestBrokerHeartbeat_Version(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	id := tid("ver-hb")
	createSummaryBroker(t, s, versionTestBroker("ver-hb", "v1.2.0"))
	stored := func() string {
		b, err := s.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		return b.Version
	}

	// Version only: no other refreshed field, so the guard must admit it.
	sendAsBroker(t, srv, id, map[string]interface{}{"status": "online", "version": "v1.3.0"})
	assert.Equal(t, "v1.3.0", stored(), "a version-only self heartbeat replaces the stale value")

	sendAsBroker(t, srv, id, map[string]interface{}{"status": "online"})
	assert.Equal(t, "v1.3.0", stored(), "an omitted version keeps the stored one")
	sendAsBroker(t, srv, id, map[string]interface{}{"status": "online", "version": ""})
	assert.Equal(t, "v1.3.0", stored(), "an empty version keeps the stored one")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+id+"/heartbeat",
		map[string]interface{}{"status": "online", "version": "v9.9.9"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "v1.3.0", stored(), "a user heartbeat never sets the version")

	sendAsBroker(t, srv, id, map[string]interface{}{"status": "online", "version": "v2\n" + strings.Repeat("z", 200)})
	assert.Equal(t, "v2"+strings.Repeat("z", hubInstanceMaxVersionBytes-2), stored(), "bounded before it is stored")
}

// Rollback: a broker rolled back to a build without the heartbeat version
// keeps its last heartbeat-written token (omitted keeps); a re-join by that
// older build clears it to "" (unknown). Documented limitation.
func TestBrokerVersion_RollbackKeepsUntilRejoin(t *testing.T) {
	f := newOwnedBrokerFixture(t, false, "ver-rollback")
	setHubFleet(t, f.s, "v1.3.0")
	sendAsBroker(t, f.srv, f.id, map[string]interface{}{"status": "online", "version": "v1.3.0"})
	sendAsBroker(t, f.srv, f.id, map[string]interface{}{"status": "online"})
	assert.Equal(t, "v1.3.0", f.row(t).Version, "an older heartbeat keeps the token")

	resp := f.reregister(t, nil)
	rec := doRequestNoAuthLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers/join",
		BrokerJoinRequest{BrokerID: f.id, JoinToken: resp.JoinToken, Hostname: "ver-rollback", Version: ""})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "", f.row(t).Version)
}

// Join always stores the sent version: an older untagged broker re-joining
// with "" over a stored token shows unknown (not a false match); a token is
// stored bounded.
func TestBrokerJoin_AlwaysStoresVersion(t *testing.T) {
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "ver-join")
		setHubFleet(t, f.s, "v1.2.0")
		join := func(ver string) {
			t.Helper()
			resp := f.reregister(t, func(r *CreateBrokerRegistrationRequest) { r.PreserveSettings = true })
			req := BrokerJoinRequest{BrokerID: f.id, JoinToken: resp.JoinToken, Hostname: "ver-join", Version: ver}
			if flat {
				req.RuntimeTarget = f.base.RuntimeTarget
			}
			rec := doRequestNoAuthLoopback(t, f.srv, http.MethodPost, "/api/v1/brokers/join", req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}

		join("v1.2.0")
		assert.Equal(t, "v1.2.0", f.row(t).Version)

		join("")
		assert.Equal(t, "", f.row(t).Version, "an empty version is stored, not the stale token")
		resp, _ := getHealthSummary(t, f.srv)
		rows := summaryBrokerByID(resp)
		assert.Equal(t, "unknown", rows[f.id].VersionStatus)
		assert.Empty(t, versionItems(resp))

		join(strings.Repeat("j", 200))
		assert.Equal(t, strings.Repeat("j", hubInstanceMaxVersionBytes), f.row(t).Version)
	})
}

// The co-located flat registration writes the hub's build token on every
// registration, rewriting an old 0.1.0 placeholder.
func TestEmbeddedFlat_WritesHubBuildToken(t *testing.T) {
	f := newFlatRegFixture(t, true)
	ctx := context.Background()
	row := registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
	assert.Equal(t, hubBuildToken(), row.Version)

	b, err := f.s.GetRuntimeBroker(ctx, row.ID)
	require.NoError(t, err)
	b.Version = "0.1.0"
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, b))

	registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{})
	b, err = f.s.GetRuntimeBroker(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, hubBuildToken(), b.Version, "the placeholder is rewritten on update")
}

// The deprecated project registration keeps the stored version when it
// sends none (hubsync), and bounds a sent one on create and update.
func TestRegisterProjectBroker_Version(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	brokerID := tid("ver-register-project")
	register := func(n, ver string) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
			Name: "ver-register-project-" + n, GitRemote: "github.com/org/ver-register-project-" + n,
			Broker: &RegisterProjectBrokerInfo{ID: brokerID, Name: "ver-register-project", Version: ver},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	stored := func() string {
		b, err := s.GetRuntimeBroker(ctx, brokerID)
		require.NoError(t, err)
		return b.Version
	}

	register("1", strings.Repeat("c", 200))
	assert.Equal(t, strings.Repeat("c", hubInstanceMaxVersionBytes), stored(), "bounded on create")
	register("2", "v1.2.0")
	assert.Equal(t, "v1.2.0", stored())
	register("3", "")
	assert.Equal(t, "v1.2.0", stored(), "an empty version keeps the stored one")
}

// The projectId-filtered list carries hubVersion and each broker's
// versionStatus too.
func TestRuntimeBrokerList_ProjectFilteredVersionStatus(t *testing.T) {
	srv, s := testServer(t)
	setHubFleet(t, s, "v1.3.0")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "ver-project-list", GitRemote: "github.com/org/ver-project-list",
		Broker: &RegisterProjectBrokerInfo{ID: tid("ver-project-broker"), Name: "ver-project-broker", Version: "v1.2.0"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var reg RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	require.NotNil(t, reg.Project)

	list := listBrokersForVersion(t, srv, reg.Project.ID)
	assert.Equal(t, "v1.3.0", list.HubVersion)
	require.Len(t, list.Brokers, 1)
	assert.Equal(t, tid("ver-project-broker"), list.Brokers[0].ID)
	assert.Equal(t, "mismatch", list.Brokers[0].VersionStatus)
}
