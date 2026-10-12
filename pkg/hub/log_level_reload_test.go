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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file-mode reload applies server.log_level to the shared level, and a
// reload after the key is removed reverts to info.
func TestReloadSettings_LogLevel(t *testing.T) {
	isolateLogLevelState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath,
		[]byte("schema_version: \"1\"\nserver:\n  log_level: debug\n"), 0o644))

	srv := newStartupNamedServer(t, "boot-hub")
	results := srv.reloadSettings()
	assert.Contains(t, results["applied"], "log_level")
	assert.Equal(t, slog.LevelDebug, logging.EffectiveLevel(""))
	assert.Equal(t, slog.LevelInfo, stdLogBridgeLevel(), "the std-log bridge level must not change")

	require.NoError(t, os.WriteFile(settingsPath,
		[]byte("schema_version: \"1\"\nserver:\n  hub:\n    hub_name: boot-hub\n"), 0o644))
	srv.reloadSettings()
	assert.Equal(t, slog.LevelInfo, logging.EffectiveLevel(""))
}

// A workstation server-config save that sets server.log_level changes the
// shared level, and a save that clears it reverts to info.
func TestWorkstation_PutServerConfig_LogLevelSetAndClear(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(settingsPath,
		[]byte("schema_version: \"1\"\nserver:\n  log_level: info\n"), 0o644))
	srv, _, _ := newSQLiteHubInMode(t, true, nil)

	rr := putServerConfig(t, srv, `{"server":{"log_level":"debug"}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, slog.LevelDebug, logging.EffectiveLevel(""))

	rr = putServerConfig(t, srv, `{"server":{"log_level":""}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, slog.LevelInfo, logging.EffectiveLevel(""), "clearing server.log_level should revert to info")
	assert.Equal(t, slog.LevelInfo, stdLogBridgeLevel(), "the std-log bridge level must not change")
}
