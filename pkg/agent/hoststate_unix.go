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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// hostStateNoFollowParent is passed as agentinfolock.Options.NoFollowParent.
// UpdateHostState gives agentinfolock the lock path below the resolved
// root, so the walk from / meets no symlink above the root, and a symlink
// below it makes the update run unlocked (and then fail in the read).
const hostStateNoFollowParent = true

// hostStateDir is agentDir opened once: the resolved trusted root, then
// each component below it with openat(O_DIRECTORY|O_NOFOLLOW). Every later
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
	const dirFlags = unix.O_DIRECTORY | unix.O_RDONLY | unix.O_CLOEXEC
	fd, err := unix.Open(l.root, dirFlags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: l.root, Err: err}
	}
	cur := l.root
	for _, name := range l.rel {
		cur = filepath.Join(cur, name)
		child, err := unix.Openat(fd, name, dirFlags|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("host state dir: %w", &os.PathError{Op: "open", Path: cur, Err: err})
		}
		fd = child
	}
	return &hostStateDir{fd: fd, path: cur}, nil
}

func (d *hostStateDir) close() { _ = unix.Close(d.fd) }

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
		var st unix.Stat_t
		if serr := unix.Fstatat(d.fd, HostStateFile, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, fmt.Errorf("%w: %s", errHostStateSymlink, filepath.Join(d.path, HostStateFile))
		}
		return nil, &os.PathError{Op: "open", Path: filepath.Join(d.path, HostStateFile), Err: err}
	}
	return f, nil
}

// writeFile writes data to HostStateFile through a temp file created with
// O_EXCL|O_NOFOLLOW under a random name in agentDir, then renameat. The
// rename replaces a symlink at HostStateFile rather than writing through
// it. Each write is atomic, and parallel writers never share a temp file.
func (d *hostStateDir) writeFile(data []byte) error {
	var (
		tmp  *os.File
		name string
		err  error
	)
	for i := 0; i < 10; i++ {
		var b [8]byte
		if _, err = rand.Read(b[:]); err != nil {
			return err
		}
		name = "." + HostStateFile + "." + hex.EncodeToString(b[:]) + ".tmp"
		tmp, err = dirfd.CreateExclAt(d.fd, name, 0o600)
		if !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("host state temp file in %s: %w", d.path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = dirfd.UnlinkAt(d.fd, name)
		}
	}()
	// Set the mode explicitly; the create mode is reduced by the umask.
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := dirfd.RenameAt(d.fd, name, HostStateFile); err != nil {
		return fmt.Errorf("host state file %s: rename: %w", filepath.Join(d.path, HostStateFile), err)
	}
	ok = true
	return nil
}
