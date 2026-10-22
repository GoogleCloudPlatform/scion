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

// Package lifecycleevents derives versioned agent.lifecycle events from
// committed agent writes (WP1 design). The store calls Observer inside the
// transaction of every observed write with the row before and after it;
// Derive decides the transitions, the observer builds and serializes the
// events and writes them to the outbox in that transaction, and Drainer
// delivers the outbox to a telemetryemit.Sink after commit.
//
// The package imports neither pkg/hub nor any store implementation, so its
// decision logic is tested without hub shards.
package lifecycleevents

import (
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Row selects which row an event describes (its envelope identity and, for
// snapshot transitions, its metadata).
type Row int

const (
	// RowBefore is the row read under lock before the write.
	RowBefore Row = iota + 1
	// RowAfter is the row as committed.
	RowAfter
)

// Planned is one transition Derive decided, before identity and revisions
// are assigned.
type Planned struct {
	Transition string
	Row        Row
	Cause      string
	StopKind   string
	EndReason  string
	ErrorCode  string
	DeleteMode string
	// Gap is set on resynced(unobserved_change).
	Gap bool
	// ClosesOpenRun is set on a deleted that closes the cursor's open run
	// segment; the builder then adds segment_metadata_revision and the
	// segment's frozen run identity (review r6 N6).
	ClosesOpenRun bool
}

// CursorFacts is what Derive needs from the agent's cursor.
type CursorFacts struct {
	Exists bool
	// LastDeleted: the last observed row was deleted.
	LastDeleted bool
	// FingerprintMatches: the before row's fingerprint equals the cursor's
	// last_fingerprint, so no change ran unobserved since the last
	// observed write.
	FingerprintMatches bool
	// OpenRun is the open run segment, if any (always empty until the run
	// rules land).
	OpenRun string
}

// Facts are Derive's inputs for one observed write.
type Facts struct {
	Op           store.AgentWriteOp
	HasBefore    bool
	BeforeLive   bool // before exists and its deleted_at is unset
	HasAfter     bool
	AfterDeleted bool // after exists and its deleted_at is set
	Cursor       CursorFacts
	// Cause is the write's create-failure cause, if annotated.
	Cause Cause
}

// Plan is Derive's decision for one observed write.
type Plan struct {
	// SkipRevision: leave one revision unallocated before the events, an
	// explicit gap marker for a change that ran unobserved.
	SkipRevision bool
	Events       []Planned
	// DeleteCursor: the row is gone; remove the cursor in the same
	// transaction.
	DeleteCursor bool
}

// transitionOrder is the fixed order of several transitions from one write
// (design section 4).
var transitionOrder = map[string]int{
	telemetrycontract.TransitionResynced:        0,
	"stopped_superseded":                        1,
	telemetrycontract.TransitionCreated:         2,
	telemetrycontract.TransitionRestored:        3,
	telemetrycontract.TransitionLaunchAccepted:  4,
	telemetrycontract.TransitionConfigured:      5,
	telemetrycontract.TransitionMetadataChanged: 6,
	telemetrycontract.TransitionReincarnated:    7,
	telemetrycontract.TransitionStarted:         8,
	telemetrycontract.TransitionLaunchFailed:    9,
	telemetrycontract.TransitionStopped:         10,
	telemetrycontract.TransitionHalted:          11,
	telemetrycontract.TransitionDeleted:         12,
}

func orderKey(p Planned) int {
	if p.Transition == telemetrycontract.TransitionStopped && p.StopKind == telemetrycontract.StopKindSuperseded {
		return transitionOrder["stopped_superseded"]
	}
	return transitionOrder[p.Transition]
}

// sortPlanned puts planned transitions in the fixed section 4 order.
func sortPlanned(ps []Planned) {
	sort.SliceStable(ps, func(i, j int) bool { return orderKey(ps[i]) < orderKey(ps[j]) })
}

// deleteModeFor maps the write operation (and a create-rollback cause) to
// delete_mode.
func deleteModeFor(op store.AgentWriteOp, cause Cause) string {
	if cause.Kind == CauseCreateRolledBack {
		return telemetrycontract.DeleteModeCreateRolledBack
	}
	switch op {
	case store.AgentWriteSoftDelete:
		return telemetrycontract.DeleteModeSoft
	case store.AgentWriteProjectDelete:
		return telemetrycontract.DeleteModeProjectDeleted
	case store.AgentWritePurge:
		// A purge removes only already-deleted rows, so it emits only on the
		// N2 evidence: the deletion went unobserved and its mode and time
		// are unknown (design v6.7, vm-deploy 07:27).
		return telemetrycontract.DeleteModePurged
	default: // hard finalize, DeleteAgent
		return telemetrycontract.DeleteModeHard
	}
}

// Derive decides the lifecycle transitions of one observed write. It is a
// total function of its inputs. Slice 1 covers created, the deletion step
// (design 2.4a step 1) and the resync backstop (design section 4); every
// other write advances the cursor fingerprint and derives nothing until
// the run and phase rules are added.
//
// Rules, first match wins:
//
//  1. Create: created (from the committed row).
//  2. Removal of a row that was already deleted (purge, a hard finalize
//     or DeleteAgent of a soft-deleted row, or a project delete over it):
//     no event, cursor cleanup only — unless the cursor shows the row
//     live at the last observed write, which proves the soft delete ran
//     unobserved and no deleted was ever emitted. Then: a skipped
//     revision, resynced(unobserved_change) and deleted (review r6 N2),
//     with delete_mode purged for a purge and the operation's own mode
//     (hard, project_deleted) otherwise.
//     With no cursor (pre-rollout) nothing is emitted.
//  3. Resync: no cursor gives resynced(no_cursor); a cursor whose
//     fingerprint differs from the before row's gives a skipped revision
//     and resynced(unobserved_change). Both snapshot the before row and
//     come before the write's own transitions.
//  4. Deletion of a live row: [launch_failed(<create cause>) when a
//     create-failure cause is annotated] + deleted. The snapshot is the
//     before row when the row is removed and the committed row on a soft
//     delete.
//
// Known non-detection (design section 4; documented limits, not tested
// behaviour): the resync compares only the before row of an observed write
// with the last observed row, so an unobserved change that is reverted
// before the next observed write, or an unobserved change with no later
// observed write, is never seen. Rule 2 also ignores the fingerprint once
// the cursor saw the row deleted: an unobserved restore and second soft
// delete followed by a purge emits nothing, because a fingerprint mismatch
// on a deleted row cannot tell a second deletion from any other unobserved
// change, and emitting deleted would assert a deletion that may not have
// happened.
func Derive(f Facts) Plan {
	var plan Plan
	add := func(p Planned) { plan.Events = append(plan.Events, p) }

	if f.Op == store.AgentWriteCreate || !f.HasBefore {
		if f.HasAfter {
			add(Planned{Transition: telemetrycontract.TransitionCreated, Row: RowAfter})
		}
		return plan
	}

	deletion := !f.HasAfter || (f.BeforeLive && f.AfterDeleted)
	if !f.HasAfter && !f.BeforeLive {
		if f.Cursor.Exists && !f.Cursor.LastDeleted {
			plan.SkipRevision = true
			add(Planned{Transition: telemetrycontract.TransitionResynced, Row: RowBefore,
				Cause: telemetrycontract.CauseUnobservedChange, Gap: true})
			add(Planned{Transition: telemetrycontract.TransitionDeleted, Row: RowBefore,
				DeleteMode: deleteModeFor(f.Op, Cause{}), ClosesOpenRun: f.Cursor.OpenRun != ""})
		}
		plan.DeleteCursor = true
		return plan
	}

	switch {
	case !f.Cursor.Exists:
		add(Planned{Transition: telemetrycontract.TransitionResynced, Row: RowBefore,
			Cause: telemetrycontract.CauseNoCursor})
	case !f.Cursor.FingerprintMatches:
		plan.SkipRevision = true
		add(Planned{Transition: telemetrycontract.TransitionResynced, Row: RowBefore,
			Cause: telemetrycontract.CauseUnobservedChange, Gap: true})
	}

	if deletion {
		if f.Cause.Kind != "" {
			add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowBefore,
				EndReason: f.Cause.Kind, ErrorCode: f.Cause.Stage})
		}
		row := RowBefore
		if f.HasAfter {
			row = RowAfter
		}
		add(Planned{Transition: telemetrycontract.TransitionDeleted, Row: row,
			DeleteMode: deleteModeFor(f.Op, f.Cause), ClosesOpenRun: f.Cursor.OpenRun != ""})
		plan.DeleteCursor = !f.HasAfter
	}
	sortPlanned(plan.Events)
	return plan
}
