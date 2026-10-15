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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// fakeHubSettingRows is a store.HubSettingStore that serves fixed rows;
// only GetHubSetting is implemented.
type fakeHubSettingRows struct {
	store.HubSettingStore
	rows map[string]*store.HubSetting
}

func (f fakeHubSettingRows) GetHubSetting(_ context.Context, section string) (*store.HubSetting, error) {
	if r, ok := f.rows[section]; ok {
		return r, nil
	}
	return nil, store.ErrNotFound
}

// TestManagedImageRegistryConfigured covers the deferred workstation check:
// a saved hub value keeps the co-located broker, including a pre-origin row
// that only the boot backfill would mark managed, and a saved value of the
// broker's default profile; a seeded value does not.
func TestManagedImageRegistryConfigured(t *testing.T) {
	row := func(origin, updatedBy, value string) *store.HubSetting {
		return &store.HubSetting{Origin: origin, UpdatedBy: updatedBy, Value: []byte(value)}
	}
	tests := []struct {
		name   string
		rows   map[string]*store.HubSetting
		active string
		want   bool
	}{
		{"no rows", nil, "", false},
		{"managed endpoints value", map[string]*store.HubSetting{"endpoints": row("managed", "admin@example.com", `{"image_registry":"db.example/r"}`)}, "", true},
		{"pre-origin admin row", map[string]*store.HubSetting{"endpoints": row("seeded", "admin@example.com", `{"image_registry":"db.example/r"}`)}, "", true},
		{"seeded endpoints value", map[string]*store.HubSetting{"endpoints": row("seeded", "seed", `{"image_registry":"file.example/r"}`)}, "", false},
		{"managed endpoints without value", map[string]*store.HubSetting{"endpoints": row("managed", "a", `{"hub_name":"h"}`)}, "", false},
		{"managed default profile value", map[string]*store.HubSetting{"profiles": row("managed", "a", `{"k8s":{"runtime":"kubernetes","image_registry":"db.example/k8s"},"local":{"runtime":"docker"}}`)}, "k8s", true},
		{"managed value of another profile", map[string]*store.HubSetting{"profiles": row("managed", "a", `{"k8s":{"image_registry":"db.example/k8s"},"local":{"runtime":"docker"}}`)}, "local", false},
		{"single stored profile is the default", map[string]*store.HubSetting{"profiles": row("managed", "a", `{"only":{"image_registry":"db.example/only"}}`)}, "missing", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := managedImageRegistryConfigured(context.Background(), fakeHubSettingRows{rows: tt.rows}, tt.active)
			assert.Equal(t, tt.want, got)
		})
	}
	assert.False(t, managedImageRegistryConfigured(context.Background(), nil, ""), "no store")
}

// TestResolveInitImageRegistry: 'scion init --global' counts the node env as
// configured (no prompt) but never saves it.
func TestResolveInitImageRegistry(t *testing.T) {
	prompted := false
	prompt := func() string { prompted = true; return "typed.example" }

	save, env := resolveInitImageRegistry("", "", "env.example", prompt)
	assert.Equal(t, "", save, "the env value is never saved")
	assert.Equal(t, "env.example", env)
	assert.False(t, prompted, "an env value skips the prompt")

	save, _ = resolveInitImageRegistry("flag.example", "file.example", "env.example", prompt)
	assert.Equal(t, "flag.example", save)
	save, _ = resolveInitImageRegistry("", "file.example", "env.example", prompt)
	assert.Equal(t, "file.example", save)

	save, env = resolveInitImageRegistry("", "", "", prompt)
	assert.Equal(t, "typed.example", save)
	assert.Equal(t, "", env)
	assert.True(t, prompted)

	save, env = resolveInitImageRegistry("", "", "", nil)
	assert.Equal(t, "", save)
	assert.Equal(t, "", env)
}

// TestResolveMaintenanceConfig_ImageRegistryOverride: the maintenance
// registry is an override for maintenance builds only, and the settings
// file's registry is not copied into the maintenance config.
func TestResolveMaintenanceConfig_ImageRegistryOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	writeHomeSettingsYAML(t, home, "schema_version: \"1\"\nimage_registry: file.example/r\n")

	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	assert.Equal(t, "", resolveMaintenanceConfig(&config.GlobalConfig{}).ImageRegistryOverride)

	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "maint.example/r")
	assert.Equal(t, "maint.example/r", resolveMaintenanceConfig(&config.GlobalConfig{}).ImageRegistryOverride)
}

// TestFlatInstanceImageRegistries lists this host's flat instances that set
// agent_defaults.image_registry.
func TestFlatInstanceImageRegistries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeSettingsYAML(t, home, `schema_version: "1"
server:
  broker:
    instances:
      - key: docker-1
        runtime_target:
          type: docker
        agent_defaults:
          image_registry: inst.example/r
      - key: docker-2
        runtime_target:
          type: docker
`)
	assert.Equal(t, map[string]string{"docker-1": "inst.example/r"}, flatInstanceImageRegistries())
}

// writeHomeSettingsYAML writes ~/.scion/settings.yaml under home.
func writeHomeSettingsYAML(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBootstrapImageRegistryConfigured: the deferred workstation check
// counts the hub's file tier, including SCION_SERVER_IMAGEREGISTRY and
// SCION_SEED_IMAGEREGISTRY, which only the bootstrap merge reads.
func TestBootstrapImageRegistryConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHomeSettingsYAML(t, home, "schema_version: \"1\"\nprofiles:\n  k8s:\n    image_registry: k8s.example\n")
	t.Setenv("SCION_SERVER_IMAGEREGISTRY", "")
	t.Setenv("SCION_SEED_IMAGEREGISTRY", "")
	assert.False(t, bootstrapImageRegistryConfigured("", "staging"))
	assert.True(t, bootstrapImageRegistryConfigured("", "k8s"), "the active profile's file value")
	t.Setenv("SCION_SERVER_IMAGEREGISTRY", "server.example")
	assert.True(t, bootstrapImageRegistryConfigured("", "staging"))
	t.Setenv("SCION_SERVER_IMAGEREGISTRY", "")
	t.Setenv("SCION_SEED_IMAGEREGISTRY", "seed.example")
	assert.True(t, bootstrapImageRegistryConfigured("", "staging"))
}
