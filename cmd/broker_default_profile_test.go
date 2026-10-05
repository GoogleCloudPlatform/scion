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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Join reports the broker's active profile, including an empty one; with
// no settings it reports nothing, so the hub keeps what it has.
func TestBrokerRegistrationDefaultProfile(t *testing.T) {
	assert.Nil(t, brokerRegistrationDefaultProfile(nil))

	got := brokerRegistrationDefaultProfile(&config.Settings{ActiveProfile: "k8s"})
	require.NotNil(t, got)
	assert.Equal(t, "k8s", *got)

	got = brokerRegistrationDefaultProfile(&config.Settings{})
	require.NotNil(t, got)
	assert.Equal(t, "", *got)
}

// The running broker's heartbeat omits the default profile when its
// settings failed to load, instead of clearing the hub's value.
func TestBrokerHeartbeatDefaultProfile(t *testing.T) {
	assert.Nil(t, brokerHeartbeatDefaultProfile(&config.Settings{ActiveProfile: "k8s"}, false))

	got := brokerHeartbeatDefaultProfile(&config.Settings{ActiveProfile: "k8s"}, true)
	require.NotNil(t, got)
	assert.Equal(t, "k8s", *got)

	got = brokerHeartbeatDefaultProfile(&config.Settings{}, true)
	require.NotNil(t, got)
	assert.Equal(t, "", *got)
}

// The server's settings load decides the default profile its broker
// heartbeat reports: the active profile when settings load, nil when they
// fail (so the hub keeps its value instead of having it cleared).
func TestLoadServerSettings_DefaultProfile(t *testing.T) {
	setup := func(t *testing.T, settingsYAML string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(settingsYAML), 0o644))
		t.Chdir(home)
	}

	t.Run("loaded", func(t *testing.T) {
		setup(t, "schema_version: \"1\"\nactive_profile: k8s\nprofiles:\n  k8s:\n    runtime: kubernetes\n")
		settings, dp := loadServerSettings("")
		require.NotNil(t, settings)
		require.NotNil(t, dp)
		assert.Equal(t, "k8s", *dp)
	})
	t.Run("failed to load", func(t *testing.T) {
		setup(t, "schema_version: [this is not: valid yaml\n")
		settings, dp := loadServerSettings("")
		require.NotNil(t, settings, "falls back to empty settings")
		assert.Nil(t, dp)
	})
}
