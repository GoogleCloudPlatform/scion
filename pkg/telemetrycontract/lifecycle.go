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

package telemetrycontract

// This file holds the agent.lifecycle event (WP1 design section 3.1). The
// field names, json tags and declaration order are the wire contract.
// Field values that are unknown are omitted (empty string, nil pointer);
// labels are always written ([] for none).

import (
	"errors"
	"fmt"
	"strings"
)

// Lifecycle transitions (the closed transition enum).
const (
	TransitionCreated         = "created"
	TransitionLaunchAccepted  = "launch_accepted"
	TransitionConfigured      = "configured"
	TransitionStarted         = "started"
	TransitionLaunchFailed    = "launch_failed"
	TransitionMetadataChanged = "metadata_changed"
	TransitionStopped         = "stopped"
	TransitionHalted          = "halted"
	TransitionReincarnated    = "reincarnated"
	TransitionDeleted         = "deleted"
	TransitionRestored        = "restored"
	TransitionResynced        = "resynced"
)

// stop_kind values.
const (
	StopKindStopped    = "stopped"
	StopKindSuspended  = "suspended"
	StopKindError      = "error"
	StopKindSuperseded = "superseded"
)

// end_reason values.
const (
	EndReasonNotLaunched      = "not_launched"
	EndReasonFailed           = "failed"
	EndReasonTimedOut         = "timed_out"
	EndReasonLost             = "lost"
	EndReasonCreateRolledBack = "create_rolled_back"
	EndReasonCreateFailed     = "create_failed"
	EndReasonLaunchError      = "launch_error"
	EndReasonDispatchFailed   = "dispatch_failed"
	EndReasonRestartFailed    = "restart_failed"
	EndReasonCancelled        = "cancelled"
)

// delete_mode values.
const (
	DeleteModeSoft             = "soft"
	DeleteModeHard             = "hard"
	DeleteModeCreateRolledBack = "create_rolled_back"
	DeleteModeProjectDeleted   = "project_deleted"
	// DeleteModePurged: the retention purge removed a row whose deletion
	// was never observed (observer off); the original delete mode and time
	// are unknown, and event_ts is the purge commit.
	DeleteModePurged = "purged"
)

// cause values with a fixed meaning. launch_failed carries the broker's
// cause code verbatim instead.
const (
	CauseUnobservedChange = "unobserved_change" // resynced
	CauseNoCursor         = "no_cursor"         // resynced
	CauseDeleteRollback   = "delete_rollback"   // stopped
	CauseRestartStopLeg   = "restart_stop_leg"  // stopped
	CauseStartUnobserved  = "start_unobserved"  // stopped
	CauseRunUncorrelated  = "run_uncorrelated"  // started
)

// adoption_source values.
const (
	AdoptionSourceCAS            = "cas"
	AdoptionSourceHeartbeatAdopt = "heartbeat_adopt"
)

var (
	lifecycleTransitions = setOf(TransitionCreated, TransitionLaunchAccepted, TransitionConfigured,
		TransitionStarted, TransitionLaunchFailed, TransitionMetadataChanged, TransitionStopped,
		TransitionHalted, TransitionReincarnated, TransitionDeleted, TransitionRestored, TransitionResynced)
	snapshotTransitions = setOf(TransitionCreated, TransitionConfigured, TransitionMetadataChanged,
		TransitionReincarnated, TransitionDeleted, TransitionRestored, TransitionResynced)
	stopKinds  = setOf(StopKindStopped, StopKindSuspended, StopKindError, StopKindSuperseded)
	endReasons = setOf(EndReasonNotLaunched, EndReasonFailed, EndReasonTimedOut, EndReasonLost,
		EndReasonCreateRolledBack, EndReasonCreateFailed, EndReasonLaunchError, EndReasonDispatchFailed,
		EndReasonRestartFailed, EndReasonCancelled)
	deleteModes     = setOf(DeleteModeSoft, DeleteModeHard, DeleteModeCreateRolledBack, DeleteModeProjectDeleted, DeleteModePurged)
	resyncedCauses  = setOf(CauseUnobservedChange, CauseNoCursor)
	stoppedCauses   = setOf(CauseDeleteRollback, CauseRestartStopLeg, CauseStartUnobserved)
	startedCauses   = setOf(CauseRunUncorrelated)
	adoptionSources = setOf(AdoptionSourceCAS, AdoptionSourceHeartbeatAdopt)
	// launch_failed end reasons that never name a launch: the attempt has
	// no run identity of its own (design 2.4a step 3, P8).
	launchlessEndReasons = setOf(EndReasonDispatchFailed, EndReasonRestartFailed, EndReasonCancelled)
)

func setOf(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// IsLifecycleTransition reports whether v is a member of the transition
// enum.
func IsLifecycleTransition(v string) bool { return lifecycleTransitions[v] }

// IsSnapshotTransition reports whether transition v carries a full
// metadata snapshot.
func IsSnapshotTransition(v string) bool { return snapshotTransitions[v] }

// LifecycleEvent is one agent.lifecycle event: an authoritative transition
// of one agent, committed by the Hub. (producer_namespace, event_id) is the
// primary key; (agent_id, transition_revision) orders an agent's events.
//
// Run attribution (WP1 attribution design v2.8; schema_version 1 is
// unchanged):
//
// On a lifecycle event, an absent launch_id means the run attribution is
// unknown or unverified: the producer could not prove which launch the
// transition belongs to. It does NOT mean zero usage, that no run existed,
// or a launch-to-generation mapping, and it does not open or close a
// matched usage interval. Consumers must not join usage to such an event
// by launch, must not pair launch-less events by adjacency, and must not
// infer a launch from neighbouring events. On a closure without launch_id,
// incarnation_id and segment_metadata_revision describe an unattributed
// segment; they also must not be used to infer the launch. The transition
// itself is real and counts in coverage accounting. Usage that cannot be
// attributed stays in totals and in coverage accounting as unmatched; it
// is never treated as zero and never dropped.
//
// A closing event without launch_id can follow an earlier started that
// named a run. The producer then could not prove the pairing at close
// time, for example after a run-mismatch delete. This does not retract the
// earlier event, and it is not zero usage. Do not infer the run from the
// earlier event or from incarnation_id and segment_metadata_revision. The
// matched interval is simply not available.
//
// A segment whose stored identity is malformed (only reachable through
// corruption or an unsupported writer) is discarded without a closing
// event. The stream marks the loss with one resynced event
// (cause=unobserved_change, gap=true, no launch_id), and the
// lifecycle_attribution_stale_total diagnostic and a log line record it
// too. That resynced signals incomplete transition history. It is not a
// replacement terminal, a proven run mapping, a matched interval or
// evidence of zero usage.
type LifecycleEvent struct {
	Envelope
	Transition         string `json:"transition"`
	TransitionRevision int64  `json:"transition_revision"`
	MetadataRevision   int64  `json:"metadata_revision"`
	AsyncLaunchID      string `json:"async_launch_id,omitempty"`
	LaunchOwner        string `json:"launch_owner,omitempty"`
	EndReason          string `json:"end_reason,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
	Cause              string `json:"cause,omitempty"`
	// Gap is true only on resynced(unobserved_change): the revision before
	// it was skipped on purpose. Omitted otherwise.
	Gap        *bool  `json:"gap,omitempty"`
	StopKind   string `json:"stop_kind,omitempty"`
	DeleteMode string `json:"delete_mode,omitempty"`
	// RunChangedAt is only on stopped(superseded): the commit time of the
	// run-ID write that made another run current (FormatTS).
	RunChangedAt string `json:"run_changed_at,omitempty"`
	// SegmentMetadataRevision is only on closures of an older segment: the
	// metadata revision frozen at that segment's started. A reference, not
	// current state.
	SegmentMetadataRevision *int64             `json:"segment_metadata_revision,omitempty"`
	AdoptedFromLaunchID     string             `json:"adopted_from_launch_id,omitempty"`
	AdoptedAt               string             `json:"adopted_at,omitempty"`
	AdoptionSource          string             `json:"adoption_source,omitempty"`
	Metadata                *LifecycleMetadata `json:"metadata,omitempty"`
}

// LifecycleMetadata is the full metadata snapshot carried by snapshot
// transitions. requested_* are the Hub's resolved request; effective_* are
// the broker-verified values (unknown, so omitted, until the broker echoes
// them).
type LifecycleMetadata struct {
	Name                  string `json:"name,omitempty"`
	Slug                  string `json:"slug,omitempty"`
	RequestedTemplate     string `json:"requested_template,omitempty"`
	RequestedTemplateID   string `json:"requested_template_id,omitempty"`
	RequestedTemplateHash string `json:"requested_template_hash,omitempty"`
	RequestedModel        string `json:"requested_model,omitempty"`
	RequestedModelAlias   string `json:"requested_model_alias,omitempty"`
	Harness               string `json:"harness,omitempty"`
	HarnessConfig         string `json:"harness_config,omitempty"`
	Profile               string `json:"profile,omitempty"`
	Runtime               string `json:"runtime,omitempty"`
	CreatorPrincipalType  string `json:"creator_principal_type,omitempty"`
	CreatorPrincipalID    string `json:"creator_principal_id,omitempty"`
	ParentAgentID         string `json:"parent_agent_id,omitempty"`
	EffectiveTemplate     string `json:"effective_template,omitempty"`
	EffectiveTemplateHash string `json:"effective_template_hash,omitempty"`
	EffectiveModel        string `json:"effective_model,omitempty"`
	EffectiveHarness      string `json:"effective_harness,omitempty"`
	HarnessConfigRevision string `json:"harness_config_revision,omitempty"`
	HarnessConfigSource   string `json:"harness_config_source,omitempty"`
	EffectiveBrokerBuild  string `json:"effective_broker_build,omitempty"`
	// Labels is never omitted: [] means no labels.
	Labels Labels `json:"labels"`
}

// Validate checks the payload rules of design section 3.1 that the event
// itself can show. The envelope invariants are checked by
// Envelope.Validate (Marshal and Unmarshal always run both).
//
// Rules that depend on producer state the payload does not contain are
// enforced where the event is built, not here (review r6 N6): in
// particular that a deleted event closing an open run segment carries
// segment_metadata_revision, and that a launch_error for a run that never
// started omits launch_id. Validate only checks that
// segment_metadata_revision is >= 1 when present and appears only on
// stopped(superseded), stopped(cause=restart_stop_leg) and deleted, and it
// requires it on the two stopped closures, whose kind the payload shows.
func (e *LifecycleEvent) Validate() error {
	if e == nil {
		return errors.New("telemetrycontract: nil lifecycle event")
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if e.EventType != EventTypeLifecycle {
		add("event_type = %q, want %q", e.EventType, EventTypeLifecycle)
	}
	tr := e.Transition
	if !lifecycleTransitions[tr] {
		add("transition %q is not a known lifecycle transition", tr)
	}
	if e.TransitionRevision < 1 {
		add("transition_revision = %d, want >= 1", e.TransitionRevision)
	}
	if e.MetadataRevision < 1 {
		add("metadata_revision = %d, want >= 1", e.MetadataRevision)
	}

	// Snapshot presence.
	switch {
	case snapshotTransitions[tr] && e.Metadata == nil:
		add("metadata is required on %s", tr)
	case !snapshotTransitions[tr] && e.Metadata != nil && lifecycleTransitions[tr]:
		add("metadata is only carried by snapshot transitions, not %s", tr)
	}
	if e.Metadata != nil {
		if err := e.Metadata.Labels.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	// Transition-specific enums: required on their transition, forbidden
	// elsewhere.
	checkEnum := func(field, value, owner string, allowed map[string]bool, required bool) {
		switch {
		case tr != owner && value != "":
			add("%s is only allowed on %s, not %s", field, owner, tr)
		case tr == owner && value == "" && required:
			add("%s is required on %s", field, owner)
		case tr == owner && value != "" && allowed != nil && !allowed[value]:
			add("%s %q is not a known value", field, value)
		}
	}
	checkEnum("stop_kind", e.StopKind, TransitionStopped, stopKinds, true)
	checkEnum("end_reason", e.EndReason, TransitionLaunchFailed, endReasons, true)
	checkEnum("delete_mode", e.DeleteMode, TransitionDeleted, deleteModes, true)

	// cause: a fixed set per transition, except launch_failed (broker cause
	// code, free text).
	switch tr {
	case TransitionResynced:
		if !resyncedCauses[e.Cause] {
			add("cause %q is not a resynced cause (want one of unobserved_change, no_cursor)", e.Cause)
		}
	case TransitionStopped:
		if e.Cause != "" && !stoppedCauses[e.Cause] {
			add("cause %q is not a stopped cause", e.Cause)
		}
	case TransitionStarted:
		if e.Cause != "" && !startedCauses[e.Cause] {
			add("cause %q is not a started cause", e.Cause)
		}
	case TransitionLaunchFailed:
	default:
		if e.Cause != "" {
			add("cause is not allowed on %s", tr)
		}
	}

	// gap: true on resynced(unobserved_change), omitted everywhere else.
	unobserved := tr == TransitionResynced && e.Cause == CauseUnobservedChange
	switch {
	case unobserved && (e.Gap == nil || !*e.Gap):
		add("gap must be true on resynced(unobserved_change)")
	case !unobserved && e.Gap != nil:
		add("gap is only allowed on resynced(unobserved_change)")
	}

	superseded := tr == TransitionStopped && e.StopKind == StopKindSuperseded
	stopLeg := tr == TransitionStopped && e.Cause == CauseRestartStopLeg

	// run_changed_at: only on stopped(superseded), canonical timestamp.
	if e.RunChangedAt != "" {
		if !superseded {
			add("run_changed_at is only allowed on stopped(superseded)")
		} else if err := ValidateTS(e.RunChangedAt); err != nil {
			add("run_changed_at: %w", err)
		}
	}

	// segment_metadata_revision (review r6 N6).
	if s := e.SegmentMetadataRevision; s != nil {
		if *s < 1 {
			add("segment_metadata_revision = %d, want >= 1", *s)
		}
		if !superseded && !stopLeg && tr != TransitionDeleted {
			add("segment_metadata_revision is only allowed on stopped(superseded), stopped(restart_stop_leg) and deleted, not %s", describe(e))
		}
	} else if superseded || stopLeg {
		add("segment_metadata_revision is required on %s", describe(e))
	}

	// Adoption provenance: all three or none.
	if e.AdoptedFromLaunchID != "" {
		if e.AdoptedAt == "" || e.AdoptionSource == "" {
			add("adopted_at and adoption_source are required with adopted_from_launch_id")
		}
	} else if e.AdoptedAt != "" || e.AdoptionSource != "" {
		add("adopted_at and adoption_source are only allowed with adopted_from_launch_id")
	}
	if e.AdoptedAt != "" {
		if err := ValidateTS(e.AdoptedAt); err != nil {
			add("adopted_at: %w", err)
		}
	}
	if e.AdoptionSource != "" && !adoptionSources[e.AdoptionSource] {
		add("adoption_source %q is not a known value", e.AdoptionSource)
	}

	// Attempts with no run identity never name a launch.
	if tr == TransitionLaunchFailed && launchlessEndReasons[e.EndReason] && e.LaunchID != "" {
		add("launch_id must be omitted on launch_failed(%s)", e.EndReason)
	}

	if len(errs) > 0 {
		return fmt.Errorf("telemetrycontract: invalid lifecycle event: %w", errors.Join(errs...))
	}
	return nil
}

// describe names a transition with its distinguishing kind for messages.
func describe(e *LifecycleEvent) string {
	var parts []string
	if e.StopKind != "" {
		parts = append(parts, e.StopKind)
	}
	if e.Cause != "" {
		parts = append(parts, "cause="+e.Cause)
	}
	if len(parts) == 0 {
		return e.Transition
	}
	return e.Transition + "(" + strings.Join(parts, ", ") + ")"
}
