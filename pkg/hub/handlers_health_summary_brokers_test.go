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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getHealthSummaryBrokers fetches the health summary and returns both the
// decoded runtime broker list and the raw JSON of each row keyed by broker ID.
func getHealthSummaryBrokers(t *testing.T, srv *Server) (HealthSummaryBrokers, map[string]map[string]json.RawMessage) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	var raw struct {
		Brokers struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"runtime_brokers"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	rows := make(map[string]map[string]json.RawMessage, len(raw.Brokers.Items))
	for _, row := range raw.Brokers.Items {
		var id string
		require.NoError(t, json.Unmarshal(row["id"], &id))
		rows[id] = row
	}
	return resp.Brokers, rows
}

func createSummaryBroker(t *testing.T, s store.Store, b *store.RuntimeBroker) {
	t.Helper()
	if b.Slug == "" {
		b.Slug = b.Name
	}
	if b.Status == "" {
		b.Status = store.BrokerStatusOnline
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
}

func TestHealthSummaryBrokers_ExcludesPluginRecords(t *testing.T) {
	srv, s := testServer(t)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-runtime"), Name: "hs-runtime", LastHeartbeat: time.Now(),
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-plugin"), Name: "hs-plugin",
		Labels: map[string]string{"scion.io/plugin": "telegram"},
	})

	list, _ := getHealthSummaryBrokers(t, srv)
	require.Len(t, list.Items, 1)
	assert.Equal(t, tid("hs-runtime"), list.Items[0].ID)
	assert.Equal(t, 1, list.Total)
	assert.False(t, list.Truncated)
}

func TestHealthSummaryBrokers_RuntimeFromDefaultProfile(t *testing.T) {
	srv, s := testServer(t)
	// The default profile is deliberately neither first in the list nor
	// first alphabetically.
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-default"), Name: "hs-default", LastHeartbeat: time.Now(),
		DefaultProfile: "zeta-k8s",
		Profiles: []store.BrokerProfile{
			{Name: "alpha-docker", Type: "docker", Available: true},
			{Name: "zeta-k8s", Type: "kubernetes", Available: true},
		},
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-single"), Name: "hs-single", LastHeartbeat: time.Now(),
		Profiles: []store.BrokerProfile{{Name: "only", Type: "podman", Available: true}},
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-ambiguous"), Name: "hs-ambiguous", LastHeartbeat: time.Now(),
		Profiles: []store.BrokerProfile{
			{Name: "a", Type: "docker", Available: true},
			{Name: "b", Type: "kubernetes", Available: true},
		},
	})

	list, rows := getHealthSummaryBrokers(t, srv)
	byID := map[string]HealthSummaryBroker{}
	for _, b := range list.Items {
		byID[b.ID] = b
	}

	require.NotNil(t, byID[tid("hs-default")].Runtime)
	assert.Equal(t, HealthBrokerRuntime{Type: "kubernetes", Profile: "zeta-k8s"}, *byID[tid("hs-default")].Runtime)
	require.NotNil(t, byID[tid("hs-single")].Runtime)
	assert.Equal(t, HealthBrokerRuntime{Type: "podman", Profile: "only"}, *byID[tid("hs-single")].Runtime)
	assert.Equal(t, "null", string(rows[tid("hs-ambiguous")]["runtime"]),
		"several profiles and no default resolve to no runtime")

	for id, row := range rows {
		for _, dropped := range []string{"runtime_available", "agent_count", "agent_healthy"} {
			_, ok := row[dropped]
			assert.False(t, ok, "%s must not be returned (%s)", dropped, id)
		}
		assert.JSONEq(t, `{"total":0}`, string(row["agents"]), id)
	}
}

func TestHealthSummaryBrokers_NoProfilesRuntimeNull(t *testing.T) {
	srv, s := testServer(t)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-noprof"), Name: "hs-noprof", LastHeartbeat: time.Now(),
		DefaultProfile: "missing",
	})

	list, rows := getHealthSummaryBrokers(t, srv)
	require.Len(t, list.Items, 1)
	assert.Nil(t, list.Items[0].Runtime)
	assert.Equal(t, "null", string(rows[tid("hs-noprof")]["runtime"]))
}

func TestHealthSummaryBrokers_WorkspaceStorage(t *testing.T) {
	srv, s := testServer(t)
	nfs := func(healthy bool) *api.BrokerWorkspaceStorage {
		return &api.BrokerWorkspaceStorage{
			Backend: api.WorkspaceStorageBackendNFS,
			NFS: &api.BrokerNFSWorkspaceStorage{
				Server: "nfs.example", Export: "/export", SubPathRoot: "scion", Healthy: healthy,
			},
		}
	}
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-local"), Name: "hs-local", LastHeartbeat: time.Now(),
		WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal},
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-nfs-ok"), Name: "hs-nfs-ok", LastHeartbeat: time.Now(), WorkspaceStorage: nfs(true),
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-nfs-bad"), Name: "hs-nfs-bad", LastHeartbeat: time.Now(), WorkspaceStorage: nfs(false),
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-unreported"), Name: "hs-unreported", LastHeartbeat: time.Now(),
	})

	_, rows := getHealthSummaryBrokers(t, srv)
	require.Len(t, rows, 4)

	assert.JSONEq(t, `{"backend":"local"}`, string(rows[tid("hs-local")]["workspace_storage"]),
		"nfs_healthy must be absent for a local backend")
	assert.JSONEq(t, `{"backend":"nfs","nfs_healthy":true}`, string(rows[tid("hs-nfs-ok")]["workspace_storage"]))
	assert.JSONEq(t, `{"backend":"nfs","nfs_healthy":false}`, string(rows[tid("hs-nfs-bad")]["workspace_storage"]))
	assert.Equal(t, "null", string(rows[tid("hs-unreported")]["workspace_storage"]))
}

func TestHealthSummaryBrokers_LastHeartbeatAndVersion(t *testing.T) {
	srv, s := testServer(t)
	seen := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-seen"), Name: "hs-seen", Version: "1.2.3", LastHeartbeat: seen,
	})
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-never"), Name: "hs-never", Status: store.BrokerStatusOffline,
	})

	list, rows := getHealthSummaryBrokers(t, srv)
	byID := map[string]HealthSummaryBroker{}
	for _, b := range list.Items {
		byID[b.ID] = b
	}

	require.NotNil(t, byID[tid("hs-seen")].LastHeartbeat)
	assert.True(t, seen.Equal(*byID[tid("hs-seen")].LastHeartbeat))
	assert.Equal(t, "1.2.3", byID[tid("hs-seen")].Version)
	assert.Equal(t, "null", string(rows[tid("hs-never")]["last_heartbeat"]))
}

func TestHealthSummaryBrokers_TruncatedAboveLimit(t *testing.T) {
	srv, s := testServer(t)
	n := healthSummaryBrokerLimit + 1
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("hs-many-%03d", i)
		createSummaryBroker(t, s, &store.RuntimeBroker{ID: tid(name), Name: name, LastHeartbeat: time.Now()})
	}
	createSummaryBroker(t, s, &store.RuntimeBroker{
		ID: tid("hs-many-plugin"), Name: "hs-many-plugin",
		Labels: map[string]string{"scion.io/plugin": "discord"},
	})

	list, _ := getHealthSummaryBrokers(t, srv)
	assert.Len(t, list.Items, healthSummaryBrokerLimit)
	assert.Equal(t, n, list.Total, "total counts runtime brokers only")
	assert.True(t, list.Truncated)
}

func TestHealthSummaryBrokers_EmptyListShape(t *testing.T) {
	srv, _ := testServer(t)
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	_, hasOld := raw["brokers"]
	assert.False(t, hasOld, "the old brokers key is replaced by runtime_brokers")
	assert.JSONEq(t, `{"items":[],"total":0,"truncated":false}`, string(raw["runtime_brokers"]))
}
