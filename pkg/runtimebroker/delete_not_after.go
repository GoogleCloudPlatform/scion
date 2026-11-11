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
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Delete dispatch fencing (ptone/scion#2906).
//
// The hub's delete engine sends notAfter (RFC3339, UTC) with each delete: the
// earlier of its claim's lease expiry less a margin and the end of its
// dispatch budget. After that instant the hub may have abandoned the delete
// and allowed a new start, so a delete that reaches the broker later (queued
// in the network, a slow broker, a frozen hub that resumed) is refused with
// 409 stale_dispatch and no side effects. The check runs on arrival and again
// once the target is resolved, before the first side effect, because
// resolution can be slow.
//
// A delete without notAfter (an older hub, or a hub caller other than the
// delete engine) is not checked.
//
// Clocks: brokers are expected to keep their clock synchronised with the
// hub's (NTP or equivalent). Skew up to deleteNotAfterSkew is absorbed; a
// broker clock further ahead refuses in-time deletes (safe: the hub retries),
// and one further behind accepts late deletes for longer.

// deleteNotAfterSkew is how far past notAfter a delete is still accepted, to
// absorb a broker clock running ahead of the hub's. The hub subtracts the
// same margin from its lease when it computes notAfter, so with synchronised
// clocks the last accepted instant is the lease expiry.
const deleteNotAfterSkew = 5 * time.Second

// deleteNotAfterParam is the delete query parameter carrying the deadline.
const deleteNotAfterParam = "notAfter"

// deleteFence is a delete's deadline; set is false when the delete has none.
type deleteFence struct {
	notAfter time.Time
	set      bool
}

// parseDeleteNotAfter returns the delete's deadline, if it has one. A
// malformed value is an error (400).
func parseDeleteNotAfter(q url.Values) (deleteFence, error) {
	raw := q.Get(deleteNotAfterParam)
	if raw == "" {
		return deleteFence{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return deleteFence{}, fmt.Errorf("invalid %s %q: want an RFC3339 timestamp", deleteNotAfterParam, raw)
	}
	return deleteFence{notAfter: t, set: true}, nil
}

// deleteNow is the broker's clock for the notAfter check.
func (s *Server) deleteNow() time.Time {
	if s.config.DeleteClock != nil {
		return s.config.DeleteClock()
	}
	return time.Now()
}

// refuseStaleDelete answers 409 stale_dispatch and returns true when the
// delete has a deadline that has passed (beyond deleteNotAfterSkew). stage
// names the check for the log.
func (s *Server) refuseStaleDelete(w http.ResponseWriter, fence deleteFence, stage, id, projectID, runID string) bool {
	if !fence.set {
		return false
	}
	notAfter := fence.notAfter
	now := s.deleteNow()
	if !now.After(notAfter.Add(deleteNotAfterSkew)) {
		return false
	}
	s.agentLifecycleLog.Warn("Agent delete: refused a stale dispatch; nothing done",
		"agent_id", id, "project_id", projectID, "run_id", runID, "stage", stage,
		"not_after", notAfter.UTC().Format(time.RFC3339), "now", now.UTC().Format(time.RFC3339),
		"late_by", now.Sub(notAfter).String())
	StaleDispatch(w, fmt.Sprintf("delete dispatch arrived after its deadline (notAfter %s); nothing was done",
		notAfter.UTC().Format(time.RFC3339)))
	return true
}
