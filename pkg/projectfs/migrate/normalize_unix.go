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

package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"golang.org/x/sys/unix"
)

// Normalize says how migrated entries are made usable by agents, by the
// Mount they land in (fs-cleanup-arch ruling Q6 on ptone/scion#5819).
type Normalize int

const (
	// NormalizeNone leaves owner and mode as moved.
	NormalizeNone Normalize = iota
	// NormalizeWorkspace chowns migrated entries to the Layout's uid:gid
	// and keeps their modes (the workspace contract is uid-based).
	NormalizeWorkspace
	// NormalizeSharedDir chowns migrated entries to the Layout's uid:gid
	// and applies the shared-dir leaf contract: directories setgid 2775
	// with the default ACL shareddirs.EnsureLeaf sets, files group rw (the
	// hybrid tier's mixed-uid agents share by group).
	NormalizeSharedDir
)

// Ownership is the uid:gid agents run as (the Layout's nfs uid/gid).
type Ownership struct {
	UID, GID int
}

// OwnershipHint is appended to a report whose ownership could not be
// normalised (an export that squashes the hub's root, for example).
const OwnershipHint = "fix ownership by hand as in docs/deploy/hybrid-tier.md (chown to the agent uid:gid, then the fd-based mode/ACL fix-up); a re-run of the migration then reports 0 moves"

// fchownat is unix.Fchownat; a test seam records which entries are chowned.
var fchownat = unix.Fchownat

// normalizer applies Normalize to migrated entries. Failures never stop a
// move: the first one is kept as the Result's OwnershipErr.
type normalizer struct {
	ctx   context.Context
	mode  Normalize
	owner *Ownership
	res   *Result
}

func (n *normalizer) active() bool {
	return n.mode != NormalizeNone && (n.owner != nil || n.mode == NormalizeSharedDir)
}

func (n *normalizer) note(err error) {
	if n.res.OwnershipErr == nil {
		n.res.OwnershipErr = err
	}
}

// entry normalises the migrated entry name in dir (st is its no-follow
// stat).
func (n *normalizer) entry(dir int, name string, st *unix.Stat_t) {
	if !n.active() {
		return
	}
	if n.owner != nil && st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 {
		// A hard-linked file shares its inode with a name outside the
		// migrated set: chowning it would change that file too.
		n.note(fmt.Errorf("%s is hard-linked (%d links); owner left unchanged", name, st.Nlink))
	} else if n.owner != nil {
		if err := fchownat(dir, name, n.owner.UID, n.owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			n.note(fmt.Errorf("chown %s to %d:%d: %w", name, n.owner.UID, n.owner.GID, err))
		}
	}
	if n.mode != NormalizeSharedDir {
		return
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		fd, err := openDirAt(dir, name)
		if err != nil {
			n.note(fmt.Errorf("open %s: %w", name, err))
			return
		}
		defer closeFd(fd)
		if err := shareddirs.ChmodFd(fd, 0o2775); err != nil {
			n.note(fmt.Errorf("chmod %s: %w", name, err))
		}
		if err := shareddirs.SetLeafDefaultACL(fd); err != nil {
			n.note(fmt.Errorf("ACL %s: %w", name, err))
		}
	case unix.S_IFREG:
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			n.note(fmt.Errorf("open %s: %w", name, err))
			return
		}
		defer closeFd(fd)
		if err := unix.Fchmod(fd, uint32(st.Mode&0o777)|0o060); err != nil {
			n.note(fmt.Errorf("chmod %s: %w", name, err))
		}
	}
}

// leaf gives the directory name in dir the Mount leaf contract
// (shareddirs.EnsureLeaf's mode 2775 and default ACL). Used for a tree
// renamed into an absent leaf, which replaces the leaf the owner would
// have created.
func (n *normalizer) leaf(dir int, name string) {
	fd, err := openDirAt(dir, name)
	if err != nil {
		n.note(fmt.Errorf("open %s: %w", name, err))
		return
	}
	defer closeFd(fd)
	if err := shareddirs.ChmodFd(fd, 0o2775); err != nil {
		n.note(fmt.Errorf("chmod %s: %w", name, err))
	}
	if err := shareddirs.SetLeafDefaultACL(fd); err != nil {
		n.note(fmt.Errorf("ACL %s: %w", name, err))
	}
}

// entryAt normalises the entry name in dir by its own (no-follow) stat.
func (n *normalizer) entryAt(dir int, name string) {
	if !n.active() {
		return
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		n.note(fmt.Errorf("stat %s: %w", name, err))
		return
	}
	n.entry(dir, name, &st)
}

// tree normalises a whole renamed tree: the directory name in dir and
// everything below it, walked with no-follow fds (symlinks are chowned as
// links, never followed).
func (n *normalizer) tree(dir int, name string) {
	if !n.active() {
		return
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		n.note(fmt.Errorf("stat %s: %w", name, err))
		return
	}
	n.entry(dir, name, &st)
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		n.below(dir, name, 0)
	}
}

func (n *normalizer) below(dir int, name string, depth int) {
	if depth >= maxDepth {
		n.note(fmt.Errorf("%s: deeper than %d levels; not normalised", name, maxDepth))
		return
	}
	fd, err := openDirAt(dir, name)
	if err != nil {
		n.note(fmt.Errorf("open %s: %w", name, err))
		return
	}
	defer closeFd(fd)
	names, err := readDirNames(fd)
	if err != nil {
		n.note(fmt.Errorf("read %s: %w", name, err))
		return
	}
	for _, child := range names {
		if n.ctx != nil && n.ctx.Err() != nil {
			n.note(fmt.Errorf("%s: stopped: %w", name, n.ctx.Err()))
			return
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, child, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				n.note(fmt.Errorf("stat %s: %w", child, err))
			}
			continue
		}
		n.entry(fd, child, &st)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			n.below(fd, child, depth+1)
		}
	}
}
