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

// server.hub.membership_sweep_report_only: default off (the sweep enforces),
// reachable from settings.yaml and from its schema-declared env var, and
// round-trips through V1<->Global.

func TestMembershipSweepReportOnlySetting_DefaultOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.False(t, cfg.Hub.MembershipSweepReportOnly)
	assert.False(t, ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{}}).Hub.MembershipSweepReportOnly)
	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.MembershipSweepReportOnly, "false must not round-trip as an explicit *bool")
}

func TestMembershipSweepReportOnlySetting_EnvVar(t *testing.T) {
	envVar := schemaEnvVar(t, "membership_sweep_report_only")
	assert.Equal(t, "SCION_SERVER_HUB_MEMBERSHIPSWEEPREPORTONLY", envVar)

	t.Run("env only", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(envVar, "true")
		cfg, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.True(t, cfg.Hub.MembershipSweepReportOnly)
	})

	t.Run("env over settings.yaml", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		path := filepath.Join(tmp, "settings.yaml")
		require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    membership_sweep_report_only: false\n"), 0644))
		t.Setenv(envVar, "true")
		cfg, err := LoadGlobalConfig(path)
		require.NoError(t, err)
		assert.True(t, cfg.Hub.MembershipSweepReportOnly)
	})
}

func TestMembershipSweepReportOnlySetting_SettingsYAMLAndRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	path := filepath.Join(tmp, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    membership_sweep_report_only: true\n"), 0644))
	cfg, err := LoadGlobalConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Hub.MembershipSweepReportOnly)

	v1 := ConvertGlobalToV1ServerConfig(cfg)
	require.NotNil(t, v1.Hub.MembershipSweepReportOnly)
	assert.True(t, *v1.Hub.MembershipSweepReportOnly)
	assert.True(t, ConvertV1ServerToGlobalConfig(v1).Hub.MembershipSweepReportOnly)
}
