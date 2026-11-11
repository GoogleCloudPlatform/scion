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

package brokerconvert

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
)

// The host lock keeps a convert operation and a server start apart. The
// write modes of convert hold it for their whole run. A server start
// takes it with a refusing try-lock (never a blocking wait) and holds it
// until its broker listener is bound, then releases it: a convert that
// takes the lock after that point finds the server with its
// server-running check, and one that holds it before makes the start
// refuse. A server that is already running holds nothing.

// ErrHostLockHeld is returned (wrapped) when another process holds the
// host lock.
var ErrHostLockHeld = errors.New("a convert operation or a server start is running on this host")

// HeldByServerStartText is the fixed text a convert operation gives when
// it finds the host lock held: a server start holds it from its check
// until its broker listener is bound, or another convert operation is
// running.
const HeldByServerStartText = "a server start on this host holds the convert lock; wait for it to finish starting, or stop it, and retry (or wait for the other convert operation to finish)"

// ErrConvertInterrupted is returned (wrapped) by AcquireServerStartLock
// when the journal shows a step left "started" by an interrupted run.
var ErrConvertInterrupted = errors.New("a convert operation was interrupted on this host")

// HostLock is the held host lock. Release releases it; it is safe to call
// more than once, from several goroutines, and on a nil lock.
type HostLock struct {
	once sync.Once
	l    *filelock.Lock
}

// Release releases the lock.
func (h *HostLock) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() { h.l.Unlock() })
}

// HostLockPath returns the host lock's path under globalDir.
func HostLockPath(globalDir string) string {
	return filepath.Join(ConvertDir(globalDir), HostLockFileName)
}

// AcquireHostLock takes the host lock under globalDir without waiting. The
// convert directory is created owner-only if missing, and a link at it,
// at runtime-brokers/ or at the lock file is refused. A lock held by
// another process returns an error wrapping ErrHostLockHeld.
func AcquireHostLock(globalDir string) (*HostLock, error) {
	if _, err := openConvertTree(globalDir, true); err != nil {
		return nil, fmt.Errorf("opening the convert directory: %w", err)
	}
	l, err := filelock.Acquire(HostLockPath(globalDir), 0o600, 0)
	if errors.Is(err, filelock.ErrTimeout) {
		return nil, ErrHostLockHeld
	}
	if err != nil {
		return nil, fmt.Errorf("taking the convert host lock: %w", err)
	}
	return &HostLock{l: l}, nil
}

// AcquireServerStartLock is the server start check. It takes the host
// lock without waiting and returns it held; the caller releases it once
// its listeners are bound, or when the start fails. It refuses, with the
// lock released, when:
//   - another process holds the lock (ErrHostLockHeld): a convert
//     operation, or another server start, is running;
//   - the journal shows an item left "started" (ErrConvertInterrupted):
//     resume with 'convert --apply' or undo with 'convert --rollback';
//   - the journal exists but cannot be read privately
//     (ErrJournalUnreadable): it fails closed.
//
// A journal whose items are all pending or done does not refuse: an
// applied journal is the normal switch.
func AcquireServerStartLock(globalDir string) (*HostLock, error) {
	h, err := AcquireHostLock(globalDir)
	if err != nil {
		if errors.Is(err, ErrHostLockHeld) {
			return nil, fmt.Errorf("%w: wait for it to finish, then start the server again", ErrHostLockHeld)
		}
		return nil, err
	}
	j, err := LoadJournal(globalDir)
	if err != nil {
		h.Release()
		return nil, fmt.Errorf("%w; the server does not start until it is readable (it is in %s)", err, ConvertDir(globalDir))
	}
	if j.HasStarted() {
		h.Release()
		return nil, fmt.Errorf("%w: finish it with 'scion runtime-broker convert --apply' or undo it with 'scion runtime-broker convert --rollback', then start the server", ErrConvertInterrupted)
	}
	return h, nil
}
