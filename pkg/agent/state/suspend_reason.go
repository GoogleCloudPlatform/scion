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

package state

// SuspendReason records why an agent was suspended. It is stored with the
// suspended phase and cleared when the agent leaves it (starting keeps it,
// so a failed resume that restores suspended keeps it too). A suspended row
// with no reason (suspended before reasons were recorded, or held by the
// hub) never auto-wakes.
type SuspendReason string

const (
	// SuspendReasonUser: a user or caller suspended the agent through the
	// lifecycle API. Only an explicit resume or wake brings it back.
	SuspendReasonUser SuspendReason = "user"
	// SuspendReasonIdle: the hub's idle policy suspended an agent that
	// reported it had nothing to do.
	SuspendReasonIdle SuspendReason = "idle"
	// SuspendReasonStalled: the hub's auto_suspend_stalled policy suspended
	// an agent that stopped reporting activity.
	SuspendReasonStalled SuspendReason = "stalled"
)

// IsValid reports whether r is a recognised SuspendReason. Empty is valid
// (no reason recorded).
func (r SuspendReason) IsValid() bool {
	switch r {
	case "", SuspendReasonUser, SuspendReasonIdle, SuspendReasonStalled:
		return true
	}
	return false
}

// AutoWakes reports whether an agent suspended for reason r is resumed
// automatically when new work (a message, mention, notification or
// scheduled message) arrives for it. Only the hub's own suspensions do:
// the hub took the agent down, so bringing it back is the hub's job. A
// user's suspension is a deliberate stop and is never overridden.
func (r SuspendReason) AutoWakes() bool {
	return r == SuspendReasonIdle || r == SuspendReasonStalled
}
