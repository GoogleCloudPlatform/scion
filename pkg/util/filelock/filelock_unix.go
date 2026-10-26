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

package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock makes one non-blocking flock attempt. held reports that someone
// else holds the lock.
func tryLock(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// open opens path for locking, creating it if needed. created reports
// whether this call created it.
func open(path string, mode os.FileMode) (f *os.File, created bool, err error) {
	const flags = os.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	f, err = os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, mode)
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, fmt.Errorf("create lock file: %w", err)
	}
	f, err = openExisting(path)
	return f, false, err
}

// openExisting opens the existing lock file path, never following a
// symlink, and refuses anything but a regular file.
func openExisting(path string) (*os.File, error) {
	const flags = os.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	f, err := os.OpenFile(path, flags, 0)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EROFS) {
		// flock works on a read-only descriptor: an existing lock file this
		// user may not write (created by another user, or on a read-only
		// file system) still locks.
		f, err = os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrNotRegular, path)
		}
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat lock file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	return f, nil
}
