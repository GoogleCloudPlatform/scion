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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	yamlparser "github.com/knadh/koanf/parsers/yaml"
)

// --- ResolveHarnessConfig tests ---

func TestResolveHarnessConfig_Default(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:latest", hc.Image)
	assert.Equal(t, "scion", hc.User)
	assert.Equal(t, "gemini", hc.Harness)
}

func TestResolveHarnessConfig_Named(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
			},
			"gemini-high-security": {
				Harness: "gemini",
				Image:   "example.com/gemini:hardened",
				User:    "restricted",
				Model:   "gemini-2.5-pro",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	hc, err := vs.ResolveHarnessConfig("local", "gemini-high-security")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:hardened", hc.Image)
	assert.Equal(t, "restricted", hc.User)
	assert.Equal(t, "gemini", hc.Harness)
	assert.Equal(t, "gemini-2.5-pro", hc.Model)
}

func TestResolveHarnessConfig_WithProfileOverrides(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "staging",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
				Env:     map[string]string{"BASE_KEY": "base_value"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				Runtime: "docker",
				Volumes: []api.VolumeMount{{Source: "/profile/vol", Target: "/mnt/vol"}},
				HarnessOverrides: map[string]V1HarnessOverride{
					"gemini": {
						Image: "example.com/gemini:staging",
						Env:   map[string]string{"OVERRIDE_KEY": "override_value"},
					},
				},
			},
		},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:staging", hc.Image, "image should be overridden by profile")
	assert.Equal(t, "scion", hc.User, "user should remain from base config")
	assert.Equal(t, "base_value", hc.Env["BASE_KEY"], "base env should be preserved")
	// harness_overrides env SURVIVES the profile env removal — unchanged.
	assert.Equal(t, "override_value", hc.Env["OVERRIDE_KEY"], "override env should be merged")
	assert.Len(t, hc.Volumes, 1, "profile volume should be appended")
	assert.Equal(t, "/mnt/vol", hc.Volumes[0].Target)
}

// TestResolveHarnessConfig_ProfileEnvFieldRemoved verifies that the Env field
// no longer exists on V1ProfileConfig. profiles.<p>.env was removed from the
// struct and the JSON schema as the final step of the G3-full removal — config
// files that still declare it will now fail schema validation with an
// additionalProperties error.
//
// This test remains as a regression guard: profile volumes and
// harness_overrides must still be merged correctly despite the removal.
func TestResolveHarnessConfig_ProfileEnvFieldRemoved(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "dev",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				Env: map[string]string{
					"SHARED_KEY": "from-harness-config",
				},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"dev": {
				Runtime: "docker",
				HarnessOverrides: map[string]V1HarnessOverride{
					"gemini": {Env: map[string]string{"OVERLAP_KEY": "from-override"}},
				},
				Volumes: []api.VolumeMount{{Source: "/profile/vol", Target: "/mnt/profile"}},
			},
		},
	}

	hc, err := vs.ResolveHarnessConfig("dev", "gemini")
	require.NoError(t, err)

	// Existence control: profile volumes must still be merged.
	require.Len(t, hc.Volumes, 1, "existence control: the profile must have been found and merged")
	require.Equal(t, "/mnt/profile", hc.Volumes[0].Target, "existence control: the merged volume must be the profile's")

	// The two surviving env sources still work.
	assert.Equal(t, "from-override", hc.Env["OVERLAP_KEY"],
		"profiles.<p>.harness_overrides.<hc>.env must survive the profile env removal")
	assert.Equal(t, "from-harness-config", hc.Env["SHARED_KEY"],
		"harness_configs.<hc>.env must survive — it is the migration path for the removed profile env")
}

// TestResolveHarnessConfig_ImagePullPolicy pins ptone/scion#2156: a Hub
// settings harness_configs.<h>.image_pull_policy value resolves the same way
// .image already does, including the profile-level harness_overrides rank.
func TestResolveHarnessConfig_ImagePullPolicy(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "staging",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"claude": {
				Harness:         "claude",
				Image:           "example.com/claude:latest",
				ImagePullPolicy: "IfNotPresent",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				Runtime: "docker",
				HarnessOverrides: map[string]V1HarnessOverride{
					"claude": {ImagePullPolicy: "Always"},
				},
			},
			"local": {Runtime: "docker"},
		},
	}

	hc, err := vs.ResolveHarnessConfig("local", "claude")
	require.NoError(t, err)
	assert.Equal(t, "IfNotPresent", hc.ImagePullPolicy, "base harness-config value with no profile override")

	hc, err = vs.ResolveHarnessConfig("staging", "claude")
	require.NoError(t, err)
	assert.Equal(t, "Always", hc.ImagePullPolicy, "profile harness_overrides must outrank the base harness-config value")
}

func TestResolveHarnessConfig_NotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {Harness: "gemini", Image: "test"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	// When the config name is not found, an empty base should be returned (not an error)
	hc, err := vs.ResolveHarnessConfig("", "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, hc.Image, "empty base should have no image")
	assert.Empty(t, hc.User, "empty base should have no user")
	assert.Empty(t, hc.Harness, "empty base should have no harness")
}

func TestResolveHarnessConfig_NotFoundWithProfileOverrides(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "staging",
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				HarnessOverrides: map[string]V1HarnessOverride{
					"custom": {
						Image: "custom-image:latest",
						Env:   map[string]string{"KEY": "val"},
					},
				},
			},
		},
	}

	// Even when base config not found, profile overrides should apply
	hc, err := vs.ResolveHarnessConfig("", "custom")
	require.NoError(t, err)
	assert.Equal(t, "custom-image:latest", hc.Image)
	assert.Equal(t, "val", hc.Env["KEY"])
}

func TestResolveHarnessConfig_ProfileNotFound(t *testing.T) {
	// When the profile is not found, we should still return the base config without error.
	vs := &VersionedSettings{
		ActiveProfile: "missing-profile",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {Harness: "gemini", Image: "test", User: "scion"},
		},
		Profiles: map[string]V1ProfileConfig{},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "test", hc.Image)
	assert.Equal(t, "scion", hc.User)
}

// --- ResolveRuntime tests ---

func TestResolveRuntime_Basic(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker", Host: "tcp://localhost:2375"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	rtConfig, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "docker", runtimeType)
	assert.Equal(t, "tcp://localhost:2375", rtConfig.Host)
}

func TestResolveRuntime_WithType(t *testing.T) {
	// Runtime with explicit Type field different from map key
	vs := &VersionedSettings{
		ActiveProfile: "remote",
		Runtimes: map[string]V1RuntimeConfig{
			"my-remote-cluster": {
				Type:      "kubernetes",
				Namespace: "scion",
				Context:   "prod-cluster",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"remote": {Runtime: "my-remote-cluster"},
		},
	}

	rtConfig, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "kubernetes", runtimeType, "should use explicit Type field")
	assert.Equal(t, "scion", rtConfig.Namespace)
	assert.Equal(t, "prod-cluster", rtConfig.Context)
}

func TestResolveRuntime_TypeFromKey(t *testing.T) {
	// Type field absent — should fall back to map key name
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Host: "unix:///var/run/docker.sock"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	_, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "docker", runtimeType, "should fall back to map key name when Type is empty")
}

// TestResolveRuntime_ProfileEnvNotMerged verifies that ResolveRuntime no longer
// merges profiles.<p>.env into the runtime config. The Env field was removed
// from V1ProfileConfig as part of the G3-full removal.
func TestResolveRuntime_ProfileEnvNotMerged(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {
				Type: "docker",
				Env:  map[string]string{"RUNTIME_KEY": "runtime_value"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime: "docker",
			},
		},
	}

	rtConfig, _, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "runtime_value", rtConfig.Env["RUNTIME_KEY"], "runtime env should be preserved")
	assert.Len(t, rtConfig.Env, 1, "only runtime env should be present — profile env is no longer merged")
}

func TestResolveRuntime_ProfileNotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "nonexistent",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker"},
		},
		Profiles: map[string]V1ProfileConfig{},
	}

	_, _, err := vs.ResolveRuntime("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestResolveRuntime_RuntimeNotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes:      map[string]V1RuntimeConfig{},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "missing-runtime"},
		},
	}

	_, _, err := vs.ResolveRuntime("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing-runtime")
}

// --- CloudRunInstances koanf unmarshal tests ---

func TestCloudRunInstancesConfig_KoanfUnmarshalFromYAML(t *testing.T) {
	// Verify that the CloudRunInstances nested struct is correctly populated
	// when loading from a YAML file via koanf. This was the failing path
	// described in issue #984: the nested struct fields (especially ProjectID)
	// were silently dropped.
	tmpDir := t.TempDir()
	settingsYAML := `
schema_version: "1"
active_profile: cr-prod
runtimes:
  cr:
    type: cloudrun-instances
    cloudrun_instances:
      project_id: my-gcp-project
      region: us-central1
profiles:
  cr-prod:
    runtime: cr
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(settingsYAML), 0644))

	k := koanf.New(".")
	require.NoError(t, k.Load(file.Provider(filepath.Join(tmpDir, "settings.yaml")), yamlparser.Parser()))

	settings := &VersionedSettings{
		Runtimes: make(map[string]V1RuntimeConfig),
		Profiles: make(map[string]V1ProfileConfig),
	}
	require.NoError(t, k.Unmarshal("", settings))

	rt, ok := settings.Runtimes["cr"]
	require.True(t, ok, "runtime 'cr' should exist")
	assert.Equal(t, "cloudrun-instances", rt.Type)
	require.NotNil(t, rt.CloudRunInstances, "CloudRunInstances should not be nil")
	assert.Equal(t, "my-gcp-project", rt.CloudRunInstances.ProjectID)
	assert.Equal(t, "us-central1", rt.CloudRunInstances.Region)
}

func TestCloudRunInstancesConfig_KoanfUnmarshalFromConfmap(t *testing.T) {
	// Verify that flat koanf keys (as used by confmap.Provider or env vars)
	// correctly populate the CloudRunInstances nested struct.
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]interface{}{
		"runtimes.cr.type":                          "cloudrun-instances",
		"runtimes.cr.cloudrun_instances.project_id": "my-gcp-project",
		"runtimes.cr.cloudrun_instances.region":     "us-central1",
		"runtimes.docker.type":                      "docker",
	}, "."), nil))

	settings := &VersionedSettings{
		Runtimes: make(map[string]V1RuntimeConfig),
	}
	require.NoError(t, k.Unmarshal("", settings))

	rt, ok := settings.Runtimes["cr"]
	require.True(t, ok, "runtime 'cr' should exist")
	assert.Equal(t, "cloudrun-instances", rt.Type)
	require.NotNil(t, rt.CloudRunInstances, "CloudRunInstances should not be nil")
	assert.Equal(t, "my-gcp-project", rt.CloudRunInstances.ProjectID)
	assert.Equal(t, "us-central1", rt.CloudRunInstances.Region)

	// Docker runtime should not have CloudRunInstances
	docker, ok := settings.Runtimes["docker"]
	require.True(t, ok)
	assert.Nil(t, docker.CloudRunInstances)
}

func TestCloudRunInstancesConfig_JSONRoundTrip(t *testing.T) {
	// Verify that JSON marshal/unmarshal of V1RuntimeConfig preserves
	// the CloudRunInstances nested struct. This exercises the DB storage
	// path in operational settings (populateMapSections).
	original := map[string]V1RuntimeConfig{
		"cr": {
			Type: "cloudrun-instances",
			CloudRunInstances: &V1CloudRunInstancesConfig{
				ProjectID: "my-gcp-project",
				Region:    "us-central1",
			},
		},
		"docker": {
			Type: "docker",
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored map[string]V1RuntimeConfig
	require.NoError(t, json.Unmarshal(data, &restored))

	rt := restored["cr"]
	assert.Equal(t, "cloudrun-instances", rt.Type)
	require.NotNil(t, rt.CloudRunInstances)
	assert.Equal(t, "my-gcp-project", rt.CloudRunInstances.ProjectID)
	assert.Equal(t, "us-central1", rt.CloudRunInstances.Region)
	assert.Nil(t, rt.CloudRun, "CloudRun should be nil for cloudrun-instances type")
}

func TestCloudRunInstancesConfig_ResolveRuntime(t *testing.T) {
	// Verify that ResolveRuntime correctly returns the CloudRunInstances
	// config when the active profile references a cloudrun-instances runtime.
	vs := &VersionedSettings{
		ActiveProfile: "cr-prod",
		Runtimes: map[string]V1RuntimeConfig{
			"cr": {
				Type: "cloudrun-instances",
				CloudRunInstances: &V1CloudRunInstancesConfig{
					ProjectID: "my-gcp-project",
					Region:    "us-central1",
				},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"cr-prod": {Runtime: "cr"},
		},
	}

	rtConfig, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "cloudrun-instances", runtimeType)
	require.NotNil(t, rtConfig.CloudRunInstances)
	assert.Equal(t, "my-gcp-project", rtConfig.CloudRunInstances.ProjectID)
	assert.Equal(t, "us-central1", rtConfig.CloudRunInstances.Region)
}

func TestCloudRunInstancesConfig_CoexistsWithCloudRun(t *testing.T) {
	// Verify that both CloudRun and CloudRunInstances can be configured
	// as separate runtime entries without interference.
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]interface{}{
		"runtimes.cr-service.type":                            "cloudrun",
		"runtimes.cr-service.cloudrun.project_id":             "service-project",
		"runtimes.cr-service.cloudrun.location":               "us-east1",
		"runtimes.cr-instances.type":                          "cloudrun-instances",
		"runtimes.cr-instances.cloudrun_instances.project_id": "instances-project",
		"runtimes.cr-instances.cloudrun_instances.region":     "us-west1",
	}, "."), nil))

	settings := &VersionedSettings{
		Runtimes: make(map[string]V1RuntimeConfig),
	}
	require.NoError(t, k.Unmarshal("", settings))

	// Check cloudrun service entry
	svc := settings.Runtimes["cr-service"]
	assert.Equal(t, "cloudrun", svc.Type)
	require.NotNil(t, svc.CloudRun)
	assert.Equal(t, "service-project", svc.CloudRun.ProjectID)
	assert.Equal(t, "us-east1", svc.CloudRun.Location)
	assert.Nil(t, svc.CloudRunInstances)

	// Check cloudrun-instances entry
	inst := settings.Runtimes["cr-instances"]
	assert.Equal(t, "cloudrun-instances", inst.Type)
	require.NotNil(t, inst.CloudRunInstances)
	assert.Equal(t, "instances-project", inst.CloudRunInstances.ProjectID)
	assert.Equal(t, "us-west1", inst.CloudRunInstances.Region)
	assert.Nil(t, inst.CloudRun)
}

func TestProfileRuntimeType(t *testing.T) {
	vs := &VersionedSettings{
		Profiles: map[string]V1ProfileConfig{
			"gke":     {Runtime: "gke"},
			"plain":   {Runtime: "kubernetes"},
			"missing": {Runtime: "nowhere"},
			"none":    {},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"gke":        {Type: "kubernetes"},
			"kubernetes": {},
		},
	}
	for _, tc := range []struct {
		profile, key, typ string
		ok                bool
	}{
		{"gke", "gke", "kubernetes", true},
		{"plain", "kubernetes", "kubernetes", true},
		{"missing", "nowhere", "nowhere", true},
		{"none", "", "", false},
		{"absent", "", "", false},
	} {
		key, typ, ok := vs.ProfileRuntimeType(tc.profile)
		if key != tc.key || typ != tc.typ || ok != tc.ok {
			t.Errorf("ProfileRuntimeType(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.profile, key, typ, ok, tc.key, tc.typ, tc.ok)
		}
	}
	var nilVS *VersionedSettings
	if _, _, ok := nilVS.ProfileRuntimeType("gke"); ok {
		t.Error("nil settings must not resolve")
	}
}
