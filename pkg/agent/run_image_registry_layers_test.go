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

package agent

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// registryLayerSettings has a file profile registry and a file top-level
// registry, so each higher layer is visible in the rewritten image.
const registryLayerSettings = `schema_version: "1"
image_registry: file-top.example
active_profile: staging
profiles:
  staging:
    runtime: docker
    image_registry: file-profile.example
  other:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
`

// installRegistryOverlay installs a global settings overlay fed with in for
// the duration of the test.
func installRegistryOverlay(t *testing.T, in config.ImageRegistryHubInputs) {
	t.Helper()
	o := config.NewSettingsOverlay()
	o.UpdateImageRegistryInputs(in)
	old := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(o)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
}

// TestStart_ImageRegistryLayers drives the broker's final rewrite of a bare
// image through each layer (ptone/scion#3916): node env > hub DB profile >
// hub DB top > file profile > file top.
func TestStart_ImageRegistryLayers(t *testing.T) {
	tests := []struct {
		name    string
		nodeEnv string
		overlay *config.ImageRegistryHubInputs
		want    string
	}{
		{name: "file profile beats file top", want: "file-profile.example/file-default:latest"},
		{name: "node env beats the file profile", nodeEnv: "node.example", want: "node.example/file-default:latest"},
		{
			name:    "hub DB top beats the file profile",
			overlay: &config.ImageRegistryHubInputs{ManagedTop: "db-top.example", FileTier: config.ImageRegistryFileTier{Top: "boot-top.example", Profiles: map[string]string{"staging": "boot-profile.example"}}, Catalog: []string{"other", "staging"}},
			want:    "db-top.example/file-default:latest",
		},
		{
			name:    "hub DB profile beats hub DB top",
			overlay: &config.ImageRegistryHubInputs{ManagedTop: "db-top.example", ManagedProfiles: map[string]string{"staging": "db-profile.example"}, Catalog: []string{"other", "staging"}},
			want:    "db-profile.example/file-default:latest",
		},
		{
			name:    "node env beats hub DB profile",
			nodeEnv: "node.example",
			overlay: &config.ImageRegistryHubInputs{ManagedProfiles: map[string]string{"staging": "db-profile.example"}, Catalog: []string{"other", "staging"}},
			want:    "node.example/file-default:latest",
		},
		{
			name:    "co-located file tier comes from the hub's bootstrap",
			overlay: &config.ImageRegistryHubInputs{FileTier: config.ImageRegistryFileTier{Top: "boot-top.example"}, Catalog: []string{"other", "staging"}},
			want:    "boot-top.example/file-default:latest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvImageRegistry, tt.nodeEnv)
			projectScionDir := imagePrecedenceFixture(t, "", registryLayerSettings)
			if tt.overlay != nil {
				installRegistryOverlay(t, *tt.overlay)
			}
			got, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
			if got.Image != tt.want {
				t.Fatalf("image = %q, want %q", got.Image, tt.want)
			}
		})
	}
}

// TestStart_ImageRegistryIgnoresStaleOverlayActiveProfile: an agent with
// no profile resolves its registry through the broker's live
// active_profile, never the hub's ActiveProfile copy in the overlay feed.
func TestStart_ImageRegistryIgnoresStaleOverlayActiveProfile(t *testing.T) {
	t.Setenv(config.EnvImageRegistry, "")
	projectScionDir := imagePrecedenceFixture(t, "", registryLayerSettings)
	installRegistryOverlay(t, config.ImageRegistryHubInputs{
		ManagedProfiles: map[string]string{"staging": "db-staging.example", "other": "db-other.example"},
		Catalog:         []string{"other", "staging"},
		ActiveProfile:   "other",
	})
	got, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if got.Image != "db-staging.example/file-default:latest" {
		t.Fatalf("image = %q, want the live active profile's (staging) registry", got.Image)
	}
}
