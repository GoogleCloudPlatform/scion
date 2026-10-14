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
