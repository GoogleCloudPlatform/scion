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

package runtimebroker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// The launch journal (design wave 3 §1 C-a, ptone/scion#4668) keeps each
// in-flight async launch on disk so a restarted broker process can report
// what happened to it and clean up after it (recoverLaunches). It is laid
// out as
//
//	<stateDir>/launches/<instanceID>.lock      flock held for the process lifetime
//	<stateDir>/launches/<instanceID>/<launchID>.json
//
// with directories 0700 and files 0600. flock is advisory and not reliable
// on NFS or other shared filesystems: broker replicas that share one broker
// ID must use a local filesystem per replica, or distinct state dirs.
//
// A record holds plain identifiers only (journalRecord): never StartOptions,
// environment, secrets, tokens or the request body.
//
// The journal is a best-effort aid. Any failure to open or write it is
// logged and the launch proceeds as it would without one.

const (
	// journalDirName is the journal's directory under the broker state dir.
	journalDirName = "launches"
	// journalVersion is the on-disk record version.
	journalVersion = 1
	// journalDirMode and journalFileMode keep the journal private to the
	// broker's user.
	journalDirMode  os.FileMode = 0o700
	journalFileMode os.FileMode = 0o600
	// journalLockRetries and journalLockRetryDelay bound the retry of this
	// instance's own lock (see open).
	journalLockRetries    = 10
	journalLockRetryDelay = 100 * time.Millisecond
)

// journalState is a record's state. Its order is the writer's rank: a write
// never replaces a higher-ranked state (design wave 3 §1 C-a, R4).
type journalState int

const (
	journalNone journalState = iota
	journalLaunching
	journalTerminalPending
	// journalCleanupPending: the hub has confirmed the launch is over and
	// asked for clean-up, which failed; only the clean-up is left, for the
	// next recovery to retry (ptone/scion#5177).
	journalCleanupPending
	journalRemoved
)

// String is the on-disk spelling of the state.
func (s journalState) String() string {
	switch s {
	case journalLaunching:
		return "launching"
	case journalTerminalPending:
		return "terminal_pending"
	case journalCleanupPending:
		return "cleanup_pending"
	case journalRemoved:
		return "removed"
	default:
		return ""
	}
}

// journalTerminal is the terminal report a launch has decided to send.
// AgentInfo is the identifier-only echo of a succeeded launch
// (hubclient.AgentLaunchReportInfo, built in runLaunch). StartFailed marks a
// failed terminal whose launch's Start failed on this broker, and
// PostCompleted one sent after the hub had already answered completed
// (ptone/scion#5368); recovery classifies their answers with
// startFailureCleanup. Both are omitted when false, so a record from an
// older broker reads them as false and journalVersion stays 1.
type journalTerminal struct {
	Succeeded     bool                             `json:"succeeded"`
	Step          string                           `json:"step,omitempty"`
	ErrorCode     string                           `json:"errorCode,omitempty"`
	Message       string                           `json:"message,omitempty"`
	Cause         string                           `json:"cause,omitempty"`
	StartFailed   bool                             `json:"startFailed,omitempty"`
	PostCompleted bool                             `json:"postCompleted,omitempty"`
	AgentInfo     *hubclient.AgentLaunchReportInfo `json:"agentInfo,omitempty"`
}

// journalOwnedRun is a flat instance's ownership key of the launch's run
// (ownedRunKey), so recovery can finish the cleaned run in its records.
type journalOwnedRun struct {
	ProjectID string `json:"projectId"`
	AgentID   string `json:"agentId"`
	RunID     string `json:"runId"`
}

// journalRecord is the on-disk record. It is an allowlist: every field is a
// plain identifier, time, step name or the terminal the hub will be sent.
type journalRecord struct {
	Version         int       `json:"version"`
	LaunchID        string    `json:"launchId"`
	AgentID         string    `json:"agentId"`
	Kind            string    `json:"kind"`
	RunID           string    `json:"runId,omitempty"`
	InstanceID      string    `json:"instanceId"`
	HubName         string    `json:"hubName,omitempty"`
	OwnerHub        string    `json:"ownerHub,omitempty"`
	Deadline        time.Time `json:"deadline"`
	Seq             int64     `json:"seq"`
	ProjectID       string    `json:"projectId,omitempty"`
	Slug            string    `json:"slug"`
	AgentName       string    `json:"agentName"`
	ProjectPath     string    `json:"projectPath,omitempty"`
	HubProjectID    string    `json:"hubProjectId,omitempty"`
	SharedWorkspace bool      `json:"sharedWorkspace,omitempty"`
	RuntimeName     string    `json:"runtimeName,omitempty"`
	// RuntimeIdentity is the identity the broker keys the launch runtime by
	// (auxiliaryRuntimeIdentity: the type, plus context and namespace for
	// Kubernetes), so recovery resolves the same runtime, not merely one of
	// the same type.
	RuntimeIdentity string               `json:"runtimeIdentity,omitempty"`
	Profile         string               `json:"profile,omitempty"`
	OwnedRun        *journalOwnedRun     `json:"ownedRun,omitempty"`
	Handles         []api.ResourceHandle `json:"handles,omitempty"`
	LastStep        string               `json:"lastStep,omitempty"`
	State           string               `json:"state"`
	Terminal        *journalTerminal     `json:"terminal,omitempty"`
	// CleanupAttempts counts the failed clean-ups of a cleanup_pending
	// record, and CleanupFirstFailedAt is when the first one failed; they
	// bound the retries (ptone/scion#5177). Counters only: no error text.
	CleanupAttempts      int       `json:"cleanupAttempts,omitempty"`
	CleanupFirstFailedAt time.Time `json:"cleanupFirstFailedAt,omitzero"`
	// CleanupScope is "resources" when the launch's clean-up was only its
	// recorded resources (a local stop or delete owns the files), so a
	// retry reruns only that; "" is the full clean-up.
	CleanupScope string `json:"cleanupScope,omitempty"`
	// ReportAttempts counts the boots whose resend of the terminal report
	// got no hub answer, and ReportFirstFailedAt is when the first one
	// failed; they bound the retries (ptone/scion#5186). Counters only.
	ReportAttempts      int       `json:"reportAttempts,omitempty"`
	ReportFirstFailedAt time.Time `json:"reportFirstFailedAt,omitzero"`
}

// key is the registry key the record's launch was registered under.
func (r *journalRecord) key() launchKey {
	return launchKey{ProjectID: r.ProjectID, Slug: r.Slug}
}

// journalNameRE limits the instance and launch IDs used as path elements.
// Both are minted as UUIDs (the instance here, the launch by the hub); a
// launch with any other ID is simply not journalled.
var journalNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// launchJournal is one broker process's journal. A nil *launchJournal is a
// disabled journal: every method is a no-op.
type launchJournal struct {
	root       string // <stateDir>/launches
	instanceID string
	log        *slog.Logger

	mu       sync.Mutex
	opened   bool
	disabled bool
	lock     *os.File

	// testHookAfterSnapshot, when set (tests only), runs inside a write
	// after the snapshot is taken and before the file is written.
	testHookAfterSnapshot func(e *journalEntry, state journalState)
}

// newLaunchJournal returns the journal for instanceID under stateDir, or nil
// (disabled) when stateDir is empty. Nothing is created on disk until the
// first launch is journalled.
func newLaunchJournal(stateDir, instanceID string, log *slog.Logger) *launchJournal {
	if stateDir == "" || !journalNameRE.MatchString(instanceID) {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &launchJournal{root: filepath.Join(stateDir, journalDirName), instanceID: instanceID, log: log}
}

// dir is this instance's record directory.
func (j *launchJournal) dir() string {
	return filepath.Join(j.root, j.instanceID)
}

// ensureOpen creates the journal directories and takes this instance's lock
// on first use. It reports false, and disables the journal for the rest of
// the process, if that fails.
func (j *launchJournal) ensureOpen() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.disabled {
		return false
	}
	if j.opened {
		return true
	}
	if err := j.open(); err != nil {
		j.log.Warn("Launch journal disabled: async launches will not survive a broker restart",
			"dir", j.root, "error", err)
		j.disabled = true
		return false
	}
	j.opened = true
	return true
}

func (j *launchJournal) open() error {
	if err := mkdirPrivate(j.root); err != nil {
		return err
	}
	// The lock is taken before the record directory exists, so a booting
	// process's recovery, which only acts on directories whose lock it can
	// take, never sees this instance's records unlocked. A sibling's boot
	// recovery may hold this fresh instance's lock-only entry for a moment
	// (it finds no records and removes it), so contention is retried
	// briefly before the journal is given up.
	var f *os.File
	var err error
	for attempt := 0; ; attempt++ {
		f, err = lockJournalInstance(j.root, j.instanceID, false)
		if !errors.Is(err, errJournalLocked) || attempt >= journalLockRetries {
			break
		}
		time.Sleep(journalLockRetryDelay)
	}
	if err != nil {
		return err
	}
	if err := mkdirPrivate(j.dir()); err != nil {
		unlockJournalInstance(f)
		return err
	}
	j.lock = f
	return nil
}

// Close releases this instance's lock. Records stay on disk for the next
// boot's recovery.
func (j *launchJournal) Close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lock != nil {
		unlockJournalInstance(j.lock)
		j.lock = nil
	}
	j.disabled = true
}

// mkdirPrivate creates dir (mode 0700) if missing and tightens its mode.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, journalDirMode); err != nil {
		return err
	}
	return os.Chmod(dir, journalDirMode)
}

// errJournalLocked is returned by lockJournalInstance when another process
// holds the instance's lock.
var errJournalLocked = errors.New("launch journal instance is locked by a live process")

// journalEntry is one launch's journal record. Every write goes through
// write, under mu (design wave 3 §1 C-a, R4).
type journalEntry struct {
	j    *launchJournal
	path string
	rec  *launchRecord

	// base holds the record's fixed fields, set at admit.
	base journalRecord

	sender atomic.Pointer[launchSender]

	mu sync.Mutex
	// written is the highest state this entry has written.
	written journalState
	// lastStep is the step the last launching write recorded. Atomic, so
	// the progress hook (stepChanged) never takes mu, which write holds
	// across file I/O.
	lastStep atomic.Pointer[string]

	stepQueued atomic.Bool
}

// admit journals a newly accepted launch (state launching) and returns its
// entry, or nil when the journal is disabled, the IDs are not usable as
// file names, or the write failed. A nil entry is a no-op.
func (j *launchJournal) admit(rec *launchRecord, base journalRecord) *journalEntry {
	if j == nil || rec == nil {
		return nil
	}
	if !journalNameRE.MatchString(rec.ID) {
		j.log.Warn("Launch not journalled: its launch ID is not usable as a file name", "launch_id", rec.ID)
		return nil
	}
	if !j.ensureOpen() {
		return nil
	}
	base.Version = journalVersion
	base.LaunchID = rec.ID
	base.AgentID = rec.AgentID
	base.Kind = rec.Kind
	base.RunID = rec.RunID
	base.InstanceID = j.instanceID
	base.HubName = rec.HubName
	base.Deadline = rec.Deadline
	e := &journalEntry{j: j, path: filepath.Join(j.dir(), rec.ID+".json"), rec: rec, base: base}
	if !e.write(journalLaunching, nil) {
		return nil
	}
	return e
}

// setSender lets later snapshots read the sender's seq and step.
func (e *journalEntry) setSender(s *launchSender) {
	if e == nil {
		return
	}
	e.sender.Store(s)
}

// update rewrites the launching record, e.g. after a new handle.
func (e *journalEntry) update() {
	if e == nil {
		return
	}
	e.write(journalLaunching, nil)
}

// stepChanged queues one launching write if the sender's step differs from
// the one last written. It never blocks: it is called from the runtime's
// non-blocking progress hook. Calls while a write is queued coalesce.
func (e *journalEntry) stepChanged() {
	if e == nil {
		return
	}
	s := e.sender.Load()
	if s == nil {
		return
	}
	last := ""
	if p := e.lastStep.Load(); p != nil {
		last = *p
	}
	same := s.LastStep() == last
	if same || !e.stepQueued.CompareAndSwap(false, true) {
		return
	}
	go func() {
		e.stepQueued.Store(false)
		e.write(journalLaunching, nil)
	}()
}

// terminal journals the terminal the launch is about to send.
func (e *journalEntry) terminal(t journalTerminal) {
	if e == nil {
		return
	}
	e.write(journalTerminalPending, &t)
}

// cleanupPending keeps the record, as cleanup_pending with one failed
// attempt at failedAt and the clean-up scope (cleanupScopeResources, or ""
// for the full clean-up), for the next recovery to retry the clean-up
// (ptone/scion#5177). Only removal follows it. It reports whether the
// record is now on disk.
func (e *journalEntry) cleanupPending(failedAt time.Time, scope string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	e.base.CleanupAttempts = 1
	e.base.CleanupFirstFailedAt = failedAt.UTC()
	e.base.CleanupScope = scope
	e.mu.Unlock()
	return e.write(journalCleanupPending, nil)
}

// remove deletes the record. Nothing is written for this entry afterwards.
func (e *journalEntry) remove() {
	if e == nil {
		return
	}
	e.write(journalRemoved, nil)
}

// write is the entry's only writer (design wave 3 §1 C-a, R4, Nit-3). Under
// e.mu, in order: drop the write if it would go backwards (its state ranks
// below the highest written), if the entry is removed, or if a terminal is
// already written (only removal follows a terminal); take the snapshot;
// write it; record the state. It reports whether the state is now on disk.
func (e *journalEntry) write(state journalState, t *journalTerminal) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch e.written {
	case journalTerminalPending:
		// Only a failed clean-up's cleanup_pending, or removal, follows a
		// terminal.
		if state != journalCleanupPending && state != journalRemoved {
			return false
		}
	case journalCleanupPending:
		if state != journalRemoved {
			return false
		}
	}
	if state < e.written {
		return false
	}
	if state == journalRemoved {
		if err := os.Remove(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.j.log.Warn("Launch journal: failed to remove a record", "launch_id", e.base.LaunchID, "error", err)
		}
		e.written = journalRemoved
		return true
	}
	snap := e.snapshot(state, t)
	if h := e.j.testHookAfterSnapshot; h != nil {
		h(e, state)
	}
	data, err := json.Marshal(snap)
	if err == nil {
		err = writeJournalFile(e.path, data)
	}
	if err != nil {
		e.j.log.Warn("Launch journal: failed to write a record", "launch_id", e.base.LaunchID, "state", state.String(), "error", err)
		if state == journalTerminalPending {
			// A launching record left behind a decided terminal would be
			// recovered as a launch that never finished; no record is
			// safer than that one.
			_ = os.Remove(e.path)
			e.written = journalTerminalPending
		}
		return false
	}
	e.written = state
	if state == journalLaunching {
		step := snap.LastStep
		e.lastStep.Store(&step)
	}
	return true
}

// snapshot builds the record to write. Called with e.mu held; it takes
// rec.mu (through HandlesSnapshot and OwnerHub) inside it, never the other
// way round.
func (e *journalEntry) snapshot(state journalState, t *journalTerminal) journalRecord {
	r := e.base
	r.State = state.String()
	r.Handles = e.rec.HandlesSnapshot()
	r.OwnerHub = e.rec.OwnerHub()
	e.rec.ownedRunMu.Lock()
	if k := e.rec.ownedRun; k.runID != "" {
		r.OwnedRun = &journalOwnedRun{ProjectID: k.projectID, AgentID: k.agentID, RunID: k.runID}
	}
	e.rec.ownedRunMu.Unlock()
	if s := e.sender.Load(); s != nil {
		r.Seq = s.currentSeq()
		r.LastStep = s.LastStep()
	}
	if t != nil {
		tt := *t
		r.Terminal = &tt
	}
	return r
}

// writeJournalFile writes data to path atomically: a unique temp file in the
// same directory (mode 0600), fsync, rename, fsync of the directory.
func writeJournalFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, strings.TrimSuffix(filepath.Base(path), ".json")+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(journalFileMode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readJournalRecord reads one record file.
func readJournalRecord(path string) (*journalRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r journalRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.Version != journalVersion {
		return nil, fmt.Errorf("unsupported launch journal record version %d", r.Version)
	}
	return &r, nil
}
