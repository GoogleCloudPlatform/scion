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
			assert.Contains(t, (*lines)[0], "gid")
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
