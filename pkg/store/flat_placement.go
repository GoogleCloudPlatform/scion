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

import "errors"

// Flat Runtime Broker placement (.design/flat-runtime-brokers-contract.md
// sections 7 and 8).

var (
	// ErrRuntimeTargetChanged: a flat Runtime Broker row already stores a
	// different runtime target ID or type. The stored value is unchanged.
	ErrRuntimeTargetChanged = errors.New("runtime target changed")
	// ErrRuntimeBrokerNotFlat: the Runtime Broker row has no stored runtime
	// target (legacy); it is never converted implicitly.
	ErrRuntimeBrokerNotFlat = errors.New("runtime broker is not a flat runtime broker")
	// ErrPinnedPlacementChanged: SetAgentPinnedRuntimeTarget's expected
	// placement no longer matches the row.
	ErrPinnedPlacementChanged = errors.New("pinned placement changed")
	// ErrInvalidPinnedPlacement: SetAgentPinnedRuntimeTarget was asked to
	// write an incomplete placement (it never unpins).
	ErrInvalidPinnedPlacement = errors.New("invalid pinned placement")
	// ErrFlatRuntimeBrokerReassign: ReassignAgentsToBroker refused a flat
	// destination; agents are never bulk-moved onto a flat Runtime Broker.
	ErrFlatRuntimeBrokerReassign = errors.New("refusing to reassign agents to a flat runtime broker")
)

// PinnedPlacement is an agent's Runtime Broker plus pinned runtime target.
// An empty RuntimeTargetID means a NULL pin (and a NULL
// pinned_runtime_broker_id); RuntimeBrokerID then still names the agent's
// runtime_broker_id ("" for none).
type PinnedPlacement struct {
	RuntimeBrokerID   string
	RuntimeTargetID   string
	RuntimeTargetType string
}

// IsPinned reports whether the agent carries a pin at all.
func (a *Agent) IsPinned() bool {
	return a != nil && a.PinnedRuntimeTargetID != ""
}

// PinValid reports whether the agent's pin is valid: pinned, and pinned to
// the Runtime Broker it is currently assigned to. A pin whose Runtime Broker
// differs from RuntimeBrokerID is stale.
func (a *Agent) PinValid() bool {
	return a.IsPinned() && a.PinnedRuntimeBrokerID == a.RuntimeBrokerID
}

// Placement returns the agent's current placement as a PinnedPlacement, for
// use as SetAgentPinnedRuntimeTarget's expected value.
func (a *Agent) Placement() PinnedPlacement {
	p := PinnedPlacement{RuntimeBrokerID: a.RuntimeBrokerID}
	if a.IsPinned() {
		p.RuntimeTargetID = a.PinnedRuntimeTargetID
		p.RuntimeTargetType = a.PinnedRuntimeTargetType
	}
	return p
}

// IsFlat reports whether the Runtime Broker row stores a runtime target.
func (b *RuntimeBroker) IsFlat() bool {
	return b != nil && b.RuntimeTarget != nil && b.RuntimeTarget.ID != ""
}
