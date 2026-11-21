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

package hub

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// workspaceBeforeOpenHook, when set (tests only), runs immediately before
// openWorkspaceRegularForRead and openWorkspaceFileForWrite open name.
var workspaceBeforeOpenHook atomic.Pointer[func(name string)]

func openatNoIntr(dirfd int, name string, flags int, mode uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags, mode)
		if err != unix.EINTR {
			return fd, err
		}
	}
}

// openAtInRoot opens the slash-separated relative path name under dir using
// kernel-enforced openat flags. Intermediate directory components are walked
// with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC and the leaf is opened with
// flags|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC. Unlike (*os.Root).OpenFile, which
// resolves in-root symlinks even when O_NOFOLLOW is set, this refuses any
// symlink component directly in the kernel.
func openAtInRoot(dir *os.Root, name string, flags int, perm uint32) (*os.File, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("%w: invalid workspace path", errWorkspaceWriteRefused)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("%w: invalid workspace path", errWorkspaceWriteRefused)
		}
	}

	rootDir, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rootDir.Close() }()

	rawConn, err := rootDir.SyscallConn()
	if err != nil {
		return nil, err
	}

	leafFD := -1
	var openErr error
	ctrlErr := rawConn.Control(func(rootFD uintptr) {
		curFD := int(rootFD)
		ownedFD := -1
		defer func() {
			if ownedFD >= 0 {
				_ = unix.Close(ownedFD)
			}
		}()

		const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		for _, comp := range parts[:len(parts)-1] {
			nextFD, err := openatNoIntr(curFD, comp, dirFlags, 0)
			if err != nil {
				openErr = err
				return
			}
			if ownedFD >= 0 {
				_ = unix.Close(ownedFD)
			}
			curFD = nextFD
			ownedFD = nextFD
		}

		leaf := parts[len(parts)-1]
		fd, err := openatNoIntr(curFD, leaf, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, perm)
		if err != nil {
			openErr = err
			return
		}
		leafFD = fd
	})
	if ctrlErr != nil {
		if leafFD >= 0 {
			_ = unix.Close(leafFD)
		}
		return nil, ctrlErr
	}
	if openErr != nil {
		return nil, &fs.PathError{Op: "openat", Path: name, Err: openErr}
	}
	return os.NewFile(uintptr(leafFD), name), nil
}

// openWorkspaceRegularForRead opens name under dir read-only without blocking
// on special files or following any symlink component, then verifies that the
// opened descriptor is a regular file matching expected (when non-nil).
func openWorkspaceRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	f, err := openAtInRoot(dir, name, unix.O_RDONLY, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ENXIO), errors.Is(err, unix.EOPNOTSUPP):
			return nil, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
		default:
			return nil, nil, err
		}
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || (expected != nil && !os.SameFile(info, expected)) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
	}
	return f, info, nil
}

// openWorkspaceFileForWrite opens name under dir for writing without truncating,
// without blocking on special files, and without following any symlink component.
// When exists is false, O_CREAT|O_EXCL ensures an entry appearing in between
// is refused rather than opened or followed.
func openWorkspaceFileForWrite(dir *os.Root, name string, exists bool) (*os.File, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	flags := unix.O_WRONLY
	if !exists {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	f, err := openAtInRoot(dir, name, flags, 0o644)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			return nil, fmt.Errorf("%w: target appeared while opening it", errWorkspaceWriteRefused)
		case errors.Is(err, unix.ENXIO), errors.Is(err, unix.ELOOP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EISDIR):
			return nil, fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
		default:
			return nil, err
		}
	}
	return f, nil
}
