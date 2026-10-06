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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogSharedDirStorageStartup: the pure half of the once-per-process
// startup log/warning must be directly testable with a captured logf,
// independent of any real settings file or broker/hub wiring.
func TestLogSharedDirStorageStartup(t *testing.T) {
	capture := func() (func(format string, args ...interface{}), *[]string) {
		var lines []string
		return func(format string, args ...interface{}) {
			lines = append(lines, fmt.Sprintf(format, args...))
		}, &lines
	}

	t.Run("nil config logs nothing", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(nil, logf)
		assert.Empty(t, *lines)
	})

	t.Run("local backend logs nothing", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(&config.V1SharedDirStorageConfig{Backend: "local"}, logf)
		assert.Empty(t, *lines)
	})

	t.Run("nfs backend with ignored fields warns and logs the layout", func(t *testing.T) {
		logf, lines := capture()
		sdCfg := &config.V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &config.V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []config.V1NFSShare{{ID: "scion-shared", PVName: "scion-shared-pv"}},
				UID:       1000,
				GID:       1000,
			},
		}
		logSharedDirStorageStartup(sdCfg, logf)
		if assert.Len(t, *lines, 2) {
			assert.Contains(t, (*lines)[0], "Warning")
			assert.Contains(t, (*lines)[0], "uid")
			// gid is the leaf-group allowlist, not ignored (ptone/scion#3155).
			assert.NotContains(t, (*lines)[0], "gid")
			assert.Contains(t, (*lines)[1], "resolved layout")
			assert.Contains(t, (*lines)[1], "backend=nfs")
			assert.Contains(t, (*lines)[1], "scion-shared-pv")
		}
	})

	t.Run("clean nfs backend logs only the layout line, no warning", func(t *testing.T) {
		logf, lines := capture()
		sdCfg := &config.V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &config.V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []config.V1NFSShare{{ID: "scion-shared"}},
			},
		}
		logSharedDirStorageStartup(sdCfg, logf)
		if assert.Len(t, *lines, 1) {
			assert.Contains(t, (*lines)[0], "resolved layout")
			assert.NotContains(t, (*lines)[0], "Warning")
		}
	})
}

// TestLoadAndLogSharedDirStorageStartup_LoadFailure: a global settings file
// that fails to load is silent UNLESS its raw bytes
// mention shared_dir_storage, in which case it must warn -- once -- rather
// than fail closed with no explanation at startup.
func TestLoadAndLogSharedDirStorageStartup_LoadFailure(t *testing.T) {
	capture := func() (func(format string, args ...interface{}), *[]string) {
		var lines []string
		return func(format string, args ...interface{}) {
			lines = append(lines, fmt.Sprintf(format, args...))
		}, &lines
	}

	t.Run("unreadable settings mentioning shared_dir_storage warns", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpHome, ".scion", "settings.yaml"),
			[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs: [unterminated\n"), 0o644))

		logf, lines := capture()
		loadAndLogSharedDirStorageStartup(logf)
		if assert.Len(t, *lines, 1) {
			assert.Contains(t, (*lines)[0], "Warning")
			assert.Contains(t, (*lines)[0], "shared_dir_storage")
		}
	})

	t.Run("unreadable settings not mentioning shared_dir_storage stays silent", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpHome, ".scion", "settings.yaml"),
			[]byte("schema_version: \"1\"\nserver:\n  broker: [unterminated\n"), 0o644))

		logf, lines := capture()
		loadAndLogSharedDirStorageStartup(logf)
		assert.Empty(t, *lines)
	})

	t.Run("no settings file at all stays silent", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		logf, lines := capture()
		loadAndLogSharedDirStorageStartup(logf)
		assert.Empty(t, *lines)
	})
}

// TestSharedDirStorageStartupLogWanted: the decision of
// whether THIS process should call logSharedDirStorageStartupOnce at all
// (hub-only, broker-only, or a combined hub+broker process) is its own tiny,
// table-tested function rather than logic embedded at the call site.
func TestSharedDirStorageStartupLogWanted(t *testing.T) {
	tests := []struct {
		name          string
		brokerEnabled bool
		hubEnabled    bool
		want          bool
	}{
		{"neither enabled", false, false, false},
		{"broker-only", true, false, true},
		{"hub-only", false, true, true},
		{"combo (broker and hub)", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sharedDirStorageStartupLogWanted(tt.brokerEnabled, tt.hubEnabled))
		})
	}
}

// TestLoadAndLogSharedDirStorageStartup_ProfileOverride: a gke profile
// overriding the backend to nfs, with the export not mounted on this host,
// logs one summary line for that profile and no warning. Startup checks
// configuration only and never looks at the mount, so a missing mount
// cannot affect startup or local-backend agents.
func TestLoadAndLogSharedDirStorageStartup_ProfileOverride(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755))
	mountRoot := filepath.Join(tmpHome, "not-mounted")
	require.NoError(t, os.WriteFile(filepath.Join(tmpHome, ".scion", "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: `+mountRoot+`
      shares:
        - id: share-1
          pv_name: pv-1
`), 0o644))

	var lines []string
	loadAndLogSharedDirStorageStartup(func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if assert.Len(t, lines, 1, "only the gke profile has an override: %v", lines) {
		assert.Contains(t, lines[0], "profile gke")
		assert.Contains(t, lines[0], "backend=nfs")
		assert.Contains(t, lines[0], "profiles.gke.shared_dir_storage_backend")
		assert.NotContains(t, lines[0], "Warning")
	}
	_, err := os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(err), "startup must not create the mount path")
}

// TestLogSharedDirStorageOverridesStartup_InvalidOverrideWarns: an nfs
// override without an nfs block logs a warning naming the key, plus the
// profile's summary line.
func TestLogSharedDirStorageOverridesStartup_InvalidOverrideWarns(t *testing.T) {
	gs := &config.VersionedSettings{
		Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes", SharedDirStorageBackend: "nfs"}},
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s"}, "local": {Runtime: "docker"}},
	}
	var lines []string
	logSharedDirStorageOverridesStartup(gs, func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if assert.Len(t, lines, 2, "%v", lines) {
		assert.Contains(t, lines[0], "Warning")
		assert.Contains(t, lines[0], "runtimes.k8s.shared_dir_storage_backend")
		assert.Contains(t, lines[1], "profile gke: backend=nfs (from runtimes.k8s.shared_dir_storage_backend)")
	}
}

// TestLogSharedDirStorageOverridesStartup_PerDirEntries: a line per profile
// and shared dir whose backend comes from a per-dir entry; a runtime entry
// that the profile's single value overrides is not listed.
func TestLogSharedDirStorageOverridesStartup_PerDirEntries(t *testing.T) {
	gs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{SharedDirStorage: &config.V1SharedDirStorageConfig{
			Backend: "local",
			NFS:     &config.V1NFSConfig{MountRoot: "/mnt/nfs", Shares: []config.V1NFSShare{{ID: "share-1", PVName: "pv-1"}}},
		}},
		Runtimes: map[string]config.V1RuntimeConfig{
			"k8s":    {Type: "kubernetes", SharedDirStorageBackends: map[string]string{"gocache": "local"}},
			"docker": {Type: "docker"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"gke":   {Runtime: "k8s", SharedDirStorageBackends: map[string]string{"notes": "nfs"}},
			"fast":  {Runtime: "k8s", SharedDirStorageBackend: "nfs"},
			"local": {Runtime: "docker"},
		},
	}
	var lines []string
	logSharedDirStorageOverridesStartup(gs, func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "shared_dir_storage for profile gke, shared dir gocache: backend=local (from runtimes.k8s.shared_dir_storage_backends.gocache)")
	assert.Contains(t, joined, "shared_dir_storage for profile gke, shared dir notes: backend=nfs (from profiles.gke.shared_dir_storage_backends.notes)")
	assert.NotContains(t, joined, "profile fast, shared dir", "the profile single value wins over the runtime per-dir entry")
	assert.NotContains(t, joined, "Warning")
}

func TestLogHomeStorageStartup(t *testing.T) {
	gs := &config.VersionedSettings{
		Server: &config.V1ServerConfig{HomeStorage: &config.V1HomeStorageConfig{Leaf: "node"}},
		Runtimes: map[string]config.V1RuntimeConfig{
			"k8s":    {Type: "kubernetes", HomeStorageLeaf: "broker"},
			"docker": {Type: "docker"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"gke":   {Runtime: "k8s", HomeStorageBackend: "nfs"},
			"local": {Runtime: "docker", HomeStorageBackend: "nfs"},
			"plain": {Runtime: "k8s"},
		},
	}
	var lines []string
	logHomeStorageStartup(gs, func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if assert.Len(t, lines, 3, "%v", lines) {
		assert.Contains(t, lines[0], "server.home_storage.leaf")
		assert.Contains(t, lines[1], "profiles.local.home_storage_backend")
		assert.Equal(t, "home_storage for profile gke: backend=nfs (from profiles.gke.home_storage_backend), leaf=broker", lines[2])
	}
}
