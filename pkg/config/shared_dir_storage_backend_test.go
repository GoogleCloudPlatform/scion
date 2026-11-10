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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sdsTestNFSBlock() *V1NFSConfig {
	return &V1NFSConfig{
		MountRoot: "/srv/nfs",
		Shares:    []V1NFSShare{{ID: "share-1", PVName: "pv-1"}},
	}
}

func TestResolveProfileSetting_Generic(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"k8s": {SharedDirStorageClass: "rt-class"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "k8s", SharedDirStorageClass: "profile-class"},
			"b": {Runtime: "k8s"},
			"c": {Runtime: "missing"},
		},
	}
	fromP := func(p V1ProfileConfig) string { return p.SharedDirStorageClass }
	fromR := func(r V1RuntimeConfig) string { return r.SharedDirStorageClass }

	v, src := vs.ResolveProfileSetting("a", "shared_dir_storage_class", fromP, fromR)
	assert.Equal(t, "profile-class", v)
	assert.Equal(t, "profiles.a.shared_dir_storage_class", src)

	v, src = vs.ResolveProfileSetting("b", "shared_dir_storage_class", fromP, fromR)
	assert.Equal(t, "rt-class", v)
	assert.Equal(t, "runtimes.k8s.shared_dir_storage_class", src)

	v, src = vs.ResolveProfileSetting("c", "shared_dir_storage_class", fromP, fromR)
	assert.Empty(t, v)
	assert.Empty(t, src)
}

func TestResolveProfileValue_NonString(t *testing.T) {
	type limits struct{ max int }
	vs := &VersionedSettings{
		ActiveProfile: "b",
		Runtimes:      map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}, "docker": {Type: "docker"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "k8s"},
			"b": {Runtime: "docker"},
		},
	}
	// An int keyed off fields chosen by the caller: the profile's own
	// value is 0 (unset) for "b", so the runtime entry's value applies.
	fromP := func(p V1ProfileConfig) int {
		if p.Runtime == "k8s" {
			return 3
		}
		return 0
	}
	fromR := func(r V1RuntimeConfig) int {
		if r.Type == "docker" {
			return 7
		}
		return 0
	}
	v, src := ResolveProfileValue(vs, "a", "max_agents", fromP, fromR)
	assert.Equal(t, 3, v)
	assert.Equal(t, "profiles.a.max_agents", src)

	v, src = ResolveProfileValue(vs, "", "max_agents", fromP, fromR)
	assert.Equal(t, 7, v)
	assert.Equal(t, "runtimes.docker.max_agents", src)

	v, src = ResolveProfileValue(vs, "missing", "max_agents", fromP, fromR)
	assert.Zero(t, v)
	assert.Empty(t, src)

	var nilVS *VersionedSettings
	l, src := ResolveProfileValue(nilVS, "a", "x",
		func(V1ProfileConfig) limits { return limits{max: 1} },
		func(V1RuntimeConfig) limits { return limits{} })
	assert.Equal(t, limits{}, l)
	assert.Empty(t, src)
}

// The per-runtime and per-profile storage overrides are refused in a
// settings file: server.project_storage decides placement, and the error
// points at a volumes: entry for a node-local directory.
func TestValidateSettings_SharedDirStorageBackend(t *testing.T) {
	const doc = `schema_version: "1"
active_profile: local
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /srv/nfs
      shares:
        - id: share-1
          pv_name: pv-1
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
    shared_dir_storage_backend: nfs
    home_storage_leaf: broker
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backends:
      notes: nfs
    home_storage_backend: nfs
`
	errs, err := ValidateSettings([]byte(doc), "1")
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range errs {
		got[e.Path] = e.Message
	}
	for _, path := range []string{
		"runtimes.k8s.shared_dir_storage_backend",
		"runtimes.k8s.home_storage_leaf",
		"profiles.gke.shared_dir_storage_backends",
		"profiles.gke.home_storage_backend",
	} {
		msg, ok := got[path]
		if assert.True(t, ok, "%s must be rejected: %v", path, errs) {
			assert.Contains(t, msg, "is no longer supported")
			assert.Contains(t, msg, "volumes:")
		}
	}
}

func TestLoadGlobalSettingsWithOverlay(t *testing.T) {
	old := GetGlobalSettingsOverlay()
	t.Cleanup(func() { SetGlobalSettingsOverlay(old) })
	SetGlobalSettingsOverlay(nil)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /srv/nfs
      shares:
        - id: share-1
runtimes:
  docker:
    type: docker
profiles:
  local:
    runtime: docker
`), 0o644))

	// A project's settings must never be merged, even from inside it.
	projectDir := filepath.Join(tmpDir, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, ".scion", "settings.yaml"), []byte(`schema_version: "1"
profiles:
  local:
    runtime: docker
    shared_dir_storage_backend: nfs
`), 0o644))
	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(projectDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// No overlay: file values only; the project's settings are not merged.
	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	cfg := vs.Server.EffectiveSharedDirStorage()
	require.NotNil(t, cfg)
	assert.Equal(t, "local", cfg.Backend)
	assert.Empty(t, vs.Profiles["local"].SharedDirStorageBackend, "project settings must not be merged")

	// Overlay installed: its profiles replace the file's.
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"docker": {Type: "docker"}, "k8s": {Type: "kubernetes"}},
		map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
			"gke":   {Runtime: "k8s", DefaultTemplate: "first"},
		}, nil)
	SetGlobalSettingsOverlay(o)

	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, "first", vs.Profiles["gke"].DefaultTemplate)
	assert.Equal(t, "/srv/nfs", vs.Server.EffectiveSharedDirStorage().NFS.MountRoot, "the storage block still comes from the global file")

	// A later overlay update applies to the next load, with no restart.
	o.Update(nil, map[string]V1ProfileConfig{
		"local": {Runtime: "docker"},
		"gke":   {Runtime: "k8s", DefaultTemplate: "second"},
	}, nil)
	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, "second", vs.Profiles["gke"].DefaultTemplate)

	// The plain loader still ignores the overlay.
	plain, _, err := LoadGlobalSettings()
	require.NoError(t, err)
	_, hasGKE := plain.Profiles["gke"]
	assert.False(t, hasGKE)
}
