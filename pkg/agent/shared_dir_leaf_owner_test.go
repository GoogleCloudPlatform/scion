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

//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// resolveSharedDirs hands the shared-dir Layout's nfs uid and gid to the
// leaf walk as the owner a root broker gives the leaves it creates; a leaf
// in that group is then given to the agent as its supplemental group, and
// a root-group leaf is not.
func TestResolveSharedDirs_NFS_LeafOwnerAndAgentGroup(t *testing.T) {
	var owners []shareddirs.Owner
	prevLeaf := ensureSharedDirLeaf
	t.Cleanup(func() { ensureSharedDirLeaf = prevLeaf })
	ensureSharedDirLeaf = func(hostBase, rel string, owner shareddirs.Owner) (int, bool, error) {
		owners = append(owners, owner)
		// Create the leaf without chowning (the test is not root).
		return shareddirs.EnsureLeafOwned(hostBase, rel, shareddirs.Owner{})
	}
	leafGID := uint32(2001)
	prevGID := fdGID
	t.Cleanup(func() { fdGID = prevGID })
	fdGID = func(int) (uint32, error) { return leafGID, nil }

	mountRoot := newResolvedTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "scion-shared"), 0o775))
	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.UID = 2000
	sdCfg.NFS.GID = 2001
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	_, realization, err := resolveSharedDirs(sdLayoutFor(t, sdCfg), false, "/unused", "pid-42", "docker", dirs, "/workspace")
	require.NoError(t, err)
	assert.Equal(t, []shareddirs.Owner{{UID: 2000, GID: 2001}}, owners, "the leaf walk gets the Layout's nfs uid and gid")
	require.NotNil(t, realization)
	assert.Equal(t, []int64{2001}, realization.SupplementalGroups, "a leaf in the nfs gid is the agent's group")

	// A leaf left in the root group (what a root creator without the owner
	// produced) is never given to the agent.
	owners, leafGID = nil, 0
	_, realization, err = resolveSharedDirs(sdLayoutFor(t, sdCfg), false, "/unused", "pid-42", "docker", dirs, "/workspace")
	require.NoError(t, err)
	assert.Empty(t, realization.SupplementalGroups)
}
