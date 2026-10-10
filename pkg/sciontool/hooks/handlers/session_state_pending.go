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
	"os"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// Pending reports
//
// A finalized session summary is not dropped from the state file until a
// send has been attempted. The side that finalizes a session (the
// session-end hook in Update, or the init daemon in CloseOpenSession)
// replaces the open session with a pending report claimed by its own
// process, in the same write that ends the session. It then sends the
// report and removes it (CompleteReport, CompleteReportsNoFollow), whether
// or not the send succeeded, so a report the Hub rejects is not retried
// forever.
//
// If the sender dies first, the report stays in the file with a claim by a
// process that no longer exists. A later hook process
// (ClaimAbandonedReports) or the init daemon's shutdown check
// (CloseOpenSessionAndClaimPending) claims it and sends it. A report whose
// claimer is still running is left to that claimer, so two live processes
// never send the same report. The init daemon releases every claim at
// startup (ClearSessionTombstone), before any hook can run, because no
// sender from the previous run can still be alive.
//
// A sender killed after the Hub stored its report but before removing it
// leads to one resend; the Hub stores one row per agent, session ID and
// start time, so the resend is absorbed there.
//
// A report is identified by its session ID and start time (sameSegment),
// as on the Hub: a session resumed with the same ID is a new segment with
// its own start time, so its report never replaces or completes the report
// of an earlier segment that is still pending.
//
// The paths that already gave up the whole file on a write failure (the
// tombstone write in CloseOpenSession, the save in Update) still remove it,
// and any pending reports with it: losing a report stays preferred to
// reporting a session twice.

const (
	// maxPendingReports bounds the pending list. Reports pile up only when
	// senders keep dying, so the oldest are dropped past this.
	maxPendingReports = 16

	// pendingClaimTTL is how long a claim by a running process is
	// honoured. It covers a claimer PID reused by an unrelated process.
	// A send is bounded by a few seconds, so a live claimer is well inside
	// it.
	pendingClaimTTL = time.Minute
)

// pendingReport is a finalized session summary that has not been sent yet,
// or whose sender has not confirmed the attempt.
type pendingReport struct {
	Summary telemetry.SessionSummary `json:"summary"`

	// ClaimPID is the process that is sending the report; 0 means none.
	ClaimPID  int       `json:"claim_pid,omitempty"`
	ClaimedAt time.Time `json:"claimed_at,omitempty"`
}

// Test seams for claim ownership.
var (
	currentPID   = os.Getpid
	pendingNow   = time.Now
	processAlive = processExists
)

// processExists reports whether pid names a running process. EPERM means
// it exists but belongs to another user (a hook checking the root init
// daemon's claim).
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// abandoned reports whether nobody is sending r.
func (r pendingReport) abandoned(now time.Time) bool {
	return r.ClaimPID == 0 || !processAlive(r.ClaimPID) || now.Sub(r.ClaimedAt) > pendingClaimTTL
}

// sameSegment reports whether a and b are reports of the same session
// segment: the same session ID and start time.
func sameSegment(a, b telemetry.SessionSummary) bool {
	return a.SessionID == b.SessionID && a.StartedAt.Equal(b.StartedAt)
}

// addPending adds summary as a report claimed by the current process. A
// report already pending for the same segment is replaced; reports of other
// segments of the same session are kept.
func (f *sessionStateFile) addPending(summary telemetry.SessionSummary) {
	kept := f.Pending[:0]
	for _, r := range f.Pending {
		if sameSegment(r.Summary, summary) {
			log.Info("Session metrics: replacing an unsent report for session %s", summary.SessionID)
			continue
		}
		kept = append(kept, r)
	}
	kept = append(kept, pendingReport{Summary: summary, ClaimPID: currentPID(), ClaimedAt: pendingNow()})
	if drop := len(kept) - maxPendingReports; drop > 0 {
		for _, r := range kept[:drop] {
			log.Error("Session metrics: dropping unsent report for session %s: too many pending", r.Summary.SessionID)
		}
		kept = kept[drop:]
	}
	f.Pending = kept
}

// claimAbandoned claims every abandoned report for the current process and
// returns their summaries; f changed when any was claimed.
func (f *sessionStateFile) claimAbandoned() []telemetry.SessionSummary {
	now := pendingNow()
	var claimed []telemetry.SessionSummary
	for i := range f.Pending {
		if !f.Pending[i].abandoned(now) {
			continue
		}
		f.Pending[i].ClaimPID = currentPID()
		f.Pending[i].ClaimedAt = now
		claimed = append(claimed, f.Pending[i].Summary)
	}
	return claimed
}

// complete removes the reports of the segments of summaries that the
// current process claimed. It reports whether f changed.
func (f *sessionStateFile) complete(summaries ...telemetry.SessionSummary) bool {
	pid := currentPID()
	kept := f.Pending[:0]
	for _, r := range f.Pending {
		if r.ClaimPID == pid && containsSegment(summaries, r.Summary) {
			continue
		}
		kept = append(kept, r)
	}
	changed := len(kept) != len(f.Pending)
	f.Pending = kept
	return changed
}

// containsSegment reports whether summaries holds a report of s's segment.
func containsSegment(summaries []telemetry.SessionSummary, s telemetry.SessionSummary) bool {
	for _, c := range summaries {
		if sameSegment(c, s) {
			return true
		}
	}
	return false
}

// releaseClaims clears every claim. It reports whether f changed.
func (f *sessionStateFile) releaseClaims() bool {
	changed := false
	for i := range f.Pending {
		if f.Pending[i].ClaimPID != 0 {
			f.Pending[i].ClaimPID = 0
			f.Pending[i].ClaimedAt = time.Time{}
			changed = true
		}
	}
	return changed
}

// empty reports whether f holds nothing worth keeping: no session, no
// tombstone and no pending report.
func (f *sessionStateFile) empty() bool {
	return !f.Closed && len(f.Pending) == 0 && f.Aggregator.SessionID == "" && !f.Aggregator.Open
}

// ClaimAbandonedReports claims the pending reports whose sender died before
// confirming them, for a hook process to send. The caller must call
// CompleteReport for each returned summary after the send attempt.
func (s *FileSessionState) ClaimAbandonedReports() ([]telemetry.SessionSummary, error) {
	var claimed []telemetry.SessionSummary
	err := s.modify(func(file *sessionStateFile) bool {
		claimed = file.claimAbandoned()
		return len(claimed) > 0
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// CompleteReport removes the pending report of summary's segment that this
// process claimed, once its send was attempted.
func (s *FileSessionState) CompleteReport(summary telemetry.SessionSummary) error {
	return s.modify(func(file *sessionStateFile) bool {
		return file.complete(summary)
	})
}

// modify is the hook processes' read-modify-write of the state file under
// the lock: fn changes file and reports whether it did. An empty result
// removes the file. A missing or unusable file is left alone.
func (s *FileSessionState) modify(fn func(file *sessionStateFile) bool) error {
	unlock, err := s.lock()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionStateUnavailable, err)
	}
	defer unlock()

	file, ok := s.load()
	if !ok || !fn(&file) {
		return nil
	}
	if file.empty() {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", s.Path, err)
		}
		return nil
	}
	return s.save(file)
}

// CloseOpenSessionAndClaimPending is the init daemon's shutdown check. In
// one locked pass it closes the open session as CloseOpenSession does,
// keeping its summary as a pending report claimed by this process, and
// claims every abandoned pending report. It returns all the summaries to
// send; the caller must then call CompleteReportsNoFollow with them. It follows CloseOpenSession's no-follow rules.
func (s *FileSessionState) CloseOpenSessionAndClaimPending(errMsg string) ([]telemetry.SessionSummary, error) {
	var out []telemetry.SessionSummary
	err := s.withLockedStateNoFollow(syscall.O_RDWR, func(dirFd int, leaf string, f *os.File, file sessionStateFile) error {
		summary, closed, err := closeOpenSessionLocked(dirFd, leaf, f, &file, errMsg)
		if err != nil {
			return err
		}
		claimed := file.claimAbandoned()
		if len(claimed) > 0 {
			if err := writeStateFileInPlace(f, file); err != nil {
				// The claims are not recorded; leave the reports for a
				// later check rather than risk a concurrent second send.
				log.Error("Session metrics: cannot claim %d unsent report(s): %v", len(claimed), err)
				claimed = nil
			}
		}
		if closed {
			out = append(out, summary)
		}
		out = append(out, claimed...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CompleteReportsNoFollow removes the pending reports of the segments of
// summaries that this process claimed, after the init daemon attempted to
// send them. It follows CloseOpenSession's no-follow rules.
func (s *FileSessionState) CompleteReportsNoFollow(summaries ...telemetry.SessionSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	return s.withLockedStateNoFollow(syscall.O_RDWR, func(dirFd int, leaf string, f *os.File, file sessionStateFile) error {
		if !file.complete(summaries...) {
			return nil
		}
		return writeOrRemoveNoFollow(dirFd, leaf, f, file)
	})
}

// writeOrRemoveNoFollow writes file in place through f, or unlinks leaf when
// file is empty.
func writeOrRemoveNoFollow(dirFd int, leaf string, f *os.File, file sessionStateFile) error {
	if file.empty() {
		if err := dirfd.UnlinkAt(dirFd, leaf); err != nil {
			return fmt.Errorf("removing state: %w", err)
		}
		return nil
	}
	return writeStateFileInPlace(f, file)
}

// writeStateFileInPlace encodes file and replaces f's content with it.
func writeStateFileInPlace(f *os.File, file sessionStateFile) error {
	file.Version = sessionStateVersion
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	if err := writeSessionStateInPlace(f, data); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	return nil
}
