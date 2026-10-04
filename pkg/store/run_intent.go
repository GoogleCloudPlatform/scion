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

// RunIntent records whether an agent should be running. It is written when a
// lifecycle request is accepted, before any dispatch to a runtime broker, so
// it reflects what was asked for even when the dispatch fails or is queued.
type RunIntent string

const (
	// RunIntentRunning means a start, restart, wake or create-and-start was
	// accepted for the agent and no later stop has been accepted.
	RunIntentRunning RunIntent = "running"
	// RunIntentStopped means a stop, suspend or delete was accepted (or the
	// agent was provisioned without being started).
	RunIntentStopped RunIntent = "stopped"
)

// Valid reports whether r is one of the defined intents.
func (r RunIntent) Valid() bool {
	return r == RunIntentRunning || r == RunIntentStopped
}

// RunIntentResolution is the precision run_intent_at is stored at. Postgres
// timestamps hold microseconds, so every value is truncated to this before
// it is written; the value SetRunIntent returns then compares equal to the
// value read back on either backend.
const RunIntentResolution = time.Microsecond

// NormalizeRunIntentTime truncates t to RunIntentResolution in UTC.
func NormalizeRunIntentTime(t time.Time) time.Time {
	return t.UTC().Truncate(RunIntentResolution)
}

// RunIntentMatches reports whether a's stored intent is intent, written at
// exactly at (compared at RunIntentResolution).
func (a *Agent) RunIntentMatches(intent RunIntent, at time.Time) bool {
	if a == nil || a.RunIntent != intent || a.RunIntentAt == nil {
		return false
	}
	return NormalizeRunIntentTime(*a.RunIntentAt).Equal(NormalizeRunIntentTime(at))
}
