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

// This file holds the agent-write observer contract (WP1 design 5.1): the
// store calls one observer inside the transaction of every observed agent
// write, with the row before and after the write, so lifecycle events are
// derived from committed transitions, in commit order, whichever hub path
// made the write. The observer is installed on the concrete store
// (entadapter.CompositeStore.SetAgentWriteObserver), not through the Store
// interface, so store fakes are unaffected.

import (
	"context"
	"time"
)

// AgentWriteOp names the kind of observed agent write.
type AgentWriteOp string

const (
	// AgentWriteCreate: CreateAgent. Before is nil.
	AgentWriteCreate AgentWriteOp = "create"
	// AgentWriteSoftDelete: a soft finalize (deleted_at set; the row stays).
	AgentWriteSoftDelete AgentWriteOp = "soft_delete"
	// AgentWriteHardDelete: a hard finalize or DeleteAgent. After is nil.
	AgentWriteHardDelete AgentWriteOp = "hard_delete"
	// AgentWriteProjectDelete: DeleteProject's bulk removal of the
	// project's agents. After is nil.
	AgentWriteProjectDelete AgentWriteOp = "project_delete"
	// AgentWritePurge: the retention purge of a soft-deleted row. After
	// is nil.
	AgentWritePurge AgentWriteOp = "purge"

	// AgentWriteUpdate: UpdateAgent (a whole-row CAS write), or
	// UpdateAgentConfig (a CAS write of the configuration columns).
	AgentWriteUpdate AgentWriteOp = "update"
	// AgentWriteStatus: UpdateAgentStatus, every branch.
	AgentWriteStatus AgentWriteOp = "status"
	// AgentWriteContainerMissing: MarkAgentContainerMissing(IfUnchanged).
	AgentWriteContainerMissing AgentWriteOp = "container_missing"
	// AgentWriteRunID: a run-ID write (SetAgentRunID, CompareAndSwap,
	// Revert, Adopt); Hints.Kind says which.
	AgentWriteRunID AgentWriteOp = "run_id"
	// AgentWriteLaunch: a launch-store write (BeginLaunch, MarkLaunchAccepted,
	// EndLaunch, a launch report, a reaper reap).
	AgentWriteLaunch AgentWriteOp = "launch"
	// AgentWriteDeletion: UpdateAgentDeletion (delete claim, progress,
	// rollback).
	AgentWriteDeletion AgentWriteOp = "deletion"
	// AgentWriteReincarnationClaim: ClaimAgentReincarnation.
	AgentWriteReincarnationClaim AgentWriteOp = "reincarnation_claim"
	// AgentWriteRestartStopLeg: RecordRestartStopLeg. It writes no agents
	// column, so Before and After are the same locked row.
	AgentWriteRestartStopLeg AgentWriteOp = "restart_stop_leg"
	// AgentWriteEffectiveConfig: SetAgentEffectiveConfig, the broker's
	// effective-configuration echo (WP1 slice 3). A config-resolution write.
	AgentWriteEffectiveConfig AgentWriteOp = "effective_config"
)

// AgentWriteKind is the write kind K of WP1 design 2.4a.
type AgentWriteKind string

const (
	WriteKindOther AgentWriteKind = ""
	// WriteKindStatusHeartbeat: a broker heartbeat through UpdateAgentStatus.
	WriteKindStatusHeartbeat AgentWriteKind = "status_heartbeat"
	// WriteKindStopRecord: UpdateAgentStatus with IfRunID (recordStopStatus).
	WriteKindStopRecord AgentWriteKind = "stop_record"
	// WriteKindRunMint: SetAgentRunID.
	WriteKindRunMint AgentWriteKind = "run_mint"
	// WriteKindRunRevert: RevertAgentRunID.
	WriteKindRunRevert AgentWriteKind = "run_revert"
	// WriteKindRunAdopt: CompareAndSwapAgentRunID, AdoptAgentRunID or
	// SettleLaunchedRun.
	WriteKindRunAdopt AgentWriteKind = "run_adopt"
	// WriteKindDeletion: UpdateAgentDeletion.
	WriteKindDeletion AgentWriteKind = "deletion_write"
)

// Adoption sources of a run_adopt write (adoption_source on the wire).
const (
	AdoptionSourceCAS       = "cas"
	AdoptionSourceHeartbeat = "heartbeat_adopt"
)

// AgentWriteHints are the facts of a write the row pair alone does not
// carry, set by the writer that knows them (WP1 design 2.4a inputs K, H, E).
type AgentWriteHints struct {
	Kind AgentWriteKind
	// AdoptionSource is set on a run_adopt write.
	AdoptionSource string

	// Status writes (UpdateAgentStatus).
	// StartWrite mirrors AgentStatusUpdate.StartWrite (evidence E(i)).
	StartWrite bool
	// AgentReport: the write is a status report (ReportSource set).
	AgentReport bool
	// ReportRunID is the report's run identity, "" when it names none.
	ReportRunID string
	// Restore mirrors AgentStatusUpdate.Restore (a start rollback).
	Restore bool

	// Launch writes.
	// LaunchAccepted: MarkLaunchAccepted accepted the launch.
	LaunchAccepted bool
	LaunchOwner    string
	// LaunchEnd is set when the write ends an async launch as failed
	// (a coded EndLaunch, a failed report, a reaper reap).
	LaunchEnd *LaunchEndHint
	// LaunchReport: a succeeded or failed launch report for the row's
	// launch (evidence E(iii)); it is also a config-resolution write.
	LaunchReport bool

	// StopLegRunID is the run RecordRestartStopLeg confirmed stopped.
	StopLegRunID string
}

// LifecycleEndReasonCancelled is the end_reason of an attempt a stop
// cancelled (a wind-down reap); the other launch end reasons are the
// store's LaunchEndReason* values, which share the wire names.
const LifecycleEndReasonCancelled = "cancelled"

// LaunchEndHint describes a failed launch end.
type LaunchEndHint struct {
	// Reason is the wire end_reason (timed_out, lost, failed, not_launched,
	// cancelled).
	Reason string
	// Code is the error code (launch_error), Cause the broker cause code.
	Code, Cause string
	// OmitLaunchID: the attempt never reached running (a cancelled
	// wind-down), so the event names no run.
	OmitLaunchID bool
	// SkipIfOpen: emit nothing when the row's run segment is open (a
	// wind-down of a run that already started; the stop closes it).
	SkipIfOpen bool
}

// AgentWrite is one observed agent write.
type AgentWrite struct {
	Op AgentWriteOp
	// Before is the full row read under lock before the write (nil for a
	// create). After is the row as committed (nil when the row was
	// removed).
	Before, After *Agent
	// Now is the writer's commit-time clock, the transition's event time.
	Now time.Time
	// Hints are the write's facts beyond the row pair (slice 2).
	Hints AgentWriteHints
}

// AgentID returns the written agent's ID.
func (w AgentWrite) AgentID() string {
	if w.After != nil {
		return w.After.ID
	}
	if w.Before != nil {
		return w.Before.ID
	}
	return ""
}

// AgentWriteObserver is called inside the writer's transaction, after the
// write and before commit. A returned error rolls the write back, so an
// observer returns errors only for database failures of its own writes;
// it handles derivation and serialization failures itself (counted and
// logged, never blocking the product write).
type AgentWriteObserver interface {
	ObserveAgentWrite(ctx context.Context, tx LifecycleTx, w AgentWrite) error
}

// LifecycleTx is the observer's access to the writer's transaction: the
// per-agent lifecycle cursor and the outbox. Every method runs in that
// transaction.
type LifecycleTx interface {
	// GetLifecycleCursor reads the agent's cursor, locked for update
	// where the database supports row locks. It returns nil, nil when the
	// agent has no cursor.
	GetLifecycleCursor(ctx context.Context, agentID string) (*LifecycleCursor, error)
	// BumpLifecycleCursor allocates n transition revisions for the agent
	// (transition_revision += n, n >= 0) and writes upd, then returns the
	// cursor as written. With upd.Create it inserts the cursor with
	// transition_revision = n instead.
	BumpLifecycleCursor(ctx context.Context, agentID string, n int, upd LifecycleCursorUpdate) (*LifecycleCursor, error)
	// DeleteLifecycleCursor removes the agent's cursor, if any.
	DeleteLifecycleCursor(ctx context.Context, agentID string) error
	// InsertLifecycleOutbox inserts serialized events, ready to drain.
	InsertLifecycleOutbox(ctx context.Context, rows []LifecycleOutboxRow) error
	// AfterCommit runs f once the writer's transaction has committed, and
	// never if it rolls back.
	AfterCommit(f func())
}

// LifecycleCursor is one agent's lifecycle sequencing state (design 4):
// operational state, not analytics, deleted with the agent.
type LifecycleCursor struct {
	AgentID            string
	ProjectID          string
	TransitionRevision int64
	MetadataRevision   int64
	MetadataDigest     string
	LastFingerprint    string
	// LastDeleted is whether the last observed row was deleted (deleted_at
	// set), so a removal that finds a live last state knows a deletion
	// went unobserved (review r6 N2).
	LastDeleted bool

	// Run-segment state (design 2.4a), written by every bump (slice 2).
	OpenRun                 string
	OpenRunGeneration       string
	OpenRunMetadataRevision int64
	PendingRunChangeAt      *time.Time
	ReincarnationBaseRunID  string
	PendingAdoptedFrom      string
	PendingAdoptedAt        *time.Time
	PendingAdoptionSource   string
	// PendingAdoptedTo is the run the pending adoption made current.
	PendingAdoptedTo string

	// Creator principal captured at created, copied into later snapshots.
	CreatorPrincipalType string
	CreatorPrincipalID   string
	// RequestedTemplate is the requested template name captured at
	// created ("" when the cursor was created later).
	RequestedTemplate string

	// Attribution is the persistent run attribution (WP1 attribution
	// design v2.8). AttributionStamp is written from the same value as
	// UpdatedAt in every bump that writes Attribution; nil on a cursor no
	// attribution-aware writer has bumped yet.
	Attribution      LifecycleAttribution
	AttributionStamp *time.Time

	UpdatedAt time.Time
}

// LifecycleCursorUpdate is what one observed write records in the cursor.
type LifecycleCursorUpdate struct {
	// Create inserts the cursor (the agent has none yet).
	Create    bool
	ProjectID string
	// MetadataRevision and MetadataDigest are the values after this write.
	MetadataRevision int64
	MetadataDigest   string
	LastFingerprint  string
	LastDeleted      bool
	// Creator fields and RequestedTemplate are written only with Create.
	CreatorPrincipalType string
	CreatorPrincipalID   string
	RequestedTemplate    string
	Now                  time.Time
	// Run is the run-segment state after this write; every bump writes it
	// whole (design 2.4a cursor run state).
	Run LifecycleRunState
	// Attribution is the run attribution after this write; every bump
	// writes it whole, and the store writes attribution_stamp from the same
	// value as updated_at in the same statement.
	Attribution LifecycleAttribution
}

// Attribution states and quarantine modes (WP1 attribution design 3.2).
// The empty values are the column defaults and the fail-closed readings:
// a LEGACY cursor and an UNKNOWN quarantine history.
const (
	AttributionLegacy  = ""
	AttributionClear   = "clear"
	AttributionHeld    = "held"
	QuarantineUnknown  = ""
	QuarantineComplete = "complete"
)

// LifecycleAttribution is a cursor's persistent run attribution (WP1
// attribution design 3.2-3.4). Generations are decimal strings, as the
// event incarnation_id. QuarantinedRuns is the stored comma-separated
// list, newest first; lifecycleevents parses and validates it.
type LifecycleAttribution struct {
	State                      string
	LastRun                    string
	LastGeneration             string
	AttributedRun              string
	AttributedGeneration       string
	QuarantinedRuns            string
	QuarantineMode             string
	ReincarnationMintRun       string
	UntrustedOpenRun           string
	UntrustedOpenRunGeneration string
	UntrustedOpenRunRev        int64
}

// LifecycleRunState is the cursor's run-segment state (design 2.4a).
type LifecycleRunState struct {
	OpenRun                 string
	OpenRunGeneration       string
	OpenRunMetadataRevision int64
	PendingRunChangeAt      *time.Time
	ReincarnationBaseRunID  string
	PendingAdoptedFrom      string
	PendingAdoptedAt        *time.Time
	PendingAdoptionSource   string
	// PendingAdoptedTo is the run the pending adoption made current; the
	// provenance rides only on an event naming this run.
	PendingAdoptedTo string
}

// RunState returns the cursor's run-segment state.
func (c *LifecycleCursor) RunState() LifecycleRunState {
	if c == nil {
		return LifecycleRunState{}
	}
	return LifecycleRunState{
		OpenRun:                 c.OpenRun,
		OpenRunGeneration:       c.OpenRunGeneration,
		OpenRunMetadataRevision: c.OpenRunMetadataRevision,
		PendingRunChangeAt:      c.PendingRunChangeAt,
		ReincarnationBaseRunID:  c.ReincarnationBaseRunID,
		PendingAdoptedFrom:      c.PendingAdoptedFrom,
		PendingAdoptedAt:        c.PendingAdoptedAt,
		PendingAdoptionSource:   c.PendingAdoptionSource,
		PendingAdoptedTo:        c.PendingAdoptedTo,
	}
}

// Equal reports whether two run states are the same.
func (r LifecycleRunState) Equal(o LifecycleRunState) bool {
	eqT := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Equal(*b)
	}
	return r.OpenRun == o.OpenRun && r.OpenRunGeneration == o.OpenRunGeneration &&
		r.OpenRunMetadataRevision == o.OpenRunMetadataRevision && eqT(r.PendingRunChangeAt, o.PendingRunChangeAt) &&
		r.ReincarnationBaseRunID == o.ReincarnationBaseRunID && r.PendingAdoptedFrom == o.PendingAdoptedFrom &&
		eqT(r.PendingAdoptedAt, o.PendingAdoptedAt) && r.PendingAdoptionSource == o.PendingAdoptionSource &&
		r.PendingAdoptedTo == o.PendingAdoptedTo
}
