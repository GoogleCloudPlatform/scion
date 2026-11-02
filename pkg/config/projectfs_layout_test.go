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
