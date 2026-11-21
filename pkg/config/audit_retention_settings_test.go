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

func TestLoadGlobalConfig_AuditRetentionDaysEnvVar(t *testing.T) {
	envVar := schemaEnvVar(t, "audit_retention_days")

	t.Run("without settings.yaml (legacy env-only path)", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(envVar, "45")

		cfg, err := LoadGlobalConfig("")
		require.NoError(t, err)
		require.NotNil(t, cfg.Hub.AuditRetentionDays, "%s should set Hub.AuditRetentionDays", envVar)
		assert.Equal(t, 45, *cfg.Hub.AuditRetentionDays)
	})

	t.Run("with settings.yaml server key (applyEnvOverrides path)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		configPath := filepath.Join(tmpDir, "settings.yaml")
		configContent := `
schema_version: "1"
server:
  hub:
    port: 9999
`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))
		t.Setenv(envVar, "0")

		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		require.NotNil(t, cfg.Hub.AuditRetentionDays, "%s=0 should set Hub.AuditRetentionDays to explicit 0", envVar)
		assert.Equal(t, 0, *cfg.Hub.AuditRetentionDays)
		assert.Equal(t, 9999, cfg.Hub.Port)
	})
}

func TestAuditRetentionDays_RoundTrip(t *testing.T) {
	t.Run("unset stays nil", func(t *testing.T) {
		v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{}}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.Nil(t, gc.Hub.AuditRetentionDays)

		v1Out := ConvertGlobalToV1ServerConfig(gc)
		assert.Nil(t, v1Out.Hub.AuditRetentionDays)
	})

	t.Run("explicit zero round-trips", func(t *testing.T) {
		zero := 0
		v1 := &V1ServerConfig{
			Hub: &V1ServerHubConfig{
				AuditRetentionDays: &zero,
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		require.NotNil(t, gc.Hub.AuditRetentionDays, "explicit 0 must not become nil")
		assert.Equal(t, 0, *gc.Hub.AuditRetentionDays)

		v1Out := ConvertGlobalToV1ServerConfig(gc)
		require.NotNil(t, v1Out.Hub.AuditRetentionDays)
		assert.Equal(t, 0, *v1Out.Hub.AuditRetentionDays)
	})

	t.Run("positive value round-trips", func(t *testing.T) {
		days := 30
		v1 := &V1ServerConfig{
			Hub: &V1ServerHubConfig{
				AuditRetentionDays: &days,
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		require.NotNil(t, gc.Hub.AuditRetentionDays)
		assert.Equal(t, 30, *gc.Hub.AuditRetentionDays)

		v1Out := ConvertGlobalToV1ServerConfig(gc)
		require.NotNil(t, v1Out.Hub.AuditRetentionDays)
		assert.Equal(t, 30, *v1Out.Hub.AuditRetentionDays)
	})
}
