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
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

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
	euid         uint32
	egid         uint32
	groups       map[uint32]struct{}
	rootVerified bool
}

func newStatAccessChecker() statAccessChecker {
	c := statAccessChecker{
		euid: uint32(statAccessEUID()),
		egid: uint32(statAccessEGID()),
	}
	if gs, err := statAccessGroups(); err == nil && len(gs) > 0 {
		c.groups = make(map[uint32]struct{}, len(gs))
		for _, g := range gs {
			c.groups[uint32(g)] = struct{}{}
		}
	}
	return c
}

// checkReadable checks whether the current process can read (or traverse+read
// for directories) the entry described by info.
//
// For non-root processes, it performs a zero-open POSIX DAC check against the
// process's effective uid/gid and supplementary groups.
//
// For euid 0 (root), it performs squash detection with at most one open per
// walk:
//   - world-readable/traversable entries are accepted with zero opens;
//   - on the first non-world-readable entry, a single real open/opendir probe
//     runs through pkg/util/rootopen;
//   - if that probe succeeds, the export does not squash root, rootVerified is
//     set, and all remaining entries in the walk are accepted without further
//     opens;
//   - if that probe fails (e.g. fs.ErrPermission on root_squash), the error is
//     returned immediately.
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
	if c.euid == 0 {
		if c.rootVerified || perm&otherNeed == otherNeed {
			return nil
		}
		if err := rootReadProbe(root, filepath.FromSlash(rel), isDir); err != nil {
			return err
		}
		c.rootVerified = true
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
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
