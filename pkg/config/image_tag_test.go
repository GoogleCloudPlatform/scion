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
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestResolveImageTagLayers_Precedence checks every set/unset combination
// of the five layers: env > hub DB profile > hub DB top > file profile >
// file top, the same order as image_registry.
func TestResolveImageTagLayers_Precedence(t *testing.T) {
	for mask := 0; mask < 1<<len(imageRegistryLayerOrder); mask++ {
		got := ResolveImageTagLayers(layersFromMask(mask))
		want := ResolvedImageTag{Source: ImageRegistrySourceDefault}
		for i, src := range imageRegistryLayerOrder {
			if mask&(1<<i) != 0 {
				want = ResolvedImageTag{Value: "v" + strconv.Itoa(i), Source: src}
				break
			}
		}
		if got != want {
			t.Errorf("mask %05b: got %+v, want %+v", mask, got, want)
		}
	}
}

// TestResolveImageTagLayers_Table names the precedence cases the design
// lists (env > hub DB > settings, profile before top within a source).
func TestResolveImageTagLayers_Table(t *testing.T) {
	tests := []struct {
		name   string
		layers ImageTagLayers
		want   ResolvedImageTag
	}{
		{"unset", ImageTagLayers{}, ResolvedImageTag{Source: ImageRegistrySourceDefault}},
		{"blank values are unset", ImageTagLayers{NodeEnv: " ", HubDB: "\t", File: ""}, ResolvedImageTag{Source: ImageRegistrySourceDefault}},
		{"env beats everything", ImageTagLayers{NodeEnv: "v1", HubDBProfile: "p", HubDB: "h", FileProfile: "fp", File: "f"}, ResolvedImageTag{Value: "v1", Source: ImageRegistrySourceNodeEnv}},
		{"hub DB profile beats hub DB top", ImageTagLayers{HubDBProfile: "p", HubDB: "h"}, ResolvedImageTag{Value: "p", Source: ImageRegistrySourceHubDBProfile}},
		{"hub DB top beats file profile", ImageTagLayers{HubDB: "h", FileProfile: "fp", File: "f"}, ResolvedImageTag{Value: "h", Source: ImageRegistrySourceHubDB}},
		{"file profile beats file top", ImageTagLayers{FileProfile: "fp", File: "f"}, ResolvedImageTag{Value: "fp", Source: ImageRegistrySourceFileProfile}},
		{"file top", ImageTagLayers{File: " version "}, ResolvedImageTag{Value: "version", Source: ImageRegistrySourceFile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveImageTagLayers(tt.layers); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestVersionedSettingsImageTagLayers checks the settings-file layers:
// the profile value and the top-level value, for the named and the active
// profile, with the env layer passed through.
func TestVersionedSettingsImageTagLayers(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "a",
		ImageTag:      "top",
		Profiles: map[string]V1ProfileConfig{
			"a": {ImageTag: "pa"},
			"b": {},
		},
	}
	tests := []struct {
		profile, env string
		want         ResolvedImageTag
	}{
		{"", "", ResolvedImageTag{Value: "pa", Source: ImageRegistrySourceFileProfile}},
		{"a", "", ResolvedImageTag{Value: "pa", Source: ImageRegistrySourceFileProfile}},
		{"b", "", ResolvedImageTag{Value: "top", Source: ImageRegistrySourceFile}},
		{"missing", "", ResolvedImageTag{Value: "top", Source: ImageRegistrySourceFile}},
		{"a", "env", ResolvedImageTag{Value: "env", Source: ImageRegistrySourceNodeEnv}},
	}
	for _, tt := range tests {
		if got := ResolveImageTagLayers(vs.ImageTagLayers(tt.profile, tt.env)); got != tt.want {
			t.Errorf("profile %q env %q: got %+v, want %+v", tt.profile, tt.env, got, tt.want)
		}
	}
	// A flat instance view has no profile layer: only the top-level value
	// (and the env) count, even with profiles and an active profile set.
	flat := &VersionedSettings{
		ActiveProfile: "a",
		ImageTag:      "top",
		Profiles:      map[string]V1ProfileConfig{"a": {ImageTag: "pa"}},
		flat:          &FlatInstancePolicy{InstanceKey: "i"},
	}
	for _, profile := range []string{"", "a"} {
		if got, want := flat.ImageTagLayers(profile, "e"), (ImageTagLayers{NodeEnv: "e", File: "top"}); got != want {
			t.Errorf("flat view, profile %q: got %+v, want %+v", profile, got, want)
		}
	}
	view := vs.ForFlatInstance(NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: "i"}))
	if got, want := view.ImageTagLayers("a", ""), (ImageTagLayers{File: "top"}); got != want {
		t.Errorf("ForFlatInstance view: got %+v, want %+v", got, want)
	}
	var nilVS *VersionedSettings
	if got := nilVS.ImageTagLayers("a", "e"); got != (ImageTagLayers{NodeEnv: "e"}) {
		t.Errorf("nil settings: got %+v", got)
	}
}

func TestValidateImageTag(t *testing.T) {
	valid := []string{"", "version", "latest", "v0.3.0-preview.5", "nightly-20261105", "abc1234", "_x", "A.b-C_9", strings.Repeat("a", 128)}
	for _, v := range valid {
		if err := ValidateImageTag(v); err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
		}
	}
	invalid := []string{".v1", "-v1", "v1:2", "v1/2", "v 1", "v1@x", "tag!", strings.Repeat("a", 129), "ü", " ", " v1", "v1 ", " v1 ", "\tversion"}
	for _, v := range invalid {
		if err := ValidateImageTag(v); err == nil {
			t.Errorf("%q: want an error", v)
		}
	}
	// Surrounding whitespace gets its own, explicit error.
	if err := ValidateImageTag(" v1 "); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("\" v1 \": got %v, want a whitespace error", err)
	}
}

// TestEffectiveImageTag is the resolution table (ptone/scion#6625): an
// unset value defaults to a release build's own version and leaves every
// other build unchanged (images keep :latest); "version" is the release
// version or latest; an explicit value always wins.
func TestEffectiveImageTag(t *testing.T) {
	tests := []struct {
		setting, version, want string
		wantErr                bool
	}{
		// 1. a release build defaults to its own version
		{"", "v1.0.0", "v1.0.0", false},
		{"", "v0.3.0-preview.5", "v0.3.0-preview.5", false},
		{"", "v0.3.0-rc.1", "v0.3.0-rc.1", false},
		{"  ", "v1.0.0", "v1.0.0", false},
		// 2. a dev or unversioned build keeps :latest (no change)
		{"", "", "", false},
		{"", "dev", "", false},
		{"", "v0.3.0-dirty", "", false},
		{"", "nightly-20261105", "", false},
		{"", "v0.0.0-20261108002803-39f9e3d31e76", "", false},
		{"", "v0.3.0+meta", "", false},
		{"version", "nightly-20261105", "latest", false},
		{"version", "dev", "latest", false},
		{"version", "", "latest", false},
		{"version", "v1+meta", "latest", false},
		// 3. an explicit value always wins
		{"latest", "v0.3.0", "latest", false},
		{"v0.2.9", "v0.3.0", "v0.2.9", false},
		{"v0.3.0-preview.5", "dev", "v0.3.0-preview.5", false},
		{" stable ", "", "stable", false},
		{"version", "v0.3.0-preview.5", "v0.3.0-preview.5", false},
		{"version", "v0.3.0-rc.2", "v0.3.0-rc.2", false},
		// invalid settings give no tag
		{"bad tag", "v1.0.0", "", true},
		{".hidden", "v1.0.0", "", true},
	}
	for _, tt := range tests {
		got, err := EffectiveImageTag(tt.setting, tt.version)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("EffectiveImageTag(%q, %q) = %q, %v; want %q, error %v", tt.setting, tt.version, got, err, tt.want, tt.wantErr)
		}
	}
}

// TestReleaseImageTag: only the release tag forms are release versions.
func TestReleaseImageTag(t *testing.T) {
	for _, v := range []string{"v0.3.0", "v1.2.3", "v0.3.0-preview.5", "v0.3.0-rc.1", "v10.0.0-preview.12"} {
		if got, ok := ReleaseImageTag(v); !ok || got != v {
			t.Errorf("ReleaseImageTag(%q) = %q, %v; want %q, true", v, got, ok, v)
		}
	}
	for _, v := range []string{"", "dev", "latest", "nightly-20261105", "0.3.0", "v0.3", "v0.3.0-dirty",
		"v0.3.0+meta", "v0.3.0-rc1", "v0.3.0-preview", "v01.2.3", "v0.0.0-20261108002803-39f9e3d31e76", " v0.3.0"} {
		if got, ok := ReleaseImageTag(v); ok || got != "" {
			t.Errorf("ReleaseImageTag(%q) = %q, %v; want \"\", false", v, got, ok)
		}
	}
}

// TestRewriteImageTag is the rewrite table: only a short-form scion-* ref
// whose tag is latest or absent takes the tag.
func TestRewriteImageTag(t *testing.T) {
	tests := []struct {
		name, image, tag, want string
	}{
		{"latest becomes the tag", "scion-claude:latest", "v1", "scion-claude:v1"},
		{"untagged takes the tag", "scion-claude", "v1", "scion-claude:v1"},
		{"short path latest", "team/scion-claude:latest", "v1", "team/scion-claude:v1"},
		{"explicit tag untouched", "scion-claude:v0.2", "v1", "scion-claude:v0.2"},
		{"digest untouched", "scion-claude@" + testDigest, "v1", "scion-claude@" + testDigest},
		{"tag and digest untouched", "scion-claude:latest@" + testDigest, "v1", "scion-claude:latest@" + testDigest},
		{"FQ host untouched", "ghcr.io/org/scion-claude:latest", "v1", "ghcr.io/org/scion-claude:latest"},
		{"FQ host:port untouched", "localhost:5000/scion-claude", "v1", "localhost:5000/scion-claude"},
		{"non-scion image untouched", "ubuntu:latest", "v1", "ubuntu:latest"},
		{"empty tag is a no-op", "scion-claude:latest", "", "scion-claude:latest"},
		{"empty image", "", "v1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RewriteImageTag(tt.image, tt.tag); got != tt.want {
				t.Fatalf("RewriteImageTag(%q, %q) = %q, want %q", tt.image, tt.tag, got, tt.want)
			}
		})
	}
}

// TestRewriteImageRef combines the tag and registry rewrites: the tag is
// applied first, on the short form, then the registry; the registry
// rewrite's own output is retagged, any other FQ ref is left alone.
func TestRewriteImageRef(t *testing.T) {
	tests := []struct {
		name, image, registry, tag, want string
	}{
		{"registry plus tag", "scion-claude:latest", "ghcr.io/org", "v1", "ghcr.io/org/scion-claude:v1"},
		{"registry trailing slash", "scion-claude", "ghcr.io/org/", "v1", "ghcr.io/org/scion-claude:v1"},
		{"tag only", "scion-claude:latest", "", "v1", "scion-claude:v1"},
		{"registry only", "scion-claude:latest", "ghcr.io/org", "", "ghcr.io/org/scion-claude:latest"},
		{"explicit tag keeps it, takes registry", "scion-claude:v0.2", "ghcr.io/org", "v1", "ghcr.io/org/scion-claude:v0.2"},
		{"registry rewrite output is retagged", "ghcr.io/org/scion-claude:latest", "ghcr.io/org", "v1", "ghcr.io/org/scion-claude:v1"},
		{"retag is stable", "ghcr.io/org/scion-claude:v1", "ghcr.io/org", "v1", "ghcr.io/org/scion-claude:v1"},
		{"other FQ untouched", "quay.io/x/scion-claude:latest", "ghcr.io/org", "v1", "quay.io/x/scion-claude:latest"},
		{"deeper path under registry untouched", "ghcr.io/org/sub/scion-claude:latest", "ghcr.io/org", "v1", "ghcr.io/org/sub/scion-claude:latest"},
		{"digest under registry untouched", "ghcr.io/org/scion-claude@" + testDigest, "ghcr.io/org", "v1", "ghcr.io/org/scion-claude@" + testDigest},
		{"non-scion takes registry only", "ubuntu:latest", "ghcr.io/org", "v1", "ghcr.io/org/ubuntu:latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RewriteImageRef(tt.image, tt.registry, tt.tag); got != tt.want {
				t.Fatalf("RewriteImageRef(%q, %q, %q) = %q, want %q", tt.image, tt.registry, tt.tag, got, tt.want)
			}
		})
	}
}

// TestRewriteImageRef_UnsetTagIsByteIdentical: with no tag, the combined
// rewrite is exactly RewriteImageRegistry for every input, so an unset
// image_tag (or the experiment off, which resolves to no tag) changes
// nothing.
func TestRewriteImageRef_UnsetTagIsByteIdentical(t *testing.T) {
	images := []string{
		"", "scion-claude", "scion-claude:latest", "scion-claude:v0.2", "team/scion-claude:latest",
		"scion-claude@" + testDigest, "ghcr.io/org/scion-claude:latest", "localhost:5000/scion-pi", "ubuntu:latest",
	}
	for _, reg := range []string{"", "ghcr.io/org", "ghcr.io/org/", "localhost:5000"} {
		for _, img := range images {
			if got, want := RewriteImageRef(img, reg, ""), RewriteImageRegistry(img, reg); got != want {
				t.Errorf("RewriteImageRef(%q, %q, \"\") = %q, want %q", img, reg, got, want)
			}
			if got := RewriteImageTag(img, ""); got != img {
				t.Errorf("RewriteImageTag(%q, \"\") = %q, want it unchanged", img, got)
			}
		}
	}
}

// TestUpdateSetting_ImageTag: `scion config set image_tag` (UpdateSetting)
// stores a valid value exactly as given, and refuses an invalid one,
// including one with surrounding whitespace.
func TestUpdateSetting_ImageTag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := UpdateSetting("", "image_tag", "version", true); err != nil {
		t.Fatalf("set version: %v", err)
	}
	for _, bad := range []string{"bad tag", " v1 ", ".v1"} {
		if err := UpdateSetting("", "image_tag", bad, true); err == nil || !strings.Contains(err.Error(), "invalid image_tag") {
			t.Fatalf("set %q: got %v, want an invalid image_tag error", bad, err)
		}
	}
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetVersionedSettingValue(vs, "image_tag"); err != nil || got != "version" {
		t.Fatalf("image_tag = %q, %v; want the earlier valid value", got, err)
	}
	if err := UpdateSetting("", "image_tag", "v0.3.0-preview.5", true); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	vs, err = LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.ImageTag != "v0.3.0-preview.5" {
		t.Fatalf("ImageTag = %q", vs.ImageTag)
	}
}

// TestUpdateSetting_InvalidImageTagChangesNothing: an invalid image_tag is
// refused by the canonical pre-write check (ValidateSettingValue), before
// any read, lock or legacy-file migration, so every file state, the legacy
// format included, stays byte-identical, at both layers.
func TestUpdateSetting_InvalidImageTagChangesNothing(t *testing.T) {
	if err := ValidateSettingValue("image_tag", " v1 "); err == nil {
		t.Fatal("ValidateSettingValue must refuse an invalid image_tag")
	}
	for _, global := range []bool{true, false} {
		for _, fx := range writePolicyFixtures {
			for _, bad := range []string{"bad tag", " v1 "} {
				name := "project"
				if global {
					name = "global"
				}
				t.Run(name+"/"+fx.name+"/"+bad, func(t *testing.T) {
					home := t.TempDir()
					t.Setenv("HOME", home)
					dir := filepath.Join(home, ".scion")
					projectPath := ""
					if !global {
						dir = filepath.Join(home, "proj", ".scion")
						projectPath = dir
					}
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if fx.file != "" {
						if err := os.WriteFile(filepath.Join(dir, fx.file), []byte(fx.content), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					before := dirSnapshot(t, dir)
					if err := UpdateSetting(projectPath, "image_tag", bad, global); err == nil || !strings.Contains(err.Error(), "invalid image_tag") {
						t.Fatalf("got %v, want an invalid image_tag error", err)
					}
					if after := dirSnapshot(t, dir); !reflect.DeepEqual(before, after) {
						t.Fatalf("a refused image_tag write changed the settings directory:\nbefore %v\nafter  %v", before, after)
					}
				})
			}
		}
	}
}

// TestImageTagEnvReadOnlyByHelper is the guard that only ImageTagFromEnv
// reads SCION_IMAGE_TAG, and that the settings env mappers skip it.
func TestImageTagEnvReadOnlyByHelper(t *testing.T) {
	assertEnvReadOnlyByHelper(t, EnvImageTag, "EnvImageTag", "ImageTagFromEnv", "image_tag.go")
	if !isSettingsExcludedEnv(EnvImageTag) {
		t.Fatalf("%s must be excluded from the settings env mappers", EnvImageTag)
	}
}

func TestImageTagFromEnv(t *testing.T) {
	t.Setenv(EnvImageTag, " v1 ")
	if got := ImageTagFromEnv(); got != "v1" {
		t.Fatalf("got %q", got)
	}
}

// TestConvert_ProfileImageTagReported: a profile image_tag has no flat
// instance counterpart, so the converter reports it as a notice instead
// of dropping it silently.
func TestConvert_ProfileImageTagReported(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{"a": {Runtime: "cluster", ImageTag: "version"}},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	found := 0
	for _, fd := range got.Findings {
		if fd.Path != "profiles.a.image_tag" {
			continue
		}
		found++
		if fd.Code != ConversionLegacyFieldNoEffect || fd.Severity != ConversionSeverityNotice || !strings.Contains(fd.Message, "not converted") {
			t.Errorf("finding %+v: want a legacy_field_no_effect notice saying it is not converted", fd)
		}
	}
	if found != 1 {
		t.Fatalf("want one finding for profiles.a.image_tag, got %d: %+v", found, got.Findings)
	}
}

// TestImageTagSettingPattern_MatchesSchemaAndValidator: the settings
// schema carries ImageTagSettingPattern at the top level and in profiles,
// and the pattern accepts exactly what ValidateImageTag accepts, including
// refusing surrounding whitespace.
func TestImageTagSettingPattern_MatchesSchemaAndValidator(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("schemas", "settings-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"pattern": "`+ImageTagSettingPattern+`"`); n != 2 {
		t.Fatalf("settings schema carries the image_tag pattern %d times, want 2 (top level and profiles)", n)
	}
	re := regexp.MustCompile(ImageTagSettingPattern)
	for _, v := range []string{"", "version", "latest", "v0.3.0-preview.5", "_x", strings.Repeat("a", 128), ".v1", "-v1", "v1:2", "v 1", strings.Repeat("a", 129), " v1 ", " ", "v1 "} {
		if got, want := re.MatchString(v), ValidateImageTag(v) == nil; got != want {
			t.Errorf("%q: pattern match %v, ValidateImageTag ok %v", v, got, want)
		}
	}
}
