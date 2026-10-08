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

package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// ErrSessionStateRefused reports that CloseOpenSession found something other
// than a plain regular file at the state or lock path (a symlink, FIFO,
// hard link, directory, ...) and refused to use it.
var ErrSessionStateRefused = errors.New("session metrics state refused")

// CloseOpenSession is the init daemon's shutdown backstop for sessions whose
// session-end was never handled: the container was stopped, the harness was
// killed, or the harness has no session-end hook.
//
// Under the same lock the hook processes use, it reads the state file. If
// it holds an open session, CloseOpenSession finalizes it with errMsg (empty
// for a clean exit, which yields status "completed"; otherwise "error"),
// replaces the file's content with a closed tombstone, and returns the
// summary with ok=true. The caller then reports it. The tombstone makes
// Update ignore later hook events for that session, so a session-end hook
// still in flight cannot report the session again. If the hook process
// finished the session first, the file is gone and ok is false: a session
// is reported once, by whichever side finalizes it.
//
// The caller runs as root and the state directory belongs to the workload,
// so nothing here follows a symlink: every directory component is opened
// with O_NOFOLLOW from "/" down (dirfd.OpenParentNoFollow), the lock and
// state files are opened O_NOFOLLOW|O_NONBLOCK so a planted FIFO cannot
// block, and each must be a regular file with a single link. The lock file
// is never created here, because a root-owned lock file would lock the hook
// processes out; without a lock file no hook ever saved state. The tombstone
// is written in place through the already-checked descriptor, so the file
// keeps its workload ownership. A missing file is ok=false with a nil error.
func (s *FileSessionState) CloseOpenSession(errMsg string) (telemetry.SessionSummary, bool, error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return telemetry.SessionSummary{}, false, nil
		}
		return telemetry.SessionSummary{}, false, refusedIfLoop(err)
	}
	defer func() { _ = syscall.Close(dirFd) }()

	lockFile, err := openRegularNoFollowAt(dirFd, leaf+".lock", syscall.O_RDONLY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return telemetry.SessionSummary{}, false, nil
		}
		return telemetry.SessionSummary{}, false, err
	}
	unlock, err := flockWait(lockFile, s.lockWait())
	if err != nil {
		return telemetry.SessionSummary{}, false, err
	}
	defer unlock()

	f, err := openRegularNoFollowAt(dirFd, leaf, syscall.O_RDWR)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already finalized and reported by the session-end hook.
			return telemetry.SessionSummary{}, false, nil
		}
		return telemetry.SessionSummary{}, false, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, sessionStateMaxBytes+1))
	if err != nil {
		return telemetry.SessionSummary{}, false, fmt.Errorf("reading %s: %w", s.Path, err)
	}
	file, err := decodeSessionState(data)
	if err != nil {
		return telemetry.SessionSummary{}, false, fmt.Errorf("%s %v", s.Path, err)
	}
	if file.Closed || !file.Aggregator.Open {
		return telemetry.SessionSummary{}, false, nil
	}

	agg := telemetry.NewAggregator()
	agg.RestoreState(file.Aggregator)
	summary := agg.Finalize(0, 0, 0, 0, errMsg)

	tombstone, err := json.Marshal(sessionStateFile{
		Version: sessionStateVersion,
		Aggregator: telemetry.AggregatorState{
			SessionID: summary.SessionID,
			StartedAt: summary.StartedAt,
		},
		Closed: true,
	})
	if err != nil {
		return telemetry.SessionSummary{}, false, fmt.Errorf("encoding tombstone: %w", err)
	}
	if err := writeInPlace(f, tombstone); err != nil {
		// Without the tombstone, remove the state so at least this file
		// cannot be reported twice. If that fails too, do not report.
		if uerr := dirfd.UnlinkAt(dirFd, leaf); uerr != nil {
			return telemetry.SessionSummary{}, false, fmt.Errorf("closing %s: %v; removing it: %v", s.Path, err, uerr)
		}
	}
	return summary, true, nil
}

// openRegularNoFollowAt opens name under dirFd without following a symlink
// and without blocking on a FIFO, and checks that it is a regular file with
// exactly one link. A missing file satisfies errors.Is(err, os.ErrNotExist);
// anything else that is refused wraps ErrSessionStateRefused.
func openRegularNoFollowAt(dirFd int, name string, access int) (*os.File, error) {
	f, err := dirfd.OpenAt(dirFd, name, access|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, fmt.Errorf("opening %s: %w", name, os.ErrNotExist)
		}
		return nil, refusedIfLoop(fmt.Errorf("opening %s: %w", name, err))
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a single-link regular file", ErrSessionStateRefused, name)
	}
	return f, nil
}

// refusedIfLoop marks a symlink refusal as ErrSessionStateRefused: ELOOP
// from O_NOFOLLOW on a leaf, or ENOTDIR from O_DIRECTORY|O_NOFOLLOW on a
// directory component that is a symlink (or not a directory at all).
func refusedIfLoop(err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("%w: %v", ErrSessionStateRefused, err)
	}
	return err
}

// writeInPlace replaces f's whole content with data.
func writeInPlace(f *os.File, data []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	return f.Sync()
}
