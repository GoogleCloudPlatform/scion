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
	"syscall"
)

type statAccessChecker struct {
	euid   uint32
	egid   uint32
	groups map[uint32]struct{}
}

func newStatAccessChecker() statAccessChecker {
	c := statAccessChecker{
		euid: uint32(os.Geteuid()),
		egid: uint32(os.Getegid()),
	}
	if gs, err := os.Getgroups(); err == nil && len(gs) > 0 {
		c.groups = make(map[uint32]struct{}, len(gs))
		for _, g := range gs {
			c.groups[uint32(g)] = struct{}{}
		}
	}
	return c
}

// checkReadable performs a stat-only POSIX DAC readability check against the
// current process's effective uid/gid and supplementary groups without issuing
// an open/close syscall per file. For directories, both read and traverse
// (execute) bits are required; for regular files, read bit is required.
func (c statAccessChecker) checkReadable(info fs.FileInfo, isDir bool) error {
	if c.euid == 0 || info == nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	var ownerNeed, groupNeed, otherNeed fs.FileMode
	if isDir {
		ownerNeed, groupNeed, otherNeed = 0o500, 0o050, 0o005
	} else {
		ownerNeed, groupNeed, otherNeed = 0o400, 0o040, 0o004
	}
	perm := info.Mode().Perm()
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
