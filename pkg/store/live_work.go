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

// LiveWork is live work an agent's container saw on its last heartbeat
// (ptone/scion#7855): a process doing CPU work, or a tool call in flight.
// sciontool init's liveness sampler produces it; the wire shape matches
// pkg/sciontool/liveness.LiveWork.
type LiveWork struct {
	// Kind says what was seen: "process" (CPU work) or "tool" (a live
	// tool-call process).
	Kind string `json:"kind,omitempty"`
	// SuppressStall reports whether this work keeps the agent from being
	// marked stalled. Work that does not still protects the agent from
	// auto-suspend.
	SuppressStall bool `json:"suppressStall,omitempty"`
	// Detail is a short display string (process name, pid, CPU time). It
	// is not stored.
	Detail string `json:"detail,omitempty"`
}

// LiveWorkFreshness is how recent LastLiveProcessAt must be for
// HasRecentLiveWork: three container heartbeats.
const LiveWorkFreshness = 90 * time.Second

// HasRecentLiveWork reports whether the agent's container reported live
// work of any kind within LiveWorkFreshness of now. A sweep that would stop
// the agent (auto-suspend of a stalled agent, or of an idle one) skips an
// agent for which this is true: live work is never suspended.
func (a *Agent) HasRecentLiveWork(now time.Time) bool {
	if a == nil || a.LastLiveProcessAt == nil {
		return false
	}
	return now.Sub(*a.LastLiveProcessAt) < LiveWorkFreshness
}
