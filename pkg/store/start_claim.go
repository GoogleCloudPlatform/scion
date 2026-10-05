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
	"errors"
	"fmt"
	"time"
)

// StartClaimKind names what took a start claim.
type StartClaimKind string

const (
	// StartClaimUser is a user start or resume.
	StartClaimUser StartClaimKind = "user"
	// StartClaimRestart is a restart; it is taken before the stop leg.
	StartClaimRestart StartClaimKind = "restart"
	// StartClaimWake is a start triggered by a direct message.
	StartClaimWake StartClaimKind = "wake"
	// StartClaimCreate is a create-and-start.
	StartClaimCreate StartClaimKind = "create"
	// StartClaimRecovery is an automatic restart after a lost container.
	StartClaimRecovery StartClaimKind = "recovery"
	// StartClaimReincarnate is a reincarnation's start.
	StartClaimReincarnate StartClaimKind = "reincarnate"
	// StartClaimStop is held while a queued stop is applied, so a start
	// accepted after the stop was checked cannot be stopped by it. It never
	// changes run intent.
	StartClaimStop StartClaimKind = "stop"
)

// Valid reports whether k is a defined kind.
func (k StartClaimKind) Valid() bool {
	switch k {
	case StartClaimUser, StartClaimRestart, StartClaimWake, StartClaimCreate,
		StartClaimRecovery, StartClaimReincarnate, StartClaimStop:
		return true
	}
	return false
}

// StartClaimState is the state of a held start claim.
type StartClaimState string

const (
	// StartClaimLive means the holder is running the start and renews the
	// lease.
	StartClaimLive StartClaimState = "live"
	// StartClaimUnconfirmed means the start's outcome is unknown: it may
	// have reached the runtime. The claim is held until the runtime shows
	// what happened or a stop supersedes it.
	StartClaimUnconfirmed StartClaimState = "unconfirmed"
)

// StartClaim describes a claim returned by ClaimAgentStart or
// ClaimAgentStop. All times are store-clock times.
type StartClaim struct {
	ID         string
	Kind       StartClaimKind
	Owner      string
	Target     string
	At         time.Time
	LeaseUntil time.Time
	// RunIntentAt is the run_intent_at the claim wrote (start kinds) or
	// matched (stop kind).
	RunIntentAt time.Time
}

// ErrClaimPredicate is returned by ClaimAgentStart and ClaimAgentStop when
// no claim is held but the agent is not eligible: it is deleted, being
// deleted, mid-reincarnation, or (for a stop claim) its run intent changed.
var ErrClaimPredicate = errors.New("agent is not eligible for a start claim")

// ClaimHeldError is returned when another claim is already held.
type ClaimHeldError struct {
	ClaimID       string
	Kind          StartClaimKind
	State         StartClaimState
	Owner         string
	Since         time.Time
	LeaseUntil    *time.Time
	UnconfirmedAt *time.Time
	// HoldUntil is nil for a claim made unconfirmed by a launch's end; use
	// StartClaimHolds.HoldExpiry for the effective end of the hold.
	HoldUntil *time.Time
}

func (e *ClaimHeldError) Error() string {
	return fmt.Sprintf("a %s start claim (%s) is already held since %s", e.Kind, e.State, e.Since.UTC().Format(time.RFC3339))
}

// HeldClaimFromAgent returns the claim a holds as a ClaimHeldError, or nil
// when a holds none.
func HeldClaimFromAgent(a *Agent) *ClaimHeldError {
	if a == nil || a.StartClaimID == "" {
		return nil
	}
	e := &ClaimHeldError{
		ClaimID:       a.StartClaimID,
		Kind:          a.StartClaimKind,
		State:         a.StartClaimState,
		Owner:         a.StartClaimOwner,
		LeaseUntil:    a.StartClaimLeaseUntil,
		UnconfirmedAt: a.StartClaimUnconfirmedAt,
		HoldUntil:     a.StartClaimHoldUntil,
	}
	if a.StartClaimAt != nil {
		e.Since = *a.StartClaimAt
	}
	return e
}

// StartClaimHolds are the maximum unconfirmed hold durations by kind.
type StartClaimHolds struct {
	// Default applies to every kind except create and stop.
	Default time.Duration
	// Create applies to create and stop claims: a new agent has no earlier
	// session to protect, and a stop has no start to wait for.
	Create time.Duration
}

// For returns the hold for kind.
func (h StartClaimHolds) For(kind StartClaimKind) time.Duration {
	if kind == StartClaimCreate || kind == StartClaimStop {
		return h.Create
	}
	return h.Default
}

// HoldExpiry returns the end of a's unconfirmed hold: the stored
// hold_until, or unconfirmed_at plus the kind's hold when hold_until was
// not stored (a claim demoted by a launch's terminal write). ok is false
// when a holds no unconfirmed claim.
func (h StartClaimHolds) HoldExpiry(a *Agent) (time.Time, bool) {
	if a == nil || a.StartClaimID == "" || a.StartClaimState != StartClaimUnconfirmed {
		return time.Time{}, false
	}
	if a.StartClaimHoldUntil != nil {
		return *a.StartClaimHoldUntil, true
	}
	if a.StartClaimUnconfirmedAt != nil {
		return a.StartClaimUnconfirmedAt.Add(h.For(a.StartClaimKind)), true
	}
	return time.Time{}, false
}

// LaunchEndClaimSettlement says what a launch's terminal write does to a
// create claim on the same row: release it, or demote a live claim to
// unconfirmed. A launch that ended without reaching the broker, or whose
// outcome is known, releases; one whose outcome is unknown demotes.
func LaunchEndClaimSettlement(endReason string) (release, demote bool) {
	switch endReason {
	case LaunchEndReasonSucceeded, LaunchEndReasonRunningObserved,
		LaunchEndReasonFailed, LaunchEndReasonNotLaunched:
		return true, false
	case LaunchEndReasonTimedOut, LaunchEndReasonLost, LaunchEndReasonSuperseded:
		return false, true
	}
	return false, false
}

// RecoveryObservedState is what a complete inventory showed for an agent.
type RecoveryObservedState string

const (
	// ObservedPresentRunning: the container or pod is listed and not
	// terminal (a pending pod counts as running).
	ObservedPresentRunning RecoveryObservedState = "present_running"
	// ObservedPresentTerminal: listed but exited, failed or completed.
	ObservedPresentTerminal RecoveryObservedState = "present_terminal"
	// ObservedAbsent: not listed by a complete inventory of its target.
	ObservedAbsent RecoveryObservedState = "absent"
)

// Valid reports whether s is a defined state.
func (s RecoveryObservedState) Valid() bool {
	return s == ObservedPresentRunning || s == ObservedPresentTerminal || s == ObservedAbsent
}

// RecoveryObservation is one agent's observation from one heartbeat.
type RecoveryObservation struct {
	AgentID  string
	Target   string
	State    RecoveryObservedState
	InFlight bool
}

// RecoveryObservationRecord is a stored observation.
type RecoveryObservationRecord struct {
	AgentID       string
	BrokerID      string
	Target        string
	State         RecoveryObservedState
	ObservedAt    time.Time
	FirstAbsentAt *time.Time
	InFlight      bool
}

// BrokerTargetInventory is the last complete inventory time of one runtime
// target on one broker.
type BrokerTargetInventory struct {
	BrokerID                string
	Target                  string
	LastCompleteInventoryAt time.Time
}
