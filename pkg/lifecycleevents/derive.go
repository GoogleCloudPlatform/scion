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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	// LaunchOwner is set on launch_accepted.
	LaunchOwner string
	// Gap is set on resynced(unobserved_change).
	Gap bool
	// ClosesOpenRun is set on an event that closes the cursor's open run
	// segment while carrying that segment's frozen identity: a deleted
	// over an open segment, stopped(superseded) and
	// stopped(restart_stop_leg). The builder then adds
	// segment_metadata_revision and the segment's frozen launch_id and
	// incarnation_id (review r6 N6), and run_changed_at on a superseded.
	ClosesOpenRun bool
	// ClosesSegment: the event is the terminal that closes the committed
	// row's own open segment (S); it names that run and its generation as
	// they are, never the next incarnation (wp1s2 review r1 R2).
	ClosesSegment bool
	// AsyncLaunch: the transition belongs to the row's async launch, so
	// it carries async_launch_id (= the row's LaunchID, when set).
	AsyncLaunch bool
	// OmitLaunchID: the event names no run (an attempt that never reached
	// running, or the reincarnation pre-run configuration).
	OmitLaunchID bool
	// NextGeneration: tag the event with incarnation_id = generation + 1
	// (a reincarnation in flight whose generation is not bumped yet,
	// design 2.7).
	NextGeneration bool
	// CarriesAdoption: the event carries the cursor's pending adoption
	// provenance (adopted_from_launch_id, adopted_at, adoption_source).
	CarriesAdoption bool

	// Set by Attribute (attribution design 4.3), applied by the builder
	// after the closure override and before effective-echo filtering.
	// AttrOmitLaunch: the event's run is unproven for it; omit launch_id.
	AttrOmitLaunch bool
	// AttrLaunch: name this attributed run instead of the row's run.
	AttrLaunch string
	// DropAdoption: carry no adoption trio (the final launch_id is
	// omitted, or the adopted-from run is quarantined).
	DropAdoption bool
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
	// OpenRun is the open run segment, if any. It mirrors Run.OpenRun and
	// is kept for the slice 1 callers.
	OpenRun string
	// Run is the cursor's whole run-segment state.
	Run store.LifecycleRunState
	// Digest is the cursor's metadata digest (the last snapshot's).
	Digest string
}

// Evidence is the run-correlated running/terminal evidence E of a write
// (design 2.4a), and the legacy observations P1b names.
type Evidence int

const (
	// EvidenceNone: no evidence.
	EvidenceNone Evidence = iota
	// EvidenceStartWrite: E(i), a start path's own status write
	// (AgentStatusUpdate.StartWrite). Evidence only when ap is running.
	EvidenceStartWrite
	// EvidenceReportRun: E(ii), a status report whose run equals the
	// row's run.
	EvidenceReportRun
	// EvidenceLaunchReport: E(iii), a succeeded or failed launch report
	// for the row's launch.
	EvidenceLaunchReport
	// EvidenceDispatchResult: E(iv), the sync dispatch result of the
	// row's current run whose broker answer named the run and said
	// running. Evidence only when ap is running.
	EvidenceDispatchResult
	// EvidenceDispatchStale: a dispatch result for a run the row no
	// longer records. Not evidence of anything.
	EvidenceDispatchStale
	// EvidenceDispatchNoBrokerRun: a dispatch result of the row's run
	// whose broker answer named no run (legacy broker): P1b.
	EvidenceDispatchNoBrokerRun
	// EvidenceDispatchNotRunning: a dispatch result of the row's run
	// whose broker answer named the run but not running. Not evidence.
	EvidenceDispatchNotRunning
	// EvidenceReportNoRun: a status report with no run identity (older
	// agents and brokers): P1b.
	EvidenceReportNoRun
)

// HintKind is the hint H of a write (design 2.4a step 3), beyond the
// create causes carried in Facts.Cause.
type HintKind string

const (
	HintNone HintKind = ""
	// HintLaunchEnd: a coded EndLaunch, a failed launch report or a reap.
	HintLaunchEnd HintKind = "launch_end"
	// HintRestartFailed: recordRestartStopped's write.
	HintRestartFailed HintKind = "restart_failed"
	// HintRestore: a start rollback (AgentStatusUpdate.Restore).
	HintRestore HintKind = "restore"
	// HintRestoreUnconfirmed: the rollback of a start whose outcome is
	// unknown.
	HintRestoreUnconfirmed HintKind = "restore_unconfirmed"
	// HintRestartStopLeg: RecordRestartStopLeg; StopLegRunID names the run.
	HintRestartStopLeg HintKind = "restart_stop_leg"
)

// Hint is a write's hint.
type Hint struct {
	Kind HintKind
	// Launch end details (HintLaunchEnd).
	EndReason, ErrorCode, Cause string
	OmitLaunchID, SkipIfOpen    bool
	// StopLegRunID is the run a restart's stop leg stopped.
	StopLegRunID string
}

// Facts are Derive's inputs for one observed write.
type Facts struct {
	Op           store.AgentWriteOp
	Kind         store.AgentWriteKind
	HasBefore    bool
	BeforeLive   bool // before exists and its deleted_at is unset
	HasAfter     bool
	AfterDeleted bool // after exists and its deleted_at is set
	Cursor       CursorFacts
	// Cause is the write's create-failure cause, if annotated.
	Cause Cause
	Hint  Hint

	BeforePhase, AfterPhase string
	BeforeRunID, AfterRunID string
	// Generations of the two rows.
	BeforeGeneration, AfterGeneration int
	// AfterReincarnationInFlight: the committed row's reincarnation state
	// is in flight (store.ReincarnationInFlight).
	AfterReincarnationInFlight bool
	// BeforeReincarnationInFlight and BeforePreviousRunIDs are the before
	// row's (attribution design 4.4 and rule 2).
	BeforeReincarnationInFlight bool
	BeforePreviousRunIDs        []string

	Evidence Evidence
	// LaunchAccepted (with LaunchOwner) is MarkLaunchAccepted's write.
	LaunchAccepted bool
	LaunchOwner    string
	// ConfigResolution: the write resolves configuration (dispatch
	// result, launch report, WithConfigResolution), so a configuration
	// change is configured rather than metadata_changed.
	ConfigResolution bool
	// Snapshot digests of the two rows and configuration digests (the
	// snapshot's configuration fields only).
	BeforeDigest, AfterDigest             string
	BeforeConfigDigest, AfterConfigDigest string
	// AdoptionSource is set on a run_adopt write.
	AdoptionSource string
	// LaunchActive: the row's async launch was active before the write.
	LaunchActive bool
	// Now is the write's commit time.
	Now time.Time
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
	// Run is the cursor's run-segment state after the write.
	Run store.LifecycleRunState
	// FreezeSegment: this write opened Run.OpenRun with a started; the
	// observer sets Run.OpenRunMetadataRevision to the write's final
	// metadata revision.
	FreezeSegment bool
	// Ambiguous names an ambiguous transition counted, not emitted
	// (P5b, P9): lifecycle_ambiguous_transitions_total{kind}.
	Ambiguous string
	// UnconfirmedStart: a start rollback with an unknown outcome
	// (lifecycle_unconfirmed_starts_total).
	UnconfirmedStart bool
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
	case store.AgentWriteSoftDelete, store.AgentWriteDeletion, store.AgentWriteUpdate:
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

// Phase groups (design 2.4a). A phase outside every group (an empty or
// unknown phase) matches no rule.
type phaseGroup int

const (
	groupNone phaseGroup = iota
	groupPre
	groupRun
	groupWind
	groupRest
	groupErr
	groupHalt
)

func groupOf(phase string) phaseGroup {
	switch state.Phase(phase) {
	case state.PhaseCreated, state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting:
		return groupPre
	case state.PhaseRunning:
		return groupRun
	case state.PhaseStopping:
		return groupWind
	case state.PhaseStopped, state.PhaseSuspended:
		return groupRest
	case state.PhaseError:
		return groupErr
	case state.PhaseHalted:
		return groupHalt
	}
	return groupNone
}

// Ambiguous transition kinds (lifecycle_ambiguous_transitions_total).
const (
	AmbiguousRestNoSegment  = "rest_without_open_segment"  // P9
	AmbiguousErrorNoSegment = "error_without_open_segment" // P5b
)

// isRunWrite reports a run-ID write (step 0).
func isRunWrite(k store.AgentWriteKind) bool {
	return k == store.WriteKindRunMint || k == store.WriteKindRunRevert || k == store.WriteKindRunAdopt
}

// isEvidence reports whether the write is run-correlated evidence E for
// the committed row's run (design 2.4a). E(i) and E(iv) hold only when
// the write leaves the row running.
func (f Facts) isEvidence() bool {
	switch f.Evidence {
	case EvidenceStartWrite, EvidenceDispatchResult:
		return groupOf(f.AfterPhase) == groupRun
	case EvidenceReportRun, EvidenceLaunchReport:
		return true
	}
	return false
}

// correlated reports whether the write itself names the committed row's
// run (a start-path write, a report or launch report of that run, or a
// dispatch result naming it), whatever phase it writes.
func (f Facts) correlated() bool {
	switch f.Evidence {
	case EvidenceStartWrite, EvidenceReportRun, EvidenceLaunchReport, EvidenceDispatchResult, EvidenceDispatchNotRunning:
		return true
	}
	return false
}

// Derive decides the lifecycle transitions of one observed write. It is a
// total function of its inputs (design 2.4a; the expectation table in
// derive_table_test.go is the single source of truth it is checked
// against).
//
// Order of evaluation:
//
//   - Create: created (from the committed row).
//   - Removal of a row that was already deleted (purge, a hard finalize
//     or DeleteAgent of a soft-deleted row, or a project delete over it):
//     no event, cursor cleanup only, unless the cursor shows the row live
//     at the last observed write, which proves the soft delete ran
//     unobserved: then a skipped revision, resynced(unobserved_change)
//     and deleted (review r6 N2), with delete_mode purged for a purge and
//     the operation's own mode otherwise. With no cursor nothing is
//     emitted.
//   - Resync: no cursor gives resynced(no_cursor); a cursor whose
//     fingerprint differs from the before row's gives a skipped revision
//     and resynced(unobserved_change). Both snapshot the before row and
//     come before the write's own transitions.
//   - Step 0, run-ID writes: no event; the cursor's run state is updated
//     (rebind on an adoption of the open run, pending_run_change_at,
//     adoption provenance).
//   - RecordRestartStopLeg: stopped(stopped, cause=restart_stop_leg)
//     closing the open run when it is the stopped run; nothing otherwise.
//   - Step 1, deletion of a live row: [launch_failed(<create cause>)] +
//     deleted, which closes any open segment.
//   - Step 2: an open segment of a run other than the committed row's is
//     closed first by stopped(superseded) with its frozen identity.
//   - Metadata transitions, independent of the phase rules: restored,
//     launch_accepted, configured (config-resolution writes only),
//     reincarnated, and metadata_changed when the digest moved without
//     any other snapshot of the committed row.
//   - Step 3, hints, which replace the phase rule; otherwise step 4, the
//     phase rules P1a..P12 (with P7a before P5, P5 requiring S, and P5b,
//     per the slice 2 design rulings Q8-Q10).
//
// Known non-detection (design section 4; documented limits, not tested
// behaviour): the resync compares only the before row of an observed write
// with the last observed row, so an unobserved change that is reverted
// before the next observed write, or an unobserved change with no later
// observed write, is never seen. The removal rule also ignores the
// fingerprint once the cursor saw the row deleted: an unobserved restore
// and second soft delete followed by a purge emits nothing, because a
// fingerprint mismatch on a deleted row cannot tell a second deletion from
// any other unobserved change, and emitting deleted would assert a
// deletion that may not have happened.
func Derive(f Facts) Plan {
	plan := Plan{Run: f.Cursor.Run}
	if plan.Run.OpenRun == "" {
		plan.Run.OpenRun = f.Cursor.OpenRun
	}
	add := func(p Planned) { plan.Events = append(plan.Events, p) }

	if f.Op == store.AgentWriteCreate || !f.HasBefore {
		plan.Run = store.LifecycleRunState{}
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
				DeleteMode: deleteModeFor(f.Op, Cause{}), ClosesOpenRun: plan.Run.OpenRun != ""})
		}
		plan.DeleteCursor = true
		plan.Run = store.LifecycleRunState{}
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

	// Step 1: deletion of a live row.
	if deletion {
		if f.Cause.Kind != "" {
			add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowBefore,
				EndReason: f.Cause.Kind, ErrorCode: f.Cause.Stage, AsyncLaunch: true,
				OmitLaunchID: f.BeforeRunID == ""})
		}
		row := RowBefore
		if f.HasAfter {
			row = RowAfter
		}
		add(Planned{Transition: telemetrycontract.TransitionDeleted, Row: row,
			DeleteMode: deleteModeFor(f.Op, f.Cause), ClosesOpenRun: plan.Run.OpenRun != ""})
		plan.DeleteCursor = !f.HasAfter
		plan.Run = store.LifecycleRunState{}
		return finish(f, plan)
	}

	// Step 0: run-ID writes.
	if isRunWrite(f.Kind) {
		deriveRunWrite(f, &plan)
		f.finishMetadata(&plan)
		return finish(f, plan)
	}

	// RecordRestartStopLeg: closes R1 iff it is the open run.
	if f.Op == store.AgentWriteRestartStopLeg || f.Hint.Kind == HintRestartStopLeg {
		if run := f.Hint.StopLegRunID; run != "" && plan.Run.OpenRun == run {
			add(Planned{Transition: telemetrycontract.TransitionStopped, Row: RowAfter,
				StopKind: telemetrycontract.StopKindStopped, Cause: telemetrycontract.CauseRestartStopLeg,
				ClosesOpenRun: true})
			closeSegment(&plan)
		}
		return finish(f, plan)
	}

	// Step 2: a different run became current while a segment was open.
	if plan.Run.OpenRun != "" && f.AfterRunID != plan.Run.OpenRun {
		add(Planned{Transition: telemetrycontract.TransitionStopped, Row: RowAfter,
			StopKind: telemetrycontract.StopKindSuperseded, ClosesOpenRun: true})
		closeSegment(&plan)
	}
	s := plan.Run.OpenRun != "" && plan.Run.OpenRun == f.AfterRunID

	// Reincarnation base run: recorded by the claim, cleared at the
	// generation bump or when the reincarnation leaves flight (Q2).
	switch {
	case f.Op == store.AgentWriteReincarnationClaim:
		plan.Run.ReincarnationBaseRunID = f.AfterRunID
	case f.AfterGeneration > f.BeforeGeneration || !f.AfterReincarnationInFlight:
		plan.Run.ReincarnationBaseRunID = ""
	}

	// Metadata transitions.
	restored := !f.BeforeLive && f.HasAfter && !f.AfterDeleted
	if restored {
		add(Planned{Transition: telemetrycontract.TransitionRestored, Row: RowAfter})
	}
	if f.LaunchAccepted {
		add(Planned{Transition: telemetrycontract.TransitionLaunchAccepted, Row: RowAfter,
			LaunchOwner: f.LaunchOwner, AsyncLaunch: true})
	}
	if f.ConfigResolution && !restored && f.BeforeConfigDigest != f.AfterConfigDigest {
		add(Planned{Transition: telemetrycontract.TransitionConfigured, Row: RowAfter})
	}
	if f.AfterGeneration > f.BeforeGeneration {
		add(Planned{Transition: telemetrycontract.TransitionReincarnated, Row: RowAfter})
	}
	f.finishMetadata(&plan)

	// Step 3 (hints), else step 4 (phase rules).
	if !f.deriveHint(&plan, s) {
		f.derivePhase(&plan, s)
	}
	return finish(f, plan)
}

// closeSegment clears the open segment after an event closed it.
func closeSegment(plan *Plan) {
	plan.Run.OpenRun = ""
	plan.Run.OpenRunGeneration = ""
	plan.Run.OpenRunMetadataRevision = 0
	plan.Run.PendingRunChangeAt = nil
}

// deriveRunWrite applies step 0 to the cursor's run state.
func deriveRunWrite(f Facts, plan *Plan) {
	r := &plan.Run
	now := f.Now
	changed := f.BeforeRunID != f.AfterRunID
	switch f.Kind {
	case store.WriteKindRunAdopt:
		if !changed {
			return // a same-value settle
		}
		switch {
		case r.OpenRun != "" && r.OpenRun == f.BeforeRunID:
			// The broker reports that its runtime holds this run for the
			// Hub's current dispatch: a rebinding, not a new run.
			r.OpenRun = f.AfterRunID
		case r.OpenRun != "" && f.AfterRunID == r.OpenRun:
			// A start of a running agent minted a run and the broker
			// reported the open run back: no run change.
			r.PendingRunChangeAt = nil
		case r.OpenRun != "" && r.PendingRunChangeAt == nil:
			r.PendingRunChangeAt = &now
		}
		// A second adoption before any event carried the first one chains
		// it: the original run keeps its alias to the final run (review r2
		// O2), rather than the first alias being dropped.
		if r.PendingAdoptedFrom == "" || r.PendingAdoptedTo != f.BeforeRunID {
			r.PendingAdoptedFrom = f.BeforeRunID
		}
		if r.PendingAdoptedFrom == f.AfterRunID {
			// Adopted back to the origin: no alias remains to report.
			r.PendingAdoptedFrom, r.PendingAdoptedAt, r.PendingAdoptionSource, r.PendingAdoptedTo = "", nil, "", ""
			return
		}
		// adopted_at and adoption_source describe the hop that made the
		// final run current (the chained alias holds from then on).
		r.PendingAdoptedAt = &now
		r.PendingAdoptionSource = f.AdoptionSource
		r.PendingAdoptedTo = f.AfterRunID
	case store.WriteKindRunMint:
		switch {
		case r.OpenRun != "" && f.AfterRunID == r.OpenRun:
			r.PendingRunChangeAt = nil
		case r.OpenRun != "" && r.PendingRunChangeAt == nil:
			r.PendingRunChangeAt = &now
		}
	case store.WriteKindRunRevert:
		if r.OpenRun != "" && f.AfterRunID == r.OpenRun {
			r.PendingRunChangeAt = nil
		}
	}
}

// finishMetadata adds metadata_changed when the committed row's digest
// differs from the last snapshot and no snapshot of the committed row was
// derived (every metadata revision has a snapshot, design section 4).
func (f Facts) finishMetadata(plan *Plan) {
	if !f.HasAfter || plan.DeleteCursor {
		return
	}
	cur := f.Cursor.Digest
	if !f.Cursor.Exists {
		cur = ""
	}
	afterSnap := false
	for _, p := range plan.Events {
		if !telemetrycontract.IsSnapshotTransition(p.Transition) {
			continue
		}
		if p.Row == RowAfter {
			afterSnap = true
		} else {
			cur = f.BeforeDigest
		}
	}
	if !afterSnap && f.AfterDigest != cur {
		plan.Events = append(plan.Events, Planned{Transition: telemetrycontract.TransitionMetadataChanged, Row: RowAfter})
	}
}

// deriveHint applies step 3. It reports whether a hint decided the write.
func (f Facts) deriveHint(plan *Plan, s bool) bool {
	add := func(p Planned) { plan.Events = append(plan.Events, p) }
	ap, bp := groupOf(f.AfterPhase), groupOf(f.BeforePhase)
	switch {
	case f.Cause.Kind == CauseCreateFailed:
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: telemetrycontract.EndReasonCreateFailed, ErrorCode: f.Cause.Stage,
			AsyncLaunch: true, OmitLaunchID: f.AfterRunID == ""})
		if s {
			plan.Events[len(plan.Events)-1].ClosesSegment = true
			closeSegment(plan)
		}
		return true
	case f.Hint.Kind == HintLaunchEnd:
		if f.Hint.SkipIfOpen && s {
			return true // the run started; the stop's own write closes it
		}
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: f.Hint.EndReason, ErrorCode: f.Hint.ErrorCode, Cause: f.Hint.Cause,
			AsyncLaunch: true, OmitLaunchID: f.Hint.OmitLaunchID || f.AfterRunID == ""})
		if s && !f.Hint.OmitLaunchID {
			plan.Events[len(plan.Events)-1].ClosesSegment = true
			closeSegment(plan)
		}
		return true
	case f.Hint.Kind == HintRestartFailed:
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: telemetrycontract.EndReasonRestartFailed, OmitLaunchID: true})
		return true
	case f.Hint.Kind == HintRestoreUnconfirmed:
		plan.UnconfirmedStart = true
		return true
	case f.Hint.Kind == HintRestore && bp == groupPre && (ap == groupRest || ap == groupErr || ap == groupHalt):
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: telemetrycontract.EndReasonDispatchFailed, OmitLaunchID: true})
		return true
	}
	return false
}

// derivePhase applies the step 4 phase rules, first match wins.
func (f Facts) derivePhase(plan *Plan, s bool) {
	add := func(p Planned) { plan.Events = append(plan.Events, p) }
	ap, bp := groupOf(f.AfterPhase), groupOf(f.BeforePhase)
	e := f.isEvidence()
	stopRecord := f.Kind == store.WriteKindStopRecord
	deletionWrite := f.Kind == store.WriteKindDeletion
	legacy := f.Evidence == EvidenceReportNoRun || f.Evidence == EvidenceDispatchNoBrokerRun
	started := func(cause string) {
		add(Planned{Transition: telemetrycontract.TransitionStarted, Row: RowAfter, Cause: cause,
			AsyncLaunch: f.Evidence == EvidenceLaunchReport})
		plan.Run.OpenRun = f.AfterRunID
		plan.Run.OpenRunGeneration = f.generationTag()
		plan.Run.PendingRunChangeAt = nil
		plan.FreezeSegment = true
	}
	terminal := func(p Planned) {
		p.ClosesSegment = s
		add(p)
		if s {
			closeSegment(plan)
		}
	}
	switch {
	case ap == groupRun && !s && e: // P1a
		started("")
	case ap == groupRun && !s && bp != groupRun && !e && legacy: // P1b
		started(telemetrycontract.CauseRunUncorrelated)
	case f.BeforePhase == f.AfterPhase: // P1
	case ap == groupRun: // P2
	case ap == groupHalt: // P3
		terminal(Planned{Transition: telemetrycontract.TransitionHalted, Row: RowAfter})
	case ap == groupErr && (bp == groupPre || bp == groupWind) && !s: // P4
		// launch_id only when the write itself correlates the failed
		// attempt to the row's run (Q10 revised); the row's RunID may still
		// be the previous run's.
		// async_launch_id while the row's async launch is active
		// (design v6.16 P4, item 14).
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: telemetrycontract.EndReasonLaunchError, OmitLaunchID: !f.correlated() || f.AfterRunID == "",
			AsyncLaunch: f.LaunchActive})
	case (ap == groupRest || ap == groupErr) && bp == groupRun && !s && (stopRecord || e): // P7a (before P5, Q8)
		add(Planned{Transition: telemetrycontract.TransitionStopped, Row: RowAfter,
			StopKind: f.AfterPhase, Cause: telemetrycontract.CauseStartUnobserved})
	case ap == groupErr && (bp == groupRun || bp == groupWind || bp == groupPre) && s: // P5
		terminal(Planned{Transition: telemetrycontract.TransitionStopped, Row: RowAfter,
			StopKind: telemetrycontract.StopKindError})
	case ap == groupErr && bp == groupRun && !s: // P5b
		plan.Ambiguous = AmbiguousErrorNoSegment
	case ap == groupErr: // P6 (bp REST/HALT, or an unknown phase)
	case ap == groupRest && (bp == groupRun || bp == groupWind || bp == groupPre) && s: // P7
		cause := ""
		if deletionWrite {
			cause = telemetrycontract.CauseDeleteRollback
		}
		terminal(Planned{Transition: telemetrycontract.TransitionStopped, Row: RowAfter,
			StopKind: f.AfterPhase, Cause: cause})
	case ap == groupRest && (bp == groupPre || bp == groupWind) && !s && (stopRecord || deletionWrite): // P8
		add(Planned{Transition: telemetrycontract.TransitionLaunchFailed, Row: RowAfter,
			EndReason: telemetrycontract.EndReasonCancelled, OmitLaunchID: true})
	case ap == groupRest && !s && (bp == groupPre || bp == groupWind || bp == groupRun): // P9
		plan.Ambiguous = AmbiguousRestNoSegment
	case ap == groupRest: // P10
	case ap == groupWind: // P11
	case ap == groupPre: // P12
	}
}

// generationTag is the incarnation a new segment of the committed row's
// run carries: the next generation while a reincarnation of this agent is
// in flight and its generation is not bumped yet (design 2.7, Q2).
func (f Facts) generationTag() string {
	g := f.AfterGeneration
	base := f.Cursor.Run.ReincarnationBaseRunID
	if base != "" && f.AfterRunID != base && f.AfterReincarnationInFlight && f.AfterGeneration == f.BeforeGeneration {
		g++
	}
	return itoa(g)
}

// finish sorts the events, applies the reincarnation tagging and attaches
// a pending adoption to the first event that names the adopted run.
//
// Reincarnation tag (design 2.7, Q2; wp1s2 review r1 R2): while a
// reincarnation is in flight and the generation is not bumped yet,
//   - events of a new run (after.RunID != base) carry generation + 1,
//     except closures of the older segment (frozen identity);
//   - events of the base run carry generation + 1 and omit launch_id only
//     when they describe the next incarnation's configuration or attempt
//     (configured, metadata_changed, launch_accepted, launch_failed that
//     closes nothing); a started, stopped or halted of the base run, or
//     any event closing its segment, names the base run at generation N.
//
// Adoption (wp1s2 review r1 R1): the provenance rides on the first event
// whose launch_id is the adopted run (PendingAdoptedTo), a closure of
// that run included, and is dropped once that run can no longer be named
// (it is neither the committed row's run nor the open segment's).
func finish(f Facts, plan Plan) Plan {
	sortPlanned(plan.Events)
	base := f.Cursor.Run.ReincarnationBaseRunID
	if f.Op == store.AgentWriteReincarnationClaim {
		base = ""
	}
	tag := base != "" && f.AfterReincarnationInFlight && f.AfterGeneration == f.BeforeGeneration
	for i := range plan.Events {
		p := &plan.Events[i]
		if !tag || p.ClosesOpenRun || p.ClosesSegment || p.Row != RowAfter ||
			p.Transition == telemetrycontract.TransitionResynced || p.Transition == telemetrycontract.TransitionDeleted {
			continue
		}
		if f.AfterRunID != base {
			p.NextGeneration = true
			continue
		}
		switch p.Transition {
		case telemetrycontract.TransitionConfigured, telemetrycontract.TransitionMetadataChanged,
			telemetrycontract.TransitionLaunchAccepted, telemetrycontract.TransitionLaunchFailed:
			// Configuration or an attempt of a run not minted yet.
			p.NextGeneration = true
			p.OmitLaunchID = true
		}
	}

	pa := f.Cursor.Run
	replaced := plan.Run.PendingAdoptedFrom != "" &&
		(plan.Run.PendingAdoptedFrom != pa.PendingAdoptedFrom || plan.Run.PendingAdoptedTo != pa.PendingAdoptedTo)
	if pa.PendingAdoptedFrom != "" && !replaced {
		target := pa.PendingAdoptedTo
		carried := false
		for i := range plan.Events {
			p := &plan.Events[i]
			if target == "" || p.Transition == telemetrycontract.TransitionResynced || f.eventLaunchID(*p) != target {
				continue
			}
			p.CarriesAdoption = true
			carried = true
			break
		}
		if carried || target == "" || (f.AfterRunID != target && plan.Run.OpenRun != target) {
			plan.Run.PendingAdoptedFrom, plan.Run.PendingAdoptedAt = "", nil
			plan.Run.PendingAdoptionSource, plan.Run.PendingAdoptedTo = "", ""
		}
	}
	return plan
}

// eventLaunchID is the launch_id the builder gives p.
func (f Facts) eventLaunchID(p Planned) string {
	switch {
	case p.ClosesOpenRun:
		if f.Cursor.Run.OpenRun != "" {
			return f.Cursor.Run.OpenRun
		}
		return f.Cursor.OpenRun
	case p.OmitLaunchID:
		return ""
	case p.Row == RowBefore:
		return f.BeforeRunID
	}
	return f.AfterRunID
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
