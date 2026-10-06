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
	"context"
	"errors"
	"strings"
)

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

// FlatRuntimeBrokerProfilesDroppedMessage is the warning UpdateRuntimeBroker
// logs when it strips Runtime Broker Profiles written to a flat Runtime
// Broker row. Shared so tests asserting on it cannot drift from the store.
const FlatRuntimeBrokerProfilesDroppedMessage = "dropping Runtime Broker Profiles written to a flat Runtime Broker"

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

// RuntimeBrokerNameConflict returns a Runtime Broker row, other than
// excludeID, whose name equals name case-insensitively or whose slug equals
// slug exactly (the flat contract's name/slug comparison). With flatOnly, only
// rows that store a runtime target are candidates. It returns nil when there
// is no such row. It is a read-only scan used by the creation-time
// collision checks; existing rows may already share names, so it is not a
// uniqueness guarantee.
func RuntimeBrokerNameConflict(ctx context.Context, s RuntimeBrokerStore, name, slug, excludeID string, flatOnly bool) (*RuntimeBroker, error) {
	cursor := ""
	for {
		page, err := s.ListRuntimeBrokers(ctx, RuntimeBrokerFilter{}, ListOptions{Limit: 200, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			b := &page.Items[i]
			if b.ID == excludeID || (flatOnly && !b.IsFlat()) {
				continue
			}
			if (name != "" && strings.EqualFold(b.Name, name)) || (slug != "" && b.Slug == slug) {
				return b, nil
			}
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			return nil, nil
		}
		cursor = page.NextCursor
	}
}
