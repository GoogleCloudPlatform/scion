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

func brokerNameTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	return globalDir
}

func writeBrokerNameSettings(t *testing.T, globalDir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(content), 0644))
}

func TestConfiguredBrokerName(t *testing.T) {
	globalDir := brokerNameTestHome(t)
	host, err := os.Hostname()
	require.NoError(t, err)

	assert.Empty(t, ConfiguredBrokerName(), "nothing configured")
	assert.Equal(t, host, LocalBrokerName("fallback"), "hostname by default")

	writeBrokerNameSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    broker_name: named\n")
	assert.Equal(t, "named", ConfiguredBrokerName())

	writeBrokerNameSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    broker_name: named\n    broker_nickname: nick\n")
	assert.Equal(t, "nick", ConfiguredBrokerName(), "broker_nickname wins, as in 'server start'")
	assert.Equal(t, "nick", LocalBrokerName("fallback"))
}

// TestBrokerNameSettingKey_RoundTrip: the key register writes is the one
// ConfiguredBrokerName reads, for versioned and legacy settings files.
func TestBrokerNameSettingKey_RoundTrip(t *testing.T) {
	t.Run("versioned", func(t *testing.T) {
		globalDir := brokerNameTestHome(t)
		writeBrokerNameSettings(t, globalDir, "schema_version: \"1\"\n")
		require.NoError(t, UpdateSetting(globalDir, BrokerNameSettingKey, "rig-1", true))
		assert.Equal(t, "rig-1", ConfiguredBrokerName())
		data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "broker_nickname: rig-1")
	})
	t.Run("no settings file", func(t *testing.T) {
		globalDir := brokerNameTestHome(t)
		require.NoError(t, UpdateSetting(globalDir, BrokerNameSettingKey, "rig-2", true))
		assert.Equal(t, "rig-2", ConfiguredBrokerName())
	})
}
