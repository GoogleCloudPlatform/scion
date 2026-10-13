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

package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// DefaultSandboxBin is the path to the Cloud Run sandbox launcher binary.
// Exported so that pkg/config (which cannot import this package without
// creating a cycle) can assert equality with its own copy at test time.
const DefaultSandboxBin = "/usr/local/gcp/bin/sandbox"

// defaultScionRoot is the parent directory for all writable sandbox paths.
// Per-agent bind mounts under this directory satisfy the write-visibility
// requirement for agent home, workspace, tmux sockets, and shared dirs.
// With --rootfs /, the rootfs is READ-ONLY: writes to unmounted paths fail
// with EROFS (confirmed by diag-sbx6). Per-agent bind mounts under this
// directory provide the writable paths agents need.
const defaultScionRoot = "/scion"

// defaultStatePath is the default location for the sandbox state store.
const defaultStatePath = "/tmp/scion-sandbox-state.json"

// sandboxAgentHome is the path where the agent home is mounted INSIDE the
// sandbox. This differs from the host-side path (scionPaths.agentHome, e.g.
// /scion/agents/<slug>/home) because mountsFor maps source→destination with
// different paths. The rootfs is read-only (EROFS), so only mounted paths
// are writable.
//
// This path must match:
//   - supervisor.go:115 which hardcodes HOME="/home/<username>"
//   - The provisioner command in harness configs (e.g. /home/scion/.scion/...)
//
// When writing paths for code that runs INSIDE the sandbox, use this constant.
// When writing paths for code that runs on the HOST, use scionPaths.agentHome.
const sandboxAgentHome = "/home/scion"

// sandboxWorkspace is the path where the agent workspace is mounted INSIDE
// the sandbox. Like sandboxAgentHome, this differs from the host-side path
// (scionPaths.workspace, e.g. /scion/agents/<slug>/workspace). mountsFor
// maps the host path to this destination so that sciontool init — which
// defaults to /workspace when SCION_WORKSPACE_PATH is unset — writes to
// the writable bind mount rather than the read-only rootfs.
//
// This matches the Docker/Podman convention (common.go:210) where the
// workspace is always mounted at /workspace regardless of the host-side path.
const sandboxWorkspace = "/workspace"

// entrypointLogFile is the filename (relative to agentHome) where the
// entrypoint's stdout/stderr is captured.  This file is the ONLY record
// of failures that occur before, around, or outside sciontool's own
// logger — shell-level errors ("command not found"), exec failures, and
// any diagnostic the process writes to stdout/stderr without going
// through log.go.  Those failures produce a bare exit code in the UI
// and nothing else; this file is where the explanation lives.
//
// The file is opened with >> (append) so that a restart preserves the
// previous run's output.  A run-delimiter line is emitted at the start
// of each run to make boundaries unambiguous.  See #110 for the history.
//
// Growth is unbounded across restarts, matching agent.log (which also
// appends without rotation in the same directory).  If disk pressure
// from diagnostic logs becomes a concern on this tier, rotation should
// be added to BOTH files together.
const entrypointLogFile = ".scion-entrypoint.log"

// sandboxUID and sandboxGID control the UID/GID passed to the sandbox via
// SCION_HOST_UID / SCION_HOST_GID.
//
// Ideally these would be 1000 (the scion user) to match Docker/Podman
// behaviour, but Cloud Run's gVisor sandbox does not grant CAP_SETUID or
// CAP_SETGID, making runtime privilege drops impossible. Until the sandbox
// CLI gains a --user flag (or the capability bounding set is extended),
// the sandbox process must run as root (UID 0).
//
// Running as UID 0 means:
//   - Harnesses that reject root (e.g. Claude Code's --dangerously-skip-permissions)
//     will not work on this runtime.
//   - Harnesses that accept root (e.g. antigravity, gemini-cli) work normally.
//
// TODO(single-node): Switch back to 1000 when the sandbox CLI supports --user.
const sandboxUID = 0
const sandboxGID = 0

// SandboxLauncherAvailable reports whether the Cloud Run Sandbox launcher
// binary is present on the filesystem.
func SandboxLauncherAvailable() bool {
	_, err := os.Stat(DefaultSandboxBin)
	return err == nil
}

// -----------------------------------------------------------------------
// State Store
//
// The sandbox CLI has no `list` command. The runtime maintains its own
// state in a JSON file (design doc §4.5).
// -----------------------------------------------------------------------

// sandboxStateEntry tracks a single sandbox's lifecycle.
type sandboxStateEntry struct {
	SandboxName   string            `json:"sandbox_name"`
	AgentID       string            `json:"agent_id"`
	ProjectID     string            `json:"project_id"`
	Project       string            `json:"project"`
	Labels        map[string]string `json:"labels"`
	Template      string            `json:"template"`
	HarnessConfig string            `json:"harness_config"`
	CreatedAt     time.Time         `json:"created_at"`
	AgentHome     string            `json:"agent_home"`
	Workspace     string            `json:"workspace"`
	Image         string            `json:"image"`
	Stopped       bool              `json:"stopped"`
	ExitCode      *int              `json:"exit_code,omitempty"`
	StoppedAt     *time.Time        `json:"stopped_at,omitempty"`
	// EntrypointLogOffset is the entrypoint log size when this run started,
	// so the exit watcher reports this run's output only.
	EntrypointLogOffset int64 `json:"entrypoint_log_offset,omitempty"`
	// Nonce is regenerated on every write of this entry; every write
	// compares it against the nonce its caller observed (compare-and-set).
	// Entries written before nonces existed load with "".
	Nonce string `json:"nonce,omitempty"`
	// Starting marks the entry Run writes before `sandbox run` and replaces
	// with the started entry once the probe passes (ptone/scion#4757). A
	// Starting entry is never Stopped. StartingAt is when it was written;
	// StartingUntil is the time until which reconcile keeps it whatever
	// the probe says (startingUntil).
	Starting      bool       `json:"starting,omitempty"`
	StartingAt    *time.Time `json:"starting_at,omitempty"`
	StartingUntil *time.Time `json:"starting_until,omitempty"`
}

// sandboxNonce is the nonce a caller observed for a state key: either a
// present entry's Nonce (possibly the legacy "") or nonceAbsent, a missing
// key. nonceAbsent never equals a present entry, legacy or not.
type sandboxNonce struct {
	value   string
	present bool
}

// nonceAbsent is the observed nonce of a key with no entry.
var nonceAbsent = sandboxNonce{}

// nonceOf returns the observed nonce of e (nonceAbsent for nil).
func nonceOf(e *sandboxStateEntry) sandboxNonce {
	if e == nil {
		return nonceAbsent
	}
	return sandboxNonce{value: e.Nonce, present: true}
}

func (n sandboxNonce) String() string {
	if !n.present {
		return "<absent>"
	}
	return strconv.Quote(n.value)
}

// errStateChanged is returned by a compare-and-set state write whose key
// no longer holds the nonce the caller observed. Nothing was written.
var errStateChanged = errors.New("sandbox state entry changed")

// sandboxStateFileLocks serializes the read-compare-write of each state
// file within this process, across every sandboxStateStore (the factory
// builds a runtime, and so a store, per lookup). Keyed by path; entries
// are a few bytes each and there is one per state path ever used (one in
// production), so the map is not pruned. Another process on the same host
// is not fenced (ptone/scion#3951).
var sandboxStateFileLocks sync.Map // state path -> *sync.Mutex

// logUnreadableSandboxState logs, at ERROR, a state file that cannot be
// read. Every Run and Delete fails closed until it is fixed by hand.
func logUnreadableSandboxState(path string, err error) {
	runtimeLog().Error("sandbox state file is unreadable; every cloudrun-sandbox start and delete fails until it is fixed: "+
		"inspect it, then move it aside (its entries are then forgotten)",
		"path", path, "error", err)
}

// lockStateFile takes the file mutex for path and returns its unlock.
func lockStateFile(path string) func() {
	v, _ := sandboxStateFileLocks.LoadOrStore(path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// readSandboxStateFile reads the state file at path. A missing file is an
// empty map; any other read or parse error is returned, so that a caller
// about to write fails closed rather than overwriting a file it could not
// read.
func readSandboxStateFile(path string) (map[string]*sandboxStateEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return make(map[string]*sandboxStateEntry), nil
		}
		return nil, fmt.Errorf("read sandbox state %s: %w", path, err)
	}
	var entries map[string]*sandboxStateEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse sandbox state %s: %w", path, err)
	}
	if entries == nil {
		entries = make(map[string]*sandboxStateEntry)
	}
	return entries, nil
}

// writeSandboxStateFile atomically replaces the state file at path: it
// writes a unique temporary file in the same directory (0600, audit M1)
// and renames it over path (audit M2). There is no fsync, as before (the
// default path is under /tmp). A crash between the create and the rename
// leaves a "<name>.tmp-*" file behind; it is never read.
func writeSandboxStateFile(path string, entries map[string]*sandboxStateEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sandbox state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create sandbox state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create sandbox state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write sandbox state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close sandbox state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename sandbox state temp file: %w", err)
	}
	return nil
}

// sandboxStateStore is a JSON-backed store of sandbox entries.
//
// The file is authoritative. Every write is a compare-and-set of one key
// against the nonce its caller observed (casPut, casRemove,
// casMarkStopped), made under the process-wide file mutex for the path;
// entries is this instance's cache of the file, refreshed per key on each
// of its own writes, and serves get and list only.
//
// Lock order: a sandbox name lock (lockSandboxNameCtx), then the file
// mutex (lockStateFile), then mu. Store methods never take a name lock,
// and mu is never held while the file mutex is taken.
type sandboxStateStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*sandboxStateEntry
}

func newSandboxStateStore(path string) *sandboxStateStore {
	ss := &sandboxStateStore{
		path:    path,
		entries: make(map[string]*sandboxStateEntry),
	}
	ss.load()
	return ss
}

// diskEntry returns a copy of the file's entry for name (nil if none) and
// its observed nonce. A read or parse error other than a missing file is
// returned.
func (s *sandboxStateStore) diskEntry(name string) (*sandboxStateEntry, sandboxNonce, error) {
	unlock := lockStateFile(s.path)
	entries, err := readSandboxStateFile(s.path)
	unlock()
	if err != nil {
		return nil, nonceAbsent, err
	}
	e := entries[name]
	if e == nil {
		return nil, nonceAbsent, nil
	}
	cp := *e
	return &cp, nonceOf(&cp), nil
}

// casUpdate is the one write path of the store. Under the file mutex it
// loads the file (failing closed on any error but a missing file), checks
// that name's nonce equals expect (errStateChanged otherwise, writing
// nothing), and replaces the key with next(current); a nil result removes
// it. The file is written only when it changes, and this instance's cache
// is updated for that key only after the file write succeeds. It returns
// a copy of the key's new entry (nil if removed).
func (s *sandboxStateStore) casUpdate(name string, expect sandboxNonce,
	next func(cur *sandboxStateEntry) (*sandboxStateEntry, error)) (*sandboxStateEntry, error) {
	unlock := lockStateFile(s.path)
	defer unlock()

	entries, err := readSandboxStateFile(s.path)
	if err != nil {
		return nil, err
	}
	cur := entries[name]
	if got := nonceOf(cur); got != expect {
		return nil, fmt.Errorf("%w: %s: nonce %s, expected %s", errStateChanged, name, got, expect)
	}
	updated, err := next(cur)
	if err != nil {
		return nil, err
	}
	if updated != nil || cur != nil {
		if updated == nil {
			delete(entries, name)
		} else {
			entries[name] = updated
		}
		if err := writeSandboxStateFile(s.path, entries); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if updated == nil {
		delete(s.entries, name)
		return nil, nil
	}
	cached := *updated
	s.entries[name] = &cached
	out := *updated
	return &out, nil
}

// casPut writes entry under name with a fresh nonce if name's nonce equals
// expect, and returns the new nonce.
func (s *sandboxStateStore) casPut(name string, expect sandboxNonce, entry *sandboxStateEntry) (sandboxNonce, error) {
	written, err := s.casUpdate(name, expect, func(*sandboxStateEntry) (*sandboxStateEntry, error) {
		e := *entry
		e.SandboxName = name
		e.Nonce = rand.Text()
		return &e, nil
	})
	if err != nil {
		return nonceAbsent, err
	}
	return nonceOf(written), nil
}

// casRestore writes entry under name KEEPING entry.Nonce, if name's nonce
// equals expect. It puts back an entry a Run observed and then replaced
// with its Starting entry, after a `sandbox run` that provably created
// nothing: the entry's watchers and observers stay valid, and the
// resulting ABA is benign because it is the very entry they saw.
func (s *sandboxStateStore) casRestore(name string, expect sandboxNonce, entry *sandboxStateEntry) error {
	_, err := s.casUpdate(name, expect, func(*sandboxStateEntry) (*sandboxStateEntry, error) {
		e := *entry
		e.SandboxName = name
		return &e, nil
	})
	return err
}

// casRemove removes name if its nonce equals expect. Removing an absent
// key with expect nonceAbsent succeeds and writes nothing.
func (s *sandboxStateStore) casRemove(name string, expect sandboxNonce) error {
	_, err := s.casUpdate(name, expect, func(*sandboxStateEntry) (*sandboxStateEntry, error) {
		return nil, nil
	})
	return err
}

// casMarkStopped records that the sandbox exited, with a fresh nonce, if
// name's nonce equals expect, and returns a copy of the marked entry.
func (s *sandboxStateStore) casMarkStopped(name string, expect sandboxNonce, exitCode *int, stoppedAt *time.Time) (*sandboxStateEntry, error) {
	return s.casUpdate(name, expect, func(cur *sandboxStateEntry) (*sandboxStateEntry, error) {
		if cur == nil {
			return nil, fmt.Errorf("%w: %s: no entry to mark stopped", errStateChanged, name)
		}
		e := *cur
		e.Stopped = true
		e.ExitCode = exitCode
		e.StoppedAt = stoppedAt
		e.Nonce = rand.Text()
		return &e, nil
	})
}

// get returns an entry by sandbox name from this instance's cache, or nil
// if not found.
func (s *sandboxStateStore) get(name string) *sandboxStateEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[name]
}

// list returns a snapshot of all cached entries.
func (s *sandboxStateStore) list() []*sandboxStateEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*sandboxStateEntry, 0, len(s.entries))
	for name, e := range s.entries {
		cp := *e
		cp.SandboxName = name
		result = append(result, &cp)
	}
	return result
}

// refreshKeys re-reads names from the file, in one read, and replaces
// this instance's cached entry for each with the file's (removing it if
// the file has none). It returns copies of the file's entries, keyed by
// name; a name the file does not hold maps to nil. On a read error the
// cache is left as it is and the error returned.
func (s *sandboxStateStore) refreshKeys(names []string) (map[string]*sandboxStateEntry, error) {
	unlock := lockStateFile(s.path)
	entries, err := readSandboxStateFile(s.path)
	if err != nil {
		unlock()
		return nil, err
	}
	s.mu.Lock()
	out := make(map[string]*sandboxStateEntry, len(names))
	for _, name := range names {
		e := entries[name]
		if e == nil {
			delete(s.entries, name)
			out[name] = nil
			continue
		}
		cached := *e
		cached.SandboxName = name
		s.entries[name] = &cached
		cp := cached
		out[name] = &cp
	}
	s.mu.Unlock()
	unlock()
	return out, nil
}

// load fills the cache from the file. An unreadable file leaves the cache
// empty (and is never written over: every write re-reads the file).
func (s *sandboxStateStore) load() {
	unlock := lockStateFile(s.path)
	entries, err := readSandboxStateFile(s.path)
	unlock()
	if err != nil {
		logUnreadableSandboxState(s.path, err)
		return
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
}

// sandboxReconcileProbeTimeout bounds each start-up liveness probe. A
// variable so tests can shorten it.
var sandboxReconcileProbeTimeout = 5 * time.Second

// sandboxGoneOutputs are the output fragments (lower case) that make a
// failed probe a definitive "this sandbox does not exist" answer.
//
// UNVERIFIED: the sandbox CLI's wording for an unknown sandbox is not
// documented and these fragments have not been checked against the real
// binary. They err on the side of keeping entries: a dead sandbox whose
// output matches none of them keeps its entry, which Delete still removes,
// whereas a dropped entry of a live sandbox cannot be recovered.
var sandboxGoneOutputs = []string{"not found", "no such", "does not exist"}

// sandboxProbeSaysGone reports whether a failed `sandbox exec <name> --
// /bin/true` probe (out, err, run under probeCtx) shows that the sandbox
// is gone. Only a CLI that exited non-zero on its own, before the probe's
// deadline, with not-found output qualifies. A timeout, a cancelled
// context, a CLI killed by a signal or one that could not start, and any
// other output are ambiguous: a slow but live sandbox can produce them.
func sandboxProbeSaysGone(probeCtx context.Context, out string, err error) bool {
	if err == nil || probeCtx.Err() != nil {
		return false
	}
	return exitedWithGoneOutput(out, err)
}

// exitedWithGoneOutput reports whether err is a non-zero exit of a CLI
// that exited on its own (not killed by a signal) and whose output
// contains one of sandboxGoneOutputs.
func exitedWithGoneOutput(out string, err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !exitErr.Exited() {
		return false
	}
	lower := strings.ToLower(out)
	for _, frag := range sandboxGoneOutputs {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// sandboxDeleteResult classifies the outcome of `sandbox delete --force`.
type sandboxDeleteResult int

const (
	// sandboxDeleteDeleted: the CLI exited zero.
	sandboxDeleteDeleted sandboxDeleteResult = iota
	// sandboxDeleteGone: the CLI exited non-zero on its own with not-found
	// output (sandboxGoneOutputs): the sandbox was already gone.
	sandboxDeleteGone
	// sandboxDeleteFailed: any other non-zero exit, a CLI that could not
	// start or was killed, and ctx cancellation during the delete.
	sandboxDeleteFailed
	// sandboxDeleteTimedOut: the delete-hang workaround's timeout fired.
	sandboxDeleteTimedOut
)

func (r sandboxDeleteResult) String() string {
	switch r {
	case sandboxDeleteDeleted:
		return "deleted"
	case sandboxDeleteGone:
		return "gone"
	case sandboxDeleteFailed:
		return "failed"
	case sandboxDeleteTimedOut:
		return "timedOut"
	}
	return fmt.Sprintf("sandboxDeleteResult(%d)", int(r))
}

// classifySandboxDelete classifies a `sandbox delete --force` run under
// ctx. out is the CLI's combined output, err its exec error, and timedOut
// reports that the delete-hang workaround's timeout fired. A cancelled
// ctx makes any unsuccessful delete "failed", even with not-found output:
// a killed CLI's partial output is not a definitive answer.
func classifySandboxDelete(ctx context.Context, out string, err error, timedOut bool) sandboxDeleteResult {
	switch {
	case timedOut:
		return sandboxDeleteTimedOut
	case err == nil:
		return sandboxDeleteDeleted
	case ctx.Err() != nil:
		return sandboxDeleteFailed
	case exitedWithGoneOutput(out, err):
		return sandboxDeleteGone
	default:
		return sandboxDeleteFailed
	}
}

// maxSandboxCommandOutput bounds how much captured command output (delete,
// launch) is read back.
const maxSandboxCommandOutput = 64 << 10

// maxSandboxCommandOutputLog bounds the copy of command output that is
// logged; classification and errors still see up to
// maxSandboxCommandOutput.
const maxSandboxCommandOutputLog = 4 << 10

// sandboxCommandOutputForLog returns out truncated to
// maxSandboxCommandOutputLog bytes for logging.
func sandboxCommandOutputForLog(out string) string {
	if len(out) <= maxSandboxCommandOutputLog {
		return out
	}
	return out[:maxSandboxCommandOutputLog] + "...(truncated)"
}

// newSandboxCommandOutput returns an unlinked temporary file to use as a
// sandbox command's stdout and stderr (verb names the command in the
// file name and the warning), so the output is captured without a pipe.
// A pipe would make cmd.Wait block until every holder of its write end
// exits, and processes the command leaves behind hold inherited fds: the
// delete-hang defect's orphaned runsc processes
// (cloudrun_sandbox_delete_workaround.go), and a detached sandbox's own
// processes after `sandbox run`. Closing a pipe's read end early instead
// (exec.Cmd.WaitDelay) would kill such a holder with SIGPIPE on its next
// write. With a file, later writes land harmlessly in it; an unlinked file
// held by a long-lived writer keeps growing in $TMPDIR until that writer
// exits. The caller must Close the file. On error it returns nil, and the
// caller leaves stdio nil (/dev/null) and never uses a pipe.
func newSandboxCommandOutput(verb string) *os.File {
	f, err := os.CreateTemp("", "scion-sandbox-"+verb+"-*")
	if err != nil {
		runtimeLog().Warn("cloudrun-sandbox: cannot capture command output in a file", "verb", verb, "error", err)
		return nil
	}
	_ = os.Remove(f.Name())
	return f
}

// readSandboxCommandOutputRaw returns what has been written to f so far,
// bounded by limit bytes. Reading a regular file never blocks, even while
// another process still writes to it.
func readSandboxCommandOutputRaw(f *os.File, limit int64) string {
	if f == nil {
		return ""
	}
	b, _ := io.ReadAll(io.NewSectionReader(f, 0, limit))
	return string(b)
}

// readSandboxCommandOutput returns f's output, bounded by
// maxSandboxCommandOutput and trimmed.
func readSandboxCommandOutput(f *os.File) string {
	return strings.TrimSpace(readSandboxCommandOutputRaw(f, maxSandboxCommandOutput))
}

// maxSandboxLaunchOutputRead bounds how much `sandbox run` output is read
// back: 4 MiB, above Linux's whole-argv limit (ARG_MAX, 2 MiB by default),
// so an echo of the CLI's argv, which carries every --env KEY=VALUE, is
// read whole. Independently of that, each --env KEY=VALUE is one argv
// string, which Linux caps at sandboxMaxEnvArgBytes (or the CLI never
// starts), so any occurrence of an env value that starts within the first
// maxSandboxCommandOutput bytes ends within what was read
// (shortenSandboxOutput).
const maxSandboxLaunchOutputRead = 4 << 20

// shortenSandboxOutput returns raw, the CLI output read back (at most
// maxSandboxLaunchOutputRead bytes), as an error-message value: when it is
// longer than maxSandboxCommandOutput, it is shortened and marked
// "...(truncated)".
//
// The cut rule: the output is never cut inside an occurrence of an
// environment value in values. The cut starts at maxSandboxCommandOutput
// and moves back to the start of any occurrence that spans it, until none
// does.
func shortenSandboxOutput(raw string, values map[string]string) string {
	if len(raw) <= maxSandboxCommandOutput {
		return redactEnvValues(raw, values)
	}
	cut := maxSandboxCommandOutput
	for moved := true; moved; {
		moved = false
		for _, v := range values {
			if len(v) < envValueRedactionFloor {
				continue // below the minimum length of the env value handling
			}
			// An occurrence at i spans cut when cut-len(v) < i < cut.
			lo := max(0, cut-len(v)+1)
			hi := min(len(raw), cut+len(v)-1)
			if lo >= cut || hi <= cut {
				continue
			}
			if i := strings.Index(raw[lo:hi], v); i >= 0 && lo+i < cut {
				cut = lo + i
				moved = true
			}
		}
	}
	return redactEnvValues(raw[:cut], values) + "...(truncated)"
}

// sandboxReconcileBeforeRemoveHook, if set, runs in reconcile after an
// entry's probe and before its compare-and-set removal. Nil in
// production; tests use it to model a concurrent write in that window.
var sandboxReconcileBeforeRemoveHook func(name string)

// sandboxStartingGrace is how long a Starting entry written by a start
// with no ctx deadline (synchronous) is kept by reconcile whatever its
// probe says, and the minimum for any Starting entry. A variable so tests
// can shorten it.
var sandboxStartingGrace = 2 * time.Minute

// sandboxNow is the clock for Starting entries: StartingAt, a synchronous
// start's StartingUntil, and reconcile's comparison against both. A
// variable so tests can move it.
var sandboxNow = time.Now

// startingUntil returns the StartingUntil of a Starting entry written at
// now under ctx: with a ctx deadline (an async launch) the broker's
// cleanup horizon (AsyncCleanupHorizon), so that the entry outlives every
// cleanup that may still look for it; otherwise now +
// sandboxStartingGrace. now is the entry's StartingAt, so the clock is
// read once.
func startingUntil(ctx context.Context, now time.Time) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return AsyncCleanupHorizon(dl)
	}
	return now.Add(sandboxStartingGrace)
}

// sandboxStartsInFlight is the set of Runs in progress in this process,
// sandbox name -> run ID. Run adds its (name, run) right after taking the
// name lock and removes it before releasing it, so the set changes only
// under name locks. List hides, and reconcile keeps, the Starting entry of
// a start in flight: its Run still owns it.
var sandboxStartsInFlight = struct {
	sync.Mutex
	runs map[string]string
}{runs: make(map[string]string)}

// markSandboxStartInFlight records a start of runID on name; the returned
// func clears it. The caller holds the name lock.
func markSandboxStartInFlight(name, runID string) func() {
	sandboxStartsInFlight.Lock()
	sandboxStartsInFlight.runs[name] = runID
	sandboxStartsInFlight.Unlock()
	return func() {
		sandboxStartsInFlight.Lock()
		if cur, ok := sandboxStartsInFlight.runs[name]; ok && cur == runID {
			delete(sandboxStartsInFlight.runs, name)
		}
		sandboxStartsInFlight.Unlock()
	}
}

// sandboxStartInFlight reports whether a start of entry's run on entry's
// sandbox name is in progress in this process.
func sandboxStartInFlight(entry *sandboxStateEntry) bool {
	sandboxStartsInFlight.Lock()
	defer sandboxStartsInFlight.Unlock()
	run, ok := sandboxStartsInFlight.runs[entry.SandboxName]
	return ok && run == entry.Labels[api.LabelRunID]
}

// keepStartingEntry reports whether reconcile must keep the Starting entry
// without probing it: its start is in flight in this process, or it is
// before max(StartingAt + sandboxStartingGrace, StartingUntil), which
// covers starts in other processes (a `scion` CLI on the host) and the
// broker's async cleanup horizon. A missing time counts as the zero time.
func keepStartingEntry(entry *sandboxStateEntry) bool {
	if sandboxStartInFlight(entry) {
		return true
	}
	var keepUntil time.Time
	if entry.StartingAt != nil {
		keepUntil = entry.StartingAt.Add(sandboxStartingGrace)
	}
	if entry.StartingUntil != nil && entry.StartingUntil.After(keepUntil) {
		keepUntil = *entry.StartingUntil
	}
	return sandboxNow().Before(keepUntil)
}

// reconcile checks each entry for liveness and removes entries whose
// sandbox is confirmed gone. Called once at startup. Stopped entries are
// removed. A running entry is removed only when its probe gives a
// definitive not-found answer (sandboxProbeSaysGone); on a timeout or any
// other failure the entry is kept and a warning logged. List reads only
// the state and the sandbox CLI has no list command, so dropping the entry
// of a slow but live sandbox would leave it running with no way to stop or
// delete it through scion (ptone/scion#3738).
//
// A Starting entry (ptone/scion#4757) is kept without a probe while its
// start is in flight in this process or before its horizon
// (keepStartingEntry). After that it is probed like a running entry and
// removed only on a not-found answer; a passing probe keeps it Starting,
// never adopting it as running: only its Run promotes it.
//
// The entries and their nonces are snapshotted under mu, which is then
// released; the probes run with no lock held, and each removal is a
// compare-and-set against the snapshot's nonce, so an entry rewritten
// since the snapshot (a newer run on the same name) is left alone. A
// removal must not be made while mu is held: casRemove takes the file
// mutex and then mu, which is not re-entrant.
func (s *sandboxStateStore) reconcile(bin string) {
	s.mu.Lock()
	snapshot := make([]*sandboxStateEntry, 0, len(s.entries))
	for name, e := range s.entries {
		cp := *e
		cp.SandboxName = name
		snapshot = append(snapshot, &cp)
	}
	s.mu.Unlock()

	for _, entry := range snapshot {
		name := entry.SandboxName
		if entry.Starting && keepStartingEntry(entry) {
			continue
		}
		if !entry.Stopped {
			// Probe liveness: try to exec 'true' in the sandbox.
			// R1: absolute path required — the sandbox launcher resolves argv[0]
			// before the sandbox environment (including PATH) is in effect.
			ctx, cancel := context.WithTimeout(context.Background(), sandboxReconcileProbeTimeout)
			out, err := runSimpleCommand(ctx, bin, "exec", name, "--", "/bin/true")
			gone := sandboxProbeSaysGone(ctx, out, err)
			cancel()
			switch {
			case err == nil:
				continue
			case gone:
				runtimeLog().Info("sandbox state reconcile: sandbox not found, removing",
					"name", name, "agentID", entry.AgentID)
			default:
				runtimeLog().Warn("sandbox state reconcile: liveness probe failed without a not-found answer; keeping the entry",
					"name", name, "agentID", entry.AgentID, "error", err)
				continue
			}
		}

		if sandboxReconcileBeforeRemoveHook != nil {
			sandboxReconcileBeforeRemoveHook(name)
		}
		if err := s.casRemove(name, nonceOf(entry)); err != nil {
			if errors.Is(err, errStateChanged) {
				runtimeLog().Info("sandbox state reconcile: entry changed since the snapshot; keeping it",
					"name", name, "error", err)
			} else {
				runtimeLog().Warn("sandbox state reconcile: failed to remove entry",
					"name", name, "error", err)
			}
		}
	}
}

// -----------------------------------------------------------------------
// Sandbox name helpers
// -----------------------------------------------------------------------

// sandboxNameRe matches characters NOT allowed in sandbox names.
var sandboxNameRe = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeSandboxName produces a safe sandbox name from an agent slug.
func sanitizeSandboxName(name string) string {
	s := strings.ToLower(name)
	s = sandboxNameRe.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-")
	if s == "" {
		s = "sandbox"
	}
	return s
}

// -----------------------------------------------------------------------
// /scion directory layout
// -----------------------------------------------------------------------

// scionPaths holds the resolved /scion paths for a single sandbox agent.
type scionPaths struct {
	root      string // e.g. /scion
	agentHome string // /scion/agents/<slug>/home
	workspace string // /scion/agents/<slug>/workspace
}

// prepareScionLayout creates the /scion directory structure for a sandbox
// agent and relocates broker-provisioned content into it.
//
// Motivation (§3.2a): --rootfs / is READ-ONLY (EROFS; no writable overlay,
// confirmed by diag-sbx6). --write only makes MOUNTED filesystems writable.
// Per-agent bind mounts under /scion make writable paths visible to the host.
func prepareScionLayout(rootDir, slug string, cfg RunConfig) (scionPaths, error) {
	p := scionPaths{
		root:      rootDir,
		agentHome: filepath.Join(rootDir, "agents", slug, "home"),
		workspace: filepath.Join(rootDir, "agents", slug, "workspace"),
	}

	// Create agent home and workspace directories.
	for _, d := range []string{p.agentHome, p.workspace} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return p, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Relocate agent home to /scion and symlink back for broker readback.
	// The broker provisions agent-info.json etc. at config.HomeDir. The
	// symlink ensures the broker reads from the /scion copy where sandbox
	// writes are visible via the bind mount.
	if cfg.HomeDir != "" && !strings.HasPrefix(cfg.HomeDir, rootDir+"/") && cfg.HomeDir != rootDir {
		// Reject a home directory that is not an allowed agent-home path
		// before relocating from it -- the same second-gate reasoning as the
		// Workspace check just below, and the Cloud Run counterpart to
		// buildCommonRunArgs and the Kubernetes runtime's Run().
		resolvedHomeDir, err := ValidateAgentHomeSource(cfg.HomeDir, "")
		if err != nil {
			return p, err
		}
		// A second launch of the same sandbox agent finds cfg.HomeDir
		// already relocated: the first launch replaced it with a symlink to
		// p.agentHome, and resolving that symlink (as validation must, to
		// judge the real target) makes resolvedHomeDir p.agentHome itself.
		// relocateToScion below must run on the ORIGINAL, still-symlinked
		// cfg.HomeDir in that case, not the resolved path: its own Lstat
		// check recognizes a symlink and does nothing, which is exactly
		// right on a restart. Passing the already-resolved path instead
		// would make relocateToScion operate on p.agentHome as both source
		// and destination -- Lstat then sees a real directory, not a
		// symlink, so that short-circuit never fires, every entry gets
		// renamed onto itself, and the whole directory is removed and
		// replaced with a symlink to itself, losing the agent's home state
		// and leaving a symlink loop. Skip relocation entirely once the
		// resolved path already names this agent's own home, rootDir
		// itself, or anything else under rootDir: there is nothing left
		// outside /scion to relocate from.
		if resolvedHomeDir != p.agentHome && resolvedHomeDir != rootDir && !strings.HasPrefix(resolvedHomeDir, rootDir+string(filepath.Separator)) {
			if err := relocateToScion(cfg.HomeDir, p.agentHome); err != nil {
				runtimeLog().Warn("could not relocate HomeDir to /scion; agent home writes may go to overlay",
					"homeDir", cfg.HomeDir, "scionHome", p.agentHome, "error", err)
			}
		}
	}

	// Workspace: copy content from broker-provisioned workspace to /scion.
	// Do NOT remove the original — it may be shared among agents.
	if cfg.Workspace != "" && !strings.HasPrefix(cfg.Workspace, rootDir+"/") && cfg.Workspace != rootDir {
		// Reject a workspace source that is not an allowed workspace path
		// before copying from it. This is a second gate matching the one at
		// buildCommonRunArgs and the k8s runtime's Run(): cfg.Workspace
		// already went through pkg/agent Start()'s validation to reach this
		// RunConfig, but this call site should not depend on that alone. No
		// per-project root is passed for the same reason as those two sites:
		// RunConfig carries no flag to tell "workspace outside repo root"
		// (a supported shape) apart from a bad value, so only the fixed
		// deny-set (and its named ~/.scion allow list) applies.
		resolvedWorkspace, err := ValidateWorkspaceSource(cfg.Workspace)
		if err != nil {
			return p, err
		}
		if copyErr := copyDirContents(resolvedWorkspace, p.workspace); copyErr != nil {
			// Non-fatal: agent can still read via rootfs; writes go to /scion.
			runtimeLog().Debug("workspace content not copied to /scion",
				"workspace", resolvedWorkspace, "scionWorkspace", p.workspace, "error", copyErr)
		}
	}

	// Create shared dirs under /scion/shared/<name>.
	for _, sd := range cfg.SharedDirs {
		sdPath := filepath.Join(rootDir, "shared", sd.Name)
		if err := os.MkdirAll(sdPath, 0755); err != nil {
			runtimeLog().Warn("failed to create shared dir in /scion layout",
				"name", sd.Name, "error", err)
		}
		// When InWorkspace is true, the mount destination is
		// <workspace>/.scion-volumes/<name> inside the sandbox. The
		// workspace bind mount maps p.workspace → /workspace, so create
		// the .scion-volumes/<name> subdirectory on the host side to
		// ensure the bind mount destination exists.
		if sd.InWorkspace {
			iwPath := filepath.Join(p.workspace, ".scion-volumes", sd.Name)
			if err := os.MkdirAll(iwPath, 0755); err != nil {
				runtimeLog().Warn("failed to create in-workspace shared dir",
					"name", sd.Name, "path", iwPath, "error", err)
			}
		}
	}

	// Recursively chown agent home and workspace to the sandbox user.
	// This runs AFTER all file setup (directory creation, relocation,
	// workspace copy) so that every file — including those created by
	// relocateToScion and copyDirContents — is owned by the sandbox user.
	//
	// Guard: only chown when running as root (Cloud Run) AND the target
	// UID is non-zero. When sandboxUID is 0 (root), files are already
	// root-owned and chown-to-self is a no-op that wastes syscalls on
	// large directory trees.
	//
	// fsutil.ChownTree refuses a critical-system-path or filesystem-root-lookalike
	// root before touching anything, keeps the walk from crossing onto a
	// different device, and Lchown's every entry rather than Chown'ing it
	// (a relocated directory's symlink is re-owned itself, never its
	// target, which may be outside our mount) -- all independent of
	// whatever p.agentHome/p.workspace actually resolved to.
	if os.Getuid() == 0 && sandboxUID > 0 {
		for _, d := range []string{p.agentHome, p.workspace} {
			if chownErr := fsutil.ChownTree(context.Background(), d, sandboxUID, sandboxGID); chownErr != nil {
				runtimeLog().Warn("failed to chown directory to sandbox user",
					"dir", d, "uid", sandboxUID, "gid", sandboxGID, "error", chownErr)
			}
		}
	}

	return p, nil
}

// relocateToScion moves the contents of src to dst and replaces src with
// a symlink to dst. If src does not exist or is already a symlink, this
// is a no-op.
func relocateToScion(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		// src doesn't exist — nothing to relocate.
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Already a symlink (perhaps from a previous run).
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("relocateToScion: %s is not a directory", src)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", src, err)
	}

	// Move each entry from src to dst (atomic on same filesystem).
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if err := os.Rename(srcPath, dstPath); err != nil {
			// Cross-filesystem: fall back to not relocating this entry.
			runtimeLog().Debug("could not rename to /scion, skipping",
				"src", srcPath, "dst", dstPath, "error", err)
		}
	}

	// Replace original directory with symlink so broker readback works.
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove original dir %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
		return fmt.Errorf("ensure parent of symlink %s: %w", src, err)
	}
	if err := os.Symlink(dst, src); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", src, dst, err)
	}
	return nil
}

// copyDirContents copies files from src to dst (non-recursive, best-effort).
func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			// Shallow: skip subdirectories for now.
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			continue
		}
		info, _ := entry.Info()
		perm := fs.FileMode(0644)
		if info != nil {
			perm = info.Mode().Perm()
		}
		_ = os.WriteFile(dstPath, data, perm)
	}
	return nil
}

// -----------------------------------------------------------------------
// Mount and environment construction
// -----------------------------------------------------------------------

// mountsFor returns the per-agent mount descriptors. Each sandbox gets
// only its own home, workspace, and entitled shared dirs --
// NOT the entire /scion root (which would expose every agent's paths).
//
// No tmux mount: gVisor runs without --host-uds, so AF_UNIX cannot cross
// the sandbox boundary (§4.4a). The tmux socket is deliberately
// sandbox-internal; tmux uses its default path inside the sandbox.
func mountsFor(paths scionPaths, sharedDirs []api.SharedDir) []string {
	// Mount the agent home at sandboxAgentHome (/home/scion). Note:
	// supervisor.go:113 only sets HOME when UID > 0 || Rootless, so when
	// the sandbox runs as root (UID 0, non-rootless) HOME is inherited
	// from the container environment. envFor() now sets HOME explicitly
	// to ensure it resolves here. The rootfs is read-only (EROFS,
	// confirmed by diag-sbx6), so the previous rm -rf / ln -sfn approach
	// fails. Mounting with a different destination avoids rootfs mutation
	// entirely and was confirmed working by diag-sbx6.
	//
	// Mount the workspace at sandboxWorkspace (/workspace) so that
	// sciontool init's git clone (which defaults to /workspace via
	// SCION_WORKSPACE_PATH) writes to the writable bind mount. Without
	// this remapping the workspace was mounted at the host-side path
	// (/scion/agents/<slug>/workspace) while /workspace remained on the
	// read-only rootfs, causing every git-linked agent to fail at init.
	mounts := []string{
		fmt.Sprintf("type=bind,source=%s,destination=%s", paths.agentHome, sandboxAgentHome),
		fmt.Sprintf("type=bind,source=%s,destination=%s", paths.workspace, sandboxWorkspace),
	}
	for _, sd := range sharedDirs {
		sdPath := filepath.Join(filepath.Dir(paths.agentHome), "..", "..", "shared", sd.Name)
		mountDst := fmt.Sprintf("/scion-volumes/%s", sd.Name)
		if sd.InWorkspace {
			mountDst = fmt.Sprintf("%s/.scion-volumes/%s", sandboxWorkspace, sd.Name)
		}
		spec := fmt.Sprintf("type=bind,source=%s,destination=%s", sdPath, mountDst)
		if sd.ReadOnly {
			spec += ",readonly"
		}
		mounts = append(mounts, spec)
	}
	return mounts
}

// envFor builds the environment variable map for a sandbox agent.
// Env vars are passed via repeatable --env flags (confirmed by AC-0 retest).
func envFor(cfg RunConfig, paths scionPaths) map[string]string {
	env := make(map[string]string)

	// PATH is empty inside the sandbox (AC-0 retest finding). Set a
	// reasonable default so the harness and its children can find binaries.
	env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// Parse broker-provided env vars.
	for _, e := range cfg.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	// Harness-specific env vars. Pass sandboxAgentHome (the path visible
	// INSIDE the sandbox) rather than paths.agentHome (the host-side mount
	// source). Any {{ .AgentHome }} expansion in env_template must resolve
	// to a path the agent can actually reach. No current harness uses
	// {{ .AgentHome }} in env_template (the authoring guide warns against
	// it), but if one did, it would silently get a nonexistent path without
	// this.
	if cfg.Harness != nil {
		for k, v := range cfg.Harness.GetEnv(cfg.Name, sandboxAgentHome, cfg.UnixUsername) {
			env[k] = v
		}
	}

	// No TMUX_TMPDIR: gVisor runs without --host-uds, so AF_UNIX cannot
	// cross the sandbox boundary (§4.4a). tmux uses its default socket
	// path inside the sandbox. Every other runtime uses the default too
	// (common.go:480, k8s_runtime.go:934).

	// Project identity.
	if cfg.Project != "" {
		env["SCION_PROJECT"] = cfg.Project
	}
	if cfg.ProjectID != "" {
		env["SCION_PROJECT_ID"] = cfg.ProjectID
	}

	// Workspace path: tell sciontool init where the writable workspace is
	// mounted inside the sandbox. mountsFor maps the host-side workspace
	// (e.g. /scion/agents/<slug>/workspace) to sandboxWorkspace (/workspace).
	// Without this, sciontool init defaults to /workspace which is correct,
	// but setting it explicitly documents the contract and insulates against
	// any future change to the default.
	env["SCION_WORKSPACE_PATH"] = sandboxWorkspace

	// Workspace backend.
	if cfg.WorkspaceBackendName != "" {
		env["SCION_WORKSPACE_BACKEND"] = cfg.WorkspaceBackendName
	}

	// Host UID/GID for container user synchronisation.
	//
	// On Cloud Run the launcher (and therefore os.Getuid()) runs as root
	// (UID 0). Passing UID 0 to the sandbox causes sciontool init's
	// setupHostUser to keep the process running as root, which makes
	// Claude Code ≥ 2.1.246 refuse to start ("--dangerously-skip-permissions
	// cannot be used with root/sudo privileges for security reasons").
	//
	// The omni-image creates a `scion` user at UID 1000 (see
	// image-build/scion-base/Dockerfile). Pass that UID/GID so that
	// sciontool init drops privileges to the scion user inside the
	// sandbox, regardless of the launcher's own UID.
	env["SCION_HOST_UID"] = strconv.Itoa(sandboxUID)
	env["SCION_HOST_GID"] = strconv.Itoa(sandboxGID)

	// Set HOME, USER and LOGNAME explicitly for the sandbox.
	// supervisor.go:113 only sets HOME when (UID > 0 || Rootless). On
	// Cloud Run the launcher runs as root (UID 0, non-rootless), so
	// HOME is inherited as /root. tmux reads ~/.tmux.conf relative to
	// HOME, so the pane-exited hook in the template home is never found.
	// Setting HOME here ensures the sandbox-mounted agent home at
	// sandboxAgentHome is used regardless of supervisor behaviour.
	env["HOME"] = sandboxAgentHome
	env["USER"] = "scion"
	env["LOGNAME"] = "scion"

	return env
}

// envArgs converts an env map to a sorted list of KEY=VALUE strings
// suitable for --env flag values.
func envArgs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys))
	for _, k := range keys {
		args = append(args, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return args
}

// -----------------------------------------------------------------------
// Entrypoint construction
// -----------------------------------------------------------------------

// buildEntrypoint constructs the process tree that runs inside the sandbox:
//
//	sciontool init -- sh -c '<tmux-command>'
//
// The tmux command follows the pattern from common.go:469-482, adapted for
// sandboxes which have no TTY (sandbox run --detach does not allocate one):
//
//	tmux new-session -d -s scion -n agent <agent-window-cmd> \;
//	    set-option -g window-size latest \;
//	    new-window -t scion -n shell \;
//	    select-window -t scion:agent;
//	    while tmux has-session -t scion 2>/dev/null; do sleep 2; done
func buildEntrypoint(cfg RunConfig) ([]string, error) {
	// Build the harness command line.
	cmdLine, ok := harnessCmdLine(cfg, harnessCmdLineOptions{})
	if !ok {
		return nil, fmt.Errorf("cloudrun-sandbox: no harness provided")
	}

	// Wrap the harness in a shell that records its real exit code (see
	// tmuxAgentWindowCmd for the pattern). Use absolute path for sh —
	// this runs as a tmux window command where PATH is available, but
	// absolute paths are used throughout buildEntrypoint for consistency.
	agentWindowCmd := tmuxAgentWindowCmd("/bin/sh", cmdLine)

	// Build tmux command (buildTmuxStartCmd, adapted for sandbox via
	// tmuxPollSession).
	//
	// Finding #12: `sandbox run --detach` provides no TTY. Docker allocates
	// one with `docker run -t` (docker.go:76), but sandboxes do not.
	// `tmux attach-session` fails immediately with "open terminal failed:
	// not a terminal" (rc=1), killing PID 1 and destroying the sandbox.
	//
	// Fix: replace `attach-session` with a poll loop that tracks the tmux
	// session's lifetime without needing a terminal. PID 1 exits when the
	// session ends (all windows closed), providing the same lifecycle
	// semantics as attach-session did in the Docker case.
	tmuxCmd := buildTmuxStartCmd(agentWindowCmd, tmuxPollSession)

	// CRITICAL: argv[0] must be an absolute path. The sandbox launcher resolves
	// argv[0] BEFORE the PATH env var (set by envFor) is in effect, so bare "sh"
	// silently fails (Finding #10).
	//
	// No symlink chain: agent home is bind-mounted directly at /home/scion
	// by mountsFor(). HOME is set by envFor() to sandboxAgentHome
	// (/home/scion) so it resolves to the writable mount regardless of
	// whether supervisor.go sets it. The rootfs is read-only (EROFS,
	// confirmed by diag-sbx6).
	//
	// Entrypoint output capture (#22): wrap the command in a group whose
	// stdout/stderr are redirected to a log file. On the happy path `exec`
	// replaces the shell with sciontool (which inherits the redirected fds
	// — acceptable because tmux manages its own pty). On failure the log
	// captures the error output.
	//
	// IMPORTANT: paths here are INSIDE the sandbox. Use sandboxAgentHome
	// (/home/scion, the mount destination), NOT agentHome (the host-side
	// mount source). Writing to agentHome inside the sandbox hits the
	// read-only rootfs (EROFS). The DOA probe in Run() reads from
	// agentHome on the HOST, where the bind mount makes the same files
	// visible at the source path.
	//
	// No .rc file: `exec` replaces the shell on success (echo never runs),
	// and `sandbox wait` already provides the exit code on the host side.
	logPath := filepath.Join(sandboxAgentHome, entrypointLogFile)
	// Append (>>), not truncate (>): a restart must preserve the previous
	// run's diagnostic output.  The run delimiter makes boundaries
	// unambiguous when multiple runs accumulate in the same file.  #110.
	wrappedCmd := fmt.Sprintf("echo \"--- entrypoint start $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) ---\" >> %s 2>&1; { exec sciontool init -- /bin/sh -c %s; } >> %s 2>&1",
		logPath, shellQuote(tmuxCmd), logPath)
	return []string{"/bin/sh", "-c", wrappedCmd}, nil
}

// -----------------------------------------------------------------------
// CloudRunSandboxRuntime
// -----------------------------------------------------------------------

// CloudRunSandboxRuntime implements the Runtime interface for Cloud Run
// Sandboxes. Sandboxes are nested isolated workloads launched from inside
// a Cloud Run Instance via the `sandbox` CLI binary.
//
// Architecture:
//   - Each sandbox gets per-agent bind mounts (home, workspace, tmux, shared dirs).
//   - Environment is injected via repeatable --env flags.
//   - State is tracked locally in a JSON file (no `sandbox list` command).
//   - A watcher goroutine runs `sandbox wait` to detect exits.
type CloudRunSandboxRuntime struct {
	bin           string             // path to sandbox binary
	state         *sandboxStateStore // local state (sandbox CLI has no list command)
	rootDir       string             // /scion by default; overridable for testing
	deleteTimeout time.Duration      // timeout for sandbox delete --force; 0 = default

	watchMu      sync.Mutex                    // guards watchCancels
	watchCancels map[string]context.CancelFunc // per-sandbox watcher cancel functions
}

// NewCloudRunSandboxRuntime returns a new CloudRunSandboxRuntime.
func NewCloudRunSandboxRuntime(cfg *config.V1CloudRunSandboxConfig) *CloudRunSandboxRuntime {
	bin := DefaultSandboxBin
	if cfg != nil && cfg.SandboxBin != "" {
		bin = cfg.SandboxBin
	}

	ss := newSandboxStateStore(defaultStatePath)
	ss.reconcile(bin)

	return &CloudRunSandboxRuntime{
		bin:          bin,
		state:        ss,
		rootDir:      defaultScionRoot,
		watchCancels: make(map[string]context.CancelFunc),
	}
}

func (r *CloudRunSandboxRuntime) Name() string { return "cloudrun-sandbox" }

func (r *CloudRunSandboxRuntime) ExecUser() string { return "scion" }

// Run launches an agent inside a sandbox. See brief §Deliverable 2.
// waitForSandboxLiveness polls with backoff until probe returns nil or ctx is
// cancelled. Returns nil on success, ctx.Err() on cancellation, or the last
// probe error if all attempts fail.
func waitForSandboxLiveness(ctx context.Context, delays []time.Duration, probe func(ctx context.Context) error, name string) error {
	var probeErr error
	for i, delay := range delays {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		probeErr = probe(probeCtx)
		probeCancel()
		if probeErr == nil {
			runtimeLog().Info("sandbox liveness probe passed", "name", name, "attempt", i+1)
			return nil
		}
		runtimeLog().Debug("sandbox liveness probe failed, retrying",
			"name", name, "attempt", i+1, "delay", delay, "error", probeErr)
	}
	return probeErr
}

// sandboxMaxEnvArgBytes bounds a single env entry passed to the sandbox
// CLI. Each entry travels as one "--env KEY=VALUE" argv string, which Linux
// caps at 128 KiB including the NUL terminator (MAX_ARG_STRLEN). The limit
// is checked against the whole KEY=VALUE string, not the value alone.
const sandboxMaxEnvArgBytes = 128 * 1024

// sandboxEnvLimit applies sandboxMaxEnvArgBytes to the full argv string.
var sandboxEnvLimit = envSizeLimit{maxBytes: sandboxMaxEnvArgBytes, includeKey: true}

// sandboxRuntimeEnvKeys are fixed by envFor, so an env-type secret must not
// supply them. SCION_HOST_UID/SCION_HOST_GID drive the sandbox user setup
// (the Cloud Run instance runtime reserves the same two keys).
// SCION_WORKSPACE_PATH, HOME, USER and LOGNAME describe the sandbox mount
// layout and let tmux find the pane-exited hook in the agent home; the
// user cannot change those paths, so a secret must not override them.
// PATH stays overridable, as in Docker.
var sandboxRuntimeEnvKeys = []string{
	"SCION_HOST_UID", "SCION_HOST_GID",
	"SCION_WORKSPACE_PATH", "HOME", "USER", "LOGNAME",
}

// applySecretEnvOverrides sets each env-type secret key in env to its value
// from cfgEnv, after harness, auth and synthesised env have been applied.
// As in Docker, where the secret -e flag comes last, the secret wins.
// secretKeys never collide with the caller's original cfg.Env keys, since
// applyResolvedSecretsToEnv skips those.
func applySecretEnvOverrides(env map[string]string, cfgEnv []string, secretKeys []string) {
	if len(secretKeys) == 0 {
		return
	}
	want := make(map[string]struct{}, len(secretKeys))
	for _, k := range secretKeys {
		want[k] = struct{}{}
	}
	for _, e := range cfgEnv {
		k, v, ok := strings.Cut(e, "=")
		if _, isSecret := want[k]; ok && isSecret {
			env[k] = v
		}
	}
}

// sandboxNameLocks serializes Run, Stop and Delete per sandbox name
// (ptone/scion#3738). The sandbox name is derived from the agent name and
// reused across runs, and the sandbox CLI keeps no labels, so overlapping
// operations on one name could otherwise interleave: an older Run's
// dead-on-arrival cleanup deleting the newer Run's sandbox by name, an
// older Run's probe passing on the newer sandbox and recording it as its
// own, or a Delete acting between a Run's check and its record. Run holds
// the name's lock from before its first check through the launch, the
// probe and the state record or cleanup; Delete holds it from its state
// read through the delete and the entry's removal.
//
// Each lock is a channel semaphore, so a waiter gives up when its ctx
// ends (lockSandboxNameCtx). The locks are package-level, so same-name
// operations are serialized within one process across every
// CloudRunSandboxRuntime (the factory builds one per call). A second
// process on the same host is NOT fenced (ptone/scion#3951). Entries are
// never pruned: each is an empty-struct channel, one per sandbox name
// this process has started, stopped or deleted, which is bounded by the
// broker's agents.
var sandboxNameLocks sync.Map // sandbox name -> chan struct{} (capacity 1)

// lockSandboxNameCtx takes the lock for a sandbox name and returns its
// unlock. If ctx ends first (or has already ended) it returns ctx's error
// without the lock.
func lockSandboxNameCtx(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, _ := sandboxNameLocks.LoadOrStore(name, make(chan struct{}, 1))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// select picks at random when both cases are ready: do not go ahead on
	// an already-ended ctx.
	if err := ctx.Err(); err != nil {
		<-sem
		return nil, err
	}
	return func() { <-sem }, nil
}

// sandboxUnlabelledRun is what sandboxHeldByOtherRun returns for a live
// entry with no run label.
const sandboxUnlabelledRun = "<unlabelled>"

// sandboxHeldByOtherRun returns the run ID of entry, the state file's
// entry for the sandbox name, if it is live (not stopped; a Starting entry
// is live) and does not belong to runID, or "" otherwise. A live entry
// with no run label is held too (sandboxUnlabelledRun): Run cannot tell
// whose it is, and its sandbox may be live (ptone/scion#4757). A Run with
// no run ID never conflicts. Only Run applies this rule; Stop and Delete
// still delete a legacy entry (sandboxRunCheck).
func sandboxHeldByOtherRun(entry *sandboxStateEntry, runID string) string {
	if runID == "" || entry == nil || entry.Stopped {
		return ""
	}
	run := entry.Labels[api.LabelRunID]
	switch {
	case run == "":
		return sandboxUnlabelledRun
	case run != runID:
		return run
	}
	return ""
}

// sandboxStartCleanupTimeout bounds a failed start's cleanup delete
// (failStart), on a fresh context: the start's ctx may be done.
const sandboxStartCleanupTimeout = 10 * time.Second

// sandboxLiveEntry returns e if it is live (not stopped), else nil.
func sandboxLiveEntry(e *sandboxStateEntry) *sandboxStateEntry {
	if e == nil || e.Stopped {
		return nil
	}
	return e
}

// writeStartingEntry writes entry, the full Starting entry of this start,
// before `sandbox run` (ptone/scion#4757): a compare-and-set put against
// observed, the nonce Run read under the name lock with observedEntry.
// It returns the new nonce and prior, the live entry it replaced (nil if
// none): with the held check passed, a live entry of this same run (an
// earlier attempt of the dispatch), or any live entry for a Run with no
// run ID. Any error means nothing was written and nothing may be launched.
//
// As CR-0's record did, it tolerates one benign change of the key since
// the held check: exit watchers and reconcile of any runtime instance
// write without the name lock (an old watcher marking the prior entry
// stopped, a reconcile removing a stopped entry). On errStateChanged the
// file is re-read once, the held check re-applied (ErrRunConflict) and
// the put retried once against the re-read nonce.
func (r *CloudRunSandboxRuntime) writeStartingEntry(slug, runID string, observed sandboxNonce, observedEntry, entry *sandboxStateEntry) (sandboxNonce, *sandboxStateEntry, error) {
	prior := sandboxLiveEntry(observedEntry)
	nonce, err := r.state.casPut(slug, observed, entry)
	if err == nil {
		return nonce, prior, nil
	}
	if !errors.Is(err, errStateChanged) {
		return nonceAbsent, nil, fmt.Errorf("cloudrun-sandbox: record starting sandbox %s: %w", slug, err)
	}
	if sandboxRecordRunHook != nil {
		sandboxRecordRunHook(slug, "starting-reread")
	}
	cur, curNonce, err := r.state.diskEntry(slug)
	if err != nil {
		logUnreadableSandboxState(r.state.path, err)
		return nonceAbsent, nil, fmt.Errorf("cloudrun-sandbox: record starting sandbox %s: %w", slug, err)
	}
	if other := sandboxHeldByOtherRun(cur, runID); other != "" {
		runtimeLog().Info("Sandbox of another run took the agent's sandbox name before the start; not reusing it",
			"sandbox", slug, "run_id", runID, "sandbox_run_id", other)
		return nonceAbsent, nil, fmt.Errorf("cloudrun-sandbox: sandbox %s belongs to run %q, not %q: %w", slug, other, runID, ErrRunConflict)
	}
	nonce, err = r.state.casPut(slug, curNonce, entry)
	if err != nil {
		return nonceAbsent, nil, fmt.Errorf("cloudrun-sandbox: record starting sandbox %s: %w", slug, err)
	}
	return nonce, sandboxLiveEntry(cur), nil
}

// runSandboxLaunch runs the `sandbox run` CLI: runSimpleCommand with Start
// and Wait split, so that a CLI that never started is told apart, and with
// its output captured in an unlinked temporary file
// (newSandboxCommandOutput), not a pipe: the detached sandbox's processes
// may inherit and hold the CLI's stdout and stderr, and with a file Wait
// returns when the CLI exits and their later writes are harmless. If the
// file cannot be created, the output is discarded (stdio is /dev/null, as
// on the delete paths): a pipe would let a holder block Wait forever, even
// after ctx is cancelled, with the name lock held.
//
// The output read back is bounded by maxSandboxLaunchOutputRead and is
// not processed here: the caller shortens it (shortenSandboxOutput). Cancel stays the default (kill): a graceful
// signal would let the CLI exit on its own and be classified as a
// self-failed run, and nothing would be deleted. started reports whether
// Start succeeded. The error is wrapped as runSimpleCommand wraps it, so
// callers' error text is unchanged.
func runSandboxLaunch(ctx context.Context, bin string, args ...string) (out string, started bool, err error) {
	// Log the command name and argument count only — never the argument
	// values, which carry --env KEY=VALUE pairs (see runSimpleCommand).
	runtimeLog().Debug("Executing command", "cmd", bin, "argc", len(args))
	begin := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	outFile := newSandboxCommandOutput("run")
	if outFile != nil {
		defer func() { _ = outFile.Close() }()
		cmd.Stdout = outFile
		cmd.Stderr = outFile
	}
	output := func() string { return readSandboxCommandOutputRaw(outFile, maxSandboxLaunchOutputRead) }
	if err := cmd.Start(); err != nil {
		startOut := output()
		logCommandFailure(ctx, bin, len(args), time.Since(begin), []byte(sandboxCommandOutputForLog(startOut)))
		return startOut, false, fmt.Errorf("%s failed: %w", bin, err)
	}
	err = cmd.Wait()
	elapsed := time.Since(begin)
	out = output()
	if err != nil {
		logCommandFailure(ctx, bin, len(args), elapsed, []byte(sandboxCommandOutputForLog(out)))
		return out, true, fmt.Errorf("%s failed: %w", bin, err)
	}
	runtimeLog().Debug("Command completed", "cmd", bin, "argc", len(args), "duration", elapsed)
	return strings.TrimSpace(out), true, nil
}

// sandboxLaunchOutcome classifies how the `sandbox run` CLI ended.
type sandboxLaunchOutcome int

const (
	// sandboxLaunchOK: the CLI exited 0.
	sandboxLaunchOK sandboxLaunchOutcome = iota
	// sandboxLaunchNeverStarted: Start failed; nothing was created.
	sandboxLaunchNeverStarted
	// sandboxLaunchExitedOnItsOwn: a non-zero exit, not a kill. The CLI
	// failed by itself, possibly because the name is held by a sandbox no
	// entry describes, so nothing may be deleted by name.
	sandboxLaunchExitedOnItsOwn
	// sandboxLaunchMayExist: killed by a signal (the ctx kill), or a Wait
	// failure after a successful Start: the sandbox may exist.
	sandboxLaunchMayExist
)

// classifySandboxLaunch classifies a runSandboxLaunch result. It looks
// only at what the CLI did, never at ctx: a CLI that exited non-zero on
// its own is self-failed even if ctx ended afterwards.
func classifySandboxLaunch(started bool, err error) sandboxLaunchOutcome {
	switch {
	case !started:
		return sandboxLaunchNeverStarted
	case err == nil:
		return sandboxLaunchOK
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return sandboxLaunchExitedOnItsOwn
	}
	return sandboxLaunchMayExist
}

// restoreOrRemove undoes this start's Starting entry (nonce myNonce) after
// a `sandbox run` that never started or exited on its own: it restores
// prior, the live entry the start replaced, with its original nonce, or
// removes the key if there was none. Both are compare-and-sets against
// myNonce; nothing is deleted by name, whatever happens. A failure is
// logged and leaves the file as it is.
func (r *CloudRunSandboxRuntime) restoreOrRemove(slug string, myNonce sandboxNonce, prior *sandboxStateEntry) {
	var err error
	if prior != nil {
		err = r.state.casRestore(slug, myNonce, prior)
	} else {
		err = r.state.casRemove(slug, myNonce)
	}
	switch {
	case err == nil:
	case errors.Is(err, errStateChanged):
		runtimeLog().Info("cloudrun-sandbox: state entry changed during the failed start; leaving it",
			"sandbox", slug, "error", err)
	default:
		runtimeLog().Warn("cloudrun-sandbox: could not undo the failed start's state entry; it is left as it is",
			"sandbox", slug, "error", err)
	}
}

// failStart cleans up after a start whose sandbox may exist: `sandbox run`
// was killed (or its Wait failed), the probe was cancelled, the sandbox
// was dead on arrival, or the started entry could not be recorded. It
// returns cause unchanged.
//
// It reads the key from the file. If the key still holds this start's
// entry (myNonce) or nothing, the sandbox is deleted through the
// classified delete (deleteForCleanup): the entry is removed on success
// and kept if the delete failed. If the key holds another entry, nothing
// is deleted. If the file cannot be read, the sandbox is deleted by name,
// best effort, and the entry left (as CR-0's record did): the caller holds
// the name lock, passed the held check and wrote its Starting entry, so
// the sandbox named slug is this start's.
//
// The async hooks are not consulted: until ptone/scion#4758 adds the
// hooks-active skip (with the handle report), this deviates from the
// RunConfig.OnResourceCreated contract (the runtime skips its own start
// cleanup when it is set). That is safe only while the sandbox runtime
// keeps its async launch opt-out, so the opt-out lift (ptone/scion#4759)
// must not land before ptone/scion#4758.
//
// The caller holds the name lock.
func (r *CloudRunSandboxRuntime) failStart(slug, runID string, myNonce sandboxNonce, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), sandboxStartCleanupTimeout)
	defer cancel()
	cur, nonce, err := r.state.diskEntry(slug)
	if err != nil {
		logUnreadableSandboxState(r.state.path, err)
		r.cancelWatcher(slug)
		result, delErr := r.classifiedDelete(cleanupCtx, slug)
		runtimeLog().Warn("cloudrun-sandbox: failed start: state file unreadable; deleted the sandbox by name, best effort",
			"sandbox", slug, "run_id", runID, "error", cause, "delete_result", result.String(), "delete_error", delErr)
		return cause
	}
	if cur != nil && nonce != myNonce {
		runtimeLog().Info("cloudrun-sandbox: failed start: the sandbox's state entry is no longer this start's; not deleting",
			"sandbox", slug, "run_id", runID, "sandbox_run_id", cur.Labels[api.LabelRunID], "error", cause)
		return cause
	}
	if err := r.deleteForCleanup(cleanupCtx, slug, sandboxObserved{entry: cur, nonce: nonce}); err != nil {
		runtimeLog().Warn("cloudrun-sandbox: failed start: deleting the sandbox failed; its state entry is kept",
			"sandbox", slug, "run_id", runID, "error", cause, "delete_error", err)
	}
	return cause
}

// deleteForCleanup deletes sandbox name with the classified delete and
// removes obs, the entry the caller read under the name lock, when the
// sandbox is gone afterwards (deleted, already gone, or the delete-hang
// workaround's timeout, which kills it). A failed delete keeps the entry
// and returns an error, so that a later delete can find it. Unlike Delete
// on the workaround path, a failure is never treated as success.
//
// The caller holds the name lock; this never takes it.
func (r *CloudRunSandboxRuntime) deleteForCleanup(ctx context.Context, name string, obs sandboxObserved) error {
	r.cancelWatcher(name)
	result, err := r.classifiedDelete(ctx, name)
	switch result {
	case sandboxDeleteDeleted, sandboxDeleteGone, sandboxDeleteTimedOut:
		r.removeAfterDelete(name, obs)
		return nil
	}
	if err == nil {
		err = errors.New(result.String())
	}
	return fmt.Errorf("cloudrun-sandbox: delete --force %s: %s: %w", name, result, err)
}

// promoteStartingEntry records the started sandbox once its probe passed:
// a compare-and-set put of entry (the full entry, no longer Starting)
// against myNonce, this start's Starting entry. If another writer removed
// that entry, it is re-created (a put against an absent key). If the key
// holds another entry, nothing is written and nothing deleted: another
// run's live entry gives ErrRunConflict, anything else errStateChanged;
// the sandbox is then live but untracked (logged at WARN). A state read or
// write failure goes to failStart. It returns the new nonce, for the exit
// watcher.
func (r *CloudRunSandboxRuntime) promoteStartingEntry(slug, runID string, myNonce sandboxNonce, entry *sandboxStateEntry) (sandboxNonce, error) {
	nonce, err := r.state.casPut(slug, myNonce, entry)
	if err == nil {
		return nonce, nil
	}
	if !errors.Is(err, errStateChanged) {
		return nonceAbsent, r.failStart(slug, runID, myNonce, fmt.Errorf("cloudrun-sandbox: record sandbox %s: %w", slug, err))
	}
	if sandboxRecordRunHook != nil {
		sandboxRecordRunHook(slug, "reread")
	}
	cur, _, readErr := r.state.diskEntry(slug)
	if readErr != nil {
		return nonceAbsent, r.failStart(slug, runID, myNonce, fmt.Errorf("cloudrun-sandbox: record sandbox %s: %w", slug, readErr))
	}
	if cur != nil {
		if other := sandboxHeldByOtherRun(cur, runID); other != "" {
			runtimeLog().Warn("cloudrun-sandbox: another run's sandbox took the name during this start; "+
				"this start's sandbox is live but untracked (no state entry): remove it with a by-name delete (sandbox delete --force <name>)",
				"sandbox", slug, "run_id", runID, "sandbox_run_id", other)
			return nonceAbsent, fmt.Errorf("cloudrun-sandbox: sandbox %s belongs to run %q, not %q: %w", slug, other, runID, ErrRunConflict)
		}
		runtimeLog().Warn("cloudrun-sandbox: the sandbox's state entry changed during this start; "+
			"this start's sandbox is live but untracked (no state entry): remove it with a by-name delete (sandbox delete --force <name>)",
			"sandbox", slug, "run_id", runID, "error", err)
		return nonceAbsent, fmt.Errorf("cloudrun-sandbox: record sandbox %s: %w", slug, err)
	}
	if sandboxRecordRunHook != nil {
		sandboxRecordRunHook(slug, "retry")
	}
	nonce, err = r.state.casPut(slug, nonceAbsent, entry)
	switch {
	case err == nil:
		return nonce, nil
	case errors.Is(err, errStateChanged):
		runtimeLog().Warn("cloudrun-sandbox: the sandbox's state entry changed again during this start; "+
			"this start's sandbox is live but untracked (no state entry): remove it with a by-name delete (sandbox delete --force <name>)",
			"sandbox", slug, "run_id", runID, "error", err)
		return nonceAbsent, fmt.Errorf("cloudrun-sandbox: record sandbox %s: %w", slug, err)
	default:
		return nonceAbsent, r.failStart(slug, runID, myNonce, fmt.Errorf("cloudrun-sandbox: record sandbox %s: %w", slug, err))
	}
}

// sandboxRecordRunHook, if set, runs after a nonce mismatch: in
// writeStartingEntry with stage "starting-reread" before the file is
// re-read, and in promoteStartingEntry with stage "reread" before the
// file is re-read and "retry" before the re-creating put. Nil in
// production; tests use it to change the state file in those windows.
var sandboxRecordRunHook func(name, stage string)

// sandboxBeforeStartingWriteHook, if set, runs after the held check and
// the launch preparation, just before the Starting entry is written. Nil
// in production; tests use it to change the state file in that window.
var sandboxBeforeStartingWriteHook func(name string)

// sandboxLaunchReturnedHook, if set, runs after the `sandbox run` command
// returns and before its result is classified. Nil in production; tests
// use it to cancel ctx in that window.
var sandboxLaunchReturnedHook func(name string)

// sandboxAfterLaunchHook, if set, runs after a successful `sandbox run`
// and before the liveness probe. Nil in production; tests use it to model
// a concurrent start or state change inside that window.
var sandboxAfterLaunchHook func(name string)

func (r *CloudRunSandboxRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	slug := sanitizeSandboxName(cfg.Name)
	runID := cfg.Labels[api.LabelRunID]

	// Serialize same-name Runs, Stops and Deletes, from before the held
	// check to the state record or cleanup (sandboxNameLocks). Taken before
	// anything else; a cancelled wait returns the ctx error.
	unlock, err := lockSandboxNameCtx(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("cloudrun-sandbox: wait for sandbox name %s: %w", slug, err)
	}
	defer unlock()
	// This start is in flight until Run returns (sandboxStartsInFlight).
	// Deferred after the unlock, so it is cleared before the lock is
	// released.
	defer markSandboxStartInFlight(slug, runID)()

	// Fold resolved secrets into cfg.Env before anything is created and
	// before envFor reads it, so they reach the sandbox as --env values and
	// are covered by the error-output redaction (externalEnvValues reads
	// cfg.Env).
	secretKeys, err := applyResolvedSecretsToEnv(&cfg, sandboxEnvLimit, sandboxRuntimeEnvKeys...)
	if err != nil {
		return "", fmt.Errorf("cloudrun-sandbox: %w", err)
	}

	// The state file, not this instance's cache, is authoritative: the
	// factory builds an instance per lookup, so the cache can predate
	// another Run's record. An unreadable file fails closed, before
	// anything is created.
	current, observed, err := r.state.diskEntry(slug)
	if err != nil {
		logUnreadableSandboxState(r.state.path, err)
		return "", fmt.Errorf("cloudrun-sandbox: %w", err)
	}

	// The sandbox name is held by a live sandbox of another run: reusing
	// the name would take that sandbox over (its probe passes and this
	// run's entry replaces the other's), so refuse, as the Cloud Run and
	// Kubernetes runtimes do. Start's pre-clean normally removes it
	// first; this is reached after a failed listing.
	if other := sandboxHeldByOtherRun(current, runID); other != "" {
		runtimeLog().Info("Sandbox of another run holds the agent's sandbox name; not reusing it",
			"sandbox", slug, "run_id", runID, "sandbox_run_id", other)
		return "", fmt.Errorf("cloudrun-sandbox: sandbox %s belongs to run %q, not %q: %w", slug, other, runID, ErrRunConflict)
	}

	// OQ-14 (§11.12) proved that Vertex AI and gcloud-adc auth modes work
	// on this runtime via the metadata emulator. The emulator runs INSIDE
	// each sandbox on loopback (GCE_METADATA_HOST=localhost:18380), not on
	// the launcher's link-local address (reverted in 9877da59). It mints
	// no tokens locally — it delegates to the hub via SCION_HUB_ENDPOINT.
	// This path works for all callers that honour the GCE_METADATA_HOST
	// environment variable — which includes all five shipped harnesses and
	// the standard Google auth SDKs (google-auth-library for Node,
	// google-auth for Python, cloud.google.com/go/compute/metadata for Go).
	//
	// Callers that hardcode 169.254.169.254 and ignore GCE_METADATA_HOST
	// will fail outright: iptables -t nat does not exist in gVisor, so
	// there is no transparent interception fallback (§4.10).
	if cfg.ResolvedAuth != nil {
		method := cfg.ResolvedAuth.Method
		if method == "vertex-ai" || strings.Contains(strings.ToLower(method), "gcloud") {
			runtimeLog().Info("cloudrun-sandbox: GCP auth mode in sandbox — "+
				"credentials provided via in-sandbox metadata emulator on loopback",
				"method", method, "agent", cfg.Name)
		}
	}

	// Prepare the /scion directory layout (§3.2a: rootfs is read-only;
	// only mounted paths are writable).
	paths, err := prepareScionLayout(r.rootDir, slug, cfg)
	if err != nil {
		return "", fmt.Errorf("cloudrun-sandbox: prepare layout: %w", err)
	}

	// Stat the entrypoint log BEFORE the sandbox launches to capture the
	// current size as an offset. This run's output starts after this point;
	// prior content (from earlier runs, if PR 1323 append-mode is present)
	// is skipped. The entrypoint has not started yet, so nothing can write
	// to the file between this stat and sandbox launch.
	entrypointLogPath := filepath.Join(paths.agentHome, entrypointLogFile)
	var entrypointLogOffset int64
	if st, err := os.Stat(entrypointLogPath); err == nil {
		entrypointLogOffset = st.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Unexpected stat error (permission denied, I/O error). Proceed with
		// offset 0 but warn — this may cause prior-run output to be re-shipped.
		runtimeLog().Warn("entrypoint log stat failed unexpectedly; starting from offset 0",
			"path", entrypointLogPath, "error", err, "sandbox", slug)
	}
	// ENOENT is the normal case for first-ever run: offset stays 0.

	// Build environment.
	env := envFor(cfg, paths)

	// Apply resolved auth env vars and files (mirrors Docker path at
	// common.go applyResolvedAuth). Without this, auth env vars like
	// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION do not reach the
	// sandbox process.
	if cfg.ResolvedAuth != nil {
		for k, v := range cfg.ResolvedAuth.EnvVars {
			env[k] = v
		}

		// Copy auth files into the agent home directory on the host side.
		// paths.agentHome is bind-mounted into the sandbox at
		// sandboxAgentHome (/home/scion), so files placed here are visible
		// inside the sandbox at the expected paths.
		//
		// Defense-in-depth: ApplyAuthSettings (run.go) already stages auth
		// files via auth-candidates.json before the runtime runs, so files
		// normally reach the sandbox through that path. This block mirrors
		// the Docker runtime's file-copy mode (common.go:820-839) for
		// robustness in case the staging path is bypassed or incomplete.
		for _, f := range cfg.ResolvedAuth.Files {
			if f.SourcePath == "" {
				continue
			}
			containerPath := expandTildeTarget(f.ContainerPath, sandboxAgentHome)
			var relPath string
			if strings.HasPrefix(containerPath, sandboxAgentHome+"/") {
				relPath = strings.TrimPrefix(containerPath, sandboxAgentHome+"/")
			} else {
				relPath = strings.TrimPrefix(containerPath, "/")
			}
			dst := filepath.Join(paths.agentHome, relPath)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				runtimeLog().Warn("cloudrun-sandbox: failed to create directory for auth file",
					"dst", dst, "error", err)
				continue
			}
			if err := util.CopyFile(f.SourcePath, dst); err != nil {
				runtimeLog().Warn("cloudrun-sandbox: failed to copy auth file",
					"src", f.SourcePath, "dst", dst, "error", err)
				continue
			}
		}
	}

	// Env-type secrets win over harness, auth and synthesised env, as in
	// Docker, except sandboxRuntimeEnvKeys. Keys from the caller's cfg.Env
	// still win over secrets.
	applySecretEnvOverrides(env, cfg.Env, secretKeys)

	// Build entrypoint command.
	entrypoint, err := buildEntrypoint(cfg)
	if err != nil {
		return "", err
	}

	// Build the sandbox run command.
	// C1: No `create` verb. Use `sandbox run <name> --detach --rootfs / --write --allow-egress`.
	// C3: --allow-egress is MANDATORY.
	args := []string{
		"run", slug,
		"--detach",
		"--rootfs", "/",
		"--write",
		"--allow-egress",
	}

	// Per-agent mounts -- each sandbox gets only its own paths (not /scion root).
	for _, m := range mountsFor(paths, cfg.SharedDirs) {
		args = append(args, "--mount", m)
	}

	// The template env fallback (ptone/scion#4524) is the LAST env step:
	// only keys no source above set, never a suppressed key.
	emitted := make(map[string]bool, len(env))
	for k := range env {
		emitted[k] = true
	}
	for _, e := range FallbackEnvEntries(cfg.FallbackEnv, cfg.SuppressedFallbackKeys, emitted) {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}

	// Environment variables via --env flags (repeatable, confirmed by AC-0 retest).
	for _, e := range envArgs(env) {
		args = append(args, "--env", e)
	}

	args = append(args, "--")
	args = append(args, entrypoint...)

	WriteRuntimeDebugFile(cfg, r.bin, args)

	// Record the start before anything is launched (ptone/scion#4757): the
	// full entry, marked Starting, so that a start interrupted between
	// `sandbox run` and the probe (a cancelled synchronous request, a
	// crash) leaves an entry that Delete, Stop, pre-clean and reconcile can
	// find. If it cannot be written, nothing is launched.
	startedAt := sandboxNow()
	until := startingUntil(ctx, startedAt)
	entry := &sandboxStateEntry{
		SandboxName:   slug,
		AgentID:       cfg.Name,
		ProjectID:     cfg.ProjectID,
		Project:       cfg.Project,
		Labels:        cfg.Labels,
		Template:      cfg.Template,
		HarnessConfig: labelValue(cfg.Labels, "scion.harness_config"),
		CreatedAt:     startedAt,
		AgentHome:     paths.agentHome,
		Workspace:     paths.workspace,
		Image:         cfg.Image,

		EntrypointLogOffset: entrypointLogOffset,

		Starting:      true,
		StartingAt:    &startedAt,
		StartingUntil: &until,
	}
	if sandboxBeforeStartingWriteHook != nil {
		sandboxBeforeStartingWriteHook(slug)
	}
	myNonce, prior, err := r.writeStartingEntry(slug, runID, observed, current, entry)
	if err != nil {
		return "", err
	}

	out, started, err := runSandboxLaunch(ctx, r.bin, args...)
	if sandboxLaunchReturnedHook != nil {
		sandboxLaunchReturnedHook(slug)
	}
	if outcome := classifySandboxLaunch(started, err); outcome != sandboxLaunchOK {
		// #1342: `out` is the sandbox binary's combined stdout+stderr, and a CLI
		// that reports the offending invocation on an argument error will echo
		// its own argv -- which carries every --env KEY=VALUE pair. This error
		// reaches an HTTP response body via handlers.go RuntimeError(), so the
		// values are redacted before they are spliced in. Measured, not assumed:
		// see TestCloudRunSandboxRun_ErrorDoesNotLeakEnvValues.
		//
		// `err` itself is safe to wrap as-is on this base: #127 P5 reduced
		// runSimpleCommand's error to `%s failed: %w` with no argv. The silent
		// case of that test is the guard on it -- if P5 is ever reverted, the
		// leak returns through THIS %w and that case goes red.
		//
		// %w is deliberately preserved. handlers.go calls errors.Is(err,
		// agent.ErrContainerNameInUse) at :899 and :1422, and
		// classifyLaunchRuntimeError calls errors.Is(err, exec.ErrNotFound);
		// rebuilding this error from a string would sever that chain silently.
		//
		// Long output is shortened without cutting inside an environment
		// value (shortenSandboxOutput).
		safeOut := shortenSandboxOutput(out, externalEnvValues(cfg, env))
		runErr := fmt.Errorf("cloudrun-sandbox: run failed: %w (output: %s)", err, safeOut)
		if outcome == sandboxLaunchMayExist {
			// Killed (by ctx) or its Wait failed: the sandbox may exist.
			return "", r.failStart(slug, runID, myNonce, runErr)
		}
		// Never started, or exited on its own: nothing was created by this
		// start. Undo its entry; never delete by name (the name may be held
		// by a sandbox no entry describes).
		r.restoreOrRemove(slug, myNonce, prior)
		return "", runErr
	}

	if sandboxAfterLaunchHook != nil {
		sandboxAfterLaunchHook(slug)
	}

	// Post-run liveness probe: `sandbox run --detach` returns rc=0 even for
	// sandboxes that die immediately (e.g. because the entrypoint binary
	// cannot be resolved — see R1 absolute-path fix above). The exit code
	// carries no information about whether the sandbox is actually alive.
	// Probe with `sandbox exec` to confirm liveness before reporting success.
	// Retry with backoff to give the sandbox time to initialize its control
	// server.
	//
	// Finding from §1 E2E walk (2026-08-26): 4 of 6 test sandboxes returned
	// rc=0 from `run --detach` but were dead within 5 seconds. Without this
	// probe, dead-on-arrival sandboxes are permanently reported as running.
	//
	// The retry delays are a best-guess bounded by observation: sandboxes
	// that die do so in under 5 seconds (diag-sbx3 matrix). There is no
	// precise measurement of sandbox startup latency. If this probe proves
	// flaky in practice, lengthen the ladder rather than removing the probe.
	probeDelays := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	probeErr := waitForSandboxLiveness(ctx, probeDelays, func(probeCtx context.Context) error {
		_, err := runSimpleCommand(probeCtx, r.bin, "exec", slug, "--", "/bin/true")
		return err
	}, slug)
	if probeErr != nil {
		// A cancelled context is not a dead sandbox — the caller asked us
		// to stop. Return the context error without dead-on-arrival
		// diagnostics; the sandbox may be live, so it is deleted
		// (failStart), as long as the entry is still this start's.
		if ctx.Err() != nil {
			return "", r.failStart(slug, runID, myNonce, ctx.Err())
		}

		// Read entrypoint diagnostics from the host filesystem.
		// paths.agentHome is the HOST-SIDE mount source; the sandbox writes
		// to sandboxAgentHome (/home/scion, the mount destination). The bind
		// mount makes them the same file. We cannot use `sandbox exec` here
		// — the probe just proved the sandbox is dead.
		// Only this run's output (from entrypointLogOffset): with append
		// mode, the whole file would show a prior run's output as the cause.
		var diagInfo string
		logPath := filepath.Join(paths.agentHome, entrypointLogFile)
		if logStr := entrypointLogTail(logPath, entrypointLogOffset, entrypointLogTailMax); logStr != "" {
			diagInfo = fmt.Sprintf("\nentrypoint log (%s):\n%s", logPath, logStr)
		} else {
			diagInfo = fmt.Sprintf("\nno entrypoint output from this run (%s)", logPath)
		}

		runtimeLog().Error("sandbox dead on arrival: all liveness probes failed",
			"name", slug, "agentID", cfg.Name, "error", probeErr, "diagnostics", diagInfo)
		errMsg := fmt.Sprintf("cloudrun-sandbox: sandbox dead on arrival after run returned rc=0 — "+
			"all liveness probes failed: %v", probeErr)
		if diagInfo != "" {
			errMsg += diagInfo
		}
		// Clean up — the sandbox may be in a broken state.
		return "", r.failStart(slug, runID, myNonce, errors.New(errMsg))
	}

	// Record the started sandbox: the full entry, no longer Starting,
	// replacing this start's Starting entry (or re-creating it if another
	// writer removed it).
	running := *entry
	running.CreatedAt = time.Now()
	running.Starting = false
	running.StartingAt = nil
	running.StartingUntil = nil
	nonce, err := r.promoteStartingEntry(slug, runID, myNonce, &running)
	if err != nil {
		return "", err
	}

	// Start a watcher goroutine that blocks on `sandbox wait` and records
	// the exit in the state store when the sandbox terminates.
	// The context is cancelled by deleteOrWorkaround to unblock the watcher
	// when the sandbox is force-deleted.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	r.watchMu.Lock()
	r.watchCancels[slug] = watchCancel
	r.watchMu.Unlock()
	go r.watchSandbox(watchCtx, slug, nonce)
	go r.tailEntrypointLog(watchCtx, slug, cfg.Name, cfg.Project, paths, entrypointLogOffset)

	return slug, nil
}

// Stop removes the sandbox ref.ID: there is no stop/pause verb, so Stop
// is Delete, including its run check (sandboxRunCheck).
func (r *CloudRunSandboxRuntime) Stop(ctx context.Context, ref RunRef) error {
	// sandbox delete requires --force for running sandboxes.
	// There is no stop/pause verb; Stop == Delete.
	return r.Delete(ctx, ref)
}

// Delete removes the sandbox ref.ID. It holds the sandbox name lock
// (lockSandboxNameCtx) from its state read through the delete and the
// entry's removal, so it waits for a same-name Run in progress; a
// cancelled wait returns the ctx error. The sandbox name is reused across
// runs, so when ref.RunID is set the state file's entry is checked first
// (sandboxRunCheck) and a sandbox of another run is left untouched
// (ptone/scion#2550).
func (r *CloudRunSandboxRuntime) Delete(ctx context.Context, ref RunRef) error {
	id := ref.ID
	unlock, err := lockSandboxNameCtx(ctx, id)
	if err != nil {
		return fmt.Errorf("cloudrun-sandbox: wait for sandbox name %s: %w", id, err)
	}
	defer unlock()

	// The state file, not this instance's cache, is authoritative.
	entry, nonce, err := r.state.diskEntry(id)
	if err != nil {
		logUnreadableSandboxState(r.state.path, err)
		return fmt.Errorf("cloudrun-sandbox: %w", err)
	}
	if proceed, err := sandboxRunCheck(ref, entry); !proceed {
		return err
	}
	// Always use --force: sandbox delete without it silently fails for
	// running sandboxes. NEVER fall back to plain delete (without --force) --
	// it refuses AND kills the sandbox anyway, leaving orphaned
	// runsc-gofer/runsc-sandbox processes behind a CLI that reports "not
	// running". This is the more dangerous defect. See
	// defect-sandbox-delete-hang.md.
	return r.deleteOrWorkaround(ctx, id, sandboxObserved{entry: entry, nonce: nonce})
}

// sandboxRunCheck enforces ref.RunID before a Stop or Delete, from entry,
// the state file's entry for ref.ID (the only record of sandboxes: the CLI
// has no list). It reports whether the delete may proceed:
//   - no run ID: proceed by name, as before run IDs existed;
//   - an entry of ref.RunID, or a legacy entry with no run label (the rule
//     k8s and the broker apply): proceed;
//   - an entry of another run: do not proceed; ErrRunMismatch;
//   - no entry: do not proceed; nil. The run's sandbox is already gone
//     (deleted, or pruned by reconcile), as k8s treats a missing pod, and a
//     sandbox of a newer run still starting (run, before its entry is
//     recorded) is not deleted by name.
func sandboxRunCheck(ref RunRef, entry *sandboxStateEntry) (bool, error) {
	if ref.RunID == "" {
		return true, nil
	}
	if entry == nil {
		runtimeLog().Info("Run-scoped sandbox delete: no state entry; treating the run's sandbox as gone",
			"sandbox", ref.ID, "run_id", ref.RunID)
		return false, nil
	}
	if run := entry.Labels[api.LabelRunID]; run != "" && run != ref.RunID {
		runtimeLog().Info("Left a sandbox of another run untouched",
			"sandbox", ref.ID, "run_id", ref.RunID, "sandbox_run_id", run)
		return false, fmt.Errorf("sandbox %s belongs to run %q, not %q: %w", ref.ID, run, ref.RunID, ErrRunMismatch)
	}
	return true, nil
}

// sandboxObserved is the state entry a delete read under the name lock
// (nil if none) and its nonce; the delete's removal compares against it.
type sandboxObserved struct {
	entry *sandboxStateEntry
	nonce sandboxNonce
}

// deleteOrWorkaround dispatches to the workaround or the plain path based
// on the SCION_CLOUDRUN_DELETE_WORKAROUND kill switch. The watcher cancel
// is performed here so neither downstream path needs it -- and removing
// the workaround file does not lose the watcher cancel logic. The caller
// holds the name lock; obs is what it read under it.
func (r *CloudRunSandboxRuntime) deleteOrWorkaround(ctx context.Context, id string, obs sandboxObserved) error {
	r.cancelWatcher(id)
	if deleteWorkaroundEnabled {
		return r.deleteWithTimeout(ctx, id, obs)
	}
	return r.deletePlain(ctx, id, obs)
}

// cancelWatcher cancels this instance's exit watcher for the sandbox id,
// if any, so `sandbox wait` doesn't hang after the sandbox is deleted.
func (r *CloudRunSandboxRuntime) cancelWatcher(id string) {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if cancel, ok := r.watchCancels[id]; ok {
		cancel()
		delete(r.watchCancels, id)
	}
}

// classifiedDelete runs `sandbox delete --force id` on the path the
// SCION_CLOUDRUN_DELETE_WORKAROUND kill switch selects and returns its
// classification, with no user-facing mapping and no state change. err is
// the delete's error, if any: the CLI's exec error, a CLI that could not
// start, or ctx's error when the workaround's delete was cancelled (all
// classified failed). The caller holds the name lock and has cancelled
// the watcher.
func (r *CloudRunSandboxRuntime) classifiedDelete(ctx context.Context, id string) (sandboxDeleteResult, error) {
	if deleteWorkaroundEnabled {
		result, cmdErr, abort := r.runDeleteWithTimeout(ctx, id)
		if abort != nil {
			return sandboxDeleteFailed, abort
		}
		return result, cmdErr
	}
	return r.runDeletePlain(ctx, id)
}

// removeAfterDelete removes the entry of a deleted sandbox: a
// compare-and-set removal against obs, the entry the delete read under the
// name lock.
//
// A refinement of the design's strict compare-and-set (CR-0,
// ptone/scion#4755): the exit watcher of the deleted sandbox usually runs
// in another runtime instance (the factory builds one per lookup), which
// the delete cannot cancel. When the delete ends, that watcher's `sandbox
// wait` returns and it may mark the entry stopped first, changing its
// nonce. So on errStateChanged the file is re-read once, and the entry is
// removed against the re-read nonce only if it is the same run's entry
// (same run label and CreatedAt) now marked stopped; anything else is left
// alone and logged.
func (r *CloudRunSandboxRuntime) removeAfterDelete(id string, obs sandboxObserved) {
	err := r.state.casRemove(id, obs.nonce)
	if err == nil {
		return
	}
	if !errors.Is(err, errStateChanged) {
		runtimeLog().Warn("cloudrun-sandbox: failed to remove the deleted sandbox's state entry",
			"sandbox", id, "error", err)
		return
	}
	cur, curNonce, readErr := r.state.diskEntry(id)
	if readErr != nil {
		runtimeLog().Warn("cloudrun-sandbox: failed to remove the deleted sandbox's state entry",
			"sandbox", id, "error", readErr)
		return
	}
	if !sameRunMarkedStopped(obs.entry, cur) {
		runtimeLog().Info("cloudrun-sandbox: state entry changed during the delete; leaving it",
			"sandbox", id, "error", err)
		return
	}
	if err := r.state.casRemove(id, curNonce); err != nil {
		if errors.Is(err, errStateChanged) {
			// Benign: e.g. another instance's reconcile removed it first.
			runtimeLog().Info("cloudrun-sandbox: state entry changed again during the delete; leaving it",
				"sandbox", id, "error", err)
			return
		}
		runtimeLog().Warn("cloudrun-sandbox: failed to remove the deleted sandbox's state entry",
			"sandbox", id, "error", err)
	}
}

// sameRunMarkedStopped reports whether cur is observed's entry (same run
// label and CreatedAt) marked stopped since.
func sameRunMarkedStopped(observed, cur *sandboxStateEntry) bool {
	if observed == nil || cur == nil || !cur.Stopped {
		return false
	}
	return cur.Labels[api.LabelRunID] == observed.Labels[api.LabelRunID] &&
		cur.CreatedAt.Equal(observed.CreatedAt)
}

// deletePlain is the non-workaround path: plain `sandbox delete --force`.
// Used when SCION_CLOUDRUN_DELETE_WORKAROUND=off.
//
// A sandbox that is already gone (sandboxDeleteGone) counts as deleted:
// the entry is removed and nil returned. Any failure keeps the entry.
func (r *CloudRunSandboxRuntime) deletePlain(ctx context.Context, id string, obs sandboxObserved) error {
	switch result, err := r.runDeletePlain(ctx, id); result {
	case sandboxDeleteDeleted, sandboxDeleteGone:
	default:
		return fmt.Errorf("cloudrun-sandbox: delete --force failed: %w", err)
	}
	r.removeAfterDelete(id, obs)
	return nil
}

// runDeletePlain runs a plain `sandbox delete --force id` and returns its
// classification and exec error; it changes no state.
func (r *CloudRunSandboxRuntime) runDeletePlain(ctx context.Context, id string) (sandboxDeleteResult, error) {
	cmd := exec.CommandContext(ctx, r.bin, "delete", "--force", id)
	outFile := newSandboxCommandOutput("delete")
	if outFile != nil {
		defer func() { _ = outFile.Close() }()
		cmd.Stdout = outFile
		cmd.Stderr = outFile
	}
	err := cmd.Run()
	out := readSandboxCommandOutput(outFile)
	result := classifySandboxDelete(ctx, out, err, false)
	switch result {
	case sandboxDeleteDeleted:
	case sandboxDeleteGone:
		runtimeLog().Info("cloudrun-sandbox: delete --force found the sandbox already gone",
			"sandbox", id, "output", sandboxCommandOutputForLog(out))
	default:
		runtimeLog().Warn("cloudrun-sandbox: delete --force failed",
			"sandbox", id, "result", result.String(), "error", err, "output", sandboxCommandOutputForLog(out))
	}
	return result, err
}

// List returns agent info for all tracked sandboxes, applying an optional
// label filter.
//
// A Starting entry (ptone/scion#4757) whose start is in flight in this
// process is hidden: its Run owns it, and a concurrent start's pre-clean
// must not delete it. Any other Starting entry is re-read from the file
// first, since this instance's cache may predate its promotion or
// removal; the file's entry replaces the cached one and every rule is
// applied to it (in flight: hidden; Starting: phase "starting"; started:
// "running"; stopped: "stopped"; gone: omitted). If the file cannot be
// read, the cached entry is reported as "starting".
func (r *CloudRunSandboxRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	entries := r.listEntries()

	var agents []api.AgentInfo
	for _, entry := range entries {
		if entry.Starting && sandboxStartInFlight(entry) {
			continue
		}
		// Label filtering uses the shared LabelsMatchFilter, so the project
		// path is compared as a resolved path, as on the other runtimes.
		if !LabelsMatchFilter(entry.Labels, labelFilter) {
			continue
		}

		statusStr := "running"
		phase := "running"
		switch {
		case entry.Stopped:
			statusStr = "stopped"
			phase = "stopped"
		case entry.Starting:
			statusStr = "starting"
			phase = "starting"
		}

		// STOPGAP (§9.1b): Report ExitCode=nil for stopped sandboxes.
		// Non-zero ExitCode is hardcoded to PhaseError at handlers_runtime_brokers.go:719,
		// and Instance teardown (our normal shutdown) SIGKILLs every sandbox simultaneously.
		// Reporting the real code would put the entire fleet into PhaseError on every
		// normal teardown. This stopgap can be removed once BOTH conditions hold:
		//
		//   1. state.ExitReason gains a platform_eviction-style value so that
		//      routine Instance teardown can be distinguished from a real crash.
		//   2. The phase-promotion logic in handlers_runtime_brokers.go consults
		//      ExitReason before promoting PhaseStopped → PhaseError (condition 2
		//      is load-bearing — adding the enum value alone changes nothing).
		//
		// The state store tracks the real exit code internally (in sandboxStateEntry)
		// so it is available when the above conditions are met.
		agents = append(agents, api.AgentInfo{
			ContainerID:     entry.SandboxName,
			RunID:           entry.Labels[api.LabelRunID],
			Name:            entry.AgentID,
			ContainerStatus: statusStr,
			Phase:           phase,
			Image:           entry.Image,
			Labels:          entry.Labels,
			Template:        entry.Template,
			HarnessConfig:   entry.HarnessConfig,
			Project:         entry.Project,
			ProjectID:       entry.ProjectID,
			Runtime:         r.Name(),
		})
	}

	return agents, nil
}

// listEntries returns this instance's cached entries, with every Starting
// entry whose start is not in flight in this process replaced by the
// state file's entry for its name (dropped if the file has none). The
// file is read once, only if there is such an entry.
func (r *CloudRunSandboxRuntime) listEntries() []*sandboxStateEntry {
	entries := r.state.list()
	var stale []string
	for _, e := range entries {
		if e.Starting && !sandboxStartInFlight(e) {
			stale = append(stale, e.SandboxName)
		}
	}
	if len(stale) == 0 {
		return entries
	}
	fresh, err := r.state.refreshKeys(stale)
	if err != nil {
		logUnreadableSandboxState(r.state.path, err)
		return entries
	}
	out := entries[:0]
	for _, e := range entries {
		if f, ok := fresh[e.SandboxName]; ok {
			if f == nil {
				continue
			}
			e = f
		}
		out = append(out, e)
	}
	return out
}

func (r *CloudRunSandboxRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	// Try tmux capture-pane first (live sandbox path).
	// Absolute paths required -- PATH is empty inside a sandbox.
	logs, err := runSimpleCommand(ctx, r.bin, "exec", id, "--", "/usr/bin/tmux", "capture-pane", "-p", "-t", "scion", "-S", "-1000")
	if err == nil && strings.TrimSpace(logs) != "" {
		return logs, nil
	}

	// Fall back to reading the entrypoint log from the host filesystem
	// via the state entry. This triggers in two cases:
	//
	//   1. tmux exec failed — the sandbox is likely dead.
	//   2. tmux succeeded but returned empty output — the pane has
	//      printed nothing (or only whitespace). Showing the entrypoint
	//      log is strictly better than an empty tab, and the source
	//      label (below) tells the operator what they are looking at.
	//
	// WHY FALLBACK, NOT FLAG CHECK: on this tier, liveness flags have
	// been wrong before (#17: hub reported agents "running" when the
	// entrypoint had hung). A flag-check strategy would consult the
	// flag, and if it said "running" while the sandbox was dead, it
	// would SKIP the fallback and return the exec error — leaving the
	// tab empty in precisely the case the feature exists for. Try-and-
	// fallback is strictly more robust: it does not consult the flag.

	var reason string
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Preserve the tmux failure for diagnostics. The operator gets
		// the fallback content (better than empty), but the exec error
		// is recorded so it can be found during debugging.
		runtimeLog().Warn("tmux capture-pane failed, falling back to entrypoint log",
			"agent_id", id, "error", err)
		reason = "sandbox was not reachable via tmux"
	} else {
		reason = "tmux returned no terminal output"
	}

	return r.readEntrypointLogFromState(id, reason)
}

// readEntrypointLogFromState reads the entrypoint log from the host
// filesystem using the AgentHome path stored in the sandbox state entry.
// This is the same file and the same host-side path that the DOA handler
// (line 812-826) reads after a failed liveness probe — the mechanism
// exists, this gives it an exit through GetLogs.
func (r *CloudRunSandboxRuntime) readEntrypointLogFromState(id, reason string) (string, error) {
	entry := r.state.get(id)
	if entry == nil {
		return "", fmt.Errorf("cloudrun-sandbox: sandbox %q not found in state store", id)
	}
	if entry.AgentHome == "" {
		return "", fmt.Errorf("cloudrun-sandbox: sandbox %q has no AgentHome in state entry", id)
	}

	logPath := filepath.Join(entry.AgentHome, entrypointLogFile)
	data, err := os.ReadFile(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// State the observation, not the inference.
			return fmt.Sprintf("[no entrypoint log at %s]", logPath), nil
		}
		return "", fmt.Errorf("cloudrun-sandbox: failed to read entrypoint log at %s: %w", logPath, err)
	}

	if len(data) == 0 {
		return fmt.Sprintf("[entrypoint log at %s is empty]", logPath), nil
	}

	// Label the source so the operator knows this is startup output,
	// not live terminal content. This is mandatory: without it, the
	// output is indistinguishable from a live pane. The reason is
	// per-case so the operator can distinguish "agent is dead" from
	// "agent has not printed yet."
	header := fmt.Sprintf("[source: entrypoint log (startup) — %s]\n", reason)
	return header + string(data), nil
}

func (r *CloudRunSandboxRuntime) Attach(ctx context.Context, id string) error {
	// Look up the sandbox to verify it exists and is running.
	entry := r.state.get(id)
	if entry == nil {
		return fmt.Errorf("cloudrun-sandbox: sandbox %q not found", id)
	}
	// Set tmux window-size to latest for proper resize behavior.
	_, _ = runSimpleCommand(ctx, r.bin, "exec", id, "--",
		"/usr/bin/tmux", "set-option", "-g", "window-size", "latest")
	// Interactive attach via sandbox exec.
	// TERM=xterm-256color is load-bearing: without it tmux sees TERM=dumb
	// and exits with "terminal does not support clear" -- which looks like
	// a PTY failure but is not one (design doc section 4.4a-rev).
	return runInteractiveCommand(r.bin, "exec", id,
		"--env", "TERM=xterm-256color", "--",
		"/usr/bin/tmux", "attach-session", "-t", "scion")
}

// ImageExists returns true — the omni-image is always present.
func (r *CloudRunSandboxRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	return true, nil
}

// ImageID returns a fixed identifier — the omni-image is always present.
func (r *CloudRunSandboxRuntime) ImageID(ctx context.Context, image string) (string, error) {
	return "omni-image", nil
}

// RemoveImage is a no-op — the omni-image cannot be removed.
func (r *CloudRunSandboxRuntime) RemoveImage(ctx context.Context, image string) error {
	return nil
}

// PullImage is a no-op — the omni-image is already present.
func (r *CloudRunSandboxRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

// Sync is a no-op — the filesystem is shared via bind mounts. The
// sandbox uses --rootfs / (the launcher's filesystem) and the /scion
// bind mount makes writes visible to both sides. There is nothing
// additional to synchronise.
func (r *CloudRunSandboxRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return nil
}

func (r *CloudRunSandboxRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// sandbox exec has no --user flag; the process runs as the sandbox's
	// configured user (which is the scion user via the omni-image entrypoint).
	// Absolute paths: PATH is empty inside a sandbox, so callers must provide
	// absolute paths or the command must be on a bind-mounted path.
	args := append([]string{"exec", id, "--"}, cmd...)
	return runSimpleCommand(ctx, r.bin, args...)
}

// ExecWithStdin runs cmd inside the sandbox with stdin piped from the given
// reader. Unlike docker/podman, `sandbox exec` has no explicit -i flag: it
// forwards whatever stdio is attached to the launcher process (see Attach,
// which relies on the same behaviour for interactive tmux sessions). See
// #1355.
func (r *CloudRunSandboxRuntime) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	args := append([]string{"exec", id, "--"}, cmd...)
	return runSimpleCommandWithStdin(ctx, stdin, r.bin, args...)
}

// GetWorkspacePath returns the HOST-side workspace path from the state
// store (e.g. /scion/agents/<slug>/workspace). This is the path on the
// launcher filesystem, not the path inside the sandbox — mountsFor
// remaps the workspace to /workspace inside the sandbox, so the two
// differ. The interface contract (interface.go:121) defines the return
// value as the host path.
func (r *CloudRunSandboxRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	entry := r.state.get(id)
	if entry == nil {
		return "", fmt.Errorf("cloudrun-sandbox: sandbox %q not found in state store", id)
	}
	return entry.Workspace, nil
}

// -----------------------------------------------------------------------
// Watcher goroutine
// -----------------------------------------------------------------------

// watchSandbox blocks on `sandbox wait <name>` and updates the state
// store when the sandbox exits. The context allows deleteOrWorkaround to
// cancel the watcher when the sandbox is force-deleted. nonce is the
// nonce of the entry this watcher watches: the exit is recorded only if
// the key still holds that entry, so a late watcher never marks a newer
// run's entry stopped.
func (r *CloudRunSandboxRuntime) watchSandbox(ctx context.Context, name string, nonce sandboxNonce) {
	// sandbox wait blocks until the sandbox exits.
	cmd := exec.CommandContext(ctx, r.bin, "wait", name)
	out, err := cmd.CombinedOutput()

	// If the context was cancelled (sandbox was force-deleted), don't
	// update the state store — deleteOrWorkaround handles cleanup.
	if ctx.Err() != nil {
		runtimeLog().Debug("watcher cancelled for deleted sandbox", "name", name)
		return
	}

	now := time.Now()
	var exitCode *int

	if err != nil {
		// C5 (§4.3d): sandbox wait exit-code semantics are undocumented.
		// Treat ExitCode=nil as normal ("nil means unknown"), not an error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			exitCode = &code
		}
		// Non-ExitError (e.g. binary not found) is logged but not fatal.
	} else {
		// Try to parse exit code from stdout if provided.
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			if code, parseErr := strconv.Atoi(trimmed); parseErr == nil {
				exitCode = &code
			}
		}
	}

	// Log at info level — sandbox exit is normal (C5).
	runtimeLog().Info("sandbox exited", "name", name, "exitCode", exitCode)
	entry, markErr := r.state.casMarkStopped(name, nonce, exitCode, &now)
	switch {
	case errors.Is(markErr, errStateChanged):
		runtimeLog().Debug("sandbox state entry changed since the watcher started; not marking it stopped",
			"name", name, "error", markErr)
	case markErr != nil:
		runtimeLog().Warn("failed to record sandbox exit in the state store", "name", name, "error", markErr)
	}

	// A non-zero or unknown exit (e.g. death during provisioning) is logged
	// with this run's entrypoint output: once Delete drops the state entry,
	// GetLogs can no longer reach it. A non-zero code is an error; an
	// unknown code (nil) is treated as normal (C5) and logged at warn.
	// Only the entry this watcher marked is used.
	if exitCode == nil || *exitCode != 0 {
		if entry != nil && entry.AgentHome != "" {
			logPath := filepath.Join(entry.AgentHome, entrypointLogFile)
			logFn, msg := runtimeLog().Error, "sandbox exited abnormally"
			if exitCode == nil {
				logFn, msg = runtimeLog().Warn, "sandbox exited with unknown exit code"
			}
			logFn(msg,
				"name", name, "agentID", entry.AgentID, "exitCode", exitCode,
				"waitOutput", strings.TrimSpace(string(out)),
				"entrypointLog", entrypointLogTail(logPath, entry.EntrypointLogOffset, entrypointLogTailMax))
		}
	}

	// Clean up the cancel function from the map, unless the entry changed
	// since this watcher started: the map may then hold a newer run's
	// cancel for the name, which must stay.
	if !errors.Is(markErr, errStateChanged) {
		r.watchMu.Lock()
		delete(r.watchCancels, name)
		r.watchMu.Unlock()
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// entrypointLogTailMax bounds the entrypoint output put in errors and logs.
const entrypointLogTailMax = 2000

// entrypointLogTail returns the last max bytes of the entrypoint log
// written at or after offset (this run's output), or "" when there is
// none or the file cannot be read. A file shorter than offset was
// truncated and is read from the start. At most max bytes are read, so
// a large append-mode log is never loaded whole.
func entrypointLogTail(path string, offset int64, max int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if offset < 0 || size < offset {
		offset = 0
	}
	start := offset
	if size-int64(max) > start {
		start = size - int64(max)
	}
	if size <= start {
		return ""
	}
	buf := make([]byte, size-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	tail := string(buf[:n])
	if start > offset {
		return "...(truncated)\n" + tail
	}
	return tail
}

// labelValue safely retrieves a label value, returning "" if the map is
// nil or the key is absent.
func labelValue(labels map[string]string, key string) string {
	if labels == nil {
		return ""
	}
	return labels[key]
}
