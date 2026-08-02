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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
)

func TestRequireImageRegistryForBroker_NoRegistry(t *testing.T) {
	// Ensure no env vars provide a registry.
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")

	// Point HOME at an empty temp dir so settings.yaml is absent.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Chdir(tmpHome)

	err := requireImageRegistryForBroker()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "image_registry is not configured")
	assert.Contains(t, err.Error(), "SCION_IMAGE_REGISTRY")
}

func TestRequireImageRegistryForBroker_EnvVar(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "ghcr.io/test")

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestRequireImageRegistryForBroker_MaintenanceEnvVar(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "ghcr.io/test-maintenance")

	// Point HOME at an empty temp dir so settings.yaml is absent.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestRequireImageRegistryForBroker_Settings(t *testing.T) {
	// Use t.Setenv to register cleanup (restores original values when test
	// ends), then unset the env vars so the koanf env loader does not
	// override the settings file value with an empty string.
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	if err := os.Unsetenv("SCION_IMAGE_REGISTRY"); err != nil {
		t.Fatalf("failed to unsetenv SCION_IMAGE_REGISTRY: %v", err)
	}
	if err := os.Unsetenv("SCION_MAINTENANCE_IMAGE_REGISTRY"); err != nil {
		t.Fatalf("failed to unsetenv SCION_MAINTENANCE_IMAGE_REGISTRY: %v", err)
	}

	// Create a temp home with settings.yaml containing image_registry.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create test .scion dir: %v", err)
	}
	settingsContent := "schema_version: \"1\"\nimage_registry: ghcr.io/from-settings\n"
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write test settings: %v", err)
	}

	// Change to the temp home so LoadEffectiveSettings doesn't find
	// the workspace project root's .scion directory.
	t.Chdir(tmpHome)

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestCloudRunLogicalBrokerIDIsDeterministic(t *testing.T) {
	settings := &config.VersionedSettings{
		ActiveProfile: "default",
		Profiles: map[string]config.V1ProfileConfig{
			"default": {Runtime: "cloudrun"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"cloudrun": {
				Type: "cloudrun",
				CloudRun: &config.V1CloudRunConfig{
					Project: "test-project",
					Region:  "us-central1",
				},
			},
		},
	}
	rt := &scionruntime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	id1, err1 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err1)
	id2, err2 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err2)

	assert.NotEmpty(t, id1)
	assert.Equal(t, id1, id2)
}

func TestResolveBrokerIDPrefersConfiguredIDOverDefault(t *testing.T) {
	cfg := &config.GlobalConfig{}
	settings := &config.Settings{
		Hub: &config.HubClientConfig{BrokerID: "configured-broker"},
	}

	got := resolveBrokerID(context.Background(), cfg, settings, nil, t.TempDir(), "cloudrun-default", nil)

	assert.Equal(t, "configured-broker", got)
}
