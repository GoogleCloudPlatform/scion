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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOwnerFileInfo wraps a real fs.FileInfo while overriding the underlying
// syscall.Stat_t UID/GID so unit tests can simulate NFS anon (65534 /
// 4294967294), custom anonuid, and pod UID (1000) ownership without root
// privileges.
type fakeOwnerFileInfo struct {
	fs.FileInfo
	uid uint32
	gid uint32
}

func (f fakeOwnerFileInfo) Sys() any {
	return &syscall.Stat_t{Uid: f.uid, Gid: f.gid}
}

func TestStatAccessChecker_RootAndNonRootCases(t *testing.T) {
	dir := t.TempDir()
	ciWriteFile(t, dir, "pod-private.txt", "secret", 0o600)
	ciWriteFile(t, dir, "world.txt", "public", 0o644)
	ciWriteFile(t, dir, "group-only.txt", "grp", 0o040)
	ciWriteFile(t, dir, "other-only.txt", "oth", 0o004)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "pod-dir"), 0o700))

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
	dirInfo, err := os.Lstat(filepath.Join(dir, "pod-dir"))
	require.NoError(t, err)

	origEUID, origEGID, origGroups, origProbe := statAccessEUID, statAccessEGID, statAccessGroups, rootReadProbe
	t.Cleanup(func() {
		statAccessEUID = origEUID
		statAccessEGID = origEGID
		statAccessGroups = origGroups
		rootReadProbe = origProbe
	})

	t.Run("root on root_squash export refuses 0600 file", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		probes := 0
		rootReadProbe = func(_ *os.Root, _ string, _ bool) error {
			probes++
			return fs.ErrPermission
		}

		c := newStatAccessChecker(0)
		assert.ErrorIs(t, c.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)
		assert.Equal(t, 1, probes, "non-world-readable 0600 file must be probed and refused on root_squash")
	})

	t.Run("root on root_squash with anon 0600 file before pod 0600 file refuses pod file", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }

		const podUID uint32 = 1000
		anon16Info := fakeOwnerFileInfo{FileInfo: privInfo, uid: nfsAnonUID16, gid: nfsAnonUID16}
		anon32Info := fakeOwnerFileInfo{FileInfo: privInfo, uid: nfsAnonUID32, gid: nfsAnonUID32}
		podInfo := fakeOwnerFileInfo{FileInfo: privInfo, uid: podUID, gid: podUID}

		for _, expectedUID := range []int{0, int(podUID)} {
			t.Run(fmt.Sprintf("expectedOwnerUID=%d", expectedUID), func(t *testing.T) {
				var probed []string
				rootReadProbe = func(_ *os.Root, rel string, _ bool) error {
					probed = append(probed, rel)
					if rel == "a-anon16.txt" || rel == "a-anon32.txt" {
						// Under root_squash, root maps to anon so anon-owned 0600 opens fine.
						return nil
					}
					return fs.ErrPermission
				}

				c := newStatAccessChecker(expectedUID)
				// Anon-owned 0600 entries are accepted without probing or latching.
				require.NoError(t, c.checkReadable(root, "a-anon16.txt", anon16Info, false))
				require.NoError(t, c.checkReadable(root, "a-anon32.txt", anon32Info, false))
				assert.Empty(t, probed, "anon-owned owner-readable entries must be accepted with 0 opens")

				// Subsequent pod-owned 0600 entry must be probed and refused.
				err := c.checkReadable(root, "b-pod.txt", podInfo, false)
				assert.ErrorIs(t, err, fs.ErrPermission)
				assert.Equal(t, []string{"b-pod.txt"}, probed, "pod-owned 0600 entry must be probed after anon-owned entries")
			})
		}
	})

	t.Run("root on root_squash with custom anonuid 0600 file before pod 0600 file refuses pod file when expectedOwnerUID is set", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }

		const (
			podUID        uint32 = 1000
			customAnonUID uint32 = 9999
		)
		customAnonInfo := fakeOwnerFileInfo{FileInfo: privInfo, uid: customAnonUID, gid: customAnonUID}
		podInfo := fakeOwnerFileInfo{FileInfo: privInfo, uid: podUID, gid: podUID}

		var probed []string
		rootReadProbe = func(_ *os.Root, rel string, _ bool) error {
			probed = append(probed, rel)
			if rel == "a-custom-anon.txt" {
				// Under root_squash with anonuid=9999, root maps to 9999 so this opens fine.
				return nil
			}
			return fs.ErrPermission
		}

		c := newStatAccessChecker(int(podUID))
		require.NoError(t, c.checkReadable(root, "a-custom-anon.txt", customAnonInfo, false))
		err := c.checkReadable(root, "b-pod.txt", podInfo, false)
		assert.ErrorIs(t, err, fs.ErrPermission)
		assert.Equal(t, []string{"a-custom-anon.txt", "b-pod.txt"}, probed, "custom anonuid probe must not latch rootVerified when expectedOwnerUID is set")
	})

	t.Run("root where every entry is world-readable allows with 0 opens", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		probes := 0
		rootReadProbe = func(_ *os.Root, _ string, _ bool) error {
			probes++
			return nil
		}

		c := newStatAccessChecker(0)
		for i := 0; i < 10; i++ {
			assert.NoError(t, c.checkReadable(root, fmt.Sprintf("world-%d.txt", i), worldInfo, false))
		}
		assert.Equal(t, 0, probes, "world-readable entries must be accepted with 0 opens")
	})

	t.Run("root with 2001 readable 0600 files on no_root_squash allows with exactly 1 open", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		probes := 0
		rootReadProbe = func(_ *os.Root, _ string, _ bool) error {
			probes++
			return nil
		}

		const podUID uint32 = 1000
		podInfo := fakeOwnerFileInfo{FileInfo: privInfo, uid: podUID, gid: podUID}
		c := newStatAccessChecker(int(podUID))
		for i := 0; i < 2005; i++ {
			require.NoError(t, c.checkReadable(root, fmt.Sprintf("priv-%04d.txt", i), podInfo, false))
		}
		assert.Equal(t, 1, probes, "squash detection must perform exactly 1 open across 2001+ readable 0600 files")
	})

	t.Run("root with directory-first 0700 pod dir latches via OpenDir and accepts subsequent 0600 files", func(t *testing.T) {
		statAccessEUID = func() int { return 0 }
		statAccessEGID = func() int { return 0 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		var probed []string
		rootReadProbe = func(_ *os.Root, rel string, isDir bool) error {
			probed = append(probed, fmt.Sprintf("%s:%v", rel, isDir))
			return nil
		}

		const podUID uint32 = 1000
		podDirInfo := fakeOwnerFileInfo{FileInfo: dirInfo, uid: podUID, gid: podUID}
		podFileInfo := fakeOwnerFileInfo{FileInfo: privInfo, uid: podUID, gid: podUID}
		c := newStatAccessChecker(int(podUID))
		require.NoError(t, c.checkReadable(root, "pod-dir", podDirInfo, true))
		require.NoError(t, c.checkReadable(root, "pod-dir/a.txt", podFileInfo, false))
		require.NoError(t, c.checkReadable(root, "pod-dir/b.txt", podFileInfo, false))
		assert.Equal(t, []string{"pod-dir:true"}, probed)
	})

	t.Run("non-root owner, group, and other precedence", func(t *testing.T) {
		fileUID := os.Geteuid()
		fileGID := os.Getegid()

		// Owner match: 0600 allowed; 0040 denied (owner bits are authoritative, no group fallback).
		statAccessEUID = func() int { return fileUID }
		statAccessEGID = func() int { return fileGID }
		statAccessGroups = func() ([]int, error) { return []int{fileGID}, nil }
		cOwner := newStatAccessChecker(0)
		assert.NoError(t, cOwner.checkReadable(root, "pod-private.txt", privInfo, false))
		assert.ErrorIs(t, cOwner.checkReadable(root, "group-only.txt", grpInfo, false), fs.ErrPermission)

		// Non-owner, matching group (via supplementary group): 0040 allowed, 0600 denied.
		statAccessEUID = func() int { return fileUID + 1 }
		statAccessEGID = func() int { return fileGID + 1 }
		statAccessGroups = func() ([]int, error) { return []int{fileGID}, nil }
		cGroup := newStatAccessChecker(0)
		assert.NoError(t, cGroup.checkReadable(root, "group-only.txt", grpInfo, false))
		assert.ErrorIs(t, cGroup.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)

		// Non-owner, non-group (other): 0004 allowed, 0040 and 0600 denied.
		statAccessEUID = func() int { return fileUID + 1 }
		statAccessEGID = func() int { return fileGID + 1 }
		statAccessGroups = func() ([]int, error) { return nil, nil }
		cOther := newStatAccessChecker(0)
		assert.NoError(t, cOther.checkReadable(root, "other-only.txt", othInfo, false))
		assert.ErrorIs(t, cOther.checkReadable(root, "group-only.txt", grpInfo, false), fs.ErrPermission)
		assert.ErrorIs(t, cOther.checkReadable(root, "pod-private.txt", privInfo, false), fs.ErrPermission)
	})
}
