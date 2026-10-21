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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// ContainerStatusStopQueued is the container status of an agent whose stop
// was queued for an offline Runtime Broker. Its container may still be
// running until the stop is applied or the container is confirmed gone.
const ContainerStatusStopQueued = "stop_queued"

// BrokerQuotaCountedPhase reports whether phase counts toward the agent's
// Runtime Broker's max_agents_per_broker cap. Only stopped, suspended, error
// and halted (a run the platform ended) are excluded: a container in any
// other phase occupies, or is moving toward occupying, broker capacity.
func BrokerQuotaCountedPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseSuspended, state.PhaseError, state.PhaseHalted:
		return false
	default:
		return true
	}
}

// AgentHoldsBrokerCapacity reports whether a's broker reservation must be
// kept: it is in a counted phase, or its stop is queued for an offline
// Runtime Broker.
func AgentHoldsBrokerCapacity(a *Agent) bool {
	return BrokerQuotaCountedPhase(a.Phase) || a.ContainerStatus == ContainerStatusStopQueued
}

// QueuedStopStillIntended reports whether a's queued stop is still what the
// agent should do: container status stop_queued, run intent stopped, no
// claim of any kind, not deleted.
func QueuedStopStillIntended(a *Agent) bool {
	return a.ContainerStatus == ContainerStatusStopQueued && a.RunIntent == RunIntentStopped &&
		a.StartClaimID == "" && a.DeletedAt.IsZero()
}

// AgentReleaseCond is the condition ReleaseReservationIfAgent evaluates on
// the agent row it has locked. Every set field must hold for the release to
// happen.
type AgentReleaseCond struct {
	// ExpectedRunID is the agent's run ID when the caller read the row. It
	// is always checked, an empty value included: the release happens only
	// if the run is unchanged, so a start that minted a run in between keeps
	// the slot.
	ExpectedRunID string
	// NoStartKindClaim requires that the agent holds no claim, or only a
	// stop-kind claim. A stop-kind claim blocks every start, so no start can
	// own the slot while one is held.
	NoStartKindClaim bool
	// QueuedStopIntent requires QueuedStopStillIntended with run intent
	// written at exactly QueuedStopRunIntentAt (nil matches only an unset
	// time).
	QueuedStopIntent      bool
	QueuedStopRunIntentAt *time.Time
	// NotCounted requires !AgentHoldsBrokerCapacity on the stored row. Only
	// for callers whose read of the row is current; a caller that acts on a
	// phase observed before it is written must not set it.
	NotCounted bool
	// ReservationID, when set, limits the release to that active row: a
	// caller acting on a reservation it read never releases a newer one.
	ReservationID string
}

// Holds reports whether c holds for the agent row a.
func (c AgentReleaseCond) Holds(a *Agent) bool {
	if a.RunID != c.ExpectedRunID {
		return false
	}
	if c.NoStartKindClaim && a.StartClaimID != "" && a.StartClaimKind != StartClaimStop {
		return false
	}
	if c.QueuedStopIntent && (!QueuedStopStillIntended(a) || !sameOptionalTime(a.RunIntentAt, c.QueuedStopRunIntentAt)) {
		return false
	}
	if c.NotCounted && AgentHoldsBrokerCapacity(a) {
		return false
	}
	return true
}

// sameOptionalTime reports whether two optional times are both unset or
// equal.
func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
