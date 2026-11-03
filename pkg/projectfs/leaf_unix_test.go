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

package projectfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func permBits(t *testing.T, path string) uint32 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	// Stat_t.Mode is uint32 on linux and uint16 on darwin.
	return uint32(st.Mode) & 0o7777
}

// On a shared backend EnsureLeaf is the brokers' leaf contract: a new leaf
// (and its missing intermediates) mode 2775, an existing one reported as
// existing, a symlinked component refused.
func TestResolved_EnsureLeaf_Shared(t *testing.T) {
	base := t.TempDir()
	l := delNFSLayout(base)
	r, err := l.Resolve(Project{ID: testPID}, "", SharedDirMount(api.SharedDir{Name: "results"}, ""))
	require.NoError(t, err)

	existed, err := r.EnsureLeaf()
	require.NoError(t, err)
	assert.False(t, existed)
	assert.Equal(t, uint32(0o2775), permBits(t, r.HostPath))

	existed, err = r.EnsureLeaf()
	require.NoError(t, err)
	assert.True(t, existed)

	// A symlinked shared-dirs component is refused, its target untouched.
	other, err := l.Resolve(Project{ID: testOtherPID}, "", SharedDirMount(api.SharedDir{Name: "results"}, ""))
	require.NoError(t, err)
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "projects", testOtherPID), 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(base, "projects", testOtherPID, "shared-dirs")))
	_, err = other.EnsureLeaf()
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(target, "results"))
}

// On the local backend EnsureLeaf creates the directory as before (0755
// before the umask, parents included) and leaves an existing one alone.
func TestResolved_EnsureLeaf_Local(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project-configs", "p__1234", "shared-dirs", "results")
	r := Resolved{Backend: BackendLocal, HostPath: dir}
	existed, err := r.EnsureLeaf()
	require.NoError(t, err)
	assert.False(t, existed)
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
	assert.Zero(t, fi.Mode()&os.ModeSetgid, "no setgid on the local backend")

	existed, err = r.EnsureLeaf()
	require.NoError(t, err)
	assert.True(t, existed)

	_, err = Resolved{Backend: BackendLocal, HostPath: "relative"}.EnsureLeaf()
	require.Error(t, err)
}

// ProjectTreeIDs lists the project roots on the subpath root, skipping
// other entries, and refuses a share that is not mounted.
func TestLayout_ProjectTreeIDs(t *testing.T) {
	base := t.TempDir()
	l := delNFSLayout(base)
	for _, d := range []string{testPID, testOtherPID, ".not-a-project", "-not-a-project"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, "projects", d), 0o755))
	}
	ids, err := l.ProjectTreeIDs()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{testPID, testOtherPID}, ids)

	_, err = delNFSLayout(t.TempDir()).ProjectTreeIDs()
	require.ErrorIs(t, err, ErrNoProjectTree)
	_, err = Layout{Backend: BackendLocal}.ProjectTreeIDs()
	require.Error(t, err)
}

// ProvisionStateDir is <ProjectRoot>/provision on a shared backend.
func TestLayout_ProvisionStateDir(t *testing.T) {
	base := t.TempDir()
	r, err := delNFSLayout(base).ProvisionStateDir(testPID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("projects", testPID, "provision"), r.RelPath)
	assert.Equal(t, filepath.Join(base, "projects", testPID, "provision"), r.HostPath)
	_, err = Layout{Backend: BackendLocal}.ProvisionStateDir(testPID)
	require.Error(t, err)
}
