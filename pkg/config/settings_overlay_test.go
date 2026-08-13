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
	"sync"
	"testing"
)

func TestSettingsOverlay_Update_Apply(t *testing.T) {
	o := NewSettingsOverlay()

	// Before any Update, Apply should be a no-op.
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker", Host: "unix:///var/run/docker.sock"},
		},
		Profiles: map[string]V1ProfileConfig{
			"default": {Runtime: "docker"},
		},
		ImageRegistry: "original-registry.io",
	}

	o.Apply(vs)
	if vs.Runtimes["docker"].Host != "unix:///var/run/docker.sock" {
		t.Error("Apply on inactive overlay should not change runtimes")
	}
	if vs.ImageRegistry != "original-registry.io" {
		t.Error("Apply on inactive overlay should not change image_registry")
	}

	// Update with DB-backed values.
	dbRuntimes := map[string]V1RuntimeConfig{
		"cloudrun": {
			Type: "cloudrun-instances",
			CloudRun: &V1CloudRunConfig{
				Project: "my-project",
				Region:  "us-central1",
			},
			Env: map[string]string{"GCP_PROJECT": "my-project"},
		},
	}
	dbProfiles := map[string]V1ProfileConfig{
		"production": {
			Runtime:       "cloudrun",
			ImageRegistry: "us-docker.pkg.dev/my-project/scion",
		},
	}
	o.Update(dbRuntimes, dbProfiles, nil, "us-docker.pkg.dev/my-project/scion")

	// Apply should replace file values with DB values.
	o.Apply(vs)

	if len(vs.Runtimes) != 1 {
		t.Fatalf("expected 1 runtime after apply, got %d", len(vs.Runtimes))
	}
	cr, ok := vs.Runtimes["cloudrun"]
	if !ok {
		t.Fatal("expected 'cloudrun' runtime after apply")
	}
	if cr.Type != "cloudrun-instances" {
		t.Errorf("expected type 'cloudrun-instances', got %q", cr.Type)
	}
	if cr.CloudRun == nil || cr.CloudRun.Project != "my-project" {
		t.Error("CloudRun config not applied correctly")
	}
	if cr.Env["GCP_PROJECT"] != "my-project" {
		t.Error("Runtime env not applied correctly")
	}

	if len(vs.Profiles) != 1 {
		t.Fatalf("expected 1 profile after apply, got %d", len(vs.Profiles))
	}
	p, ok := vs.Profiles["production"]
	if !ok {
		t.Fatal("expected 'production' profile after apply")
	}
	if p.Runtime != "cloudrun" {
		t.Errorf("expected profile runtime 'cloudrun', got %q", p.Runtime)
	}

	if vs.ImageRegistry != "us-docker.pkg.dev/my-project/scion" {
		t.Errorf("expected image_registry override, got %q", vs.ImageRegistry)
	}
}

func TestSettingsOverlay_DeepCopy(t *testing.T) {
	o := NewSettingsOverlay()

	original := map[string]V1RuntimeConfig{
		"cloudrun": {
			Type: "cloudrun-instances",
			CloudRun: &V1CloudRunConfig{
				Project: "project-a",
				Region:  "us-central1",
			},
			Env: map[string]string{"KEY": "value-a"},
		},
	}
	o.Update(original, nil, nil, "")

	// Mutate the original — should NOT affect the overlay.
	original["cloudrun"].CloudRun.Project = "project-b"
	original["cloudrun"].Env["KEY"] = "value-b"

	vs := &VersionedSettings{}
	o.Apply(vs)

	cr := vs.Runtimes["cloudrun"]
	if cr.CloudRun.Project != "project-a" {
		t.Errorf("overlay mutation leaked: expected project-a, got %q", cr.CloudRun.Project)
	}
	if cr.Env["KEY"] != "value-a" {
		t.Errorf("overlay mutation leaked: expected value-a, got %q", cr.Env["KEY"])
	}

	// Mutate the applied result — should NOT affect the overlay.
	vs.Runtimes["cloudrun"] = V1RuntimeConfig{Type: "modified"}

	vs2 := &VersionedSettings{}
	o.Apply(vs2)
	if vs2.Runtimes["cloudrun"].Type != "cloudrun-instances" {
		t.Errorf("applied mutation leaked back: expected cloudrun-instances, got %q", vs2.Runtimes["cloudrun"].Type)
	}
}

func TestSettingsOverlay_NilMapNoChange(t *testing.T) {
	o := NewSettingsOverlay()

	// First update sets runtimes.
	o.Update(
		map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
		nil, nil, "",
	)

	// Second update with nil runtimes should keep the first.
	o.Update(nil, map[string]V1ProfileConfig{"p": {Runtime: "docker"}}, nil, "")

	vs := &VersionedSettings{}
	o.Apply(vs)

	if _, ok := vs.Runtimes["docker"]; !ok {
		t.Error("nil runtimes in Update should preserve previous value")
	}
	if _, ok := vs.Profiles["p"]; !ok {
		t.Error("profiles should have been set")
	}
}

func TestSettingsOverlay_ConcurrentAccess(t *testing.T) {
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
		map[string]V1ProfileConfig{"default": {Runtime: "docker"}},
		nil, "registry.io",
	)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			o.Update(
				map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
				nil, nil, "",
			)
		}()
		go func() {
			defer wg.Done()
			vs := &VersionedSettings{}
			o.Apply(vs)
		}()
	}
	wg.Wait()
}

func TestSettingsOverlay_ImageRegistryEnvVarWins(t *testing.T) {
	// When SCION_IMAGE_REGISTRY env var is set, the overlay must NOT overwrite
	// ImageRegistry — the env var takes precedence over DB values.
	t.Setenv("SCION_IMAGE_REGISTRY", "env-registry.io")

	o := NewSettingsOverlay()
	o.Update(nil, nil, nil, "db-registry.io")

	vs := &VersionedSettings{
		ImageRegistry: "env-registry.io", // as LoadEffectiveSettings would set it
	}
	o.Apply(vs)

	if vs.ImageRegistry != "env-registry.io" {
		t.Errorf("env var should win over DB overlay: expected %q, got %q",
			"env-registry.io", vs.ImageRegistry)
	}
}

func TestSettingsOverlay_ImageRegistryDBWinsWhenNoEnvVar(t *testing.T) {
	// When SCION_IMAGE_REGISTRY env var is NOT set, the overlay's DB value
	// should be applied.
	// t.Setenv is not called — SCION_IMAGE_REGISTRY is unset.

	o := NewSettingsOverlay()
	o.Update(nil, nil, nil, "db-registry.io")

	vs := &VersionedSettings{}
	o.Apply(vs)

	if vs.ImageRegistry != "db-registry.io" {
		t.Errorf("DB overlay should win when no env var: expected %q, got %q",
			"db-registry.io", vs.ImageRegistry)
	}
}

func TestSettingsOverlay_GlobalOverlay(t *testing.T) {
	// Clean state.
	old := GetGlobalSettingsOverlay()
	defer SetGlobalSettingsOverlay(old)

	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"test": {Type: "test"}},
		nil, nil, "",
	)
	SetGlobalSettingsOverlay(o)

	got := GetGlobalSettingsOverlay()
	if got != o {
		t.Error("GetGlobalSettingsOverlay should return the installed overlay")
	}

	// Clearing.
	SetGlobalSettingsOverlay(nil)
	if GetGlobalSettingsOverlay() != nil {
		t.Error("SetGlobalSettingsOverlay(nil) should clear")
	}
}
