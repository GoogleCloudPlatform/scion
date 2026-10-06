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

package store

import "time"

// AgentLaunch is the client-facing view of an agent's current or most recent
// async launch (design §3.2). Absent (nil) when the agent has never had a
// launch (LaunchID == "").
type AgentLaunch struct {
	ID        string `json:"id"`
	State     string `json:"state"` // "active" | "ended"
	Active    bool   `json:"active"`
	Kind      string `json:"kind"` // create | start | restart
	Step      string `json:"step,omitempty"`
	Error     string `json:"error,omitempty"`
	EndReason string `json:"endReason,omitempty"`

	// Present only while State == "active" (omitted otherwise).
	Deadline         *time.Time `json:"deadline,omitempty"`
	RemainingSeconds *int       `json:"remainingSeconds,omitempty"`
}

// ComputeAgentLaunch builds the client-facing AgentLaunch view from an
// Agent's launch_* columns, or returns nil when the agent has no launch
// (LaunchID == ""). now is the clock used to compute RemainingSeconds; pass
// the answering node's wall clock (design: "now = the answering node's
// clock"; node-clock skew against the store clock is small and absorbed by
// client-side slack).
//
// RemainingSeconds is max(0, ceil(deadline - now)).
func ComputeAgentLaunch(a *Agent, now time.Time) *AgentLaunch {
	if a == nil || a.LaunchID == "" {
		return nil
	}
	l := &AgentLaunch{
		ID:        a.LaunchID,
		State:     a.LaunchState,
		Active:    a.IsInFlight(),
		Kind:      a.LaunchKind,
		Step:      a.LaunchStep,
		Error:     a.LaunchError,
		EndReason: a.LaunchEndReason,
	}
	if a.LaunchState == LaunchStateActive && !a.LaunchDeadline.IsZero() {
		d := a.LaunchDeadline
		l.Deadline = &d
		remaining := ceilSeconds(d.Sub(now)) // ceilSeconds already floors at 0 for d <= 0
		l.RemainingSeconds = &remaining
	}
	return l
}

// ComputeAgentProvisionedOnly reports whether a was provisioned but not
// started (ptone/scion#2929): phase "created", run intent "stopped" (a
// provision-only create records that), no active launch, and not deleted
// or being deleted. A full create that is still in phase "created" has run
// intent "running", so it is not provision-only.
func ComputeAgentProvisionedOnly(a *Agent) bool {
	return a != nil &&
		a.Phase == "created" &&
		a.RunIntent == RunIntentStopped &&
		a.LaunchState != LaunchStateActive &&
		a.DeletedAt.IsZero() &&
		a.DeletionState == ""
}

// ceilSeconds rounds d up to the nearest whole second (design §3.2: "ceil").
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := d / time.Second
	if d%time.Second != 0 {
		secs++
	}
	return int(secs)
}
