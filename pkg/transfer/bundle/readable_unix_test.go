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

package bundle

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatAccessChecker_RootAndNonRootCases(t *testing.T) {
	dir := t.TempDir()
	ciWriteFile(t, dir, "pod-private.txt", "secret", 0o600)
	ciWriteFile(t, dir, "world.txt", "public", 0o644)
	ciWriteFile(t, dir, "group-only.txt", "grp", 0o040)
	ciWriteFile(t, dir, "other-only.txt", "oth", 0o004)

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	privInfo, err := os.Lstat(filepath.Join(dir, "pod-private.txt"))
	require.NoError(t, err)
	worldInfo, err := os.Lstat(filepath.Join(dir, "world.txt"))
	require.NoError(t, err)
	grpInfo, err := os.Lstat(filepath.Join(dir, "group-only.txt"))
	require.NoError(t, err)
	othInfo, err := os.Lstat(filepath.Join(dir, "other-only.txt"))
	require.NoError(t, err)

	origEUID, origEGID, origGroups, origProbe := statAccessEUID, statAccessEGID, statAccessGroups, rootReadProbe
	t.Cleanup(func() {
		statAccessEUID = origEUID
		statAccessEGID = origEGID
		statAccessGroups = origGroups
		rootReadProbe = origProbe
	})

	t.Run("root on root_squash export refuses 0600 file and skips open for 0644", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		probes := 0
		rootReadProbe = func(_ *os.Root, _ string, _ bool) error {
			probes++
			return fs.ErrPermission
		}

		c := newStatAccessChecker()
		assert.NoError(t, c.checkReadable(root, "world.txt", worldInfo, false))
		assert.Equal(t, 0, probes, "world-readable file must not consume a root probe")
		assert.ErrorIs(t, c.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)
		assert.Equal(t, 1, probes, "non-world-readable 0600 file must be probed and refused on root_squash")
	})

	t.Run("root on non-squashed export allows 0600 file up to cap then falls back to other bits", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		probes := 0
		rootReadProbe = func(_ *os.Root, _ string, _ bool) error {
			probes++
			return nil
		}

		c := newStatAccessChecker()
		assert.NoError(t, c.checkReadable(root, "pod-private.txt", privInfo, false))
		assert.Equal(t, 1, probes)

		// At the probe cap, root falls back to other-read bits (refusing 0600, allowing 0644).
		c.rootProbes = maxRootReadProbes
		assert.ErrorIs(t, c.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)
		assert.NoError(t, c.checkReadable(root, "world.txt", worldInfo, false))
		assert.Equal(t, 1, probes, "must not probe past maxRootReadProbes")
	})

	t.Run("non-root owner, group, and other precedence", func(t *testing.T) {
		fileUID := os.Geteuid()
		fileGID := os.Getegid()

		// Owner match: 0600 allowed; 0040 denied (owner bits are authoritative, no group fallback).
		statAccessEUID = func() int { return fileUID }
		statAccessEGID = func() int { return fileGID }
		statAccessGroups = func() ([]int, error) { return []int{fileGID}, nil }
		cOwner := newStatAccessChecker()
		assert.NoError(t, cOwner.checkReadable(root, "pod-private.txt", privInfo, false))
		assert.ErrorIs(t, cOwner.checkReadable(root, "group-only.txt", grpInfo, false), fs.ErrPermission)

		// Non-owner, matching group (via supplementary group): 0040 allowed, 0600 denied.
		statAccessEUID = func() int { return fileUID + 1 }
		statAccessEGID = func() int { return fileGID + 1 }
		statAccessGroups = func() ([]int, error) { return []int{fileGID}, nil }
		cGroup := newStatAccessChecker()
		assert.NoError(t, cGroup.checkReadable(root, "group-only.txt", grpInfo, false))
		assert.ErrorIs(t, cGroup.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)

		// Non-owner, non-group (other): 0004 allowed, 0040 and 0600 denied.
		statAccessEUID = func() int { return fileUID + 1 }
		statAccessEGID = func() int { return fileGID + 1 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		cOther := newStatAccessChecker()
		assert.NoError(t, cOther.checkReadable(root, "other-only.txt", othInfo, false))
		assert.ErrorIs(t, cOther.checkReadable(root, "group-only.txt", grpInfo, false), fs.ErrPermission)
		assert.ErrorIs(t, cOther.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)
	})
}
