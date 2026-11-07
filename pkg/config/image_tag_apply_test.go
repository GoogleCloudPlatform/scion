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
	"reflect"
	"testing"

	"github.com/knadh/koanf/v2"
)

// TestImageTagApplication is the application table of P3b
// (ptone/scion#6625): a setting value is expanded with the binary version
// (EffectiveImageTag) and applied with the registry (RewriteImageRef), as
// the hub and the broker apply it.
func TestImageTagApplication(t *testing.T) {
	tests := []struct {
		name, setting, version, image, registry, want string
	}{
		{"latest becomes the tag", "v0.3.0", "dev", "scion-claude:latest", "", "scion-claude:v0.3.0"},
		{"untagged becomes the tag", "v0.3.0", "dev", "scion-claude", "", "scion-claude:v0.3.0"},
		{"explicit tag untouched", "v0.3.0", "dev", "scion-claude:v0.2.0", "", "scion-claude:v0.2.0"},
		{"digest untouched", "v0.3.0", "dev", "scion-claude@" + testDigest, "", "scion-claude@" + testDigest},
		{"FQ ref untouched", "v0.3.0", "dev", "quay.io/x/scion-claude:latest", "ghcr.io/org", "quay.io/x/scion-claude:latest"},
		{"registry plus tag", "v0.3.0", "dev", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:v0.3.0"},
		{"registry output retagged", "v0.3.0", "dev", "ghcr.io/org/scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:v0.3.0"},
		{"version is the binary version", "version", "v0.3.0-preview.5", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:v0.3.0-preview.5"},
		{"version falls back to latest for dev", "version", "dev", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:latest"},
		{"version falls back to latest for an empty version", "version", "", "scion-claude", "", "scion-claude:latest"},
		{"unset is the release version", "", "v0.3.0", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:v0.3.0"},
		{"unset changes nothing for dev", "", "dev", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:latest"},
		{"unset changes nothing for a nightly", "", "nightly-20261105", "scion-claude", "", "scion-claude"},
		{"explicit latest opts out", "latest", "v0.3.0", "scion-claude:latest", "ghcr.io/org", "ghcr.io/org/scion-claude:latest"},
		{"non-scion image takes the registry only", "v0.3.0", "dev", "ubuntu:latest", "ghcr.io/org", "ghcr.io/org/ubuntu:latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag, err := EffectiveImageTag(tt.setting, tt.version)
			if err != nil {
				t.Fatalf("EffectiveImageTag(%q, %q): %v", tt.setting, tt.version, err)
			}
			if got := RewriteImageRef(tt.image, tt.registry, tag); got != tt.want {
				t.Fatalf("RewriteImageRef(%q, %q, %q) = %q, want %q", tt.image, tt.registry, tag, got, tt.want)
			}
		})
	}
}

// TestImageTagFileTierFromKoanf reads the top-level and per-profile
// image_tag of a bootstrap koanf, trimmed, blanks omitted, and does not
// mix in image_registry.
func TestImageTagFileTierFromKoanf(t *testing.T) {
	if got := ImageTagFileTierFromKoanf(nil); !reflect.DeepEqual(got, ImageRegistryFileTier{}) {
		t.Fatalf("nil koanf: got %+v, want an empty tier", got)
	}
	k := koanf.New(".")
	for key, v := range map[string]string{
		"image_tag":                   " v1 ",
		"image_registry":              "ghcr.io/org",
		"profiles.k8s.image_tag":      "v2",
		"profiles.k8s.image_registry": "k8s.example",
		"profiles.blank.image_tag":    "  ",
		"profiles.other.runtime":      "docker",
	} {
		if err := k.Set(key, v); err != nil {
			t.Fatal(err)
		}
	}
	got := ImageTagFileTierFromKoanf(k)
	want := ImageRegistryFileTier{Top: "v1", Profiles: map[string]string{"k8s": "v2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ImageTagFileTierFromKoanf = %+v, want %+v", got, want)
	}
	if reg := ImageRegistryFileTierFromKoanf(k); reg.Top != "ghcr.io/org" || reg.Profiles["k8s"] != "k8s.example" {
		t.Fatalf("ImageRegistryFileTierFromKoanf = %+v, want the image_registry values", reg)
	}
}

// TestImageTagSourceLabel names every layer for the hub's image_tag log.
func TestImageTagSourceLabel(t *testing.T) {
	for src, want := range map[ImageRegistrySource]string{
		ImageRegistrySourceNodeEnv:      "env",
		ImageRegistrySourceHubDBProfile: "hub_settings_profile",
		ImageRegistrySourceHubDB:        "hub_settings",
		ImageRegistrySourceFileProfile:  "settings_file_profile",
		ImageRegistrySourceFile:         "settings_file",
		ImageRegistrySourceDefault:      "unset",
		"":                              "unset",
	} {
		if got := ImageTagSourceLabel(src); got != want {
			t.Errorf("ImageTagSourceLabel(%q) = %q, want %q", src, got, want)
		}
	}
}
