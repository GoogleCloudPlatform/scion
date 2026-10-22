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

package cmd

import (
	"context"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newOverlayTestHub returns a hub whose operational settings carry one
// stored profile ("cluster") and its runtime.
func newOverlayTestHub(t *testing.T) *hub.Server {
	t.Helper()
	s := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, s)
	require.NoError(t, err)

	bootstrap := koanf.New(".")
	require.NoError(t, bootstrap.Load(confmap.Provider(map[string]interface{}{
		"runtimes": map[string]interface{}{"k8s": map[string]interface{}{"type": "kubernetes"}},
		"profiles": map[string]interface{}{"cluster": map[string]interface{}{"runtime": "k8s"}},
	}, "."), nil))
	ops := hub.NewOperationalSettings(s.(store.HubSettingStore), bootstrap, koanf.New("."))
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

func resetGlobalSettingsOverlay(t *testing.T) {
	t.Helper()
	prev := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
}

// Every DB-backed hub installs the overlay for its co-located broker, seeded
// with the stored profiles: SQLite (ptone/scion#3906) as well as Postgres,
// whose behaviour is unchanged.
func TestInstallColocatedSettingsOverlay_EveryDBDriver(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			resetGlobalSettingsOverlay(t)
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = driver

			require.True(t, installColocatedSettingsOverlay(cfg, newOverlayTestHub(t)))

			overlay := config.GetGlobalSettingsOverlay()
			require.NotNil(t, overlay, "overlay not installed for %s", driver)
			profiles := overlay.Profiles()
			require.Contains(t, profiles, "cluster")
			require.Equal(t, "k8s", profiles["cluster"].Runtime)
		})
	}
}

// Without a hub, or with an unknown driver, no overlay is installed.
func TestInstallColocatedSettingsOverlay_NotInstalled(t *testing.T) {
	t.Run("no hub", func(t *testing.T) {
		resetGlobalSettingsOverlay(t)
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "sqlite"
		require.False(t, installColocatedSettingsOverlay(cfg, nil))
		require.Nil(t, config.GetGlobalSettingsOverlay())
	})
	t.Run("unknown driver", func(t *testing.T) {
		resetGlobalSettingsOverlay(t)
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "mysql"
		require.False(t, installColocatedSettingsOverlay(cfg, newOverlayTestHub(t)))
		require.Nil(t, config.GetGlobalSettingsOverlay())
	})
}

// newStoredCatalogTestHub returns a hub with no catalog in its bootstrap
// material and the given profiles and runtimes rows stored with origin.
func newStoredCatalogTestHub(t *testing.T, profiles, runtimes, origin string) *hub.Server {
	t.Helper()
	s := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, s)
	require.NoError(t, err)
	hs := s.(store.HubSettingStore)
	ctx := context.Background()
	_, err = hs.UpsertHubSetting(ctx, "profiles", []byte(profiles), "seed", -1, origin)
	require.NoError(t, err)
	_, err = hs.UpsertHubSetting(ctx, "runtimes", []byte(runtimes), "seed", -1, origin)
	require.NoError(t, err)
	ops := hub.NewOperationalSettings(hs, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

// overlayFileFixture is a settings file whose active_profile "local" runs
// on docker.
func overlayFileFixture() *config.VersionedSettings {
	return &config.VersionedSettings{
		ActiveProfile: "local",
		Runtimes:      map[string]config.V1RuntimeConfig{"docker": {Type: "docker"}},
		Profiles:      map[string]config.V1ProfileConfig{"local": {Runtime: "docker"}},
	}
}

// Seeded empty profiles and runtimes rows (as startup wrote them before
// ptone/scion#5007) leave the settings file's catalog in place: the
// default profile resolves to the file's default runtime on every DB
// driver. A managed non-empty catalog still replaces the file's.
func TestInstallColocatedSettingsOverlay_StoredCatalogRows(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver+"/seeded empty rows", func(t *testing.T) {
			resetGlobalSettingsOverlay(t)
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = driver
			require.True(t, installColocatedSettingsOverlay(cfg, newStoredCatalogTestHub(t, `{}`, `{}`, "seeded")))

			vs := overlayFileFixture()
			config.GetGlobalSettingsOverlay().Apply(vs)
			_, rtType, err := vs.ResolveRuntime("")
			require.NoError(t, err)
			require.Equal(t, "docker", rtType)
			require.Equal(t, "local", vs.ActiveProfile)
		})
		t.Run(driver+"/managed catalog", func(t *testing.T) {
			resetGlobalSettingsOverlay(t)
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = driver
			require.True(t, installColocatedSettingsOverlay(cfg, newStoredCatalogTestHub(t,
				`{"local-docker":{"runtime":"stored-docker"}}`,
				`{"stored-docker":{"type":"docker"}}`, "managed")))

			vs := overlayFileFixture()
			config.GetGlobalSettingsOverlay().Apply(vs)
			require.NotContains(t, vs.Profiles, "local")
			require.NotContains(t, vs.Runtimes, "docker")
			// The single stored profile is the default (unchanged rule).
			_, rtType, err := vs.ResolveRuntime("")
			require.NoError(t, err)
			require.Equal(t, "docker", rtType)
			require.Equal(t, "local-docker", vs.ActiveProfile)
		})
	}
}

// Built-in profile retention (ptone/scion#5113) is enabled only for a
// legacy co-located broker: without one (a flat host, or a failed
// registration) the installed overlay behaves as before; with one, it is
// enabled.
func TestEnableColocatedProfileReferences(t *testing.T) {
	t.Run("no legacy co-located broker", func(t *testing.T) {
		resetGlobalSettingsOverlay(t)
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "sqlite"
		srv := newOverlayTestHub(t)
		require.True(t, installColocatedSettingsOverlay(cfg, srv))
		enableColocatedProfileReferences(context.Background(), srv)
		require.False(t, config.GetGlobalSettingsOverlay().RetentionEnabled())
	})
	t.Run("legacy co-located broker", func(t *testing.T) {
		resetGlobalSettingsOverlay(t)
		t.Setenv("HOME", t.TempDir())
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "sqlite"
		srv := newOverlayTestHub(t)
		srv.SetEmbeddedBrokerID("legacy-broker")
		require.True(t, installColocatedSettingsOverlay(cfg, srv))
		enableColocatedProfileReferences(context.Background(), srv)
		require.True(t, config.GetGlobalSettingsOverlay().RetentionEnabled())
	})
	t.Run("no overlay", func(t *testing.T) {
		resetGlobalSettingsOverlay(t)
		srv := newOverlayTestHub(t)
		srv.SetEmbeddedBrokerID("legacy-broker")
		enableColocatedProfileReferences(context.Background(), srv)
		require.Nil(t, config.GetGlobalSettingsOverlay())
	})
}
