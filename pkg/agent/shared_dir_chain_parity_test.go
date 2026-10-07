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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedDirChainsParity runs one table through both nfs shared-dir
// chains: agent start (resolveSharedDirs) and the resolver chat plugins
// use (runtime.ResolveSharedDirHostPath). They must apply the same path
// checks, so each case must be refused by both or accepted by both, and
// an accepted case must yield the same host directory.
func TestSharedDirChainsParity(t *testing.T) {
	const shareID = "scion-shared"
	type fixture struct {
		mountRoot string
		projectID string
		name      string
		outside   string
	}
	for _, tc := range []struct {
		desc    string
		refused bool
		// reason is a fragment of the refusal both chains must report,
		// so the case is refused by the intended guard, not a later one.
		reason string
		setup  func(t *testing.T, f *fixture)
	}{
		{desc: "valid shared dir", refused: false, setup: func(t *testing.T, f *fixture) {}},
		{desc: "symlinked leaf", refused: true, reason: "is a symlink", setup: func(t *testing.T, f *fixture) {
			parent := filepath.Join(f.mountRoot, shareID, "projects", f.projectID, "shared-dirs")
			require.NoError(t, os.MkdirAll(parent, 0o775))
			require.NoError(t, os.Symlink(f.outside, filepath.Join(parent, f.name)))
		}},
		{desc: "symlinked project directory", refused: true, reason: "is a symlink", setup: func(t *testing.T, f *fixture) {
			parent := filepath.Join(f.mountRoot, shareID, "projects")
			require.NoError(t, os.MkdirAll(parent, 0o775))
			require.NoError(t, os.Symlink(f.outside, filepath.Join(parent, f.projectID)))
		}},
		{desc: "dot-dot shared dir name", refused: true, reason: "invalid name", setup: func(t *testing.T, f *fixture) {
			f.name = "../../escape"
		}},
		{desc: "bad project id", refused: true, reason: "invalid hub project ID", setup: func(t *testing.T, f *fixture) {
			f.projectID = "../victim"
		}},
		{desc: "unmounted host base", refused: true, reason: "mounted at", setup: func(t *testing.T, f *fixture) {
			require.NoError(t, os.RemoveAll(filepath.Join(f.mountRoot, shareID)))
		}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// Each chain gets its own fresh fixture, so one chain's
			// side effects (a created leaf) cannot affect the other.
			newFixture := func(t *testing.T) *fixture {
				f := &fixture{
					mountRoot: newResolvedTempDir(t),
					projectID: "pid-1",
					name:      "scratchpad",
					outside:   t.TempDir(),
				}
				require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, shareID), 0o755))
				tc.setup(t, f)
				return f
			}
			sdCfgFor := func(f *fixture) *config.V1SharedDirStorageConfig {
				sdCfg := nfsSharedDirStorageCfg(f.mountRoot)
				sdCfg.NFS.Shares[0].ID = shareID
				return sdCfg
			}

			fa := newFixture(t)
			volumes, _, agentErr := resolveSharedDirs(sdCfgFor(fa), "/unused", fa.projectID, "docker",
				[]api.SharedDir{{Name: fa.name}}, "/workspace", false)

			fp := newFixture(t)
			gs := &config.VersionedSettings{Server: &config.V1ServerConfig{SharedDirStorage: sdCfgFor(fp)}}
			got, pluginErr := runtime.ResolveSharedDirHostPath(gs, t.TempDir(), "proj", fp.projectID, fp.name)

			if tc.refused {
				require.Error(t, agentErr, "agent start must refuse")
				require.Error(t, pluginErr, "plugin resolver must refuse")
				assert.Contains(t, agentErr.Error(), tc.reason)
				assert.Contains(t, pluginErr.Error(), tc.reason)
				assert.Empty(t, got.Path)
				assertDirEmpty(t, fa.outside)
				assertDirEmpty(t, fp.outside)
				return
			}
			require.NoError(t, agentErr)
			require.NoError(t, pluginErr)
			require.Len(t, volumes, 1)
			rel := func(base, p string) string {
				r, err := filepath.Rel(base, p)
				require.NoError(t, err)
				return r
			}
			assert.Equal(t, rel(fa.mountRoot, volumes[0].Source), rel(fp.mountRoot, got.Path),
				"both chains must resolve the same directory")
		})
	}
}

// TestResolveSharedDirs_NFS_BackstopRefusesSwappedLeaf replaces the leaf
// with a symlink after the EnsureLeaf walk has finished, which the walk
// itself cannot see, and checks that the fresh re-resolution refuses it.
func TestResolveSharedDirs_NFS_BackstopRefusesSwappedLeaf(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "scion-shared"), 0o755))
	elsewhere := t.TempDir()

	orig := ensureSharedDirLeaf
	t.Cleanup(func() { ensureSharedDirLeaf = orig })
	ensureSharedDirLeaf = func(hostBase, rel string) (int, bool, error) {
		fd, existed, err := orig(hostBase, rel)
		if err != nil {
			return fd, existed, err
		}
		leaf := filepath.Join(hostBase, rel)
		require.NoError(t, os.Rename(leaf, leaf+".moved"))
		require.NoError(t, os.Symlink(elsewhere, leaf))
		return fd, existed, nil
	}

	volumes, realization, err := resolveSharedDirs(nfsSharedDirStorageCfg(mountRoot), "/unused", "pid-1", "docker",
		[]api.SharedDir{{Name: "scratchpad"}}, "/workspace", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves through a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
}
