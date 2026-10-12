// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// hostStateNoFollowParent is passed as agentinfolock.Options.NoFollowParent.
// UpdateHostState gives agentinfolock the lock path below the resolved
// root, so the walk from / meets no symlink above the root. A symlink
// below the root has already been refused by openHostStateDir before the
// lock is taken.
const hostStateNoFollowParent = true

// hostStateDir is agentDir opened once: the resolved trusted root, then
// each component below it with openat(O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK). Every later
// operation on HostStateFile (open, temp create, rename) goes through the
// fd, so a symlink swapped into the path afterwards is not followed either.
type hostStateDir struct {
	fd   int
	path string // agentDir, for messages
}

// openHostStateDir opens l.root (already resolved, so no symlink is met)
// and walks l.rel without following a symlink. A symlink in any component
// below the root makes it fail. A missing component gives an error that
// matches fs.ErrNotExist.
func openHostStateDir(l hostStateLayout) (*hostStateDir, error) {
	// O_NONBLOCK keeps a FIFO component from blocking the open on a
	// platform where O_DIRECTORY does not refuse it first.
	const dirFlags = unix.O_DIRECTORY | unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Open(l.root, dirFlags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: l.root, Err: err}
	}
	cur := l.root
	for i, name := range l.rel {
		cur = filepath.Join(cur, name)
		child, err := unix.Openat(fd, name, dirFlags|unix.O_NOFOLLOW, 0)
		if err != nil {
			mode, statOK := lstatModeAt(fd, name)
			_ = unix.Close(fd)
			return nil, hostStateComponentErr(cur, i, mode, statOK, err)
		}
		_ = unix.Close(fd)
		fd = child
	}
	return &hostStateDir{fd: fd, path: cur}, nil
}

func (d *hostStateDir) close() { _ = unix.Close(d.fd) }

// lstatModeAt returns the file type of name in dirFd without following a
// symlink, as an fs.FileMode type (permission bits are not set).
func lstatModeAt(dirFd int, name string) (fs.FileMode, bool) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, false
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		return fs.ModeDir, true
	case unix.S_IFREG:
		return 0, true
	case unix.S_IFLNK:
		return fs.ModeSymlink, true
	default:
		return fs.ModeIrregular, true
	}
}

// openFile opens HostStateFile read-only, refusing a symlink at it
// (O_NOFOLLOW, and O_NONBLOCK so a FIFO cannot block the open).
//
// The errno for O_NOFOLLOW on a symlink differs by system (ELOOP on Linux
// and macOS, EMLINK on FreeBSD and DragonFly, EFTYPE on NetBSD), so after
// any open error an fstatat decides whether the entry is a symlink, and
// the error wraps errHostStateSymlink on every unix.
func (d *hostStateDir) openFile() (*os.File, error) {
	f, err := dirfd.OpenAt(d.fd, HostStateFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if mode, ok := lstatModeAt(d.fd, HostStateFile); ok && mode&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", errHostStateSymlink, filepath.Join(d.path, HostStateFile))
		}
		return nil, &os.PathError{Op: "open", Path: filepath.Join(d.path, HostStateFile), Err: err}
	}
	return f, nil
}

// writeFile writes data to HostStateFile with dirfd.WriteAtNoFollowWithChown
// on the agentDir fd: an O_EXCL|O_NOFOLLOW temp file, chmod 0600, fsync,
// then renameat over the leaf (ReplaceLeaf replaces the entry and never
// writes through it; the read under the same update has already refused a
// symlink or non-regular file there). uid -1 skips the chown.
//
// The helper names the temp file by pid and nanosecond time and does not
// retry. Two goroutines in one process can only race for a name when the
// update runs unlocked: the lock file is unusable, or another holder keeps
// the lock past the wait. The loser's O_EXCL create then fails with EEXIST
// and it writes nothing, so a collision fails that update (the error wraps
// EEXIST) and never corrupts the file.
func (d *hostStateDir) writeFile(data []byte) error {
	return dirfd.WriteAtNoFollowWithChown(d.fd, HostStateFile, filepath.Join(d.path, HostStateFile),
		data, 0o600, -1, -1, dirfd.ReplaceLeaf, syscall.Fchown)
}
