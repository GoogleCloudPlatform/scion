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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setTestImageRegistryFileTier sets this replica's file tier of
// image_registry (its bootstrap merge) on ops.
func setTestImageRegistryFileTier(t *testing.T, ops *OperationalSettings, top string, profiles map[string]string) {
	t.Helper()
	if top != "" {
		require.NoError(t, ops.bootstrapKoanf.Set("image_registry", top))
	}
	for name, v := range profiles {
		require.NoError(t, ops.bootstrapKoanf.Set("profiles."+name+".image_registry", v))
	}
}

// putSystemRegistry calls PUT /system/registry from loopback.
func putSystemRegistry(t *testing.T, srv *Server, value string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, "/api/v1/system/registry", `{"image_registry":"`+value+`"}`)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRegistry(rr, req)
	return rr
}

// registryRow returns a section row's image_registry (top-level), origin
// and revision.
func registryRow(t *testing.T, fake *fakeHubSettingStore, section string) (map[string]interface{}, string, int64) {
	t.Helper()
	row := storedRow(fake, section)
	require.NotNil(t, row, "no %s row", section)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m, row.Origin, row.Revision
}

// --- Hub-scope resolution ---

func TestHubScopeImageRegistry_Layers(t *testing.T) {
	ctx := context.Background()
	srv, fake, ops := newTestDBServer(t)

	assert.Equal(t, config.ResolvedImageRegistry{Source: config.ImageRegistrySourceDefault}, srv.hubScopeImageRegistry())

	setTestImageRegistryFileTier(t, ops, "file.example", map[string]string{"k8s": "file-k8s.example"})
	assert.Equal(t, config.ResolvedImageRegistry{Value: "file.example", Source: config.ImageRegistrySourceFile}, srv.hubScopeImageRegistry(),
		"hub scope uses the file top, never a profile value")

	// A seeded row mirrors the file tier and counts as file.
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"image_registry":"seeded.example"}`), "seeded")
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, config.ImageRegistrySourceFile, srv.hubScopeImageRegistry().Source, "a seeded row is not hub DB")
	assert.Equal(t, "file.example", srv.hubScopeImageRegistry().Value)

	_, err = ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Equal(t, config.ResolvedImageRegistry{Value: "db.example", Source: config.ImageRegistrySourceHubDB}, srv.hubScopeImageRegistry())

	srv.config.NodeImageRegistry = "node.example"
	assert.Equal(t, config.ResolvedImageRegistry{Value: "node.example", Source: config.ImageRegistrySourceNodeEnv}, srv.hubScopeImageRegistry())
}

// SCION_SERVER_IMAGEREGISTRY is part of the bootstrap merge, so it is the
// file tier: it applies with no managed row, and a managed row beats it.
func TestHubScopeImageRegistry_ServerEnvIsFileTier(t *testing.T) {
	tempSettingsHome(t)
	t.Setenv("SCION_SERVER_IMAGEREGISTRY", "server-env.example")
	fake := newFakeHubSettingStore()
	ops := NewOperationalSettings(fake, config.LoadBootstrapKoanf(), emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	assert.Equal(t, config.ResolvedImageRegistry{Value: "server-env.example", Source: config.ImageRegistrySourceFile}, srv.hubScopeImageRegistry())
	assert.Equal(t, "server-env.example", getServerConfigDB(t, srv, ops).ImageRegistry, "GET shows the file tier")

	_, err := ops.Update(context.Background(), "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Equal(t, config.ResolvedImageRegistry{Value: "db.example", Source: config.ImageRegistrySourceHubDB}, srv.hubScopeImageRegistry())
}

// SCION_MAINTENANCE_IMAGE_REGISTRY applies to maintenance builds only.
func TestMaintenanceImageRegistry_OverrideOnly(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "file.example", nil)
	srv.config.MaintenanceConfig.ImageRegistryOverride = "maint.example"
	assert.Equal(t, "maint.example", srv.maintenanceImageRegistry())
	assert.Equal(t, "file.example", srv.resolveImageRegistry(), "hub-scope reads ignore it")
	assert.Equal(t, "file.example", srv.dispatchImageRegistryFor("", ""), "dispatch ignores it")

	srv.config.MaintenanceConfig.ImageRegistryOverride = ""
	assert.Equal(t, "file.example", srv.maintenanceImageRegistry(), "resolved at build time without the override")
}

// captureImageRegistrySlog routes slog's default logger to a buffer for the test.
func captureImageRegistrySlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestWarnMaintenanceImageRegistryDrift(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "file.example", nil)

	buf := captureImageRegistrySlog(t)
	srv.WarnMaintenanceImageRegistryDrift()
	assert.Empty(t, buf.String(), "unset: no warning")

	srv.config.MaintenanceConfig.ImageRegistryOverride = "file.example"
	srv.WarnMaintenanceImageRegistryDrift()
	assert.Empty(t, buf.String(), "equal to the resolved value: no warning")

	srv.config.MaintenanceConfig.ImageRegistryOverride = "maint.example"
	srv.WarnMaintenanceImageRegistryDrift()
	assert.Contains(t, buf.String(), "SCION_MAINTENANCE_IMAGE_REGISTRY now applies only to maintenance image builds")
	assert.Contains(t, buf.String(), "SCION_IMAGE_REGISTRY")
}

func TestWarnShadowedImageRegistries(t *testing.T) {
	ctx := context.Background()
	srv, _, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "file.example", map[string]string{"k8s": "k8s.example", "gke": "gke.example"})

	buf := captureImageRegistrySlog(t)
	srv.WarnShadowedImageRegistries(map[string]string{"inst-1": "inst.example"})
	assert.Empty(t, buf.String(), "no managed top: nothing is shadowed")

	_, err := ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"gke":{"runtime":"kubernetes","image_registry":"db-gke.example"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	srv.WarnShadowedImageRegistries(map[string]string{"inst-1": "inst.example"})
	out := buf.String()
	assert.Contains(t, out, "k8s")
	assert.NotContains(t, out, "gke", "a profile with its own managed value is not shadowed")
	assert.Contains(t, out, "inst-1")
	assert.Contains(t, out, "move each value into the hub profile")
}

// --- Dispatch resolution ---

// colocatedBrokerLayers resolves image_registry the way the co-located
// broker does: its live settings, with the overlay fed as installation
// feeds it.
func colocatedBrokerLayers(t *testing.T, ops *OperationalSettings, profile, nodeEnv string) config.ResolvedImageRegistry {
	t.Helper()
	overlay := config.NewSettingsOverlay()
	snap := ops.Snapshot()
	overlay.Update(snap.Runtimes, snap.Profiles, snap.HarnessConfigs)
	overlay.UpdateImageRegistryInputs(ops.ImageRegistryHubInputs())
	vs, _, err := config.LoadGlobalSettings()
	require.NoError(t, err)
	overlay.Apply(vs)
	return config.ResolveImageRegistryLayers(vs.ImageRegistryLayers(profile, nodeEnv))
}

// writeTestGlobalSettings writes ~/.scion/settings.yaml of the current HOME.
func writeTestGlobalSettings(t *testing.T, content string) {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(content), 0o644))
}

// TestImageRegistry_HubBrokerParity: for the same inputs, the co-located
// broker (via the overlay) and the hub's dispatch resolution give the same
// value and source.
func TestImageRegistry_HubBrokerParity(t *testing.T) {
	const fileSettings = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
  k8s:
    runtime: kubernetes
`
	cases := []struct {
		name      string
		bootstrap map[string]interface{}
		rows      map[string][2]string // section -> {origin, doc}
		env       map[string]string
		profile   string
		want      config.ResolvedImageRegistry
	}{
		{
			name:      "seeded k8s profile and a managed top",
			bootstrap: map[string]interface{}{"profiles.k8s.image_registry": "b.example", "profiles.k8s.runtime": "kubernetes"},
			rows: map[string][2]string{
				"profiles":  {"seeded", `{"k8s":{"runtime":"kubernetes","image_registry":"b.example"},"staging":{"runtime":"docker"}}`},
				"endpoints": {"managed", `{"image_registry":"a.example"}`},
			},
			profile: "k8s",
			want:    config.ResolvedImageRegistry{Value: "a.example", Source: config.ImageRegistrySourceHubDB},
		},
		{
			name:      "server env top and a file profile",
			bootstrap: map[string]interface{}{"image_registry": "a.example", "profiles.k8s.image_registry": "b.example", "profiles.k8s.runtime": "kubernetes"},
			profile:   "k8s",
			want:      config.ResolvedImageRegistry{Value: "b.example", Source: config.ImageRegistrySourceFileProfile},
		},
		{
			name: "managed k8s profile",
			rows: map[string][2]string{
				"profiles": {"managed", `{"k8s":{"runtime":"kubernetes","image_registry":"c.example"},"staging":{"runtime":"docker"}}`},
			},
			profile: "k8s",
			want:    config.ResolvedImageRegistry{Value: "c.example", Source: config.ImageRegistrySourceHubDBProfile},
		},
		{
			name: "managed endpoints value with no catalog rows",
			rows: map[string][2]string{"endpoints": {"managed", `{"image_registry":"a.example"}`}},
			want: config.ResolvedImageRegistry{Value: "a.example", Source: config.ImageRegistrySourceHubDB},
		},
		{
			name:      "no profile: active_profile from SCION_ACTIVE_PROFILE",
			bootstrap: map[string]interface{}{"profiles.k8s.image_registry": "b.example", "profiles.k8s.runtime": "kubernetes", "profiles.staging.runtime": "docker"},
			env:       map[string]string{"SCION_ACTIVE_PROFILE": "k8s"},
			want:      config.ResolvedImageRegistry{Value: "b.example", Source: config.ImageRegistrySourceFileProfile},
		},
		{
			name: "no profile: a single stored managed profile",
			rows: map[string][2]string{
				"profiles": {"managed", `{"only":{"runtime":"docker","image_registry":"only.example"}}`},
			},
			want: config.ResolvedImageRegistry{Value: "only.example", Source: config.ImageRegistrySourceHubDBProfile},
		},
		{
			name:      "managed row without a key for the profile inherits the file value",
			bootstrap: map[string]interface{}{"profiles.k8s.image_registry": "b.example", "profiles.k8s.runtime": "kubernetes"},
			rows: map[string][2]string{
				"profiles": {"managed", `{"k8s":{"runtime":"kubernetes"},"staging":{"runtime":"docker"}}`},
			},
			profile: "k8s",
			want:    config.ResolvedImageRegistry{Value: "b.example", Source: config.ImageRegistrySourceFileProfile},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempSettingsHome(t)
			writeTestGlobalSettings(t, fileSettings)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			bootstrap := emptyKoanf()
			if tc.bootstrap != nil {
				bootstrap = newFileKoanf(t, tc.bootstrap)
			}
			fake := newFakeHubSettingStore()
			for section, r := range tc.rows {
				fake.seedWithOrigin(section, json.RawMessage(r[1]), r[0])
			}
			ops := NewOperationalSettings(fake, bootstrap, emptyKoanf())
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)
			srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
			srv.SetOperationalSettings(ops)
			srv.SetEmbeddedBrokerID("colocated")

			hub, ok := srv.dispatchImageRegistryResolved("colocated", tc.profile)
			require.True(t, ok)
			broker := colocatedBrokerLayers(t, ops, tc.profile, "")
			assert.Equal(t, tc.want, hub, "hub")
			assert.Equal(t, tc.want, broker, "co-located broker")
		})
	}
}

// ApplySnapshot feeds the co-located overlay's image_registry layers even
// when no catalog section is stored.
func TestApplySnapshot_FeedsImageRegistryOverlayWithoutCatalog(t *testing.T) {
	tempSettingsHome(t)
	prev := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
	overlay := config.NewSettingsOverlay()
	config.SetGlobalSettingsOverlay(overlay)

	srv, _, ops := newTestDBServer(t)
	_, err := ops.Update(context.Background(), "endpoints", json.RawMessage(`{"image_registry":"a.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())

	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, config.ResolvedImageRegistry{Value: "a.example", Source: config.ImageRegistrySourceHubDB},
		config.ResolveImageRegistryLayers(vs.ImageRegistryLayers("", "")))
}

// --- Persistence: stored only when the field changes ---

// seededEndpointsServer is a DB server whose file tier and seeded endpoints
// row both hold file.example.
func seededEndpointsServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	srv, fake, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "file.example", nil)
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"image_registry":"file.example","monitoring_dashboard_url":"https://grafana.example.com/d/scion"}`), "seeded")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	return srv, fake, ops
}

func TestImageRegistryPersistence_UnrelatedSavesStoreNothing(t *testing.T) {
	t.Run("hub_name save omitting image_registry", func(t *testing.T) {
		srv, fake, ops := seededEndpointsServer(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"hub_name":"new-hub"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		doc, origin, _ := registryRow(t, fake, "endpoints")
		assert.Equal(t, "managed", origin)
		assert.NotContains(t, doc, "image_registry")
		// A later file-tier change takes effect.
		setTestImageRegistryFileTier(t, ops, "new-file.example", nil)
		assert.Equal(t, config.ResolvedImageRegistry{Value: "new-file.example", Source: config.ImageRegistrySourceFile}, srv.hubScopeImageRegistry())
	})
	t.Run("hub_name save echoing image_registry", func(t *testing.T) {
		srv, fake, ops := seededEndpointsServer(t)
		get := getServerConfigDB(t, srv, ops)
		require.Equal(t, "file.example", get.ImageRegistry)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"`+get.ImageRegistry+`","server":{"hub":{"hub_name":"new-hub"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		doc, _, _ := registryRow(t, fake, "endpoints")
		assert.NotContains(t, doc, "image_registry")
	})
	t.Run("first save with no row", func(t *testing.T) {
		srv, fake, ops := newTestDBServer(t)
		setTestImageRegistryFileTier(t, ops, "file.example", nil)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"file.example","server":{"hub":{"hub_name":"new-hub"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		doc, origin, _ := registryRow(t, fake, "endpoints")
		assert.Equal(t, "managed", origin)
		assert.NotContains(t, doc, "image_registry")
	})
}

func TestImageRegistryPersistence_EchoOnlySavesWriteNothing(t *testing.T) {
	t.Run("system registry with the file-tier value", func(t *testing.T) {
		srv, fake, _ := seededEndpointsServer(t)
		rr := putSystemRegistry(t, srv, "file.example")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		_, origin, rev := registryRow(t, fake, "endpoints")
		assert.Equal(t, "seeded", origin)
		assert.Equal(t, int64(1), rev)
	})
	t.Run("full GET body echo on a seeded endpoints row", func(t *testing.T) {
		srv, fake, ops := seededEndpointsServer(t)
		get := getServerConfigDB(t, srv, ops)
		body, err := json.Marshal(map[string]interface{}{
			"image_registry": get.ImageRegistry,
			"server":         map[string]interface{}{"hub": map[string]interface{}{"monitoring_dashboard_url": get.Server.Hub.MonitoringDashboardURL}},
		})
		require.NoError(t, err)
		rr := putHubNameServerConfigDB(t, srv, ops, string(body))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		_, origin, rev := registryRow(t, fake, "endpoints")
		assert.Equal(t, "seeded", origin, "an echo does not adopt the seeded row")
		assert.Equal(t, int64(1), rev)
	})
	t.Run("no row and nothing changed creates no row", func(t *testing.T) {
		srv, fake, ops := newTestDBServer(t)
		setTestImageRegistryFileTier(t, ops, "file.example", nil)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"file.example"}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Nil(t, storedRow(fake, "endpoints"))
		rr = putSystemRegistry(t, srv, "file.example")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Nil(t, storedRow(fake, "endpoints"))
	})
	t.Run("a stale expected revision is not a no-op", func(t *testing.T) {
		srv, _, ops := seededEndpointsServer(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"file.example","expected_revisions":{"endpoints":99}}`)
		assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	})
}

// GET's top-level value is this replica's file tier, never the seeded row,
// which under HA holds another replica's bootstrap; an echo of it is a
// no-op on this replica.
func TestImageRegistryPersistence_SeededRowFromAnotherReplica(t *testing.T) {
	srv, fake, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "this-replica.example", nil)
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"image_registry":"other-replica.example"}`), "seeded")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	get := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "this-replica.example", get.ImageRegistry)
	rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"`+get.ImageRegistry+`"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	_, origin, rev := registryRow(t, fake, "endpoints")
	assert.Equal(t, "seeded", origin)
	assert.Equal(t, int64(1), rev)
}

func TestImageRegistryPersistence_ChangePinAndClear(t *testing.T) {
	srv, fake, ops := seededEndpointsServer(t)

	// A real change through the system registry PUT.
	rr := putSystemRegistry(t, srv, "db.example")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, origin, _ := registryRow(t, fake, "endpoints")
	assert.Equal(t, "managed", origin)
	assert.Equal(t, "db.example", doc["image_registry"])
	assert.Equal(t, config.ImageRegistrySourceHubDB, srv.hubScopeImageRegistry().Source)

	// Through the admin PUT, to the file-tier value: a managed key compares
	// with its stored value, so this pins the file value.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"file.example"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _, _ = registryRow(t, fake, "endpoints")
	assert.Equal(t, "file.example", doc["image_registry"])
	setTestImageRegistryFileTier(t, ops, "later-file.example", nil)
	assert.Equal(t, config.ResolvedImageRegistry{Value: "file.example", Source: config.ImageRegistrySourceHubDB}, srv.hubScopeImageRegistry(),
		"a pinned value does not follow a later file change")
	assert.Equal(t, "file.example", getServerConfigDB(t, srv, ops).ImageRegistry, "GET shows the managed value")

	// "" clears it: the scope returns to the file tier.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"image_registry":""}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _, _ = registryRow(t, fake, "endpoints")
	assert.NotContains(t, doc, "image_registry")
	assert.Equal(t, config.ResolvedImageRegistry{Value: "later-file.example", Source: config.ImageRegistrySourceFile}, srv.hubScopeImageRegistry())
}

// The row flips between the save's read and its write: the write is a CAS
// on the read revision, so it is refused rather than decided on mixed state.
func TestImageRegistryPersistence_RowFlipConflicts(t *testing.T) {
	fake := newFakeHubSettingStore()
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"image_registry":"file.example"}`), "seeded")
	raceStore := &endpointsRaceStore{fakeHubSettingStore: fake}
	ops := NewOperationalSettings(raceStore, emptyKoanf(), emptyKoanf())
	setTestImageRegistryFileTier(t, ops, "file.example", nil)
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"new.example"}`)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
}

// The k8s case: a file profile value under a managed top. Unrelated
// profile saves keep k8s on the managed top and store no copy; typing a
// value stores it in one save.
func TestImageRegistryPersistence_ProfileSaves(t *testing.T) {
	ctx := context.Background()
	newSrv := func(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
		srv, fake, ops := newTestDBServer(t)
		setTestImageRegistryFileTier(t, ops, "", map[string]string{"k8s": "b.example"})
		fake.seedWithOrigin("profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_registry":"b.example"},"local":{"runtime":"docker"}}`), "seeded")
		_, err := ops.Refresh(ctx)
		require.NoError(t, err)
		_, err = ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"a.example"}`), "admin", -1, "managed")
		require.NoError(t, err)
		return srv, fake, ops
	}
	k8sRegistry := func(t *testing.T, fake *fakeHubSettingStore) (interface{}, bool) {
		doc, _, _ := registryRow(t, fake, "profiles")
		k8s, _ := doc["k8s"].(map[string]interface{})
		v, ok := k8s["image_registry"]
		return v, ok
	}

	t.Run("setup runtime save", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		require.NoError(t, srv.putRuntimeProfileToDB(ctx, ops, "k8s", "kubernetes-new", "admin"))
		_, origin, _ := registryRow(t, fake, "profiles")
		assert.Equal(t, "managed", origin)
		_, has := k8sRegistry(t, fake)
		assert.False(t, has, "no copied registry")
		assert.Equal(t, "a.example", srv.dispatchImageRegistryFor("", "k8s"), "k8s stays on the managed top")
		get := getServerConfigDB(t, srv, ops)
		assert.Equal(t, "", get.Profiles["k8s"].ImageRegistry, "GET shows managed profile values only")
	})
	t.Run("setup runtime save that changes nothing", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		require.NoError(t, srv.putRuntimeProfileToDB(ctx, ops, "k8s", "kubernetes", "admin"))
		_, origin, rev := registryRow(t, fake, "profiles")
		assert.Equal(t, "seeded", origin)
		assert.Equal(t, int64(1), rev)
	})
	t.Run("profiles echo with an edit", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		get := getServerConfigDB(t, srv, ops)
		require.Equal(t, "", get.Profiles["k8s"].ImageRegistry, "a seeded row's copies are not shown")
		local := get.Profiles["local"]
		local.Runtime = "podman"
		get.Profiles["local"] = local
		body, err := json.Marshal(map[string]interface{}{"profiles": get.Profiles})
		require.NoError(t, err)
		rr := putHubNameServerConfigDB(t, srv, ops, string(body))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		_, has := k8sRegistry(t, fake)
		assert.False(t, has)
		assert.Equal(t, "a.example", srv.dispatchImageRegistryFor("", "k8s"))
	})
	t.Run("profiles echo without an edit is a no-op", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		get := getServerConfigDB(t, srv, ops)
		body, err := json.Marshal(map[string]interface{}{"profiles": get.Profiles})
		require.NoError(t, err)
		rr := putHubNameServerConfigDB(t, srv, ops, string(body))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		_, origin, rev := registryRow(t, fake, "profiles")
		assert.Equal(t, "seeded", origin)
		assert.Equal(t, int64(1), rev)
	})
	t.Run("typing the file value pins it in one save", func(t *testing.T) {
		srv, _, ops := newSrv(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_registry":"b.example"},"local":{"runtime":"docker"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		r, _ := srv.dispatchImageRegistryResolved("", "k8s")
		assert.Equal(t, config.ResolvedImageRegistry{Value: "b.example", Source: config.ImageRegistrySourceHubDBProfile}, r)
		assert.Equal(t, "b.example", getServerConfigDB(t, srv, ops).Profiles["k8s"].ImageRegistry)
	})
	t.Run("a managed key sent empty clears it; a deleted profile drops it", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_registry":"c.example"},"gke":{"runtime":"kubernetes","image_registry":"g.example"},"local":{"runtime":"docker"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		rr = putHubNameServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes"},"local":{"runtime":"docker"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		doc, _, _ := registryRow(t, fake, "profiles")
		assert.NotContains(t, doc, "gke", "a deleted profile is gone with its registry")
		_, has := k8sRegistry(t, fake)
		assert.False(t, has, "an omitted field clears a managed key")
		assert.Equal(t, "a.example", srv.dispatchImageRegistryFor("", "k8s"))
	})
	t.Run("runtime save on a managed row keeps managed values", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_registry":"c.example"},"local":{"runtime":"docker"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.NoError(t, srv.putRuntimeProfileToDB(ctx, ops, "local", "podman", "admin"))
		v, _ := k8sRegistry(t, fake)
		assert.Equal(t, "c.example", v)
	})
}

// The hub's node env never reaches the shared row: GET does not show it,
// and an echo of the GET body stores nothing.
func TestImageRegistryPersistence_HubNodeEnvNotEchoed(t *testing.T) {
	srv, fake, ops := seededEndpointsServer(t)
	srv.config.NodeImageRegistry = "hub-env.example"

	get := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "file.example", get.ImageRegistry)
	rr := putHubNameServerConfigDB(t, srv, ops, `{"image_registry":"`+get.ImageRegistry+`","server":{"hub":{"hub_name":"new-hub"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _, _ := registryRow(t, fake, "endpoints")
	assert.NotContains(t, doc, "image_registry")
	assert.Equal(t, "file.example", srv.dispatchImageRegistryFor("", ""), "a remote dispatch does not use the hub's env")
	assert.Equal(t, "hub-env.example", srv.resolveImageRegistry(), "hub-scope reads do")
}

// --- Superseded keys ---

func TestComputeSupersededKeys_ImageRegistryInherited(t *testing.T) {
	ctx := context.Background()
	fake := newFakeHubSettingStore()
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"image_registry":              "file.example",
		"profiles.k8s.runtime":        "kubernetes",
		"profiles.k8s.image_registry": "b.example",
	})
	ops := NewOperationalSettings(fake, bootstrap, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	_, err := ops.Update(ctx, "endpoints", json.RawMessage(`{"hub_name":"new-hub"}`), "admin", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes"}}`), "admin", -1, "managed")
	require.NoError(t, err)

	got := srv.computeSupersededKeys(ops)
	for _, k := range got["endpoints"] {
		assert.NotEqual(t, "image_registry", k.Key, "a managed row without the key inherits the file value")
	}
	assert.Empty(t, got["profiles"], "a managed profile without image_registry inherits the file value")

	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_registry":"c.example"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.NotEmpty(t, srv.computeSupersededKeys(ops)["profiles"], "a managed profile value supersedes the file one")
}

// --- Setup status and onboarding ---

func TestOnboardingStatus_ImageRegistrySaveableIsNeverNodeEnv(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	setTestImageRegistryFileTier(t, ops, "a.example", nil)
	srv.config.NodeImageRegistry = "e.example"

	status := srv.computeOnboardingStatus(context.Background())
	assert.Equal(t, "e.example", status.ImageRegistry)
	assert.Equal(t, "node_env", status.ImageRegistrySource)
	assert.Equal(t, "a.example", status.ImageRegistrySaveable)

	// Saving the pre-filled value stores nothing; remote dispatch stays on
	// the file tier.
	rr := putSystemRegistry(t, srv, status.ImageRegistrySaveable)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Nil(t, storedRow(fake, "endpoints"))
	assert.Equal(t, "a.example", srv.dispatchImageRegistryFor("", ""))

	// An admin may deliberately save the hub's env value.
	rr = putSystemRegistry(t, srv, "e.example")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, origin, _ := registryRow(t, fake, "endpoints")
	assert.Equal(t, "managed", origin)
	assert.Equal(t, "e.example", doc["image_registry"])
}

// Without operational settings, imageRegistryFor reads the settings file's
// top-level value.
func TestImageRegistry_WithoutOperationalSettingsReadsFile(t *testing.T) {
	tempSettingsHome(t)
	writeTestGlobalSettings(t, "schema_version: \"1\"\nimage_registry: file.example\nactive_profile: p\nprofiles:\n  p:\n    image_registry: profile.example\n")
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	assert.Equal(t, "file.example", srv.resolveImageRegistry())
	assert.Equal(t, "file.example", srv.dispatchImageRegistryFor("", "p"), "no profile layers without operational settings")
}
