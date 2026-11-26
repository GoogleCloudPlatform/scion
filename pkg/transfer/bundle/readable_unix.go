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

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// Well-known NFS anonymous ("nobody" / "nfsnobody") UIDs used by root_squash
// exports: 65534 (16-bit / standard Linux nobody) and 4294967294 (unsigned
// 32-bit -2).
const (
	nfsAnonUID16 uint32 = 65534
	nfsAnonUID32 uint32 = 4294967294

	// maxRootPreLatchProbes bounds the number of non-latching root readability
	// probes (entries owned by UIDs other than ExpectedOwnerUID / NFS anon)
	// before the expected owner latch fires. Past this cap, checkReadable fails
	// closed with fs.ErrPermission so an unusual ownership mix cannot cause
	// O(HomeMaxEntries) opens on NFS.
	maxRootPreLatchProbes = 64
)

func isNFSAnonUID(uid uint32) bool {
	return uid == nfsAnonUID16 || uid == nfsAnonUID32
}

// Injectable identity and probe seams for unit tests (e.g. testing euid 0
// root_squash vs non-squashed exports without requiring root privileges).
var (
	statAccessEUID   = os.Geteuid
	statAccessEGID   = os.Getegid
	statAccessGroups = os.Getgroups
	rootReadProbe    = defaultRootReadProbe
)

func defaultRootReadProbe(root *os.Root, rel string, isDir bool) error {
	if isDir {
		f, err := rootopen.OpenDir(root, rel)
		if err != nil {
			return err
		}
		_ = f.Close()
		return nil
	}
	f, _, err := rootopen.OpenRegularForRead(root, rel, nil)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

type statAccessChecker struct {
	euid             uint32
	egid             uint32
	groups           map[uint32]struct{}
	expectedOwnerUID int
	rootVerified     bool
	preLatchProbes   int
}

func newStatAccessChecker(expectedOwnerUID int) statAccessChecker {
	c := statAccessChecker{
		euid:             uint32(statAccessEUID()),
		egid:             uint32(statAccessEGID()),
		expectedOwnerUID: expectedOwnerUID,
	}
	if gs, err := statAccessGroups(); err == nil && len(gs) > 0 {
		c.groups = make(map[uint32]struct{}, len(gs))
		for _, g := range gs {
			c.groups[uint32(g)] = struct{}{}
		}
	}
	return c
}

func (c *statAccessChecker) canLatchRootVerified(ownerUID uint32) bool {
	if isNFSAnonUID(ownerUID) {
		return false
	}
	if c.expectedOwnerUID > 0 {
		return ownerUID == uint32(c.expectedOwnerUID)
	}
	return true
}

// checkReadable checks whether the current process can read (or traverse+read
// for directories) the entry described by info.
//
// For non-root processes, it performs a zero-open POSIX DAC check against the
// process's effective uid/gid and supplementary groups.
//
// For euid 0 (root), it performs squash detection:
//   - world-readable/traversable entries are accepted with zero opens;
//   - NFS anonymous-owned entries (uid 65534 or 4294967294) that are
//     owner-readable/traversable are accepted without probing or latching,
//     since they are readable whether or not root is squashed;
//   - on the first non-world-readable entry owned by the expected pod UID
//     (when expectedOwnerUID > 0) or any non-anon UID (when expectedOwnerUID
//     == 0), a single real open/opendir probe runs through pkg/util/rootopen;
//   - if that probe succeeds, the export does not squash root, rootVerified is
//     set, and all remaining entries in the walk are accepted without further
//     opens;
//   - entries owned by other UIDs before the latch is set are probed without
//     latching, preventing a custom anonuid on a root_squash export from
//     falsely latching rootVerified;
//   - if any probe fails (e.g. fs.ErrPermission on root_squash), the error is
//     returned immediately without latching.
func (c *statAccessChecker) checkReadable(root *os.Root, rel string, info fs.FileInfo, isDir bool) error {
	if info == nil {
		return nil
	}
	var ownerNeed, groupNeed, otherNeed fs.FileMode
	if isDir {
		ownerNeed, groupNeed, otherNeed = 0o500, 0o050, 0o005
	} else {
		ownerNeed, groupNeed, otherNeed = 0o400, 0o040, 0o004
	}
	perm := info.Mode().Perm()
	st, ok := info.Sys().(*syscall.Stat_t)
	if c.euid == 0 {
		if c.rootVerified || perm&otherNeed == otherNeed {
			return nil
		}
		if ok && isNFSAnonUID(st.Uid) && perm&ownerNeed == ownerNeed {
			return nil
		}
		canLatch := ok && c.canLatchRootVerified(st.Uid)
		if !canLatch && c.preLatchProbes >= maxRootPreLatchProbes {
			return fmt.Errorf("root readability probe limit exceeded: %w", fs.ErrPermission)
		}
		if err := rootReadProbe(root, filepath.FromSlash(rel), isDir); err != nil {
			return err
		}
		if canLatch {
			c.rootVerified = true
		} else {
			c.preLatchProbes++
		}
		return nil
	}
	if !ok {
		return nil
	}
	if st.Uid == c.euid {
		if perm&ownerNeed == ownerNeed {
			return nil
		}
		return fs.ErrPermission
	}
	if st.Gid == c.egid {
		if perm&groupNeed == groupNeed {
			return nil
		}
		return fs.ErrPermission
	}
	if _, inGrp := c.groups[st.Gid]; inGrp {
		if perm&groupNeed == groupNeed {
			return nil
		}
		return fs.ErrPermission
	}
	if perm&otherNeed == otherNeed {
		return nil
	}
	return fs.ErrPermission
}
