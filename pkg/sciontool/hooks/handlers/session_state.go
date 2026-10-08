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
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// SessionStateStore persists a TelemetryHandler's aggregator state between
// the short-lived `sciontool hook` processes of one agent.
type SessionStateStore interface {
	// Update loads the persisted state into agg, calls apply, and then
	// saves agg's state, or deletes it when apply reports that the session
	// ended. All of this happens under one lock. Update calls apply exactly
	// once, even when it returns an error, so the event is always counted
	// in memory. The one exception: when the persisted state marks the
	// event's session as already closed and reported (see
	// FileSessionState.CloseOpenSession), apply is not called and Update
	// returns nil, so the event can neither reopen nor report that session.
	Update(agg *telemetry.Aggregator, event *hooks.Event, apply func() (ended bool)) error
}

const (
	// SessionStateFileName is the session metrics state file, kept in the
	// agent's ~/.scion directory next to the Hub token.
	SessionStateFileName = "session-metrics.json"

	// sessionStateVersion is the state file's format version. A file with
	// any other version is discarded.
	sessionStateVersion = 1

	// sessionStateMaxBytes bounds the read of the state file. Real state is
	// a few hundred bytes plus one entry per distinct tool name.
	sessionStateMaxBytes = 1 << 20

	// sessionStateLockTimeout bounds the wait for another hook process to
	// release the lock, so a stuck process cannot hang the harness's hooks.
	sessionStateLockTimeout = 2 * time.Second
	sessionStateLockPoll    = 10 * time.Millisecond
)

// sessionStateFile is the on-disk form of the state.
type sessionStateFile struct {
	Version    int                       `json:"version"`
	Aggregator telemetry.AggregatorState `json:"aggregator"`

	// Closed marks a tombstone: the session in Aggregator (only its ID and
	// start time are kept) was finalized and reported by the init daemon
	// at shutdown, because its session-end was never handled. Hook events
	// for that session that arrive afterwards are ignored, so a session-end
	// hook still in flight at shutdown cannot report it a second time.
	Closed bool `json:"closed,omitempty"`
}

// FileSessionState is a SessionStateStore backed by a JSON file. A sibling
// ".lock" file serializes concurrent hook processes with flock, and writes
// go through a temp file and rename so a crash never leaves a partial file.
type FileSessionState struct {
	Path string

	// lockTimeout overrides sessionStateLockTimeout when positive. It is a
	// test seam: production code always uses the default.
	lockTimeout time.Duration
}

// NewFileSessionState returns a store for the state file under the given
// home directory.
func NewFileSessionState(home string) *FileSessionState {
	return &FileSessionState{Path: filepath.Join(home, ".scion", SessionStateFileName)}
}

// ErrSessionStateUnavailable reports that Update could not take the lock,
// so the persisted state was neither loaded nor saved. The event was applied
// to the in-memory aggregator only, which therefore does not hold the
// session's counts; a summary finalized from it must not be reported.
var ErrSessionStateUnavailable = errors.New("session metrics state unavailable")

// Update implements SessionStateStore.
//
// Persisted state belongs to one harness session. If the event carries a
// session ID and the persisted state has a different one, the persisted
// session is over without having been reported (its session-end was never
// delivered, e.g. the harness was killed), so its state is discarded rather
// than merged into the new session. This holds for every event, not only
// session-start, because the new session's session-start may itself have
// been missed.
func (s *FileSessionState) Update(agg *telemetry.Aggregator, event *hooks.Event, apply func() bool) error {
	unlock, err := s.lock()
	if err != nil {
		apply()
		return fmt.Errorf("%w: event counted in memory only: %v", ErrSessionStateUnavailable, err)
	}
	defer unlock()

	if st, closed, ok := s.load(); ok {
		if closed {
			if closedSessionOwnsEvent(st.SessionID, event) {
				log.Info("Session metrics: ignoring %s event for session %s, already reported at shutdown",
					event.Name, st.SessionID)
				return nil
			}
			// A new session: the tombstone has done its job.
			st = telemetry.AggregatorState{}
		}
		if st.SessionID != "" && event.Data.SessionID != "" && st.SessionID != event.Data.SessionID {
			log.Info("Session metrics: %s event for session %s discards the unreported state of session %s",
				event.Name, event.Data.SessionID, st.SessionID)
			agg.RestoreState(telemetry.AggregatorState{})
		} else {
			agg.RestoreState(st)
		}
	}

	if apply() {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", s.Path, err)
		}
		return nil
	}
	return s.save(agg.State())
}

// closedSessionOwnsEvent reports whether event belongs to the session that a
// tombstone closed. A session-start always begins a new session (a resumed
// harness session may reuse the ID; it is counted as a new segment). Any
// other event belongs to the closed session unless it carries a different
// session ID.
func closedSessionOwnsEvent(closedID string, event *hooks.Event) bool {
	if event.Name == hooks.EventSessionStart {
		return false
	}
	return event.Data.SessionID == "" || event.Data.SessionID == closedID
}

// lockWait returns how long lock waits for another holder to release it.
func (s *FileSessionState) lockWait() time.Duration {
	if s.lockTimeout > 0 {
		return s.lockTimeout
	}
	return sessionStateLockTimeout
}

// lock takes the exclusive lock, waiting up to lockWait.
func (s *FileSessionState) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	return flockWait(f, s.lockWait())
}

// flockWait takes an exclusive flock on f, polling for up to wait. On
// success it returns a func that releases the lock and closes f; on failure
// f is already closed.
func flockWait(f *os.File, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for lock %s", wait, f.Name())
		}
		time.Sleep(sessionStateLockPoll)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// load reads the persisted state. A missing file yields ok=false silently;
// an unreadable, oversized, corrupt or wrong-version file is logged and also
// yields ok=false, so the caller starts fresh. closed reports a tombstone
// (see sessionStateFile.Closed).
func (s *FileSessionState) load() (st telemetry.AggregatorState, closed bool, ok bool) {
	f, err := os.Open(s.Path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Session metrics: cannot read %s, starting fresh: %v", s.Path, err)
		}
		return telemetry.AggregatorState{}, false, false
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, sessionStateMaxBytes+1))
	if err != nil {
		log.Error("Session metrics: cannot read %s, starting fresh: %v", s.Path, err)
		return telemetry.AggregatorState{}, false, false
	}
	file, err := decodeSessionState(data)
	if err != nil {
		log.Error("Session metrics: %s %v, starting fresh", s.Path, err)
		return telemetry.AggregatorState{}, false, false
	}
	return file.Aggregator, file.Closed, true
}

// decodeSessionState parses a state file read with a limit of
// sessionStateMaxBytes+1 bytes. Its errors read as a predicate of the file
// ("is corrupt: ...").
func decodeSessionState(data []byte) (sessionStateFile, error) {
	if len(data) > sessionStateMaxBytes {
		return sessionStateFile{}, fmt.Errorf("is larger than %d bytes", sessionStateMaxBytes)
	}
	var file sessionStateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return sessionStateFile{}, fmt.Errorf("is corrupt: %v", err)
	}
	if file.Version != sessionStateVersion {
		return sessionStateFile{}, fmt.Errorf("has version %d, want %d", file.Version, sessionStateVersion)
	}
	return file, nil
}

// save writes the state to a 0600 temp file in the same directory and
// renames it over the state file.
func (s *FileSessionState) save(st telemetry.AggregatorState) error {
	data, err := json.Marshal(sessionStateFile{Version: sessionStateVersion, Aggregator: st})
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), "."+SessionStateFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting state file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing state: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("replacing %s: %w", s.Path, err)
	}
	tmpName = ""
	return nil
}
