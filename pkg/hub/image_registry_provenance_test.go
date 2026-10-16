//go:build !no_sqlite

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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageRegistryProvenance_Sources: the admin GET reports the hub-scope
// source and value for each layer (ptone/scion#3916).
func TestImageRegistryProvenance_Sources(t *testing.T) {
	tempSettingsHome(t)
	ctx := context.Background()
	bootstrap := newFileKoanf(t, map[string]interface{}{"image_registry": "file.example"})
	srv, _, ops := newSQLiteOpsServer(t, bootstrap, nil)

	get := func() *ImageRegistryProvenance {
		t.Helper()
		resp := getServerConfigDB(t, srv, ops)
		require.NotNil(t, resp.ImageRegistryProvenance)
		return resp.ImageRegistryProvenance
	}
	p := get()
	assert.Equal(t, "file", p.Source)
	assert.Equal(t, "file.example", p.Value)

	_, err := ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	p = get()
	assert.Equal(t, "hub_db", p.Source)
	assert.Equal(t, "db.example", p.Value)

	srv.config.NodeImageRegistry = "node.example"
	p = get()
	assert.Equal(t, "node_env", p.Source)
	assert.Equal(t, "node.example", p.Value)
	assert.Equal(t, "db.example", getServerConfigDB(t, srv, ops).ImageRegistry, "the editable field never shows the env")
}

// TestImageRegistryProvenance_NodeEnvOverrides lists every broker that
// reports a node env, an equal one included (it still overrides later
// changes), and marks the ones that differ; a broker reporting none (an old
// broker, or none set) is not listed.
func TestImageRegistryProvenance_NodeEnvOverrides(t *testing.T) {
	tempSettingsHome(t)
	ctx := context.Background()
	bootstrap := newFileKoanf(t, map[string]interface{}{"image_registry": "file.example"})
	srv, st, ops := newSQLiteOpsServer(t, bootstrap, nil)
	reporting, silent, _, _ := registryBrokers(t, st) // reporting = node-1.example
	equal := &store.RuntimeBroker{
		ID: tid("reg-equal-" + t.Name()), Name: "reg-equal", Slug: tidSlugSafe("reg-equal-" + t.Name()),
		Status: store.BrokerStatusOnline, Capabilities: &store.BrokerCapabilities{NodeImageRegistry: "file.example"},
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, equal))

	p := getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	require.NotNil(t, p)
	got := map[string]ImageRegistryNodeEnvOverride{}
	for _, o := range p.NodeEnvOverrides {
		got[o.BrokerID] = o
	}
	require.Contains(t, got, reporting)
	assert.Equal(t, "node-1.example", got[reporting].Value)
	assert.True(t, got[reporting].Differs)
	require.Contains(t, got, equal.ID, "a broker with an equal node env is still listed")
	assert.False(t, got[equal.ID].Differs)
	assert.NotContains(t, got, silent, "a broker reporting no node env is not listed")
}

// TestImageRegistryProvenance_Profiles reports each profile's value in
// effect and its settings-file value, for the profiles editor hint.
func TestImageRegistryProvenance_Profiles(t *testing.T) {
	tempSettingsHome(t)
	ctx := context.Background()
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"image_registry":              "file.example",
		"profiles.k8s.runtime":        "kubernetes",
		"profiles.k8s.image_registry": "k8s-file.example",
		"profiles.local.runtime":      "docker",
	})
	srv, _, ops := newSQLiteOpsServer(t, bootstrap, nil)

	p := getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	require.NotNil(t, p)
	assert.Equal(t, ImageRegistryProfileProvenance{Source: "file_profile", Value: "k8s-file.example", FileValue: "k8s-file.example",
		InheritedSource: "file_profile", InheritedValue: "k8s-file.example"}, p.Profiles["k8s"])
	assert.Equal(t, ImageRegistryProfileProvenance{Source: "file", Value: "file.example", InheritedSource: "file", InheritedValue: "file.example"}, p.Profiles["local"])

	_, err := ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	p = getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	assert.Equal(t, ImageRegistryProfileProvenance{Source: "hub_db", Value: "db.example", FileValue: "k8s-file.example",
		InheritedSource: "hub_db", InheritedValue: "db.example"}, p.Profiles["k8s"],
		"a saved hub top beats the file profile, which is still reported as the file value")

	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_registry":"db-k8s.example"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	p = getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	assert.Equal(t, ImageRegistryProfileProvenance{Source: "hub_db_profile", Value: "db-k8s.example", FileValue: "k8s-file.example",
		InheritedSource: "hub_db", InheritedValue: "db.example"}, p.Profiles["k8s"],
		"the inherited value leaves out the profile's own saved value")
}

// TestImageRegistryProvenance_RedactsUserinfo: a node env that carries
// credentials is never shown as is, in the provenance or the broker API.
func TestImageRegistryProvenance_RedactsUserinfo(t *testing.T) {
	tempSettingsHome(t)
	ctx := context.Background()
	srv, st, ops := newSQLiteOpsServer(t, emptyKoanf(), nil)
	b := &store.RuntimeBroker{
		ID: tid("reg-cred-" + t.Name()), Name: "reg-cred", Slug: tidSlugSafe("reg-cred-" + t.Name()),
		Status: store.BrokerStatusOnline, Capabilities: &store.BrokerCapabilities{NodeImageRegistry: "user:secret@registry.example/team"},
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, b))
	srv.config.NodeImageRegistry = "hub:pw@hub-registry.example"

	p := getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	require.NotNil(t, p)
	assert.Equal(t, "***@hub-registry.example", p.Value)
	require.Len(t, p.NodeEnvOverrides, 1)
	assert.Equal(t, "***@registry.example/team", p.NodeEnvOverrides[0].Value)

	shown := brokerForDisplay(*b)
	assert.Equal(t, "***@registry.example/team", shown.Capabilities.NodeImageRegistry)
	assert.Equal(t, "user:secret@registry.example/team", b.Capabilities.NodeImageRegistry, "the stored value is unchanged")
	stored, err := st.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "user:secret@registry.example/team", stored.Capabilities.NodeImageRegistry)
}

// TestImageRegistryProvenance_PagesBrokers lists overrides across pages.
func TestImageRegistryProvenance_PagesBrokers(t *testing.T) {
	tempSettingsHome(t)
	ctx := context.Background()
	old := imageRegistryProvenancePageSize
	imageRegistryProvenancePageSize = 2
	t.Cleanup(func() { imageRegistryProvenancePageSize = old })
	srv, st, ops := newSQLiteOpsServer(t, emptyKoanf(), nil)
	for _, n := range []string{"page-a", "page-b", "page-c", "page-d", "page-e"} {
		require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: tid(n + "-" + t.Name()), Name: n, Slug: tidSlugSafe(n + "-" + t.Name()), Status: store.BrokerStatusOnline,
			Capabilities: &store.BrokerCapabilities{NodeImageRegistry: n + ".example"},
		}))
	}
	p := getServerConfigDB(t, srv, ops).ImageRegistryProvenance
	require.NotNil(t, p)
	var names []string
	for _, o := range p.NodeEnvOverrides {
		names = append(names, o.BrokerName)
	}
	assert.Equal(t, []string{"page-a", "page-b", "page-c", "page-d", "page-e"}, names)
}

// brokerListErrorStore fails every broker listing.
type brokerListErrorStore struct {
	store.Store
}

func (brokerListErrorStore) ListRuntimeBrokers(context.Context, store.RuntimeBrokerFilter, store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	return nil, errors.New("database unavailable")
}

// TestImageRegistryProvenance_ListingErrorIsPartial: a broker listing
// failure leaves the overrides empty; the rest of the report and the GET
// still answer.
func TestImageRegistryProvenance_ListingErrorIsPartial(t *testing.T) {
	tempSettingsHome(t)
	srv, _, ops := newTestDBServerWithStore(t, brokerListErrorStore{})
	setTestImageRegistryFileTier(t, ops, "file.example", nil)
	p := srv.imageRegistryProvenance(context.Background(), ops)
	require.NotNil(t, p)
	assert.Equal(t, "file", p.Source)
	assert.Equal(t, "file.example", p.Value)
	assert.Empty(t, p.NodeEnvOverrides)
}

// TestPutServerConfig_EchoOfGETWithProvenance: a client that sends a whole
// GET body back, the read-only image_registry_provenance included, gets a
// 200 on a DB hub and on a workstation hub, also when the report changed
// between the GET and the PUT.
func TestPutServerConfig_EchoOfGETWithProvenance(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		t.Run(fmt.Sprintf("workstation=%v", workstation), func(t *testing.T) {
			settingsPath := tempSettingsHome(t)
			require.NoError(t, os.WriteFile(settingsPath, []byte(hostedLayer0Fixture), 0o644))
			ctx := context.Background()
			srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
			getRR := httptest.NewRecorder()
			srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
			require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &body))
			require.Contains(t, body, "image_registry_provenance")
			before := readFileString(t, settingsPath)

			// The report changes before the PUT: a broker now reports a node env.
			require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: tid("echo-node-" + t.Name()), Name: "echo-node", Slug: tidSlugSafe("echo-node-" + t.Name()),
				Status: store.BrokerStatusOnline, Capabilities: &store.BrokerCapabilities{NodeImageRegistry: "node.example"},
			}))
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			rr := putServerConfig(t, srv, string(raw))
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			assert.Equal(t, before, readFileString(t, settingsPath), "the echo writes nothing to settings.yaml")
		})
	}
}
