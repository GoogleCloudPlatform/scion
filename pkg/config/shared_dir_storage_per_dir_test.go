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
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func perDirNFSBlock() *V1SharedDirStorageConfig {
	return &V1SharedDirStorageConfig{
		Backend: "local",
		NFS: &V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    []V1NFSShare{{ID: "share-1", PVName: "pv-1"}},
		},
	}
}

func TestResolveSharedDirStorageBackend_Precedence(t *testing.T) {
	tests := []struct {
		name        string
		profile     V1ProfileConfig
		runtime     V1RuntimeConfig
		global      *V1SharedDirStorageConfig
		dir         string
		wantBackend string
		wantSource  string
		wantPerDir  bool
	}{
		{
			name:        "profile per-dir entry wins over profile single value",
			profile:     V1ProfileConfig{Runtime: "rt", SharedDirStorageBackend: "local", SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
			global:      perDirNFSBlock(),
			dir:         "notes",
			wantBackend: "nfs",
			wantSource:  "profiles.p.shared_dir_storage_backends.notes",
			wantPerDir:  true,
		},
		{
			name:        "profile single value applies to a dir its map does not name",
			profile:     V1ProfileConfig{Runtime: "rt", SharedDirStorageBackend: "local", SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
			global:      perDirNFSBlock(),
			dir:         "gocache",
			wantBackend: "local",
			wantSource:  "profiles.p.shared_dir_storage_backend",
		},
		{
			// The nearest level wins: a profile single value nfs beats a
			// runtime entry's per-dir gocache=local.
			name:        "profile single value wins over runtime per-dir entry",
			profile:     V1ProfileConfig{Runtime: "rt", SharedDirStorageBackend: "nfs"},
			runtime:     V1RuntimeConfig{SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			global:      perDirNFSBlock(),
			dir:         "gocache",
			wantBackend: "nfs",
			wantSource:  "profiles.p.shared_dir_storage_backend",
		},
		{
			// How to pin it: name the dir in the profile's own map.
			name:        "profile per-dir entry pins a dir against the profile single value",
			profile:     V1ProfileConfig{Runtime: "rt", SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			runtime:     V1RuntimeConfig{SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			global:      perDirNFSBlock(),
			dir:         "gocache",
			wantBackend: "local",
			wantSource:  "profiles.p.shared_dir_storage_backends.gocache",
			wantPerDir:  true,
		},
		{
			name:        "runtime per-dir entry wins over runtime single value",
			profile:     V1ProfileConfig{Runtime: "rt"},
			runtime:     V1RuntimeConfig{SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			global:      perDirNFSBlock(),
			dir:         "gocache",
			wantBackend: "local",
			wantSource:  "runtimes.rt.shared_dir_storage_backends.gocache",
			wantPerDir:  true,
		},
		{
			name:        "runtime single value for a dir no map names",
			profile:     V1ProfileConfig{Runtime: "rt"},
			runtime:     V1RuntimeConfig{SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			global:      perDirNFSBlock(),
			dir:         "notes",
			wantBackend: "nfs",
			wantSource:  "runtimes.rt.shared_dir_storage_backend",
		},
		{
			name:        "runtime per-dir entry wins over global",
			profile:     V1ProfileConfig{Runtime: "rt"},
			runtime:     V1RuntimeConfig{SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
			global:      perDirNFSBlock(),
			dir:         "notes",
			wantBackend: "nfs",
			wantSource:  "runtimes.rt.shared_dir_storage_backends.notes",
			wantPerDir:  true,
		},
		{
			name:        "global backend when nothing overrides",
			profile:     V1ProfileConfig{Runtime: "rt"},
			global:      perDirNFSBlock(),
			dir:         "notes",
			wantBackend: "local",
			wantSource:  SharedDirStorageGlobalSource,
		},
		{
			name:    "nothing configured",
			profile: V1ProfileConfig{Runtime: "rt"},
			dir:     "notes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := &VersionedSettings{
				Profiles: map[string]V1ProfileConfig{"p": tt.profile},
				Runtimes: map[string]V1RuntimeConfig{"rt": tt.runtime},
			}
			if tt.global != nil {
				vs.Server = &V1ServerConfig{SharedDirStorage: tt.global}
			}
			backend, source, perDir := vs.ResolveSharedDirStorageBackend("p", tt.dir)
			assert.Equal(t, tt.wantBackend, backend)
			assert.Equal(t, tt.wantSource, source)
			assert.Equal(t, tt.wantPerDir, perDir)
		})
	}
}

func TestResolveSharedDirStorageBackend_ActiveProfileAndUnknownProfile(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "p",
		Profiles: map[string]V1ProfileConfig{
			"p": {Runtime: "rt", SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
		},
		Server: &V1ServerConfig{SharedDirStorage: perDirNFSBlock()},
	}
	backend, source, perDir := vs.ResolveSharedDirStorageBackend("", "notes")
	assert.Equal(t, "nfs", backend)
	assert.Equal(t, "profiles.p.shared_dir_storage_backends.notes", source)
	assert.True(t, perDir)

	backend, source, perDir = vs.ResolveSharedDirStorageBackend("missing", "notes")
	assert.Equal(t, "local", backend)
	assert.Equal(t, SharedDirStorageGlobalSource, source)
	assert.False(t, perDir)

	var nilVS *VersionedSettings
	backend, source, perDir = nilVS.ResolveSharedDirStorageBackend("p", "notes")
	assert.Empty(t, backend)
	assert.Empty(t, source)
	assert.False(t, perDir)
}

// For a dir that no per-dir entry names, the per-dir resolution agrees with
// ResolveSharedDirStorage, which chooses the agent's default backend.
func TestResolveSharedDirStorageBackend_AgreesWithDefaultWithoutPerDirEntry(t *testing.T) {
	for _, single := range []struct{ profile, runtime string }{
		{"", ""}, {"nfs", ""}, {"", "nfs"}, {"local", "nfs"}, {"nfs", "local"},
	} {
		vs := &VersionedSettings{
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "rt", SharedDirStorageBackend: single.profile,
				SharedDirStorageBackends: map[string]string{"other": "local"}}},
			Runtimes: map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackend: single.runtime,
				SharedDirStorageBackends: map[string]string{"other": "nfs"}}},
			Server: &V1ServerConfig{SharedDirStorage: perDirNFSBlock()},
		}
		cfg, wantSource := vs.ResolveSharedDirStorage("p")
		backend, source, perDir := vs.ResolveSharedDirStorageBackend("p", "notes")
		require.NotNil(t, cfg)
		assert.Equal(t, cfg.Backend, backend, "profile=%q runtime=%q", single.profile, single.runtime)
		assert.Equal(t, wantSource, source)
		assert.False(t, perDir)
	}
}

func TestValidateSharedDirStorageBackends_PerDirEntries(t *testing.T) {
	complete := perDirNFSBlock()
	tests := []struct {
		name     string
		runtimes map[string]V1RuntimeConfig
		profiles map[string]V1ProfileConfig
		global   *V1SharedDirStorageConfig
		wantPath string
		wantMsg  string
	}{
		{
			name:     "valid entries for dirs a project may not have",
			profiles: map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"notes": "nfs", "no-such-dir": "local"}}},
			runtimes: map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackends: map[string]string{"gocache": "local"}}},
			global:   complete,
		},
		{
			name:     "invalid shared dir name",
			profiles: map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"../x": "local"}}},
			wantPath: "profiles.p.shared_dir_storage_backends.../x",
			wantMsg:  "invalid shared dir name",
		},
		{
			name:     "uppercase name",
			runtimes: map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackends: map[string]string{"Notes": "local"}}},
			wantPath: "runtimes.rt.shared_dir_storage_backends.Notes",
			wantMsg:  "invalid shared dir name",
		},
		{
			name:     "empty value",
			profiles: map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"notes": ""}}},
			wantPath: "profiles.p.shared_dir_storage_backends.notes",
			wantMsg:  `must be "local" or "nfs"`,
		},
		{
			name:     "unknown value",
			runtimes: map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackends: map[string]string{"notes": "NFS"}}},
			wantPath: "runtimes.rt.shared_dir_storage_backends.notes",
			wantMsg:  `must be "local" or "nfs"`,
		},
		{
			name:     "nfs entry without an nfs block",
			profiles: map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"notes": "nfs"}}},
			wantPath: "profiles.p.shared_dir_storage_backends.notes",
			wantMsg:  "needs a complete server.shared_dir_storage.nfs block",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateSharedDirStorageBackends(tt.runtimes, tt.profiles, tt.global)
			if tt.wantPath == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, tt.wantPath, errs[0].Path)
			assert.Contains(t, errs[0].Message, tt.wantMsg)
		})
	}
}

func TestSharedDirStorageNFSAnywhere_PerDirEntry(t *testing.T) {
	vs := &VersionedSettings{
		Profiles: map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"gocache": "local"}}},
		Server:   &V1ServerConfig{SharedDirStorage: perDirNFSBlock()},
	}
	cfg, _ := vs.SharedDirStorageNFSAnywhere()
	assert.Nil(t, cfg, "a local per-dir entry does not select nfs")

	vs.Profiles["p"] = V1ProfileConfig{SharedDirStorageBackends: map[string]string{"notes": "nfs"}}
	cfg, onlyOverrides := vs.SharedDirStorageNFSAnywhere()
	require.NotNil(t, cfg, "a profile per-dir nfs entry selects nfs")
	assert.True(t, onlyOverrides)
	assert.Equal(t, "nfs", cfg.Backend)

	vs.Profiles = nil
	vs.Runtimes = map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackends: map[string]string{"notes": "nfs"}}}
	cfg, _ = vs.SharedDirStorageNFSAnywhere()
	require.NotNil(t, cfg, "a runtime per-dir nfs entry selects nfs")
}

func TestValidateSettings_SharedDirStorageBackendsMap(t *testing.T) {
	valid := `schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_backends:
      gocache: local
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
    shared_dir_storage_backends:
      gocache: local
      notes: nfs
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/nfs
      shares:
        - id: share-1
          pv_name: pv-1
`
	errs, err := ValidateSettings([]byte(valid), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	badValue := strings.Replace(valid, "notes: nfs", "notes: disk", 1)
	errs, err = ValidateSettings([]byte(badValue), "1")
	require.NoError(t, err)
	require.NotEmpty(t, errs)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "notes") {
			found = true
		}
	}
	assert.True(t, found, "an unknown per-dir value is reported: %v", errs)

	noBlock := `schema_version: "1"
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backends:
      notes: nfs
`
	errs, err = ValidateSettings([]byte(noBlock), "1")
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Equal(t, "profiles.gke.shared_dir_storage_backends.notes", errs[0].Path)
}

// A settings file that uses only the per-dir key on a runtime entry and no
// schema_version is still read as v1, so the key is not dropped.
func TestDetectSettingsFormat_PerDirKeyIsV1Indicator(t *testing.T) {
	raw := map[string]interface{}{
		"runtimes": map[string]interface{}{
			"k8s": map[string]interface{}{"shared_dir_storage_backends": map[string]interface{}{"notes": "nfs"}},
		},
	}
	assert.True(t, hasV1RuntimeIndicators(raw))
}

// The settings schema itself rejects per-dir keys that are not shared dir
// names.
func TestSettingsSchema_SharedDirStorageBackendsKeyPattern(t *testing.T) {
	doc := func(key string) []byte {
		return []byte(`schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_backends:
      ` + key + `: local
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backends:
      ` + key + `: local
`)
	}
	errs, err := validateAgainstSchema(doc("build-cache"), "1", settingsSchemaFiles)
	require.NoError(t, err)
	assert.Empty(t, errs)
	for _, key := range []string{"Bad_Name", "-lead", "trail-", "with.dot"} {
		errs, err := validateAgainstSchema(doc(`"`+key+`"`), "1", settingsSchemaFiles)
		require.NoError(t, err)
		assert.Len(t, errs, 2, "%s: both entries are rejected by the schema: %v", key, errs)
	}
}

func TestSharedDirNamePatternMatchesValidateSharedDirs(t *testing.T) {
	re := regexp.MustCompile(SharedDirNamePattern)
	for _, name := range []string{"a", "build-cache", "a1-b2", "x--y", "", "-a", "a-", "A", "a_b", "a.b", "a/b"} {
		want := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}) == nil
		assert.Equal(t, want, re.MatchString(name), "%q", name)
	}
}

// The value check does not need server.shared_dir_storage: it reports names
// and values but not a missing nfs block.
func TestValidateSharedDirStorageBackendValues(t *testing.T) {
	errs := ValidateSharedDirStorageBackendValues(nil, map[string]V1ProfileConfig{
		"p": {SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
	})
	assert.Empty(t, errs)

	errs = ValidateSharedDirStorageBackendValues(
		map[string]V1RuntimeConfig{"rt": {SharedDirStorageBackend: "disk"}},
		map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"Bad_Name": "local", "notes": ""}}})
	require.Len(t, errs, 3)
	assert.Equal(t, "profiles.p.shared_dir_storage_backends.Bad_Name", errs[0].Path)
	assert.Equal(t, "profiles.p.shared_dir_storage_backends.notes", errs[1].Path)
	assert.Equal(t, "runtimes.rt.shared_dir_storage_backend", errs[2].Path)
}

// The JSON schema repeats the shared dir name pattern as a literal; both
// shared_dir_storage_backends entries must use SharedDirNamePattern.
func TestSettingsSchema_SharedDirStorageBackendsPatternMatchesConstant(t *testing.T) {
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var doc interface{}
	require.NoError(t, json.Unmarshal(data, &doc))
	var patterns []string
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch n := v.(type) {
		case map[string]interface{}:
			for k, child := range n {
				if k == "shared_dir_storage_backends" {
					entry, _ := child.(map[string]interface{})
					names, _ := entry["propertyNames"].(map[string]interface{})
					pattern, _ := names["pattern"].(string)
					patterns = append(patterns, pattern)
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)
	require.Len(t, patterns, 2, "runtime and profile entries")
	for _, p := range patterns {
		assert.Equal(t, SharedDirNamePattern, p)
	}
}
