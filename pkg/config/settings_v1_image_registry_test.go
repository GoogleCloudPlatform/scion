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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- RewriteImageRegistry tests ---

func TestRewriteImageRegistry(t *testing.T) {
	tests := []struct {
		name        string
		fullImage   string
		newRegistry string
		want        string
	}{
		{
			name:        "fully qualified scion image preserved",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
		},
		{
			name:        "fully qualified scion image preserved with trailing slash",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-gemini:latest",
			newRegistry: "ghcr.io/myorg/",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-gemini:latest",
		},
		{
			name:        "bare non-scion image rewritten",
			fullImage:   "ubuntu:22.04",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/ubuntu:22.04",
		},
		{
			name:        "do not rewrite custom registry image",
			fullImage:   "myregistry.io/custom-agent:v1",
			newRegistry: "ghcr.io/myorg",
			want:        "myregistry.io/custom-agent:v1",
		},
		{
			name:        "bare custom harness image rewritten",
			fullImage:   "my-custom-harness:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/my-custom-harness:latest",
		},
		{
			name:        "empty registry returns original",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
			newRegistry: "",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
		},
		{
			name:        "empty image returns empty",
			fullImage:   "",
			newRegistry: "ghcr.io/myorg",
			want:        "",
		},
		{
			name:        "fully qualified scion-base image preserved",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-base:v2",
			newRegistry: "docker.io/myuser",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-base:v2",
		},
		{
			name:        "fully qualified preserves tag",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-opencode:sha-abc123",
			newRegistry: "ghcr.io/myorg",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-opencode:sha-abc123",
		},
		{
			name:        "bare image name (no registry prefix)",
			fullImage:   "scion-claude:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/scion-claude:latest",
		},
		{
			name:        "bare image name empty registry",
			fullImage:   "scion-gemini:latest",
			newRegistry: "",
			want:        "scion-gemini:latest",
		},
		{
			name:        "explicit registry preserved",
			fullImage:   "ghcr.io/myorg/scion-elixir:latest",
			newRegistry: "docker.io/other",
			want:        "ghcr.io/myorg/scion-elixir:latest",
		},
		{
			name:        "port-based registry preserved",
			fullImage:   "localhost:5000/scion-test:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "localhost:5000/scion-test:latest",
		},
		{
			name:        "docker hub path rewritten",
			fullImage:   "myuser/scion-custom:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/scion-custom:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteImageRegistry(tt.fullImage, tt.newRegistry)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveImageRegistry(t *testing.T) {
	tests := []struct {
		name        string
		settings    *VersionedSettings
		profileName string
		want        string
	}{
		{
			name: "top-level image_registry",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "ghcr.io/myorg",
		},
		{
			name: "profile-level overrides top-level",
			settings: &VersionedSettings{
				ActiveProfile: "staging",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"staging": {
						Runtime:       "docker",
						ImageRegistry: "us-central1-docker.pkg.dev/myproject/staging",
					},
				},
			},
			profileName: "staging",
			want:        "us-central1-docker.pkg.dev/myproject/staging",
		},
		{
			name: "profile without image_registry falls back to top-level",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "ghcr.io/myorg",
		},
		{
			name: "empty profile name uses active profile",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				ImageRegistry: "ghcr.io/default",
				Profiles: map[string]V1ProfileConfig{
					"prod": {
						Runtime:       "docker",
						ImageRegistry: "ghcr.io/prod",
					},
				},
			},
			profileName: "",
			want:        "ghcr.io/prod",
		},
		{
			name: "no image_registry configured",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.settings.ResolveImageRegistry(tt.profileName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestImageRegistryYAMLRoundTrip(t *testing.T) {
	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		ImageRegistry: "ghcr.io/myorg",
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime: "docker",
			},
			"staging": {
				Runtime:       "docker",
				ImageRegistry: "us-central1-docker.pkg.dev/myproject/staging",
			},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}

	data, err := yaml.Marshal(vs)
	require.NoError(t, err)

	assert.Contains(t, string(data), "image_registry: ghcr.io/myorg")
	assert.Contains(t, string(data), "image_registry: us-central1-docker.pkg.dev/myproject/staging")

	var roundTripped VersionedSettings
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/myorg", roundTripped.ImageRegistry)
	assert.Equal(t, "us-central1-docker.pkg.dev/myproject/staging", roundTripped.Profiles["staging"].ImageRegistry)
	assert.Empty(t, roundTripped.Profiles["local"].ImageRegistry)
}

func TestIsImageRegistryConfigured(t *testing.T) {
	tests := []struct {
		name     string
		settings *VersionedSettings
		profile  string
		want     bool
	}{
		{
			name:     "empty settings",
			settings: &VersionedSettings{},
			profile:  "",
			want:     false,
		},
		{
			name: "top-level registry set",
			settings: &VersionedSettings{
				ImageRegistry: "ghcr.io/myorg",
			},
			profile: "",
			want:    true,
		},
		{
			name: "profile-level registry set",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {ImageRegistry: "ghcr.io/myorg"},
				},
			},
			profile: "local",
			want:    true,
		},
		{
			name: "no registry anywhere",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profile: "local",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.settings.IsImageRegistryConfigured(tt.profile)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRequireImageRegistry_NotConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	// Create a minimal versioned settings file without image_registry
	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	require.NoError(t, SaveVersionedSettings(dir, vs))

	err := RequireImageRegistry(dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_registry is not configured")
	assert.Contains(t, err.Error(), "image-build/README.md")
}

func TestRequireImageRegistry_Configured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		ImageRegistry: "ghcr.io/myorg",
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	require.NoError(t, SaveVersionedSettings(dir, vs))

	err := RequireImageRegistry(dir, "")
	assert.NoError(t, err)
}

// TestRewriteImageRegistry_Idempotent proves that rewriting an image that was
// already rewritten to the same registry returns it unchanged, including for
// registries without a hostname (where the first path component carries no
// "." or ":" and only the basename extraction keeps the result stable).
func TestRewriteImageRegistry_Idempotent(t *testing.T) {
	registries := []string{"ghcr.io/org", "ghcr.io/org/", "localhost:5000", "myorg", "us-docker.pkg.dev/p/r"}
	images := []string{"scion-claude:v1", "scion-claude", "ubuntu:22.04", "scion-claude@sha256:abc"}
	for _, registry := range registries {
		for _, image := range images {
			once := RewriteImageRegistry(image, registry)
			assert.Equal(t, once, RewriteImageRegistry(once, registry), "registry %q image %q", registry, image)
		}
	}
}
