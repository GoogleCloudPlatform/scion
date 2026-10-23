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

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNFSLayout(t *testing.T) {
	l, err := NFSLayout(&V1NFSConfig{
		MountRoot: "/mnt/nfs",
		Shares:    []V1NFSShare{{ID: "hub-a", Server: "10.0.0.2", Export: "/share/hub-a", PVName: "pv"}, {ID: "ignored"}},
		UID:       1000, GID: 1001,
	})
	require.NoError(t, err)
	assert.Equal(t, projectfs.Layout{
		Backend:     projectfs.BackendNFS,
		HostBase:    "/mnt/nfs/hub-a",
		SubPathRoot: DefaultWorkspaceSubPathRoot,
		Share:       projectfs.Share{ID: "hub-a", Server: "10.0.0.2", Export: "/share/hub-a", PVName: "pv"},
		UID:         1000, GID: 1001,
	}, l)

	_, err = NFSLayout(nil)
	assert.EqualError(t, err, "NFS config is nil")
	_, err = NFSLayout(&V1NFSConfig{})
	assert.EqualError(t, err, "no NFS shares configured")
	_, err = NFSLayout(&V1NFSConfig{Shares: []V1NFSShare{{ID: "a"}}, SubPathRoot: "../x"})
	assert.ErrorContains(t, err, "subpath_root")
}

// TestGetSharedDirPath_ValidatesName: the local-backend leaf comes from the
// layout authority, which refuses a name that is not a shared-dir slug.
func TestGetSharedDirPath_ValidatesName(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project-configs", "p__12345678", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	got, err := GetSharedDirPath(projectDir, "scratchpad")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(filepath.Dir(projectDir), SharedDirsSubdir, "scratchpad"), got)

	for _, bad := range []string{"../escape", "a/b", "", "Upper"} {
		_, err := GetSharedDirPath(projectDir, bad)
		assert.Error(t, err, "name %q", bad)
	}
}

func writeGlobalSettingsForTest(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if content != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
	}
}

func TestLoadHubSharedDirStorage(t *testing.T) {
	t.Run("no settings file", func(t *testing.T) {
		writeGlobalSettingsForTest(t, "")
		got, err := LoadHubSharedDirStorage()
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("nfs block", func(t *testing.T) {
		writeGlobalSettingsForTest(t, "schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs:\n      mount_root: /mnt\n      shares:\n        - id: s1\n")
		got, err := LoadHubSharedDirStorage()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "nfs", got.Backend)
		assert.Equal(t, "/mnt", got.NFS.MountRoot)
	})
	t.Run("unparseable file mentioning the key fails closed", func(t *testing.T) {
		writeGlobalSettingsForTest(t, "schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs: [unterminated\n")
		_, err := LoadHubSharedDirStorage()
		assert.ErrorContains(t, err, "global settings unreadable")
	})
	t.Run("legacy file keeps its block", func(t *testing.T) {
		// A legacy-format file is converted with its server block kept
		// (ptone/scion#3919); this incomplete block then fails Validate in
		// the hub, so it still fails closed rather than resolving locally.
		writeGlobalSettingsForTest(t, "server:\n  shared_dir_storage:\n    backend: nfs\n")
		got, err := LoadHubSharedDirStorage()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "nfs", got.Backend)
		assert.Error(t, got.Validate())
	})
	// ptone/scion#5881: a legacy
	// (pre-schema_version) file that mentions shared_dir_storage but from
	// which no block loads fails closed: an error, never a nil block that
	// the hub would read as "local".
	t.Run("legacy file mentioning the key without a block fails closed", func(t *testing.T) {
		writeGlobalSettingsForTest(t, "shared_dir_storage:\n  backend: nfs\n")
		require.True(t, GlobalSettingsIsLegacyFormat(), "fixture must load as the legacy format")
		got, err := LoadHubSharedDirStorage()
		assert.ErrorContains(t, err, "mention it but it was not loaded")
		assert.Nil(t, got, "no block (and so no local fallback) is returned")
	})
	t.Run("environment is not consulted", func(t *testing.T) {
		writeGlobalSettingsForTest(t, "schema_version: \"1\"\n")
		t.Setenv("SCION_SERVER_SHARED_DIR_STORAGE_BACKEND", "nfs")
		t.Setenv("SCION_SERVER_SHARED_DIR_STORAGE_NFS_MOUNT_ROOT", "/env")
		got, err := LoadHubSharedDirStorage()
		require.NoError(t, err)
		assert.Nil(t, got, "the hub reads shared_dir_storage env-free, like the brokers")
	})
}
