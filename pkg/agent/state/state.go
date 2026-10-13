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

// Package state defines the canonical agent state types used throughout the
// scion platform. All other packages should import these types rather than
// defining their own status/state constants.
package state

import (
	"fmt"
	"strings"
	"time"
)

// HarnessExitCodeFile is the container-local path where the tmux agent-window
// wrapper records the harness's real exit code, read by `sciontool init`.
// There is one agent per container, so a fixed path is safe. It is a shared
// contract: the runtime writes the file and `sciontool init` reads it to
// recover the authoritative harness exit code (the harness runs as a tmux
// grandchild whose exit code is otherwise invisible to the supervisor).
const HarnessExitCodeFile = "/tmp/scion-harness-exit-code"

// HarnessInitRunFile is the container-local path where `sciontool init`
// records the current init run, written at the start of each run before the
// tmux session is created. It holds one line, "nonce=HEX mode=hook" or
// "nonce=HEX mode=settle" (see FormatInitRun / ParseInitRun). The agent
// runtime reads it to decide when the harness is ready for a pasted message.
const HarnessInitRunFile = "/tmp/scion-init-run"

// HarnessReadyFile is the container-local path where `sciontool hook` records
// that the harness has emitted its first qualifying hook event for the current
// init run. It holds one line, "nonce=HEX", copying the nonce from
// HarnessInitRunFile. A marker whose nonce differs belongs to an earlier run.
const HarnessReadyFile = "/tmp/scion-harness-ready"

// Harness readiness modes recorded in HarnessInitRunFile.
const (
	// ReadyModeHook means the harness reports readiness through its hooks:
	// the runtime waits for HarnessReadyFile to carry the current nonce.
	ReadyModeHook = "hook"
	// ReadyModeSettle means the harness has no usable early hook: the runtime
	// waits until the tmux session has existed for a fixed settle period.
	ReadyModeSettle = "settle"
)

// Harness readiness timing. These constants are the single readiness rule
// shared by the runtime message buffer (which waits before pasting a message)
// and `sciontool init` (which reports the agent as working, ending the
// initializing activity).
const (
	// ReadySettleSession is how long the tmux session must exist before a
	// settle-mode harness (or one with no init-run file) counts as ready.
	ReadySettleSession = 8 * time.Second
	// ReadyDeadline bounds the wait for a hook-mode ready marker. At the
	// deadline the runtime pastes a buffered message anyway and init reports
	// the agent as working, so no agent stays initializing forever.
	ReadyDeadline = 90 * time.Second
)

// FormatInitRun renders the HarnessInitRunFile content for nonce and mode,
// including the trailing newline.
func FormatInitRun(nonce, mode string) string {
	return "nonce=" + nonce + " mode=" + mode + "\n"
}

// FormatReadyMarker renders the HarnessReadyFile content for nonce,
// including the trailing newline.
func FormatReadyMarker(nonce string) string {
	return "nonce=" + nonce + "\n"
}

// ParseInitRun parses HarnessInitRunFile content. ok is false when the
// content is not exactly one well-formed line with a hex nonce and a known
// mode; callers treat that as "no init-run file".
func ParseInitRun(content string) (nonce, mode string, ok bool) {
	fields, ok := parseReadinessFields(content)
	if !ok || len(fields) != 2 {
		return "", "", false
	}
	nonce, mode = fields["nonce"], fields["mode"]
	if !validNonce(nonce) || (mode != ReadyModeHook && mode != ReadyModeSettle) {
		return "", "", false
	}
	return nonce, mode, true
}

// ParseReadyMarker parses HarnessReadyFile content. ok is false when the
// content is not exactly one well-formed "nonce=HEX" line.
func ParseReadyMarker(content string) (nonce string, ok bool) {
	fields, ok := parseReadinessFields(content)
	if !ok || len(fields) != 1 || !validNonce(fields["nonce"]) {
		return "", false
	}
	return fields["nonce"], true
}

// maxNonceLen bounds an accepted nonce; writers use 32 hex characters.
const maxNonceLen = 64

func parseReadinessFields(content string) (map[string]string, bool) {
	content = strings.TrimSuffix(content, "\n")
	if content == "" || strings.ContainsAny(content, "\n\r\x00") {
		return nil, false
	}
	fields := map[string]string{}
	for _, tok := range strings.Split(content, " ") {
		key, val, found := strings.Cut(tok, "=")
		if !found || key == "" {
			return nil, false
		}
		if _, dup := fields[key]; dup {
			return nil, false
		}
		fields[key] = val
	}
	return fields, true
}

func validNonce(n string) bool {
	if n == "" || len(n) > maxNonceLen {
		return false
	}
	for i := 0; i < len(n); i++ {
		if !isLowerHex(n[i]) {
			return false
		}
	}
	return true
}

// isLowerHex reports whether c is 0-9 or a-f.
func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// Phase represents the infrastructure lifecycle phase of an agent.
// Phase is controlled by platform operations (broker commands, heartbeats,
// container events) — not by the LLM agent itself.
type Phase string

const (
	PhaseCreated      Phase = "created"
	PhaseProvisioning Phase = "provisioning"
	PhaseCloning      Phase = "cloning"
	PhaseStarting     Phase = "starting"
	PhaseRunning      Phase = "running"
	PhaseSuspended    Phase = "suspended"
	PhaseStopping     Phase = "stopping"
	PhaseStopped      Phase = "stopped"
	PhaseError        Phase = "error"
	// PhaseHalted is terminal: the platform ended a run that should still
	// be running (preempted, evicted, OOM-killed, or its container went
	// missing). The detail is the agent's exit reason. Unlike stopped it is
	// never a run the user or the agent ended, and unlike error it is not a
	// harness crash. A start resumes it like a start from stopped.
	PhaseHalted Phase = "halted"
)

// allPhases is the internal list; Phases() returns a copy.
var allPhases = []Phase{
	PhaseCreated,
	PhaseProvisioning,
	PhaseCloning,
	PhaseStarting,
	PhaseRunning,
	PhaseSuspended,
	PhaseStopping,
	PhaseStopped,
	PhaseError,
	PhaseHalted,
}

// Phases returns a copy of all valid Phase values.
func Phases() []Phase {
	out := make([]Phase, len(allPhases))
	copy(out, allPhases)
	return out
}

// Ordinal returns the forward-progress ordering of a phase.
// Higher values represent later lifecycle stages.
// Returns 0 for terminal or special phases (stopped, error, halted, suspended,
// stopping)
// where regression checks do not apply.
func (p Phase) Ordinal() int {
	switch p {
	case PhaseCreated:
		return 1
	case PhaseProvisioning:
		return 2
	case PhaseCloning:
		return 3
	case PhaseStarting:
		return 4
	case PhaseRunning:
		return 5
	default:
		return 0
	}
}

// IsActivePhase reports whether this phase is part of the forward-progress
// lifecycle (created through running). Regression guards apply only between
// active phases — terminal phases (stopped, error) and special phases
// (suspended, stopping) are excluded.
func (p Phase) IsActivePhase() bool {
	return p.Ordinal() > 0
}

// String implements fmt.Stringer.
func (p Phase) String() string { return string(p) }

// IsValid reports whether p is one of the defined Phase constants.
func (p Phase) IsValid() bool {
	for _, v := range allPhases {
		if p == v {
			return true
		}
	}
	return false
}

// Validate returns an error if p is not a valid Phase.
func (p Phase) Validate() error {
	if p.IsValid() {
		return nil
	}
	return fmt.Errorf("invalid phase: %q", p)
}

// Activity represents what a running agent is doing.
// Activity is only meaningful when Phase == PhaseRunning.
type Activity string

const (
	// ActivityInitializing means the container and harness process are up
	// but the harness has not yet shown it is reading input. `sciontool init`
	// reports it at start and replaces it with working once the harness is
	// ready (see ReadyDeadline). Displayed as "starting".
	ActivityInitializing    Activity = "initializing"
	ActivityWorking         Activity = "working"
	ActivityThinking        Activity = "thinking"
	ActivityExecuting       Activity = "executing"
	ActivityWaitingForInput Activity = "waiting_for_input"
	ActivityBlocked         Activity = "blocked"
	ActivityCompleted       Activity = "completed"
	ActivityLimitsExceeded  Activity = "limits_exceeded"
	ActivityStalled         Activity = "stalled"
	ActivityOffline         Activity = "offline"
	ActivityCrashed         Activity = "crashed"
)

// ExitReason describes why an agent process terminated. Unlike Activity
// (self-reported by the agent), ExitReason is runtime-derived and set by
// the platform when it observes a container/process exit.
type ExitReason string

const (
	ExitReasonCrashed        ExitReason = "crashed"
	ExitReasonLimitsExceeded ExitReason = "limits_exceeded"
	// ExitReasonPreempted is set when a Kubernetes pod's DisruptionTarget
	// condition reports PreemptionByScheduler: the scheduler evicted the
	// agent pod to make room for a higher-priority pod.
	ExitReasonPreempted ExitReason = "preempted"
	// ExitReasonEvicted is set when a Kubernetes pod was removed by any
	// other disruption path: the pod status reason Evicted (kubelet
	// node-pressure eviction), or a DisruptionTarget condition reason other
	// than PreemptionByScheduler (for example TerminationByKubelet,
	// EvictionByEvictionAPI, or another disruption-controller reason).
	ExitReasonEvicted ExitReason = "evicted"
	// ExitReasonOOMKilled is set when the agent container was terminated
	// for exceeding its memory limit (Kubernetes container termination
	// reason OOMKilled). It is a crash, not a disruption: the workload
	// exceeded its own limit, so a relaunch without a change hits it again.
	ExitReasonOOMKilled ExitReason = "oom_killed"
)

// IsValid reports whether r is a recognised ExitReason value.
// Empty string is valid (no reason given). Related values are grouped onto
// their own case line rather than listed in one combined case.
func (r ExitReason) IsValid() bool {
	if r == ExitReasonContainerMissing {
		return true
	}
	switch r {
	case "":
		return true
	case ExitReasonCrashed, ExitReasonLimitsExceeded:
		return true
	case ExitReasonPreempted, ExitReasonEvicted:
		return true
	case ExitReasonOOMKilled:
		return true
	}
	return false
}

// exitReasonLabels are the display labels of the exit reasons a halted
// agent can carry (and of the other known reasons), so text shown to users
// names a reason from a fixed set and never repeats a reported value
// verbatim.
var exitReasonLabels = map[ExitReason]string{
	ExitReasonCrashed:          "crashed",
	ExitReasonLimitsExceeded:   "limits exceeded",
	ExitReasonPreempted:        "preempted",
	ExitReasonEvicted:          "evicted",
	ExitReasonOOMKilled:        "out of memory",
	ExitReasonContainerMissing: "container lost",
}

// Label returns the display label of r for a halted agent: a fixed phrase
// for each known reason, and "unknown" for anything else, including no
// reason (a halt always records one).
func (r ExitReason) Label() string {
	if l, ok := exitReasonLabels[r]; ok {
		return l
	}
	return "unknown"
}

// ExitReasonContainerMissing is set by the Hub when the agent's runtime
// broker keeps reporting a complete runtime inventory that no longer contains
// the agent's container (for example a Kubernetes pod removed by a node
// drain), so the agent cannot still be running.
const ExitReasonContainerMissing ExitReason = "container_missing"

// allActivities is the internal list; Activities() returns a copy.
var allActivities = []Activity{
	ActivityInitializing,
	ActivityWorking,
	ActivityThinking,
	ActivityExecuting,
	ActivityWaitingForInput,
	ActivityBlocked,
	ActivityCompleted,
	ActivityLimitsExceeded,
	ActivityStalled,
	ActivityOffline,
	ActivityCrashed,
}

// Activities returns a copy of all valid Activity values.
func Activities() []Activity {
	out := make([]Activity, len(allActivities))
	copy(out, allActivities)
	return out
}

// String implements fmt.Stringer.
func (a Activity) String() string { return string(a) }

// IsValid reports whether a is one of the defined Activity constants or empty.
// An empty activity is valid (it means no activity is set, e.g. when phase != running).
func (a Activity) IsValid() bool {
	if a == "" {
		return true
	}
	for _, v := range allActivities {
		if a == v {
			return true
		}
	}
	return false
}

// Validate returns an error if a is not a valid Activity.
func (a Activity) Validate() error {
	if a.IsValid() {
		return nil
	}
	return fmt.Errorf("invalid activity: %q", a)
}

// IsSticky reports whether this activity resists being overwritten by normal
// event-driven updates. Sticky activities are only cleared by "new work" events
// (prompt-submit, agent-start, session-start).
func (a Activity) IsSticky() bool {
	switch a {
	case ActivityWaitingForInput, ActivityBlocked, ActivityCompleted, ActivityLimitsExceeded, ActivityCrashed:
		return true
	}
	return false
}

// IsTerminal reports whether this activity represents a terminal outcome that
// survives the phase transition to stopped. Terminal activities carry
// information about HOW the agent stopped and are valid when phase is stopped.
func (a Activity) IsTerminal() bool {
	switch a {
	case ActivityCrashed, ActivityLimitsExceeded:
		return true
	}
	return false
}

// ImpliesRunning reports whether this activity implies the agent must be in
// PhaseRunning. Used for auto-correcting a stale pre-running phase when the
// agent is clearly active.
func (a Activity) ImpliesRunning() bool {
	switch a {
	case ActivityWorking, ActivityThinking, ActivityExecuting,
		ActivityWaitingForInput, ActivityBlocked, ActivityCompleted:
		return true
	}
	return false
}

// IsPlatformSet reports whether this activity is set by the platform (scheduler)
// rather than by the agent itself.
func (a Activity) IsPlatformSet() bool {
	switch a {
	case ActivityStalled, ActivityOffline:
		return true
	}
	return false
}

// Detail provides freeform context about the current activity.
type Detail struct {
	ToolName    string `json:"toolName,omitempty"`
	Message     string `json:"message,omitempty"`
	TaskSummary string `json:"taskSummary,omitempty"`
}

// AgentState is the complete, canonical state representation for an agent.
type AgentState struct {
	Phase    Phase    `json:"phase"`
	Activity Activity `json:"activity,omitempty"`
	Detail   Detail   `json:"detail,omitempty"`
}

// AcceptsInput reports whether the agent's harness is ready to read input:
// the phase is running and the activity is set and is not initializing.
// Images that predate the initializing activity report working at start, so
// they count as ready at once.
//
// AcceptsInput only changes delivery timing (for example whether an
// interrupt is pasted at once or buffered until the harness is ready); it
// must not be used to reject or drop a message for a running agent.
func (s AgentState) AcceptsInput() bool {
	return s.Phase == PhaseRunning && s.Activity != "" && s.Activity != ActivityInitializing
}

// DisplayLabel returns the user-facing label for an activity. It differs
// from the raw value only for initializing, which is shown as "starting".
// Use it in display code keyed on activity; the legacy Status string comes
// from DisplayStatus instead.
func (a Activity) DisplayLabel() string {
	if a == ActivityInitializing {
		return "starting"
	}
	return string(a)
}

// DisplayStatus returns a single human-readable status string for backward
// compatibility and simple display. When phase is running and an activity is
// set, the activity is returned; otherwise the phase is returned.
//
// Running with the initializing activity returns "running": the legacy
// Status string feeds older hubs and CLIs, which must keep treating the agent
// as running exactly as before the initializing activity existed.
func (s AgentState) DisplayStatus() string {
	if s.Phase == PhaseRunning && s.Activity == ActivityInitializing {
		return string(PhaseRunning)
	}
	if s.Phase == PhaseRunning && s.Activity != "" {
		return string(s.Activity)
	}
	if s.Phase == PhaseStopped && s.Activity.IsTerminal() {
		return string(s.Activity)
	}
	return string(s.Phase)
}

// Validate checks cross-field constraints on the agent state.
// Activity is only meaningful when phase is running; setting an activity
// with a non-running phase is invalid.
func (s AgentState) Validate() error {
	if err := s.Phase.Validate(); err != nil {
		return err
	}
	if err := s.Activity.Validate(); err != nil {
		return err
	}
	if s.Activity != "" && s.Phase != PhaseRunning {
		if s.Phase != PhaseStopped || !s.Activity.IsTerminal() {
			return fmt.Errorf("activity %q is not valid when phase is %q (must be %q)", s.Activity, s.Phase, PhaseRunning)
		}
	}
	return nil
}
