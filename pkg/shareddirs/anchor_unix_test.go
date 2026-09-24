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

package shareddirs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAnchoredRoot_HappyPath confirms the ordinary case: an existing
// leaf, with no symlinks anywhere in its chain, opens successfully and the
// resulting Root is confined to it.
func TestOpenAnchoredRoot_HappyPath(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "file.txt"), []byte("hi"), 0o644))

	root, err := OpenAnchoredRoot(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad"))
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	data, err := root.ReadFile("file.txt")
	require.NoError(t, err)
	assert.Equal(t, "hi", string(data))
}

// TestOpenAnchoredRoot_MissingLeaf_ReturnsNotExist confirms the contract
// callers rely on: a missing leaf reports an fs.ErrNotExist-compatible
// error (so os.IsNotExist(err) is true), and nothing is created.
func TestOpenAnchoredRoot_MissingLeaf_ReturnsNotExist(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))

	_, err := OpenAnchoredRoot(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "want a not-exist error, got %v", err)

	_, statErr := os.Stat(filepath.Join(hostBase, "projects", "pid-1"))
	assert.True(t, os.IsNotExist(statErr), "OpenAnchoredRoot must never create anything")
}

// TestOpenAnchoredRoot_SymlinkedIntermediate_Refused: a symlinked <pid>
// component must be refused by the open-only walk itself, before
// os.OpenRoot is ever reached, exactly like EnsureLeaf's creation-time walk
// already refuses this shape.
//
// The refusal error must NOT be fs.ErrNotExist-compatible. A missing leaf
// and a refused (symlinked) leaf are different outcomes for a caller:
// openSharedDirRoot's caller in pkg/hub treats os.IsNotExist specially
// (empty listing / 404, matching "nothing here yet"), which would be the
// wrong response to a symlink-swap refusal -- that must surface as a real
// error (500), not a quiet "not found".
func TestOpenAnchoredRoot_SymlinkedIntermediate_Refused(t *testing.T) {
	hostBase := t.TempDir()
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))

	victim := t.TempDir()
	victimLeaf := filepath.Join(victim, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victimLeaf, "secret.txt"), []byte("victim data"), 0o644))

	require.NoError(t, os.Symlink(victim, filepath.Join(projectsDir, "pid-1")))

	_, err := OpenAnchoredRoot(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad"))
	require.Error(t, err, "a symlinked <pid> component must be refused, not traversed")
	assert.False(t, os.IsNotExist(err), "a symlink refusal must not look like a missing leaf, got %v", err)

	info, statErr := os.Stat(filepath.Join(victimLeaf, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestOpenAnchoredRoot_MissingHostBase_ReturnsNotExist: OpenAnchoredRoot's
// contract for a missing hostBase itself (as opposed to
// TestOpenAnchoredRoot_MissingLeaf_ReturnsNotExist's missing leaf under an
// EXISTING hostBase) must also be an fs.ErrNotExist-compatible *os.PathError
// naming hostBase, not some other, unpinned error shape.
func TestOpenAnchoredRoot_MissingHostBase_ReturnsNotExist(t *testing.T) {
	hostBase := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := OpenAnchoredRoot(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "want a not-exist error for a missing host base, got %v", err)

	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr, "want an *os.PathError naming the host base")
	assert.Equal(t, hostBase, pathErr.Path)
}

// TestOpenAnchoredRoot_ComponentSwappedAfterWalk_DevInoMismatchRefused is a
// deterministic component-swap test: <pid> starts as a REAL directory (so
// the walk succeeds and captures
// its genuine identity), and the afterWalkHook seam swaps it for a symlink
// to a victim tree with a matching shared-dirs/scratchpad subtree -- at
// exactly the instant between this call's own walk and os.OpenRoot's fresh
// resolution. os.OpenRoot then successfully opens the VICTIM's directory
// (it genuinely exists at that path once the symlink is in place); only the
// dev/ino comparison against the walk's earlier observation catches the
// substitution. Disabling that check makes this test fail.
func TestOpenAnchoredRoot_ComponentSwappedAfterWalk_DevInoMismatchRefused(t *testing.T) {
	hostBase := t.TempDir()
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))

	pidPath := filepath.Join(projectsDir, "pid-1")
	realLeaf := filepath.Join(pidPath, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(realLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(realLeaf, "keep.txt"), []byte("real"), 0o644))

	victim := t.TempDir()
	victimLeaf := filepath.Join(victim, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victimLeaf, "secret.txt"), []byte("VICTIMSECRET"), 0o600))

	old := afterWalkHook
	afterWalkHook = func() {
		require.NoError(t, os.RemoveAll(pidPath))
		require.NoError(t, os.Symlink(victim, pidPath))
	}
	t.Cleanup(func() { afterWalkHook = old })

	_, err := OpenAnchoredRoot(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad"))
	require.Error(t, err, "a component swapped out from under the walk, between the walk and the open, must be refused")
	assert.Contains(t, err.Error(), "changed between resolution and open")

	got, readErr := os.ReadFile(filepath.Join(victimLeaf, "secret.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "VICTIMSECRET", string(got), "the victim's secret must be untouched")
}
