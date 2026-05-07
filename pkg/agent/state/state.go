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

import "fmt"

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
}

// Phases returns a copy of all valid Phase values.
func Phases() []Phase {
	out := make([]Phase, len(allPhases))
	copy(out, allPhases)
	return out
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
	ActivityIdle            Activity = "idle"
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

// allActivities is the internal list; Activities() returns a copy.
var allActivities = []Activity{
	ActivityIdle,
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

// DisplayStatus returns a single human-readable status string for backward
// compatibility and simple display. When phase is running and an activity is
// set, the activity is returned; otherwise the phase is returned.
func (s AgentState) DisplayStatus() string {
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
		if !(s.Phase == PhaseStopped && s.Activity.IsTerminal()) {
			return fmt.Errorf("activity %q is not valid when phase is %q (must be %q)", s.Activity, s.Phase, PhaseRunning)
		}
	}
	return nil
}
