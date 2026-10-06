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

func TestResolveHomeStorage_Precedence(t *testing.T) {
	base := func() *VersionedSettings {
		return &VersionedSettings{
			ActiveProfile: "gke",
			Server:        &V1ServerConfig{},
			Runtimes: map[string]V1RuntimeConfig{
				"docker": {Type: "docker"},
				"k8s":    {Type: "kubernetes"},
			},
			Profiles: map[string]V1ProfileConfig{
				"gke":   {Runtime: "k8s"},
				"local": {Runtime: "docker"},
			},
		}
	}
	setProfile := func(vs *VersionedSettings, name string, f func(*V1ProfileConfig)) {
		p := vs.Profiles[name]
		f(&p)
		vs.Profiles[name] = p
	}
	setRuntime := func(vs *VersionedSettings, name string, f func(*V1RuntimeConfig)) {
		r := vs.Runtimes[name]
		f(&r)
		vs.Runtimes[name] = r
	}

	tests := []struct {
		name          string
		profile       string
		mutate        func(vs *VersionedSettings)
		wantBackend   string
		wantBackSrc   string
		wantLeaf      string
		wantLeafSrc   string
		nilSettingsVS bool
	}{
		{name: "off by default", profile: "gke", wantBackend: "local", wantLeaf: "pod"},
		{name: "nil settings", nilSettingsVS: true, wantBackend: "local", wantLeaf: "pod"},
		{
			name: "global", profile: "gke",
			mutate: func(vs *VersionedSettings) {
				vs.Server.HomeStorage = &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker"}
			},
			wantBackend: "nfs", wantBackSrc: HomeStorageBackendGlobalSource,
			wantLeaf: "broker", wantLeafSrc: HomeStorageLeafGlobalSource,
		},
		{
			name: "runtime entry wins over global", profile: "gke",
			mutate: func(vs *VersionedSettings) {
				vs.Server.HomeStorage = &V1HomeStorageConfig{Backend: "local", Leaf: "pod"}
				setRuntime(vs, "k8s", func(r *V1RuntimeConfig) { r.HomeStorageBackend = "nfs"; r.HomeStorageLeaf = "broker" })
			},
			wantBackend: "nfs", wantBackSrc: "runtimes.k8s.home_storage_backend",
			wantLeaf: "broker", wantLeafSrc: "runtimes.k8s.home_storage_leaf",
		},
		{
			name: "profile wins over runtime entry and global", profile: "gke",
			mutate: func(vs *VersionedSettings) {
				vs.Server.HomeStorage = &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker"}
				setRuntime(vs, "k8s", func(r *V1RuntimeConfig) { r.HomeStorageBackend = "nfs"; r.HomeStorageLeaf = "broker" })
				setProfile(vs, "gke", func(p *V1ProfileConfig) { p.HomeStorageBackend = "local"; p.HomeStorageLeaf = "pod" })
			},
			wantBackend: "local", wantBackSrc: "profiles.gke.home_storage_backend",
			wantLeaf: "pod", wantLeafSrc: "profiles.gke.home_storage_leaf",
		},
		{
			name: "backend and leaf resolve independently", profile: "gke",
			mutate: func(vs *VersionedSettings) {
				vs.Server.HomeStorage = &V1HomeStorageConfig{Leaf: "broker"}
				setProfile(vs, "gke", func(p *V1ProfileConfig) { p.HomeStorageBackend = "nfs" })
			},
			wantBackend: "nfs", wantBackSrc: "profiles.gke.home_storage_backend",
			wantLeaf: "broker", wantLeafSrc: HomeStorageLeafGlobalSource,
		},
		{
			name: "empty profile name uses the active profile", profile: "",
			mutate: func(vs *VersionedSettings) {
				setProfile(vs, "gke", func(p *V1ProfileConfig) { p.HomeStorageBackend = "nfs" })
			},
			wantBackend: "nfs", wantBackSrc: "profiles.gke.home_storage_backend", wantLeaf: "pod",
		},
		{
			name: "unknown profile falls to global", profile: "missing",
			mutate: func(vs *VersionedSettings) {
				vs.Server.HomeStorage = &V1HomeStorageConfig{Backend: "nfs"}
				setProfile(vs, "gke", func(p *V1ProfileConfig) { p.HomeStorageBackend = "local" })
			},
			wantBackend: "nfs", wantBackSrc: HomeStorageBackendGlobalSource, wantLeaf: "pod",
		},
		{
			name: "settings only: a docker profile still reports its nfs value", profile: "local",
			mutate: func(vs *VersionedSettings) {
				setProfile(vs, "local", func(p *V1ProfileConfig) { p.HomeStorageBackend = "nfs" })
			},
			wantBackend: "nfs", wantBackSrc: "profiles.local.home_storage_backend", wantLeaf: "pod",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var vs *VersionedSettings
			if !tt.nilSettingsVS {
				vs = base()
				if tt.mutate != nil {
					tt.mutate(vs)
				}
			}
			got := vs.ResolveHomeStorage(tt.profile)
			assert.Equal(t, tt.wantBackend, got.Backend)
			assert.Equal(t, tt.wantBackSrc, got.BackendSource)
			assert.Equal(t, tt.wantLeaf, got.Leaf)
			assert.Equal(t, tt.wantLeafSrc, got.LeafSource)
		})
	}
}

func TestV1HomeStorageConfig_Validate(t *testing.T) {
	var nilCfg *V1HomeStorageConfig
	assert.NoError(t, nilCfg.Validate())
	assert.NoError(t, (&V1HomeStorageConfig{}).Validate())
	assert.NoError(t, (&V1HomeStorageConfig{Backend: "nfs", Leaf: "broker", StopGraceSeconds: 5}).Validate())
	for _, bad := range []V1HomeStorageConfig{
		{Backend: "NFS"},
		{Backend: "nfs "},
		{Backend: "ceph"},
		{Leaf: "node"},
		{Leaf: "Pod"},
		{StopGraceSeconds: -1},
		{TerminationWaitSeconds: -1},
		{SkeletonMaxBytes: -1},
	} {
		bad := bad
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
}

func TestV1HomeStorageConfig_Defaults(t *testing.T) {
	var nilCfg *V1HomeStorageConfig
	assert.Equal(t, 30, nilCfg.StopGrace())
	assert.Equal(t, 15, nilCfg.TerminationWait())
	assert.Equal(t, int64(256<<20), nilCfg.SkeletonMax())
	c := &V1HomeStorageConfig{StopGraceSeconds: 40, TerminationWaitSeconds: 5, SkeletonMaxBytes: 10}
	assert.Equal(t, 40, c.StopGrace())
	assert.Equal(t, 5, c.TerminationWait())
	assert.Equal(t, int64(10), c.SkeletonMax())
}

func TestValidateHomeStorageOverrides(t *testing.T) {
	errs := ValidateHomeStorageOverrides(
		map[string]V1RuntimeConfig{
			"k8s":    {Type: "kubernetes", HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"},
			"broken": {Type: "kubernetes", HomeStorageBackend: "Nfs", HomeStorageLeaf: "host"},
		},
		map[string]V1ProfileConfig{
			"gke": {Runtime: "k8s", HomeStorageBackend: "local", HomeStorageLeaf: "pod"},
			"bad": {Runtime: "k8s", HomeStorageBackend: "disk"},
		})
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Path)
	}
	assert.Equal(t, []string{
		"profiles.bad.home_storage_backend",
		"runtimes.broken.home_storage_backend",
		"runtimes.broken.home_storage_leaf",
	}, paths)
}

func TestHomeStorageIgnoredWarnings(t *testing.T) {
	w := HomeStorageIgnoredWarnings(
		map[string]V1RuntimeConfig{
			"docker": {Type: "docker", HomeStorageBackend: "nfs"},
			"k8s":    {Type: "kubernetes", HomeStorageBackend: "nfs"},
		},
		map[string]V1ProfileConfig{
			"local": {Runtime: "docker", HomeStorageBackend: "nfs"},
			"gke":   {Runtime: "k8s", HomeStorageBackend: "nfs"},
			"plain": {Runtime: "docker", HomeStorageBackend: "local"},
		})
	require.Len(t, w, 2)
	assert.Contains(t, w[0], "profiles.local.home_storage_backend")
	assert.Contains(t, w[1], "runtimes.docker.home_storage_backend")
}

func TestValidateSettings_HomeStorage(t *testing.T) {
	const okYAML = `schema_version: "1"
active_profile: local
server:
  home_storage:
    backend: local
    leaf: pod
    stop_grace_seconds: 30
    termination_wait_seconds: 15
    skeleton_max_bytes: 1024
    allow_incomplete_phases: true
runtimes:
  docker:
    type: docker
    home_storage_backend: nfs
  k8s:
    type: kubernetes
    home_storage_backend: nfs
    home_storage_leaf: broker
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    home_storage_backend: nfs
    home_storage_leaf: pod
`
	errs, err := ValidateSettings([]byte(okYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs, "an nfs value on a docker entry is a warning, not an error")
	warnings := SettingsWarnings([]byte(okYAML), "1")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "runtimes.docker.home_storage_backend")

	for _, bad := range []struct{ from, to string }{
		{"    home_storage_leaf: pod", "    home_storage_leaf: node"},
		{"    home_storage_backend: nfs\n    home_storage_leaf: broker", "    home_storage_backend: ceph\n    home_storage_leaf: broker"},
		{"    leaf: pod", "    leaf: kubelet"},
		{"    stop_grace_seconds: 30", "    stop_grace_seconds: -3"},
		{"    allow_incomplete_phases: true", "    allow_incomplete_phases: true\n    unknown_key: 1"},
	} {
		doc := strings.Replace(okYAML, bad.from, bad.to, 1)
		require.NotEqual(t, okYAML, doc, bad.from)
		errs, err := ValidateSettings([]byte(doc), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "expected an error for %q", bad.to)
	}
}

// The home storage keys survive the hub's DB settings overlay on every
// load path, and an overlay update applies to the next load with no
// restart.
func TestHomeStorage_OverlayRoundTrip(t *testing.T) {
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
  home_storage:
    leaf: broker
runtimes:
  docker:
    type: docker
profiles:
  local:
    runtime: docker
`), 0o644))
	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(tmpDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"docker": {Type: "docker"}, "k8s": {Type: "kubernetes", HomeStorageLeaf: "broker"}},
		map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
			"gke":   {Runtime: "k8s", HomeStorageBackend: "nfs", HomeStorageLeaf: "pod"},
		}, nil, "")
	SetGlobalSettingsOverlay(o)

	check := func(vs *VersionedSettings, wantBackend, wantLeaf string) {
		t.Helper()
		got := vs.ResolveHomeStorage("gke")
		assert.Equal(t, wantBackend, got.Backend)
		assert.Equal(t, wantLeaf, got.Leaf)
		assert.Equal(t, "broker", vs.Runtimes["k8s"].HomeStorageLeaf)
		assert.Equal(t, "local", vs.ResolveHomeStorage("local").Backend)
	}
	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	check(vs, "nfs", "pod")
	eff, _, err := LoadEffectiveSettings(globalScionDir)
	require.NoError(t, err)
	check(eff, "nfs", "pod")

	// A managed write replaces the whole profiles map. Editing an
	// unrelated field keeps both keys when the writer carries them.
	o.Update(nil, map[string]V1ProfileConfig{
		"local": {Runtime: "docker", DefaultTemplate: "edited"},
		"gke":   {Runtime: "k8s", HomeStorageBackend: "nfs", HomeStorageLeaf: "pod", DefaultTemplate: "edited"},
	}, nil, "")
	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	check(vs, "nfs", "pod")

	// Clearing the profile keys falls back to the runtime entry, then to
	// the file's global block, on the next load.
	o.Update(nil, map[string]V1ProfileConfig{
		"local": {Runtime: "docker"},
		"gke":   {Runtime: "k8s"},
	}, nil, "")
	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	got := vs.ResolveHomeStorage("gke")
	assert.Equal(t, "local", got.Backend)
	assert.Equal(t, "broker", got.Leaf)
	assert.Equal(t, "runtimes.k8s.home_storage_leaf", got.LeafSource)
}

func TestHomeStorageWindowWarnings(t *testing.T) {
	assert.Empty(t, HomeStorageWindowWarnings(nil))
	assert.Empty(t, HomeStorageWindowWarnings(&V1HomeStorageConfig{}), "the defaults (30 + 15) fit")
	w := HomeStorageWindowWarnings(&V1HomeStorageConfig{StopGraceSeconds: 80, TerminationWaitSeconds: 15})
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "95s")
	assert.Contains(t, w[0], "90s")
	warnings := SettingsWarnings([]byte("schema_version: \"1\"\nserver:\n  home_storage:\n    stop_grace_seconds: 100\n"), "1")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "server.home_storage")
}
