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
	"os"
	"time"
)

// A recovered launch whose terminal report gets no definitive hub answer
// within its TTL keeps its journal record, as terminal_pending with the
// terminal it sent, for the next boot's recovery to resend
// (ptone/scion#5186). By then the hub has normally ended the launch (its
// reaper: lost or timed_out), so the resent report gets a definitive
// answer, and the existing predicates decide the clean-up. The retries are
// bounded: launchReportMaxAttempts boots without an answer, or
// launchReportMaxAge since the first, whichever comes first; then the
// record is removed with no clean-up, as before (`scion delete` removes
// what is left). If the hub is still unreachable at the next boot, the
// record simply waits for the boot after that.
const (
	launchReportMaxAttempts = 5
	launchReportMaxAge      = 24 * time.Hour
)

// reportRetryExhausted reports whether a record whose report has now gone
// unanswered attempts times, the first at firstFailedAt, has reached the
// retry bound.
func reportRetryExhausted(attempts int, firstFailedAt, now time.Time) bool {
	if attempts >= launchReportMaxAttempts {
		return true
	}
	return !firstFailedAt.IsZero() && now.Sub(firstFailedAt) >= launchReportMaxAge
}

// keepRecoveredReport settles a recovered record whose terminal report t got
// no definitive hub answer: it rewrites the record as terminal_pending with
// t (unchanged for a record that already held a terminal; the
// broker_restarted terminal for one that was still launching) and one more
// report attempt, or, at the bound, removes it with no clean-up. Its logs
// carry IDs, the attempt count and the first-failure time only.
func (s *Server) keepRecoveredReport(path string, jr *journalRecord, t journalTerminal) {
	now := time.Now().UTC()
	attempts := jr.ReportAttempts + 1
	first := jr.ReportFirstFailedAt
	if first.IsZero() {
		first = now
	}
	if reportRetryExhausted(attempts, first, now) {
		s.agentLifecycleLog.Warn("Launch recovery: the terminal report got no hub answer; retries given up and the record removed with no cleanup",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "run_id", jr.RunID, "instance_id", jr.InstanceID,
			"attempts", attempts, "first_failed_at", first.UTC().Format(time.RFC3339))
		removeLaunchMarkerIfMatches(jr.ProjectPath, jr.SharedWorkspace, jr.Slug, jr.LaunchID)
		_ = os.Remove(path)
		return
	}
	next := *jr
	next.State = journalTerminalPending.String()
	tt := t
	next.Terminal = &tt
	next.ReportAttempts = attempts
	next.ReportFirstFailedAt = first.UTC()
	data, err := json.Marshal(next)
	if err == nil {
		err = writeJournalFile(path, data)
	}
	if err != nil {
		s.agentLifecycleLog.Warn("Launch recovery: failed to update a journal record for a report retry; the record is left as it was",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "instance_id", jr.InstanceID)
		return
	}
	s.agentLifecycleLog.Warn("Launch recovery: the terminal report got no hub answer; its journal record is kept for a retry at the next broker start",
		"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "run_id", jr.RunID, "instance_id", jr.InstanceID,
		"attempts", attempts, "max_attempts", launchReportMaxAttempts, "first_failed_at", first.UTC().Format(time.RFC3339))
}
