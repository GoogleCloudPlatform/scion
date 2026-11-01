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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An empty host base (only the mount point, or an emptied share) is
// ErrShareEmpty, and still ErrNoProjectTree for the delete paths.
func TestCheckTreeMounted_EmptyHostBase(t *testing.T) {
	err := delNFSLayout(t.TempDir()).CheckTreeMounted()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShareEmpty), "err: %v", err)
	assert.True(t, errors.Is(err, ErrNoProjectTree), "err: %v", err)
	assert.True(t, errors.Is(err, ErrRefused), "err: %v", err)
}

// A host base with entries but no subpath root is ErrNoProjectTree, not
// ErrShareEmpty.
func TestCheckTreeMounted_MissingSubPathRoot(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "other"), 0o755))
	err := delNFSLayout(base).CheckTreeMounted()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoProjectTree), "err: %v", err)
	assert.False(t, errors.Is(err, ErrShareEmpty), "err: %v", err)
}

// A present subpath root is mounted.
func TestCheckTreeMounted_SubPathRootPresent(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "projects"), 0o755))
	assert.NoError(t, delNFSLayout(base).CheckTreeMounted())
}

// A symlinked subpath root is refused (never followed), not read as missing
// or empty.
func TestCheckTreeMounted_SymlinkedSubPathRootRefused(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(base, "projects")))
	err := delNFSLayout(base).CheckTreeMounted()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRefused), "err: %v", err)
	assert.False(t, errors.Is(err, ErrNoProjectTree), "err: %v", err)
	assert.False(t, errors.Is(err, ErrShareEmpty), "err: %v", err)
}

// A host base that cannot be read is an error, never "empty" (and never a
// missing tree): the hub reads it as "could not be checked".
func TestCheckTreeMounted_UnreadableHostBaseIsNotEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o000))
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })
	err := delNFSLayout(base).CheckTreeMounted()
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrShareEmpty), "err: %v", err)
	assert.False(t, errors.Is(err, ErrNoProjectTree), "err: %v", err)
}
