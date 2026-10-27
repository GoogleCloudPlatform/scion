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
// On Linux and macOS the lock is flock(2) on a dedicated lock file; on
// Windows it is LockFileEx on one byte of it. Either way it works across
// processes, is released by the kernel when the holding process exits
// (even on a crash or kill -9), and is per open file (description): two
// Acquire calls on the same path in one process exclude each other, so a
// nested Acquire on a path already held by the same process waits for its
// deadline and fails rather than succeeding.
//
// The lock file is created on first use and never removed: removing it
// while another process waits on it would let a third process lock a new
// file of the same name at the same time. On Windows Acquire enforces this
// by not sharing delete access on its handle.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrTimeout is returned (wrapped) by Acquire when the lock is still held
// by someone else when the wait runs out.
var ErrTimeout = errors.New("timed out waiting for lock")

// ErrNotRegular is returned (wrapped) by Acquire when the lock path exists
// but is not a regular file (for example a symlink, a junction or a
// directory).
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
// it does not exist. It does not follow a symlink (or, on Windows, any
// reparse point) at path. An existing lock file that this user cannot open
// for writing is opened read-only, which locks just the same. It retries
// until wait has passed and then returns an error wrapping ErrTimeout. A
// wait of zero or less makes a single attempt.
//
// On Windows a mode granting nothing to group or others gives a new lock
// file the owner-only security descriptor of privatefs (privatefs.ForMode);
// any other mode leaves it the parent directory's inherited ACL.
func Acquire(path string, mode os.FileMode, wait time.Duration) (*Lock, error) {
	f, created, err := open(path, mode)
	if err != nil {
		return nil, err
	}
	l, err := LockFile(f, wait)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l.Created = created
	return l, nil
}

// LockFile takes an exclusive lock on the already open file f, retrying
// until wait has passed as Acquire does. Acquire is built on it.
//
// The caller owns the file's lifetime; do not use for mutual exclusion on
// a shared name. LockFile does not check what f is, and whoever opened f
// chose its sharing: on Windows a handle that shares delete access lets
// another process delete the name while the lock is held, after which a
// third process can create and lock a new file of the same name. Use it
// only for a lock that is a liveness signal on a name nobody else
// re-acquires (such as tunnelclient's per-process lock files).
//
// On success the Lock owns f: Unlock releases the lock and closes f. On
// error f is left open and is still the caller's.
func LockFile(f *os.File, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	poll := minPoll
	for {
		held, err := tryLock(f)
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		if !held {
			return &Lock{f: f}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w %s after %s", ErrTimeout, f.Name(), wait)
		}
		time.Sleep(min(poll, remaining))
		poll = min(2*poll, maxPoll)
	}
}

// Unlock releases the lock and closes the lock file's handle. The lock
// file is left in place.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
