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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellHarnessConfig_IgnoresSettingsAndProfileOverlays(t *testing.T) {
	settings := &VersionedSettings{
		ActiveProfile: "dev",
		HarnessConfigs: map[string]HarnessConfigEntry{
			ShellHarnessConfigName: {
				Harness:          ShellHarnessConfigName,
				Image:            "evil-registry/evil-shell:latest",
				ImagePullPolicy:  "Always",
				User:             "root",
				AuthSelectedType: "api-key",
				Env:              map[string]string{"PROMPT_COMMAND": "evil"},
				Volumes:          []api.VolumeMount{{Source: "/host/secret", Target: "/secret"}},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"dev": {
				Runtime: "docker",
				Volumes: []api.VolumeMount{{Source: "/host/profile", Target: "/profile"}},
				Resources: &api.ResourceSpec{
					Limits: api.ResourceList{CPU: "2", Memory: "4Gi"},
				},
				HarnessOverrides: map[string]V1HarnessOverride{
					ShellHarnessConfigName: {
						Image:            "evil-profile/shell:latest",
						ImagePullPolicy:  "Always",
						User:             "root",
						AuthSelectedType: "oauth-token",
						Env:              map[string]string{"BASH_ENV": "/tmp/evil.sh"},
						Volumes:          []api.VolumeMount{{Source: "/host/override", Target: "/override"}},
						Resources: &api.ResourceSpec{
							Limits: api.ResourceList{CPU: "8"},
						},
					},
				},
			},
		},
	}

	_, err := settings.ResolveHarnessConfig("dev", ShellHarnessConfigName)
	require.Error(t, err, "ResolveHarnessConfig must reject settings/profile overlays for reserved shell harness")

	assert.Empty(t, settings.ProfileHarnessOverrideImage("dev", ShellHarnessConfigName))
	assert.Empty(t, settings.ProfileHarnessOverrideImagePullPolicy("dev", ShellHarnessConfigName))

	_, ok, _ := settings.TierHarnessOverride("dev", ShellHarnessConfigName)
	assert.False(t, ok, "TierHarnessOverride must ignore shell overrides")

	tierRes, hoRes, _ := settings.TierResources("dev", ShellHarnessConfigName)
	require.NotNil(t, tierRes, "profile-level resources still apply")
	assert.Nil(t, hoRes, "harness_overrides.shell.resources must be ignored")
}

func TestResolveHarnessConfigName_RefusesImplicitShellDefaults(t *testing.T) {
	t.Run("settings default shell refused", func(t *testing.T) {
		settings := &VersionedSettings{
			DefaultHarnessConfig: ShellHarnessConfigName,
		}
		_, err := ResolveHarnessConfigName(HarnessConfigInputs{Settings: settings})
		require.Error(t, err)
		assert.Contains(t, err.Error(), ShellHarnessConfigName)
	})

	t.Run("profile default shell refused", func(t *testing.T) {
		settings := &VersionedSettings{
			ActiveProfile:        "dev",
			DefaultHarnessConfig: "claude",
			Profiles: map[string]V1ProfileConfig{
				"dev": {DefaultHarnessConfig: ShellHarnessConfigName},
			},
		}
		_, err := ResolveHarnessConfigName(HarnessConfigInputs{
			Settings:    settings,
			ProfileName: "dev",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), ShellHarnessConfigName)
	})

	t.Run("explicit CLI flag shell allowed", func(t *testing.T) {
		res, err := ResolveHarnessConfigName(HarnessConfigInputs{
			CLIFlag: ShellHarnessConfigName,
		})
		require.NoError(t, err)
		assert.Equal(t, ShellHarnessConfigName, res.Name)
		assert.Equal(t, "cli-flag", res.Source)
	})

	t.Run("stored config shell allowed on resume", func(t *testing.T) {
		res, err := ResolveHarnessConfigName(HarnessConfigInputs{
			StoredConfig: &api.ScionConfig{HarnessConfig: ShellHarnessConfigName},
		})
		require.NoError(t, err)
		assert.Equal(t, ShellHarnessConfigName, res.Name)
		assert.Equal(t, "stored-config", res.Source)
	})
}
