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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStartupNamedServer builds a Server through New with hubName as the
// startup-resolved ServerConfig.HubName, as server start does from
// LoadGlobalConfig(serverConfigPath) (which honours --config).
func newStartupNamedServer(t *testing.T, hubName string) *Server {
	t.Helper()
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	cfg := DefaultServerConfig()
	cfg.HubName = hubName
	srv, err := New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

// Round-4 finding 2: an unset configured hub_name resets the running name
// to the name resolved at startup, not to the hostname, so a hub_name set
// only through server start --config survives the first ApplySnapshot and
// returns after a clear. (The GCP secret label follows the same resolved
// name in ApplySnapshot.)
func TestApplySnapshot_UnsetHubNameReturnsToStartupName(t *testing.T) {
	srv := newStartupNamedServer(t, "cfg-hub")

	ApplySnapshot(srv, Layer1Snapshot{})
	assert.Equal(t, "cfg-hub", srv.HubName(), "first apply with no configured hub_name")

	ApplySnapshot(srv, Layer1Snapshot{HubName: "db-hub"})
	assert.Equal(t, "db-hub", srv.HubName())

	ApplySnapshot(srv, Layer1Snapshot{})
	assert.Equal(t, "cfg-hub", srv.HubName(), "after a clear")

	// A file-mode snapshot from a settings.yaml without hub_name (reload
	// reads the global dir, not --config) keeps the startup name too.
	ApplySnapshot(srv, BuildLayer1SnapshotFromFile(&config.GlobalConfig{}))
	assert.Equal(t, "cfg-hub", srv.HubName())
}

// A Server not built by New has no startup name and falls back to the
// startup default.
func TestApplySnapshot_UnsetHubNameWithoutStartupNameUsesDefault(t *testing.T) {
	srv := &Server{}
	ApplySnapshot(srv, Layer1Snapshot{})
	assert.Equal(t, config.ResolveHubNameOrDefault(""), srv.HubName())
}

// Round-4 finding 1: BuildLayer1SnapshotFromFile carries the configured
// hub_name; without it every file-mode reload would reset a configured hub
// to its startup name.
func TestBuildLayer1SnapshotFromFile_CarriesHubName(t *testing.T) {
	gc := &config.GlobalConfig{}
	gc.Hub.HubName = "file-hub"
	snap := BuildLayer1SnapshotFromFile(gc)
	assert.Equal(t, "file-hub", snap.HubName)

	srv := newStartupNamedServer(t, "boot-hub")
	ApplySnapshot(srv, snap)
	assert.Equal(t, "file-hub", srv.HubName())
}

// Round-4 finding 1: a file-mode reload driven by settings.yaml
// server.hub.hub_name applies that name.
func TestReloadSettings_FileHubName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  hub:\n    hub_name: yaml-hub\n"), 0o644))

	srv := newStartupNamedServer(t, "boot-hub")
	srv.reloadSettings()
	assert.Equal(t, "yaml-hub", srv.HubName())
}
