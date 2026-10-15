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
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// A launch whose clean-up fails keeps its journal record as cleanup_pending
// so the next boot's recovery retries the clean-up (ptone/scion#5177), up to
// a bound: launchCleanupMaxAttempts failed attempts, or launchCleanupMaxAge
// since the first failure, whichever comes first. Then recovery gives up,
// logs it (IDs and an error class only) and removes the record; `scion
// delete` removes what is left.
const (
	launchCleanupMaxAttempts = 5
	launchCleanupMaxAge      = 24 * time.Hour
)

// cleanupFailureClass names what part of a launch's clean-up failed. It is a
// fixed enum, safe to log and to compare; the underlying error, which can
// name paths or runtime detail, is logged only where it happens.
type cleanupFailureClass string

const (
	cleanupFailRuntimeResources   cleanupFailureClass = "runtime_resources"
	cleanupFailRunList            cleanupFailureClass = "run_list"
	cleanupFailRunDelete          cleanupFailureClass = "run_delete"
	cleanupFailFilesLock          cleanupFailureClass = "files_lock"
	cleanupFailFilesDelete        cleanupFailureClass = "files_delete"
	cleanupFailRuntimeUnavailable cleanupFailureClass = "runtime_unavailable"
)

// cleanupFailure is a failed clean-up step. Its Error text is the class
// only, never the cause's text.
type cleanupFailure struct {
	class cleanupFailureClass
}

func (f *cleanupFailure) Error() string { return "launch clean-up failed: " + string(f.class) }

// failCleanup records on rec that its clean-up failed (the first class
// wins) and returns the failure.
func failCleanup(rec *launchRecord, class cleanupFailureClass) error {
	if rec != nil {
		c := class
		rec.cleanupFailure.CompareAndSwap(nil, &c)
	}
	return &cleanupFailure{class: class}
}

// cleanupFailureClassOf returns err's failure class, or "" for nil.
func cleanupFailureClassOf(err error) cleanupFailureClass {
	var f *cleanupFailure
	if errors.As(err, &f) {
		return f.class
	}
	if err != nil {
		return "unknown"
	}
	return ""
}

// cleanupRetryExhausted reports whether a record that has now failed
// attempts times, the first at firstFailedAt, has reached the retry bound.
func cleanupRetryExhausted(attempts int, firstFailedAt, now time.Time) bool {
	if attempts >= launchCleanupMaxAttempts {
		return true
	}
	return !firstFailedAt.IsZero() && now.Sub(firstFailedAt) >= launchCleanupMaxAge
}

// finishRecoveredCleanup settles a recovered launch's journal record after
// its clean-up (cleanupErr, nil also when no clean-up was due). Success
// removes the record. A failure keeps it as cleanup_pending with one more
// attempt for the next recovery, until the retry bound, where recovery gives
// up: it logs an Error with IDs and the failure class only (never the
// cause's text, which can name paths or credentials) and removes the
// record. If the broker is shutting down, the record is left unchanged.
func (s *Server) finishRecoveredCleanup(ctx context.Context, path string, jr *journalRecord, cleanupErr error) {
	if ctx.Err() != nil {
		return
	}
	if cleanupErr == nil {
		_ = os.Remove(path)
		return
	}
	now := time.Now().UTC()
	attempts := jr.CleanupAttempts + 1
	first := jr.CleanupFirstFailedAt
	if first.IsZero() {
		first = now
	}
	class := cleanupFailureClassOf(cleanupErr)
	if cleanupRetryExhausted(attempts, first, now) {
		s.agentLifecycleLog.Error("Launch recovery: launch clean-up given up; leftover resources are removed by `scion delete`",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "run_id", jr.RunID, "instance_id", jr.InstanceID,
			"attempts", attempts, "first_failed_at", first.Format(time.RFC3339), "error_class", string(class))
		_ = os.Remove(path)
		return
	}
	next := *jr
	next.State = journalCleanupPending.String()
	next.Terminal = nil
	next.CleanupAttempts = attempts
	next.CleanupFirstFailedAt = first
	data, err := json.Marshal(next)
	if err == nil {
		err = writeJournalFile(path, data)
	}
	if err != nil {
		s.agentLifecycleLog.Warn("Launch recovery: failed to update a journal record for a clean-up retry",
			"launch_id", jr.LaunchID, "error", err)
		return
	}
	s.agentLifecycleLog.Warn("Launch recovery: launch clean-up failed; its journal record is kept for a retry at the next broker start",
		"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "run_id", jr.RunID,
		"attempts", attempts, "max_attempts", launchCleanupMaxAttempts, "error_class", string(class))
}
