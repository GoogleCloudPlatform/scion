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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogSharedDirStorageStartup: the pure half of the once-per-process
// startup log must be directly testable with a captured logf, independent
// of any real settings file or broker/hub wiring. It logs one line for a
// shared-dir Layout on a shared backend and nothing on the local backend.
func TestLogSharedDirStorageStartup(t *testing.T) {
	capture := func() (func(format string, args ...interface{}), *[]string) {
		var lines []string
		return func(format string, args ...interface{}) {
			lines = append(lines, fmt.Sprintf(format, args...))
		}, &lines
	}
	nfs := func(uid int) *config.V1NFSConfig {
		return &config.V1NFSConfig{
			MountRoot: "/srv",
			Shares:    []config.V1NFSShare{{ID: "scion-shared", PVName: "scion-shared-pv"}},
			UID:       uid,
		}
	}

	t.Run("nil server and local backend log nothing", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(nil, logf)
		logSharedDirStorageStartup(&config.V1ServerConfig{}, logf)
		logSharedDirStorageStartup(&config.V1ServerConfig{SharedDirStorage: &config.V1SharedDirStorageConfig{Backend: "local"}}, logf)
		assert.Empty(t, *lines)
	})

	t.Run("project_storage nfs logs one layout line", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(&config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "nfs", NFS: nfs(0)}}, logf)
		if assert.Len(t, *lines, 1, "%v", *lines) {
			assert.Contains(t, (*lines)[0], "shared-dir storage layout: backend=nfs host_base=/srv/scion-shared")
			assert.Contains(t, (*lines)[0], "scion-shared-pv")
			assert.Contains(t, (*lines)[0], "server.project_storage")
		}
	})

	t.Run("legacy split with an ignored field warns and logs the layout", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(&config.V1ServerConfig{SharedDirStorage: &config.V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs(1000)}}, logf)
		if assert.Len(t, *lines, 2, "%v", *lines) {
			assert.Contains(t, (*lines)[0], "Warning")
			assert.Contains(t, (*lines)[0], "uid")
			assert.Contains(t, (*lines)[1], "backend=nfs")
			assert.Contains(t, (*lines)[1], "legacy split")
		}
	})

	t.Run("an unresolvable storage block warns", func(t *testing.T) {
		logf, lines := capture()
		logSharedDirStorageStartup(&config.V1ServerConfig{
			ProjectStorage:   &config.V1ProjectStorageConfig{Backend: "nfs", NFS: nfs(0)},
			SharedDirStorage: &config.V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs(0)},
		}, logf)
		if assert.Len(t, *lines, 1, "%v", *lines) {
			assert.Contains(t, (*lines)[0], "Warning: shared-dir storage")
		}
	})
}

// TestLogHomeStorageStartup: an invalid server.home_storage warns, and a
// global nfs home logs one line; the removed per-profile overrides are
// never reported.
func TestLogHomeStorageStartup(t *testing.T) {
	logLines := func(gs *config.VersionedSettings) []string {
		var lines []string
		logHomeStorageStartup(gs, func(format string, args ...interface{}) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
		return lines
	}
	invalid := logLines(&config.VersionedSettings{Server: &config.V1ServerConfig{HomeStorage: &config.V1HomeStorageConfig{Leaf: "node"}}})
	if assert.Len(t, invalid, 1, "%v", invalid) {
		assert.Contains(t, invalid[0], "server.home_storage.leaf")
	}
	nfsHome := logLines(&config.VersionedSettings{
		Server:   &config.V1ServerConfig{HomeStorage: &config.V1HomeStorageConfig{Backend: "nfs", Leaf: "broker"}},
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", HomeStorageBackend: "local"}},
	})
	assert.Equal(t, []string{"home_storage: backend=nfs (from server.home_storage.backend), leaf=broker (Kubernetes agents only)"}, nfsHome)
	assert.Empty(t, logLines(&config.VersionedSettings{
		Server:   &config.V1ServerConfig{},
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", HomeStorageBackend: "nfs"}},
	}), "a profile override is never reported")
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
