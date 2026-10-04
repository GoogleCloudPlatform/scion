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
	"strings"
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

func TestResolveSharedDirStorage_Precedence(t *testing.T) {
	nfs := sdsTestNFSBlock()
	base := func() *VersionedSettings {
		return &VersionedSettings{
			ActiveProfile: "local",
			Server:        &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local", NFS: nfs}},
			Runtimes: map[string]V1RuntimeConfig{
				"docker": {Type: "docker"},
				"k8s":    {Type: "kubernetes", SharedDirStorageBackend: "nfs"},
			},
			Profiles: map[string]V1ProfileConfig{
				"local":     {Runtime: "docker"},
				"gke":       {Runtime: "k8s"},
				"gke-local": {Runtime: "k8s", SharedDirStorageBackend: "local"},
				"dock-nfs":  {Runtime: "docker", SharedDirStorageBackend: "nfs"},
			},
		}
	}

	tests := []struct {
		name        string
		profile     string
		mutate      func(vs *VersionedSettings)
		wantBackend string // "" means nil config
		wantSource  string
	}{
		{name: "global when profile and runtime are unset", profile: "local", wantBackend: "local", wantSource: SharedDirStorageGlobalSource},
		{name: "empty profile uses active_profile", profile: "", wantBackend: "local", wantSource: SharedDirStorageGlobalSource},
		{name: "runtime entry overrides global", profile: "gke", wantBackend: "nfs", wantSource: "runtimes.k8s.shared_dir_storage_backend"},
		{name: "profile overrides runtime entry", profile: "gke-local", wantBackend: "local", wantSource: "profiles.gke-local.shared_dir_storage_backend"},
		{name: "profile overrides global", profile: "dock-nfs", wantBackend: "nfs", wantSource: "profiles.dock-nfs.shared_dir_storage_backend"},
		{name: "unknown profile falls back to global", profile: "nope", wantBackend: "local", wantSource: SharedDirStorageGlobalSource},
		{
			name:        "active_profile with an override",
			profile:     "",
			mutate:      func(vs *VersionedSettings) { vs.ActiveProfile = "gke" },
			wantBackend: "nfs", wantSource: "runtimes.k8s.shared_dir_storage_backend",
		},
		{
			name:    "no global block and no override is nil",
			profile: "local",
			mutate:  func(vs *VersionedSettings) { vs.Server = nil },
		},
		{
			name:        "override without a global block has no nfs details",
			profile:     "gke",
			mutate:      func(vs *VersionedSettings) { vs.Server = nil },
			wantBackend: "nfs", wantSource: "runtimes.k8s.shared_dir_storage_backend",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := base()
			if tt.mutate != nil {
				tt.mutate(vs)
			}
			cfg, source := vs.ResolveSharedDirStorage(tt.profile)
			assert.Equal(t, tt.wantSource, source)
			if tt.wantBackend == "" {
				assert.Nil(t, cfg)
				return
			}
			require.NotNil(t, cfg)
			assert.Equal(t, tt.wantBackend, cfg.Backend)
			if vs.Server != nil {
				assert.Same(t, nfs, cfg.NFS, "the nfs details always come from the global block")
			} else {
				assert.Nil(t, cfg.NFS)
			}
		})
	}
}

func TestResolveSharedDirStorage_OverrideDoesNotModifyGlobal(t *testing.T) {
	global := &V1SharedDirStorageConfig{Backend: "local", NFS: sdsTestNFSBlock()}
	vs := &VersionedSettings{
		Server:   &V1ServerConfig{SharedDirStorage: global},
		Profiles: map[string]V1ProfileConfig{"gke": {Runtime: "k8s", SharedDirStorageBackend: "nfs"}},
	}
	cfg, _ := vs.ResolveSharedDirStorage("gke")
	require.NotNil(t, cfg)
	assert.NotSame(t, global, cfg)
	assert.Equal(t, "local", global.Backend)
}

func TestResolveSharedDirStorage_NilSettings(t *testing.T) {
	var vs *VersionedSettings
	cfg, source := vs.ResolveSharedDirStorage("x")
	assert.Nil(t, cfg)
	assert.Empty(t, source)
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

func TestSharedDirStorageNFSAnywhere(t *testing.T) {
	nfs := sdsTestNFSBlock()

	t.Run("global nfs", func(t *testing.T) {
		global := &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs}
		vs := &VersionedSettings{Server: &V1ServerConfig{SharedDirStorage: global}}
		cfg, only := vs.SharedDirStorageNFSAnywhere()
		assert.Same(t, global, cfg)
		assert.False(t, only)
	})
	t.Run("profile override only", func(t *testing.T) {
		vs := &VersionedSettings{
			Server:   &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local", NFS: nfs}},
			Profiles: map[string]V1ProfileConfig{"gke": {SharedDirStorageBackend: "nfs"}},
		}
		cfg, only := vs.SharedDirStorageNFSAnywhere()
		require.NotNil(t, cfg)
		assert.Equal(t, "nfs", cfg.Backend)
		assert.Same(t, nfs, cfg.NFS)
		assert.True(t, only)
	})
	t.Run("runtime override only", func(t *testing.T) {
		vs := &VersionedSettings{
			Server:   &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{NFS: nfs}},
			Runtimes: map[string]V1RuntimeConfig{"k8s": {SharedDirStorageBackend: "nfs"}},
		}
		cfg, only := vs.SharedDirStorageNFSAnywhere()
		require.NotNil(t, cfg)
		assert.True(t, only)
	})
	t.Run("nothing selects nfs", func(t *testing.T) {
		vs := &VersionedSettings{
			Server:   &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local", NFS: nfs}},
			Profiles: map[string]V1ProfileConfig{"a": {SharedDirStorageBackend: "local"}},
		}
		cfg, only := vs.SharedDirStorageNFSAnywhere()
		assert.Nil(t, cfg)
		assert.False(t, only)
	})
}

func TestValidateSharedDirStorageBackends(t *testing.T) {
	complete := &V1SharedDirStorageConfig{Backend: "local", NFS: sdsTestNFSBlock()}

	assert.Empty(t, ValidateSharedDirStorageBackends(
		map[string]V1RuntimeConfig{"k8s": {SharedDirStorageBackend: "nfs"}, "docker": {}},
		map[string]V1ProfileConfig{"a": {SharedDirStorageBackend: "local"}, "b": {SharedDirStorageBackend: "nfs"}},
		complete))

	errs := ValidateSharedDirStorageBackends(
		map[string]V1RuntimeConfig{"k8s": {SharedDirStorageBackend: "nfs"}},
		map[string]V1ProfileConfig{"a": {SharedDirStorageBackend: "Nfs"}},
		nil)
	require.Len(t, errs, 2)
	assert.Equal(t, "profiles.a.shared_dir_storage_backend", errs[0].Path)
	assert.Contains(t, errs[0].Message, `must be "local" or "nfs"`)
	assert.Equal(t, "runtimes.k8s.shared_dir_storage_backend", errs[1].Path)
	assert.Contains(t, errs[1].Message, "server.shared_dir_storage.nfs")

	// A global block without shares is still incomplete.
	errs = ValidateSharedDirStorageBackends(nil,
		map[string]V1ProfileConfig{"gke": {SharedDirStorageBackend: "nfs"}},
		&V1SharedDirStorageConfig{NFS: &V1NFSConfig{MountRoot: "/srv"}})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "shares")
}

func TestValidateSettings_SharedDirStorageBackend(t *testing.T) {
	const okYAML = `schema_version: "1"
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
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
`
	errs, err := ValidateSettings([]byte(okYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	badEnum := strings.Replace(okYAML, "    runtime: k8s\n    shared_dir_storage_backend: nfs", "    runtime: k8s\n    shared_dir_storage_backend: ceph", 1)
	errs, err = ValidateSettings([]byte(badEnum), "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errs, "an unknown backend value must be rejected")

	noBlock := `schema_version: "1"
active_profile: local
runtimes:
  k8s:
    type: kubernetes
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
`
	errs, err = ValidateSettings([]byte(noBlock), "1")
	require.NoError(t, err)
	require.NotEmpty(t, errs, "an nfs override without an nfs block must fail validation")
	var found bool
	for _, e := range errs {
		if e.Path == "profiles.gke.shared_dir_storage_backend" {
			found = true
		}
	}
	assert.True(t, found, "error must name the override key: %v", errs)
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

	// No overlay: file values only.
	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	cfg, _ := vs.ResolveSharedDirStorage("local")
	require.NotNil(t, cfg)
	assert.Equal(t, "local", cfg.Backend, "project settings must not choose the backend")

	// Overlay installed: its profiles replace the file's.
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"docker": {Type: "docker"}, "k8s": {Type: "kubernetes"}},
		map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
			"gke":   {Runtime: "k8s", SharedDirStorageBackend: "nfs"},
		}, nil, "")
	SetGlobalSettingsOverlay(o)

	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	cfg, source := vs.ResolveSharedDirStorage("gke")
	require.NotNil(t, cfg)
	assert.Equal(t, "nfs", cfg.Backend)
	assert.Equal(t, "profiles.gke.shared_dir_storage_backend", source)
	assert.Equal(t, "/srv/nfs", cfg.NFS.MountRoot, "nfs details still come from the global file")

	// A later overlay update applies to the next load, with no restart.
	o.Update(nil, map[string]V1ProfileConfig{
		"local": {Runtime: "docker"},
		"gke":   {Runtime: "k8s", SharedDirStorageBackend: "local"},
	}, nil, "")
	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	cfg, _ = vs.ResolveSharedDirStorage("gke")
	require.NotNil(t, cfg)
	assert.Equal(t, "local", cfg.Backend)

	// The plain loader still ignores the overlay.
	plain, _, err := LoadGlobalSettings()
	require.NoError(t, err)
	_, hasGKE := plain.Profiles["gke"]
	assert.False(t, hasGKE)
}
