/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// agentLimitsMaxBytes bounds readLimitsState's read. A legitimate
// agent-limits.json is a handful of integer counters; 1 MiB is generous
// headroom with no legitimate case anywhere near it.
const agentLimitsMaxBytes = 1 << 20

// ExitCodeLimitsExceeded is the exit code used when an agent is stopped due to
// exceeding configured limits (max_turns, max_model_calls, or max_duration).
const ExitCodeLimitsExceeded = 10

// LimitsState represents the persisted limit counters in agent-limits.json.
type LimitsState struct {
	TurnCount      int    `json:"turn_count"`
	ModelCallCount int    `json:"model_call_count"`
	MaxTurns       int    `json:"max_turns"`
	MaxModelCalls  int    `json:"max_model_calls"`
	StartedAt      string `json:"started_at"`
}

// LimitsHandler tracks turn and model call counts and enforces configured limits.
// When a limit is exceeded, it updates the agent status, logs the event, and
// signals sciontool init (trigger file, SIGUSR1 fallback) to initiate
// shutdown. Init, not the hook, reports limits_exceeded to the Hub: init is
// long-lived and not bound by the harness's hook timeout.
type LimitsHandler struct {
	maxTurns        int
	maxModelCalls   int
	limitsPath      string
	triggerFilePath string
	statusHandler   *StatusHandler
	// hub reports the updated counts. nil means no Hub reporting.
	hub *HubHandler
}

// NewLimitsHandler creates a new limits handler.
// Reads SCION_MAX_TURNS and SCION_MAX_MODEL_CALLS from the environment.
// Counts are reported through hubHandler, which may be nil (no Hub). Pass
// the same HubHandler the hook dispatches to, so both share one client and
// one budget.
// Returns nil if no limits are configured.
func NewLimitsHandler(hubHandler *HubHandler) *LimitsHandler {
	maxTurns := ParseEnvInt("SCION_MAX_TURNS")
	maxModelCalls := ParseEnvInt("SCION_MAX_MODEL_CALLS")

	if maxTurns <= 0 && maxModelCalls <= 0 {
		return nil
	}

	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}

	return &LimitsHandler{
		maxTurns:        maxTurns,
		maxModelCalls:   maxModelCalls,
		limitsPath:      filepath.Join(home, "agent-limits.json"),
		triggerFilePath: LimitsTriggerFile,
		statusHandler:   NewStatusHandler(),
		hub:             hubHandler,
	}
}

// NewLimitsHandlerWithPath creates a LimitsHandler with an explicit path (for testing).
func NewLimitsHandlerWithPath(maxTurns, maxModelCalls int, limitsPath string) *LimitsHandler {
	if maxTurns <= 0 && maxModelCalls <= 0 {
		return nil
	}
	return &LimitsHandler{
		maxTurns:        maxTurns,
		maxModelCalls:   maxModelCalls,
		limitsPath:      limitsPath,
		triggerFilePath: LimitsTriggerFile,
		statusHandler:   NewStatusHandler(),
	}
}

// Handle processes a hook event and increments the appropriate counter.
// On agent-end: increments turn count, checks max_turns.
// On model-end: increments model call count, checks max_model_calls.
func (h *LimitsHandler) Handle(event *hooks.Event) error {
	if h == nil {
		return nil
	}

	switch event.Name {
	case hooks.EventAgentEnd:
		if h.maxTurns <= 0 {
			return nil
		}
		return h.incrementAndCheck("turn_count", h.maxTurns, "max_turns")

	case hooks.EventModelEnd:
		if h.maxModelCalls <= 0 {
			return nil
		}
		return h.incrementAndCheck("model_call_count", h.maxModelCalls, "max_model_calls")

	default:
		return nil
	}
}

// InitLimitsFile creates or resets the agent-limits.json file. Called
// during post-start to initialize counters (they reset on each
// start/resume). When uid > 0, the file is chowned to uid:gid (the scion
// user hook processes run as) via an fchown on the temp file's open fd
// before the rename, not a separate path-based chown afterwards — init
// runs as root and calls this after sup.Run has already started the
// workload, so a path-based chown at that point has a real race window
// against a symlink swapped in at limitsPath.
func InitLimitsFile(limitsPath string, maxTurns, maxModelCalls, uid, gid int) error {
	ls := LimitsState{
		TurnCount:      0,
		ModelCallCount: 0,
		MaxTurns:       maxTurns,
		MaxModelCalls:  maxModelCalls,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	return writeLimitsState(limitsPath, &ls, uid, gid)
}

// incrementAndCheck reads the limits file, increments the given counter field,
// checks if the limit is exceeded, and triggers shutdown if so.
func (h *LimitsHandler) incrementAndCheck(counterField string, limit int, limitName string) error {
	ls, err := h.readLimitsState()
	if err != nil {
		// If we can't read the file, log and continue - don't crash the hook pipeline
		log.Error("Failed to read agent-limits.json: %v", err)
		return nil
	}

	// Increment the appropriate counter
	var count int
	switch counterField {
	case "turn_count":
		ls.TurnCount++
		count = ls.TurnCount
	case "model_call_count":
		ls.ModelCallCount++
		count = ls.ModelCallCount
	}

	// Write the updated state. Skip chown (uid<=0): this runs from a hook
	// process that already runs as the scion user, so the rewritten file
	// keeps the ownership it's created with.
	if err := writeLimitsState(h.limitsPath, ls, 0, 0); err != nil {
		log.Error("Failed to write agent-limits.json: %v", err)
		return nil
	}

	// Check if the limit is exceeded. The local actions come before any Hub
	// call so that an unreachable Hub never delays the shutdown signal.
	if count >= limit {
		message := fmt.Sprintf("%s of %d exceeded (completed %d)", limitName, limit, count)
		h.triggerLimitsExceeded(message)
	}

	// Report updated counts to Hub
	if h.hub != nil {
		if err := h.hub.ReportCounts(ls.TurnCount, ls.ModelCallCount); err != nil {
			log.Error("Failed to report counts to Hub: %v", err)
		}
	}

	return nil
}

// triggerLimitsExceeded updates status, logs the event, and signals init.
// It makes no Hub call: init reports limits_exceeded to the Hub when it sees
// the signal (reportHookLimitsExceeded in cmd/sciontool/commands/init.go).
func (h *LimitsHandler) triggerLimitsExceeded(message string) {
	// 1. Update agent-info.json to LIMITS_EXCEEDED (sticky)
	if err := h.statusHandler.UpdateActivity(state.ActivityLimitsExceeded, ""); err != nil {
		log.Error("Failed to set limits_exceeded status: %v", err)
	}

	// 2. Log the event
	log.TaggedInfo("LIMITS_EXCEEDED", "Agent stopped: %s", message)

	// 3. Signal init process to initiate shutdown (trigger file + SIGUSR1 fallback)
	if err := h.signalLimitsExceeded(message); err != nil {
		log.Error("Failed to signal limits exceeded: %v", err)
	}
}

// readLimitsState reads the agent-limits.json file.
//
// LimitsHandler is only ever constructed inside the dropped `sciontool
// hook` subprocess today, so this read is not currently a privilege-
// boundary crossing — but its sibling writeLimitsState is already
// fd-based and no-follow, and a future caller that moves this into root's
// own context should not silently inherit an unhardened read just because
// this one predates that hardening. dirfd.ReadFileNoFollow gives it the
// same symlink/FIFO/oversize refusals as every other root-context state
// read in this codebase, at no behavioral cost to the current dropped
// caller.
func (h *LimitsHandler) readLimitsState() (*LimitsState, error) {
	data, err := dirfd.ReadFileNoFollow(h.limitsPath, agentLimitsMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", h.limitsPath, err)
	}
	var ls LimitsState
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", h.limitsPath, err)
	}
	return &ls, nil
}

// writeLimitsState writes the limits state to disk atomically. When uid > 0,
// the file is chowned to uid:gid by fchown on the temp file's open fd
// before the rename, never by a separate path-based chown afterwards that a
// symlink swapped in at path could redirect to an arbitrary file.
// chownLimitsStateFn is writeLimitsState's own fd-based chown hook —
// defaults to syscall.Fchown, overridable only by this package's own tests
// (mirroring pkg/sciontool/hub's fchownFn) so a test can observe exactly
// when the chown fires relative to the rename that publishes the new
// content at path, without needing real root to chown to an arbitrary uid.
var chownLimitsStateFn = syscall.Fchown

// writeLimitsState marshals ls and installs it at path the same fd-based,
// no-follow way every other atomic write into a workload-owned directory in
// this codebase does: this can run as root (InitLimitsFile's own caller,
// RunInit, calls it before privilege drop) against a path under agentHome,
// which the workload owns outright and can replace any entry in — a plain
// path-based os.Rename would follow a symlink planted at path to an
// arbitrary target, so this instead goes through dirfd.
// WriteFileNoFollowWithChown's fd-based temp-file-then-rename sequence,
// whose fd-based Chown happens strictly before the rename that publishes
// the new content at path, never after.
func writeLimitsState(path string, ls *LimitsState, uid, gid int) error {
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling limits state: %w", err)
	}
	// ReplaceLeaf, not RefuseSymlink: path lives inside agentHome, which the
	// workload owns outright, so whatever currently sits at the leaf is a
	// stale entry this install means to overwrite, not tamper to refuse.
	if err := dirfd.WriteFileNoFollowWithChown(path, data, 0600, uid, gid, dirfd.ReplaceLeaf, chownLimitsStateFn); err != nil {
		return fmt.Errorf("writing limits state: %w", err)
	}
	return nil
}

// LimitsTriggerFile is the well-known path for the limits-exceeded trigger file.
// When a hook handler detects a limit is exceeded, it creates this file with
// the limit message as its content. The init process watches for it to
// initiate shutdown and reports the message to the Hub (see
// ReadLimitsTriggerMessage).
const LimitsTriggerFile = "/tmp/scion-limits-exceeded"

// limitsTriggerMaxBytes bounds ReadLimitsTriggerMessage's read. The hook
// writes a one-line message well under this.
const limitsTriggerMaxBytes = 1024

// limitsTriggerMessageMaxLen bounds the message ReadLimitsTriggerMessage
// returns, matching the Hub status message length the hook used to send.
const limitsTriggerMessageMaxLen = 200

// DefaultLimitsExceededMessage is the message used when the trigger file
// carries no usable message (absent, empty, refused, or an older hook that
// wrote a fixed marker).
const DefaultLimitsExceededMessage = "limits exceeded"

// ReadLimitsTriggerMessage returns the limit message a hook wrote to the
// trigger file at path, for init to report to the Hub. Init runs as root and
// the file lives in world-writable /tmp, so the read refuses symlinks, FIFOs
// and oversize files, and the content is reduced to printable characters
// and truncated. It never fails: anything unusable yields
// DefaultLimitsExceededMessage.
func ReadLimitsTriggerMessage(path string) string {
	data, err := dirfd.ReadFileNoFollow(path, limitsTriggerMaxBytes)
	if err != nil {
		return DefaultLimitsExceededMessage
	}
	msg := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, string(data)))
	if msg == "" || msg == "exceeded" {
		return DefaultLimitsExceededMessage
	}
	return truncateMessage(msg, limitsTriggerMessageMaxLen)
}

// signalLimitsExceeded notifies PID 1 that a limit has been exceeded.
// It writes a trigger file holding message and also attempts SIGUSR1 as a
// fallback.
func (h *LimitsHandler) signalLimitsExceeded(message string) error {
	triggerPath := h.triggerFilePath
	if triggerPath == "" {
		triggerPath = LimitsTriggerFile
	}
	// Primary mechanism: create a trigger file that init watches for.
	// This works regardless of UID differences between the hook process
	// and PID 1 (init runs as root, hooks run as the scion user).
	if err := os.WriteFile(triggerPath, []byte(message), 0666); err != nil {
		log.Error("Failed to write limits trigger file: %v", err)
	}

	// Fallback: send SIGUSR1 to PID 1. This may fail with EPERM when the
	// hook process runs as a non-root user and PID 1 runs as root.
	if err := signalInitFn(syscall.SIGUSR1); err != nil {
		// Expected to fail when running as non-root; the trigger file
		// is the reliable mechanism.
		log.Debug("SIGUSR1 to PID 1 failed (expected if non-root): %v", err)
		return nil
	}
	return nil
}

// signalInitFn sends sig to PID 1 (sciontool init). Tests replace it so a
// test run as root inside an agent container can never signal that
// container's own init.
var signalInitFn = func(sig syscall.Signal) error {
	p, err := os.FindProcess(1)
	if err != nil {
		return fmt.Errorf("finding PID 1: %w", err)
	}
	return p.Signal(sig)
}

// ParseEnvInt reads an integer from an environment variable. Returns 0 if unset or invalid.
func ParseEnvInt(key string) int {
	val := os.Getenv(key)
	if val == "" {
		return 0
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return n
}
