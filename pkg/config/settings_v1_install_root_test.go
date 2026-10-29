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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestMaintenanceInstallRoot_SaveRoundTrip checks that saving settings keeps
// server.maintenance.install_root (written by deploy.sh).
func TestMaintenanceInstallRoot_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	vs := &VersionedSettings{
		SchemaVersion: "1",
		Server: &V1ServerConfig{Maintenance: &V1MaintenanceConfig{
			DeploymentTier: "binary",
			InstallRoot:    "/srv/scion-install",
		}},
	}
	require.NoError(t, SaveVersionedSettings(dir, vs))

	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "install_root: /srv/scion-install")

	var loaded VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &loaded))
	require.NotNil(t, loaded.Server)
	require.NotNil(t, loaded.Server.Maintenance)
	assert.Equal(t, "/srv/scion-install", loaded.Server.Maintenance.InstallRoot)

	// Saving the loaded settings again keeps the key.
	dir2 := t.TempDir()
	require.NoError(t, SaveVersionedSettings(dir2, &loaded))
	data2, err := os.ReadFile(filepath.Join(dir2, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data2), "install_root: /srv/scion-install")
}

func TestMaintenanceInstallRoot_Resolution(t *testing.T) {
	t.Setenv(EnvMaintenanceInstallRoot, "")
	assert.Equal(t, DefaultMaintenanceInstallRoot, MaintenanceInstallRoot(nil))
	assert.Equal(t, DefaultMaintenanceInstallRoot, MaintenanceInstallRoot(&V1MaintenanceConfig{}))
	assert.Equal(t, "/srv/a", MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "/srv/a"}))

	t.Setenv(EnvMaintenanceInstallRoot, "/srv/env")
	assert.Equal(t, "/srv/env", MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "/srv/a"}))
	assert.Equal(t, "/srv/env", MaintenanceInstallRoot(nil))

	// Relative values are ignored and the next source applies.
	t.Setenv(EnvMaintenanceInstallRoot, "opt/scion")
	assert.Equal(t, "/srv/a", MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "/srv/a"}))
	assert.Equal(t, DefaultMaintenanceInstallRoot, MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "./scion"}))
	t.Setenv(EnvMaintenanceInstallRoot, "")
	assert.Equal(t, DefaultMaintenanceInstallRoot, MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "relative"}))
	assert.Equal(t, "/srv/b", MaintenanceInstallRoot(&V1MaintenanceConfig{InstallRoot: "/srv/b/"}))
}
