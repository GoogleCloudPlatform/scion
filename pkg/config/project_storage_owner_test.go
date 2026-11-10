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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The transitional shared-dir block (shared dirs on nfs, workspaces not)
// gets the nfs owner ids' defaults, as the workspace block does, so the
// shared-dir Layout always names an owner; explicit ids are kept.
func TestSynthesizeProjectStorage_LegacySharedDirsOwnerDefaults(t *testing.T) {
	sd := func(uid, gid int) *V1SharedDirStorageConfig {
		return &V1SharedDirStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{
			MountRoot: "/mnt", UID: uid, GID: gid, Shares: []V1NFSShare{{ID: "s1"}},
		}}
	}
	ps, _, err := SynthesizeProjectStorage(nil, nil, sd(0, 0), nil)
	require.NoError(t, err)
	require.NotNil(t, ps)
	require.NotNil(t, ps.LegacySharedDirsNFS)
	assert.Equal(t, 1000, ps.LegacySharedDirsNFS.UID)
	assert.Equal(t, 1000, ps.LegacySharedDirsNFS.GID)

	ps, _, err = SynthesizeProjectStorage(nil, nil, sd(2000, 2001), nil)
	require.NoError(t, err)
	require.NotNil(t, ps.LegacySharedDirsNFS)
	assert.Equal(t, 2000, ps.LegacySharedDirsNFS.UID)
	assert.Equal(t, 2001, ps.LegacySharedDirsNFS.GID)

	layout, err := ProjectStorageLayout(ps, DefaultVolumeMountBase)
	require.NoError(t, err)
	require.NotNil(t, layout.SharedDirs)
	assert.Equal(t, 2000, layout.SharedDirs.UID)
	assert.Equal(t, 2001, layout.SharedDirs.GID)
}
