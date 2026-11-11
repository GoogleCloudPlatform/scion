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
	"errors"
	"testing"
)

// These tests pin that an empty stored catalog (a hub_settings row holding
// {} for profiles, runtimes or harness_configs) means the section is not
// managed in the hub, the same as unset: the settings file's map is kept
// and active_profile resolves against it (ptone/scion#5007).

// emptyCatalogFileFixture is a settings file with a local profile on
// docker as active_profile, plus harness configs.
func emptyCatalogFileFixture() *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"claude": {Harness: "claude"},
		},
	}
}

// Stored profiles {} and runtimes {} (the seeded empty rows): the file's
// profiles, runtimes and active_profile are used, and the default profile
// resolves to the file's default runtime.
func TestSettingsOverlay_EmptyStoredCatalogUsesFile(t *testing.T) {
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{}, map[string]V1ProfileConfig{}, map[string]HarnessConfigEntry{})
	vs := emptyCatalogFileFixture()
	o.Apply(vs)

	if vs.ActiveProfile != "local" {
		t.Errorf("ActiveProfile = %q, want the file's local", vs.ActiveProfile)
	}
	if _, ok := vs.Profiles["local"]; !ok || len(vs.Profiles) != 1 {
		t.Errorf("Profiles = %v, want the file's profiles", vs.Profiles)
	}
	if _, ok := vs.Runtimes["docker"]; !ok || len(vs.Runtimes) != 1 {
		t.Errorf("Runtimes = %v, want the file's runtimes", vs.Runtimes)
	}
	if _, ok := vs.HarnessConfigs["claude"]; !ok || len(vs.HarnessConfigs) != 1 {
		t.Errorf("HarnessConfigs = %v, want the file's harness configs", vs.HarnessConfigs)
	}
	_, rtType, err := vs.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime(\"\"): %v", err)
	}
	if rtType != "docker" {
		t.Errorf("runtime type = %q, want docker", rtType)
	}
	// A missing named profile keeps the file's plain not-found error, not
	// the stored-profiles form.
	_, _, err = vs.ResolveRuntime("vanished")
	var spe *StoredProfileNotFoundError
	if err == nil || errors.As(err, &spe) {
		t.Errorf("ResolveRuntime(vanished) error = %v, want the plain not-found error", err)
	}
	if o.Profiles() != nil {
		t.Errorf("Profiles() = %v, want nil for an empty stored map", o.Profiles())
	}
}

// Stored profiles non-empty with stored runtimes {}: the stored profiles
// replace the file's, and resolve against the file's runtimes.
func TestSettingsOverlay_StoredProfilesWithEmptyStoredRuntimesUseFileRuntimes(t *testing.T) {
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{}, map[string]V1ProfileConfig{"stored": {Runtime: "docker"}}, nil)
	vs := emptyCatalogFileFixture()
	vs.ActiveProfile = "stored"
	o.Apply(vs)

	if _, ok := vs.Profiles["local"]; ok {
		t.Errorf("Profiles = %v, want the stored profiles only", vs.Profiles)
	}
	if _, ok := vs.Runtimes["docker"]; !ok {
		t.Errorf("Runtimes = %v, want the file's runtimes", vs.Runtimes)
	}
	_, rtType, err := vs.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime(\"\"): %v", err)
	}
	if rtType != "docker" {
		t.Errorf("runtime type = %q, want docker", rtType)
	}
}

// Stored nil (unset) leaves the file's maps in place, as before.
func TestSettingsOverlay_UnsetStoredCatalogUsesFile(t *testing.T) {
	o := NewSettingsOverlay()
	o.Update(nil, nil, nil)
	vs := emptyCatalogFileFixture()
	o.Apply(vs)

	if _, rtType, err := vs.ResolveRuntime(""); err != nil || rtType != "docker" {
		t.Errorf("ResolveRuntime(\"\") = %q, %v; want docker, nil", rtType, err)
	}
	if _, ok := vs.HarnessConfigs["claude"]; !ok {
		t.Errorf("HarnessConfigs = %v, want the file's harness configs", vs.HarnessConfigs)
	}
}

// A stored catalog emptied after it held entries returns to the file's
// maps: the empty update clears the earlier stored values.
func TestSettingsOverlay_EmptiedStoredCatalogReturnsToFile(t *testing.T) {
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
		map[string]V1ProfileConfig{"cluster": {Runtime: "k8s"}, "batch": {Runtime: "k8s"}},
		map[string]HarnessConfigEntry{"gemini": {Harness: "gemini"}})
	o.Update(map[string]V1RuntimeConfig{}, map[string]V1ProfileConfig{}, map[string]HarnessConfigEntry{})
	vs := emptyCatalogFileFixture()
	o.Apply(vs)

	if _, rtType, err := vs.ResolveRuntime(""); err != nil || rtType != "docker" {
		t.Errorf("ResolveRuntime(\"\") = %q, %v; want docker, nil", rtType, err)
	}
	if _, ok := vs.HarnessConfigs["claude"]; !ok || len(vs.HarnessConfigs) != 1 {
		t.Errorf("HarnessConfigs = %v, want the file's harness configs", vs.HarnessConfigs)
	}
}
