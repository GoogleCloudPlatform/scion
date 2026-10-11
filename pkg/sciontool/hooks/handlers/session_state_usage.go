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

package handlers

import (
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// AddUsage adds natively derived usage (telemetry.SessionUsage) to the open
// session in the state file. The init daemon calls it for each increment the
// native usage deriver records, because harnesses whose usage source is
// native (Claude, Copilot, Codex, Gemini CLI) report usage only through
// their own telemetry, never on hook events.
//
// It takes the same lock as the hook processes, so it neither loses nor
// is lost to a concurrent hook's update, and it follows the same rules as
// CloseOpenSession: nothing is followed through a symlink, the lock file is
// never created, and the state is rewritten in place so the file keeps its
// workload ownership. It reports whether the usage was added.
//
// When no session is open, the usage is late usage of the session that
// ended last, and is added to the newest held report, if any (see "Held
// reports" in session_state_pending.go): its HoldUntil moves to
// nativeReportQuietPeriod from now, but never past its HoldCap. A held
// report is never claimed, so usage is never added to a report that is
// already being sent. An open session always takes the usage, because the
// usage carries no session ID: late usage of an earlier session that arrives
// after the next session opened is counted in the new session.
//
// Usage is not added, without an error, when there is no state file, when
// the file is a closed tombstone, or when no session is open and no report
// is held.
func (s *FileSessionState) AddUsage(u telemetry.SessionUsage) (bool, error) {
	if u.IsZero() {
		return false, nil
	}
	added := false
	err := s.withLockedStateNoFollow(syscall.O_RDWR, func(_ int, _ string, f *os.File, file sessionStateFile) error {
		if file.Closed {
			return nil
		}
		if !file.Aggregator.Open {
			r := file.newestHeld()
			if r == nil {
				return nil
			}
			addUsageToSummary(&r.Summary, u)
			next := pendingNow().Add(nativeReportQuietPeriod)
			if next.After(r.HoldCap) {
				next = r.HoldCap
			}
			if next.After(r.HoldUntil) {
				r.HoldUntil = next
			}
			if err := writeStateFileInPlace(f, file); err != nil {
				return err
			}
			added = true
			return nil
		}
		agg := telemetry.NewAggregator()
		agg.RestoreState(file.Aggregator)
		agg.RecordUsage(u)
		file.Aggregator = agg.State() // pending reports are kept
		if err := writeStateFileInPlace(f, file); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, err
}

// addUsageToSummary adds u to a finalized summary, as
// telemetry.Aggregator.RecordUsage adds it to an open session.
func addUsageToSummary(s *telemetry.SessionSummary, u telemetry.SessionUsage) {
	s.APICallCount += int(u.Calls)
	s.TokensInput += u.TokensInput
	s.TokensOutput += u.TokensOutput
	s.TokensCached += u.TokensCached
	s.TokensReasoning += u.TokensReasoning
}
