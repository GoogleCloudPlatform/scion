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
)

// TestBrokerStateDir_V1RoundTrip: server.broker.state_dir reaches
// GlobalConfig.RuntimeBroker.StateDir and converts back (ptone/scion#4668 C2).
func TestBrokerStateDir_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Broker: &V1BrokerConfig{StateDir: "/var/lib/scion/broker-state"}})
	assert.Equal(t, "/var/lib/scion/broker-state", gc.RuntimeBroker.StateDir, "v1 state_dir did not reach GlobalConfig")

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Broker)
	assert.Equal(t, "/var/lib/scion/broker-state", v1.Broker.StateDir, "GlobalConfig StateDir did not convert back to v1")

	assert.Empty(t, ConvertV1ServerToGlobalConfig(&V1ServerConfig{Broker: &V1BrokerConfig{}}).RuntimeBroker.StateDir, "unset stays empty (the broker default)")
	def := DefaultGlobalConfig()
	assert.Empty(t, def.RuntimeBroker.StateDir, "no default value")
}

// TestBrokerStateDir_Load: the key loads from settings.yaml, from a legacy
// server.yaml (runtimeBroker.stateDir) and from its environment variable
// SCION_SERVER_RUNTIMEBROKER_STATEDIR, which overrides the file.
func TestBrokerStateDir_Load(t *testing.T) {
	t.Run("settings.yaml", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("version: 1\nserver:\n  broker:\n    state_dir: /srv/broker-state\n"), 0o644))
		gc, found := loadGlobalConfigFromSettings(dir)
		require.True(t, found)
		assert.Equal(t, "/srv/broker-state", gc.RuntimeBroker.StateDir, "settings.yaml state_dir not loaded")
	})
	t.Run("settings.yaml with env override", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("SCION_SERVER_RUNTIMEBROKER_STATEDIR", "/env/broker-state")
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("version: 1\nserver:\n  broker:\n    state_dir: /srv/broker-state\n"), 0o644))
		gc, found := loadGlobalConfigFromSettings(dir)
		require.True(t, found)
		assert.Equal(t, "/env/broker-state", gc.RuntimeBroker.StateDir)
	})
	t.Run("legacy server.yaml", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, "server.yaml")
		require.NoError(t, os.WriteFile(path, []byte("runtimeBroker:\n  stateDir: /legacy/broker-state\n"), 0o644))
		gc, err := LoadGlobalConfig(path)
		require.NoError(t, err)
		assert.Equal(t, "/legacy/broker-state", gc.RuntimeBroker.StateDir)
	})
	t.Run("legacy server.yaml with env override", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SCION_SERVER_RUNTIMEBROKER_STATEDIR", "/env/broker-state")
		path := filepath.Join(home, "server.yaml")
		require.NoError(t, os.WriteFile(path, []byte("runtimeBroker:\n  stateDir: /legacy/broker-state\n"), 0o644))
		gc, err := LoadGlobalConfig(path)
		require.NoError(t, err)
		assert.Equal(t, "/env/broker-state", gc.RuntimeBroker.StateDir, "legacy env SCION_SERVER_RUNTIMEBROKER_STATEDIR not applied")
	})
}
