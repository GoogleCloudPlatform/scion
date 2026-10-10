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
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// ErrSessionStateRefused reports that CloseOpenSession found something other
// than a plain regular file at the state or lock path (a symlink, FIFO,
// hard link, directory, ...) and refused to use it.
var ErrSessionStateRefused = errors.New("session metrics state refused")

// CloseOpenSession is the init daemon's shutdown backstop for sessions whose
// session-end was never handled: the container was stopped, or the harness
// has no session-end hook.
//
// Under the same lock the hook processes use, it reads the state file. If
// it holds an open session, CloseOpenSession finalizes it with errMsg (empty
// for a clean exit, which yields status "completed"; otherwise "error"),
// replaces the file's content with a closed tombstone, and returns the
// summary with ok=true. The caller then reports it. If the hook process
// finished the session first, the file holds no open session and ok is
// false: a session is reported once, by whichever side finalizes it.
//
// The tombstone makes Update ignore later hook events for that session. On
// a stop, init's supervised child is only the tmux client: the harness runs
// under the tmux server and can still be running, and firing hooks, after
// this check. Such late events (including a real session-end) are dropped,
// so the reported counts can miss the session's last few events, but the
// session cannot be reported twice. The tombstone only has to last for this
// container's shutdown; ClearSessionTombstone removes it at the next start
// so a resumed session with the same ID is counted again.
//
// If the tombstone cannot be written, the session is not returned (and its
// state is removed): without the tombstone a late hook could report it too,
// and losing one report is preferred to reporting twice.
//
// A session whose harness supplied no ID is reported and tombstoned under
// telemetry.FallbackSessionID, the same ID the session-end hook would have
// used, so the two paths cannot report it under different IDs.
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
//
// The summary is also kept in the file as a pending report claimed by this
// process, so the caller must call CompleteReportsNoFollow once it has
// attempted to send it; if the caller dies first, a later hook process
// sends it (see session_state_pending.go).
func (s *FileSessionState) CloseOpenSession(errMsg string) (telemetry.SessionSummary, bool, error) {
	var summary telemetry.SessionSummary
	var ok bool
	err := s.withLockedStateNoFollow(syscall.O_RDWR, func(dirFd int, leaf string, f *os.File, file sessionStateFile) error {
		var err error
		summary, ok, err = closeOpenSessionLocked(dirFd, leaf, f, &file, errMsg)
		return err
	})
	if err != nil || !ok {
		return telemetry.SessionSummary{}, false, err
	}
	return summary, true, nil
}

// closeOpenSessionLocked is CloseOpenSession's work, run with the lock held
// and the state file open as f. It updates *file to what it wrote.
func closeOpenSessionLocked(dirFd int, leaf string, f *os.File, file *sessionStateFile, errMsg string) (telemetry.SessionSummary, bool, error) {
	if file.Closed || !file.Aggregator.Open {
		return telemetry.SessionSummary{}, false, nil
	}
	agg := telemetry.NewAggregator()
	agg.RestoreState(file.Aggregator)
	summary := agg.Finalize(0, 0, 0, 0, errMsg)

	next := sessionStateFile{
		Version: sessionStateVersion,
		Aggregator: telemetry.AggregatorState{
			SessionID: summary.SessionID,
			StartedAt: summary.StartedAt,
		},
		Closed:  true,
		Pending: file.Pending,
	}
	next.addPending(summary)
	if err := writeStateFileInPlace(f, next); err != nil {
		// Without the tombstone, a late hook event from a still-running
		// harness could report the session again, so it is not
		// returned: one lost report is better than two. The state is
		// removed so a later check cannot return it either.
		log.Error("Session metrics: session %s not reported at shutdown: cannot write the closed marker, so a second report could not be ruled out: %v",
			summary.SessionID, err)
		if uerr := dirfd.UnlinkAt(dirFd, leaf); uerr != nil {
			return telemetry.SessionSummary{}, false, fmt.Errorf("writing tombstone: %v; removing: %v", err, uerr)
		}
		*file = sessionStateFile{}
		return telemetry.SessionSummary{}, false, nil
	}
	*file = next
	return summary, true, nil
}

// ClearSessionTombstone removes a closed tombstone left by CloseOpenSession
// during the previous shutdown. The init daemon calls it at startup, before
// the harness starts, so the tombstone cannot outlive the shutdown it was
// written for: a resumed session that reuses the ID and sends no
// session-start (for example a harness resumed with its conversation ID)
// must be counted again. An open session's state is left alone. It reports
// whether a tombstone was removed, and follows the same no-follow and lock
// rules as CloseOpenSession.
//
// Pending reports are kept, and their claims are released: no sender from
// the previous run can still be alive, so the first hook process of this
// run sends them (see session_state_pending.go).
func (s *FileSessionState) ClearSessionTombstone() (bool, error) {
	cleared := false
	err := s.withLockedStateNoFollow(syscall.O_RDWR, func(dirFd int, leaf string, f *os.File, file sessionStateFile) error {
		changed := file.releaseClaims()
		if file.Closed {
			file.Closed = false
			file.Aggregator = telemetry.AggregatorState{}
			changed = true
			cleared = true
		}
		if !changed {
			return nil
		}
		if err := writeOrRemoveNoFollow(dirFd, leaf, f, file); err != nil {
			cleared = false
			return fmt.Errorf("removing tombstone: %w", err)
		}
		return nil
	})
	return cleared, err
}

// withLockedStateNoFollow opens the state directory, lock file and state
// file without following symlinks, takes the hooks' lock, decodes the state
// and calls fn with the open state file (opened with access) while the lock
// is held. A missing directory, lock file or state file means there is no
// state: fn is not called and the result is nil. Refused or undecodable
// files are errors, and fn is not called.
func (s *FileSessionState) withLockedStateNoFollow(access int, fn func(dirFd int, leaf string, f *os.File, file sessionStateFile) error) error {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return refusedIfLoop(err)
	}
	defer func() { _ = syscall.Close(dirFd) }()

	lockFile, err := openRegularNoFollowAt(dirFd, leaf+".lock", syscall.O_RDONLY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	unlock, err := flockWait(lockFile, s.lockWait())
	if err != nil {
		return err
	}
	defer unlock()

	f, err := openRegularNoFollowAt(dirFd, leaf, access)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No state, or already finalized by the session-end hook.
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, sessionStateMaxBytes+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", s.Path, err)
	}
	file, err := decodeSessionState(data)
	if err != nil {
		return fmt.Errorf("%s %v", s.Path, err)
	}
	if err := fn(dirFd, leaf, f, file); err != nil {
		return fmt.Errorf("%s: %w", s.Path, err)
	}
	return nil
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

// writeSessionStateInPlace is writeInPlace; a test replaces it to make the
// tombstone write fail.
var writeSessionStateInPlace = writeInPlace

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
