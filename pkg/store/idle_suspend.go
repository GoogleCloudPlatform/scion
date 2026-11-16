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

import (
	"fmt"
	"time"
)

// ErrIdleSuspendPrecondition is returned by ClaimIdleSuspend (when the agent
// no longer satisfies the idle-suspend guards on the locked row) and by
// UpdateAgentStatus (when IfIdleSuspendCommit is set and the claimed run is no
// longer eligible to record the idle suspension).
var ErrIdleSuspendPrecondition = fmt.Errorf("agent no longer eligible for idle suspend: %w", ErrVersionConflict)

// IdleSuspendCursor positions a keyset page strictly after (ActivitySince, ID)
// in ListIdleSuspendCandidates's (activity_since ASC, id ASC) ordering.
type IdleSuspendCursor struct {
	ActivitySince time.Time
	ID            string
}

// IdleSuspendCondition captures the snapshot and staleness preconditions that
// must still hold on the locked agent row for ClaimIdleSuspend to commit.
type IdleSuspendCondition struct {
	RunID            string
	Activity         string
	ActivitySince    time.Time
	IdleSuspendAfter time.Duration
	HeartbeatRecency time.Duration
	ClaimTTL         time.Duration
}

// HasActiveIdleSuspendClaim reports whether an idle-suspend claim lease is
// currently active on a at now.
func (a *Agent) HasActiveIdleSuspendClaim(now time.Time) bool {
	return a != nil && a.IdleSuspendClaimUntil != nil && now.Before(*a.IdleSuspendClaimUntil)
}
