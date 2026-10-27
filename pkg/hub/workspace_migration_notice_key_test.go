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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStorageMigrationLayoutKey_StableOnSplitConfigs: the storage-migration
// plan cache key is the same on every call for the same configuration,
// including the transitional splits whose Layout holds a SharedDirs
// pointer (a synthesized volume workspace keeping shared dirs local, and
// shared dirs on a shared_dir_storage export while the workspace is
// local), and differs when the configuration differs.
func TestStorageMigrationLayoutKey_StableOnSplitConfigs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	volumeSplit := &config.V1ProjectStorageConfig{Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "v", PVClaimName: "c"}, LegacySharedDirsLocal: true}
	nfsSplit := &config.V1ProjectStorageConfig{LegacySharedDirsNFS: &config.V1NFSConfig{MountRoot: "/mnt/sd",
		Shares: []config.V1NFSShare{{ID: "s", Server: "10.0.0.2", Export: "/e"}}}}
	keys := map[string]string{}
	for name, ps := range map[string]*config.V1ProjectStorageConfig{"volume split": volumeSplit, "nfs split": nfsSplit, "nfs": {Backend: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: "/mnt/nfs", Shares: []config.V1NFSShare{{ID: "a", Server: "10.0.0.2", Export: "/e"}}}}} {
		srv := &Server{config: ServerConfig{ProjectStorageConfig: ps}}
		k1, err := srv.storageMigrationLayoutKey()
		require.NoError(t, err, name)
		k2, err := srv.storageMigrationLayoutKey()
		require.NoError(t, err, name)
		assert.Equal(t, k1, k2, "%s: the key must be stable across calls", name)
		keys[name] = k1
	}
	assert.NotEqual(t, keys["volume split"], keys["nfs split"])
	assert.NotEqual(t, keys["nfs split"], keys["nfs"])
}
