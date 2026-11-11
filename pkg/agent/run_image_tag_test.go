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
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// imageTagSettings returns settings whose harness config image is image,
// with an optional top-level image_registry.
func imageTagSettings(image, registry string) string {
	s := "schema_version: \"1\"\n"
	if registry != "" {
		s += "image_registry: " + registry + "\n"
	}
	return s + `active_profile: staging
profiles:
  staging:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: ` + image + "\n"
}

// startWithImageTag runs Start with the hub defaults carrying tag (none
// when tag is empty) on a mock runtime named runtimeName, and returns the
// image handed to the runtime and the images ImageExists was asked about.
func startWithImageTag(t *testing.T, opts api.StartOptions, tag, runtimeName string, localImages map[string]bool) (string, []string) {
	t.Helper()
	var captured runtime.RunConfig
	var asked []string
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			asked = append(asked, image)
			return localImages[image], nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	ctx := context.Background()
	if tag != "" {
		ctx = api.ContextWithHubAgentDefaults(ctx, &api.HubAgentDefaults{ImageTag: tag})
	}
	opts.BrokerMode = true
	opts.NoAuth = true
	if _, err := NewManager(mockRT).Start(ctx, opts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return captured.Image, asked
}

// TestStart_ImageTagApplied drives the broker's final image through the
// hub-sent image_tag (ptone/scion#6625): the tag applies to a short-form
// scion-* image whose tag is latest or absent, before the registry
// rewrite, and leaves every other image alone.
func TestStart_ImageTagApplied(t *testing.T) {
	tests := []struct {
		name, image, registry, explicit, tag, want string
	}{
		{name: "latest becomes the tag", image: "scion-claude:latest", tag: "v0.3.0", want: "scion-claude:v0.3.0"},
		{name: "untagged becomes the tag", image: "scion-claude", tag: "v0.3.0", want: "scion-claude:v0.3.0"},
		{name: "explicit tag untouched", image: "scion-claude:v0.2.0", tag: "v0.3.0", want: "scion-claude:v0.2.0"},
		{name: "digest untouched", image: "scion-claude@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", tag: "v0.3.0",
			want: "scion-claude@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{name: "FQ ref untouched", image: "quay.io/x/scion-claude:latest", registry: "ghcr.io/org", tag: "v0.3.0", want: "quay.io/x/scion-claude:latest"},
		{name: "non-scion image untouched", image: "file-default:latest", tag: "v0.3.0", want: "file-default:latest"},
		{name: "registry plus tag", image: "scion-claude:latest", registry: "ghcr.io/org", tag: "v0.3.0", want: "ghcr.io/org/scion-claude:v0.3.0"},
		{name: "registry output retagged", image: "ghcr.io/org/scion-claude:latest", registry: "ghcr.io/org", tag: "v0.3.0", want: "ghcr.io/org/scion-claude:v0.3.0"},
		{name: "explicit request image tagged", image: "file-default:latest", explicit: "scion-codex:latest", registry: "ghcr.io/org", tag: "v0.3.0", want: "ghcr.io/org/scion-codex:v0.3.0"},
		{name: "hub-rewritten explicit image is stable", image: "file-default:latest", explicit: "ghcr.io/org/scion-codex:v0.3.0", registry: "ghcr.io/org", tag: "v0.3.0", want: "ghcr.io/org/scion-codex:v0.3.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvImageRegistry, "")
			projectScionDir := imagePrecedenceFixture(t, "", imageTagSettings(tt.image, tt.registry))
			got, _ := startWithImageTag(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging", Image: tt.explicit}, tt.tag, "mock", nil)
			if got != tt.want {
				t.Fatalf("image = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStart_ImageTagOffIsByteIdentical: with no tag from the hub (the
// experiment off, the setting unset, or a hub that predates the field) the
// image is exactly what it was before image_tag existed, on every input.
func TestStart_ImageTagOffIsByteIdentical(t *testing.T) {
	for _, tt := range []struct{ image, registry, want string }{
		{"scion-claude:latest", "", "scion-claude:latest"},
		{"scion-claude", "", "scion-claude"},
		{"scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:latest"},
		{"ghcr.io/org/scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:latest"},
		{"scion-claude:v0.2.0", "ghcr.io/org", "ghcr.io/org/scion-claude:v0.2.0"},
	} {
		t.Run(tt.image+"|"+tt.registry, func(t *testing.T) {
			t.Setenv(config.EnvImageRegistry, "")
			projectScionDir := imagePrecedenceFixture(t, "", imageTagSettings(tt.image, tt.registry))
			got, _ := startWithImageTag(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"}, "", "mock", nil)
			if got != tt.want {
				t.Fatalf("image = %q, want %q (unchanged)", got, tt.want)
			}
		})
	}
}

// TestStart_ImageTagLocalImagePreferred: on a runtime with local images,
// the local check looks for the tagged short form, so a locally built
// scion-<h>:<tag> is used as is; without it, the tagged name is qualified
// with the registry.
func TestStart_ImageTagLocalImagePreferred(t *testing.T) {
	for _, tt := range []struct {
		name  string
		local map[string]bool
		want  string
	}{
		{"local tagged image", map[string]bool{"scion-claude:v0.3.0": true}, "scion-claude:v0.3.0"},
		{"only latest is local", map[string]bool{"scion-claude:latest": true}, "ghcr.io/org/scion-claude:v0.3.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvImageRegistry, "")
			t.Setenv(runtime.ForceHostNetworkEnvVar, "")
			projectScionDir := imagePrecedenceFixture(t, "", imageTagSettings("scion-claude:latest", "ghcr.io/org"))
			got, asked := startWithImageTag(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"}, "v0.3.0", "docker", tt.local)
			if got != tt.want {
				t.Fatalf("image = %q, want %q", got, tt.want)
			}
			sawTagged := false
			for _, img := range asked {
				if img == "scion-claude:latest" {
					t.Errorf("local image check asked for the untagged image: %v", asked)
				}
				sawTagged = sawTagged || img == "scion-claude:v0.3.0"
			}
			if !sawTagged {
				t.Errorf("local image check asked %v, want the tagged short form", asked)
			}
		})
	}
}
