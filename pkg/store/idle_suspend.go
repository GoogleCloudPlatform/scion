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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	// ClaimBackoff, when positive, makes ClaimIdleSuspend refuse a row whose
	// idle_suspend_claim_until is less than ClaimBackoff in the past (the
	// marker an abandoned idle suspend leaves).
	ClaimBackoff time.Duration
}

// HasActiveIdleSuspendClaim reports whether an idle-suspend claim lease is
// currently active on a at now.
func (a *Agent) HasActiveIdleSuspendClaim(now time.Time) bool {
	return a != nil && a.IdleSuspendClaimUntil != nil && now.Before(*a.IdleSuspendClaimUntil)
}

// SetIdleSuspendFlags populates IdleSuspendClaimLeaseActive and
// IdleSuspendClaimActive on a from IdleSuspendClaimUntil, RunIntent, RunIntentAt
// and Phase against the store-clock now. A row without a claim stamp has both
// false.
//
// IdleSuspendClaimActive means "an idle suspend is in progress": the lease is
// unexpired, or, when extended is true, the stamp is still set with
// run_intent=stopped written by the claim itself (IdleSuspendIntentIsOwn) on a
// row in a phase the idle machinery can settle (running, stopping, stopped,
// error). Every user or hub lifecycle write clears the stamp, so such a row is
// an idle stop whose record (worker commit or sweep recovery) has not landed
// yet, expired or not. A newer system stop intent (a membership hold, a stalled
// auto-suspend) is not the claim's: the idle machinery leaves that row alone.
// extended is false while lifecycle.idle_suspend is off: nothing records the
// suspension then, so an expired stamp is inert and no claim check applies
// beyond the lease.
func (a *Agent) SetIdleSuspendFlags(now time.Time, extended bool) {
	if a == nil {
		return
	}
	a.IdleSuspendClaimLeaseActive = false
	a.IdleSuspendClaimActive = false
	if a.IdleSuspendClaimUntil == nil {
		return
	}
	a.IdleSuspendClaimLeaseActive = now.Before(*a.IdleSuspendClaimUntil)
	a.IdleSuspendClaimActive = a.IdleSuspendClaimLeaseActive ||
		(extended && a.RunIntent == RunIntentStopped && a.IdleSuspendIntentIsOwn() && IdleSuspendSettlablePhase(a.Phase))
}

// IdleSuspendIntentIsOwn reports whether a's run_intent was written by the idle
// claim that set IdleSuspendClaimUntil. ClaimIdleSuspend writes the intent
// (run_intent_at = now) and the stamp (now + lease) in one write, and while the
// lease is active no system writer may write an intent (they yield to it), so
// an intent time at or before the stamp is the claim's own. Any later system
// stop intent (written after the lease expired) is newer than the stamp.
func (a *Agent) IdleSuspendIntentIsOwn() bool {
	return a != nil && a.RunIntentAt != nil && a.IdleSuspendClaimUntil != nil &&
		!a.RunIntentAt.After(*a.IdleSuspendClaimUntil)
}

// IdleSuspendSettlablePhase reports whether phase is one an in-progress idle
// suspend can be in and end from: running (the stop may not have landed; the
// abandon bounds it), or stopping, stopped, error (the stop landed; recovery
// records it).
func IdleSuspendSettlablePhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseRunning, state.PhaseStopping, state.PhaseStopped, state.PhaseError:
		return true
	}
	return false
}
