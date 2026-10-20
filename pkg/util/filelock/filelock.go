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

// Package filelock provides an exclusive, cross-process advisory lock on a
// file, with a bounded wait.
//
// The lock is flock(2) on a dedicated lock file. It works across processes
// on Linux and macOS, is released by the kernel when the holding process
// exits (even on a crash or kill -9), and is per open file description: two
// Acquire calls on the same path in one process exclude each other, so a
// nested Acquire on a path already held by the same process waits for its
// deadline and fails rather than succeeding.
//
// The lock file is created on first use and never removed: removing it
// while another process waits on it would let a third process lock a new
// file of the same name at the same time.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrTimeout is returned (wrapped) by Acquire when the lock is still held
// by someone else when the wait runs out.
var ErrTimeout = errors.New("timed out waiting for lock")

// ErrNotRegular is returned (wrapped) by Acquire when the lock path exists
// but is not a regular file (for example a symlink or a directory).
var ErrNotRegular = errors.New("lock path is not a regular file")

const (
	minPoll = 2 * time.Millisecond
	maxPoll = 50 * time.Millisecond
)

// Lock is a held lock. Unlock releases it; it is safe to call more than
// once.
type Lock struct {
	f *os.File
	// Created reports whether this Acquire created the lock file, so a
	// caller can do one-time setup (such as a git exclude entry) only then.
	Created bool
}

// Acquire takes an exclusive lock on path, creating the file with mode if
// it does not exist. It does not follow a symlink at path. An existing lock
// file that this user cannot open for writing is opened read-only, which
// locks just the same. It retries until
// wait has passed and then returns an error wrapping ErrTimeout. A wait of
// zero or less makes a single attempt.
func Acquire(path string, mode os.FileMode, wait time.Duration) (*Lock, error) {
	f, created, err := open(path, mode)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	poll := minPoll
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &Lock{f: f, Created: created}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%w %s after %s", ErrTimeout, path, wait)
		}
		time.Sleep(min(poll, remaining))
		poll = min(2*poll, maxPoll)
	}
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
	f, err = os.OpenFile(path, flags, mode)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EROFS) {
		// flock works on a read-only descriptor: an existing lock file this
		// user may not write (created by another user, or on a read-only
		// file system) still locks.
		f, err = os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, false, fmt.Errorf("%w: %s is a symlink", ErrNotRegular, path)
		}
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("stat lock file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, false, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	return f, false, nil
}

// Unlock releases the lock. The lock file is left in place.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
