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

package lifecycleevents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// ObserverConfig configures an Observer.
type ObserverConfig struct {
	// HubID is the stable logical hub identity; producer_namespace is
	// "scion-hub/" + HubID. Required.
	HubID string
	// ProducerBuild is the hub's build identity.
	ProducerBuild string
	// Counters receives post-commit counts. Nil means no counting.
	Counters telemetryemit.LifecycleCounters
	// Logger receives build-failure errors. Nil means slog.Default().
	Logger *slog.Logger
	// Now is the observed_ts clock. Nil means time.Now.
	Now func() time.Time
	// NewEventID mints event IDs. Nil means telemetrycontract.NewEventID.
	NewEventID func() string
	// OnEnqueued, when set, runs after a commit that inserted outbox rows
	// (the drainer's wake-up signal). It must not block.
	OnEnqueued func()
}

// Observer implements store.AgentWriteObserver: inside the writer's
// transaction it derives the write's transitions, builds and serializes
// the events with their final revisions, and only then moves the cursor
// and inserts the outbox rows (design section 4, write order N10).
type Observer struct {
	cfg ObserverConfig
}

var _ store.AgentWriteObserver = (*Observer)(nil)

// NewObserver returns an Observer.
func NewObserver(cfg ObserverConfig) (*Observer, error) {
	if cfg.HubID == "" {
		return nil, errors.New("lifecycleevents: observer needs a hub ID")
	}
	if cfg.Counters == nil {
		cfg.Counters = telemetryemit.NopCounters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewEventID == nil {
		cfg.NewEventID = telemetrycontract.NewEventID
	}
	return &Observer{cfg: cfg}, nil
}

// rowState is one row's snapshot and digests.
type rowState struct {
	row          *store.Agent
	meta         telemetrycontract.LifecycleMetadata
	digest       string
	configDigest string
}

// ObserveAgentWrite implements store.AgentWriteObserver. It returns an
// error only when a cursor or outbox statement fails, which rolls the
// write back. A derivation or serialization failure is logged and
// counted, moves nothing and inserts nothing, and returns nil: the next
// observed write then finds a fingerprint mismatch and resyncs, so the
// loss shows as an explicit gap.
func (o *Observer) ObserveAgentWrite(ctx context.Context, tx store.LifecycleTx, w store.AgentWrite) error {
	agentID := w.AgentID()
	if agentID == "" {
		o.buildFailed(tx, w, errors.New("write has neither a before nor an after row"))
		return nil
	}
	cursor, err := tx.GetLifecycleCursor(ctx, agentID)
	if err != nil {
		return fmt.Errorf("lifecycle cursor: %w", err)
	}

	var createCtx *CreateContext
	if w.Op == store.AgentWriteCreate {
		if c, ok := CreateContextFrom(ctx); ok {
			createCtx = &c
		}
	}
	cause, _ := CauseFrom(ctx)

	var before, after *rowState
	if w.Before != nil {
		before, err = newRowState(w.Before, creatorOf(w.Before, cursor, nil, false))
		if err != nil {
			o.buildFailed(tx, w, err)
			return nil
		}
	}
	if w.After != nil {
		c := cursor
		if w.Op == store.AgentWriteCreate {
			c = nil // a create's creator comes from the create context
		}
		after, err = newRowState(w.After, creatorOf(w.After, c, createCtx, w.Op == store.AgentWriteCreate))
		if err != nil {
			o.buildFailed(tx, w, err)
			return nil
		}
	}

	facts := Facts{
		Op:        w.Op,
		Kind:      w.Hints.Kind,
		HasBefore: before != nil,
		HasAfter:  after != nil,
		Cause:     cause,
		Now:       w.Now,
	}
	if before != nil {
		facts.BeforeLive = before.row.DeletedAt.IsZero()
		facts.BeforePhase, facts.BeforeRunID, facts.BeforeGeneration = before.row.Phase, before.row.RunID, before.row.Generation
		facts.BeforeDigest, facts.BeforeConfigDigest = before.digest, before.configDigest
		facts.LaunchActive = before.row.LaunchState == store.LaunchStateActive
		facts.BeforeReincarnationInFlight = store.ReincarnationInFlight(before.row.ReincarnationState)
		facts.BeforePreviousRunIDs = before.row.PreviousRunIDs
	}
	if after != nil {
		facts.AfterDeleted = !after.row.DeletedAt.IsZero()
		facts.AfterPhase, facts.AfterRunID, facts.AfterGeneration = after.row.Phase, after.row.RunID, after.row.Generation
		facts.AfterReincarnationInFlight = store.ReincarnationInFlight(after.row.ReincarnationState)
		facts.AfterDigest, facts.AfterConfigDigest = after.digest, after.configDigest
	}
	writeFacts(ctx, w, &facts)
	// A create never inherits a cursor: one left behind (an orphan the
	// sweep has not reached) is replaced.
	staleCursor := w.Op == store.AgentWriteCreate && cursor != nil
	if staleCursor {
		cursor = nil
	}
	if cursor != nil {
		facts.Cursor = CursorFacts{
			Exists:      true,
			LastDeleted: cursor.LastDeleted,
			OpenRun:     cursor.OpenRun,
			Run:         cursor.RunState(),
			Digest:      cursor.MetadataDigest,
		}
		if before != nil {
			facts.Cursor.FingerprintMatches = Fingerprint(before.row, before.digest) == cursor.LastFingerprint
		}
	}
	// Persistent run attribution (attribution design v2.8): normalise the
	// stored state and discard a malformed segment before any Derive
	// branch, then select each event's identity from PRE and POST.
	pre := PrepareAttribution(&facts, cursor)
	plan := Derive(facts)
	att := Attribute(facts, pre, &plan)

	rowOf := func(r Row) *rowState {
		if r == RowAfter {
			return after
		}
		return before
	}

	metaRev, digest := int64(0), ""
	if cursor != nil {
		metaRev, digest = cursor.MetadataRevision, cursor.MetadataDigest
	}

	// Build every event with its final revisions before anything is
	// written (N10).
	rev := int64(0)
	if cursor != nil {
		rev = cursor.TransitionRevision
	}
	n := len(plan.Events)
	if plan.SkipRevision {
		rev++
		n++
	}
	observed := o.cfg.Now()
	rows := make([]store.LifecycleOutboxRow, 0, len(plan.Events))
	transitions := make([]string, 0, len(plan.Events))
	var resyncCauses []string
	for _, p := range plan.Events {
		rs := rowOf(p.Row)
		if rs == nil {
			o.buildFailed(tx, w, fmt.Errorf("%s needs a row the write does not have", p.Transition))
			return nil
		}
		rev++
		var md *telemetrycontract.LifecycleMetadata
		if telemetrycontract.IsSnapshotTransition(p.Transition) {
			if rs.digest != digest {
				metaRev++
				digest = rs.digest
			}
			m := rs.meta
			md = &m
		}
		ev, err := buildEvent(eventInput{
			Planned:            p,
			Row:                rs.row,
			TransitionRevision: rev,
			MetadataRevision:   metaRev,
			Metadata:           md,
			Cursor:             cursor,
			EventID:            o.cfg.NewEventID(),
			HubID:              o.cfg.HubID,
			ProducerBuild:      o.cfg.ProducerBuild,
			EventTS:            w.Now,
			ObservedTS:         observed,
		})
		if err != nil {
			o.buildFailed(tx, w, err)
			return nil
		}
		payload, err := telemetrycontract.Marshal(ev)
		if err != nil {
			o.buildFailed(tx, w, err)
			return nil
		}
		rows = append(rows, store.LifecycleOutboxRow{
			EventID:            ev.EventID,
			AgentID:            agentID,
			TransitionRevision: rev,
			Payload:            payload,
			CreatedAt:          observed,
			NextAttemptAt:      observed,
		})
		transitions = append(transitions, p.Transition)
		if p.Transition == telemetrycontract.TransitionResynced {
			resyncCauses = append(resyncCauses, p.Cause)
		}
	}

	run := plan.Run
	if plan.FreezeSegment {
		run.OpenRunMetadataRevision = metaRev
	}
	FinalizeAttribution(facts, plan, run, &att)

	// Move the cursor.
	switch {
	case w.After == nil || plan.DeleteCursor:
		if cursor != nil || staleCursor {
			if err := tx.DeleteLifecycleCursor(ctx, agentID); err != nil {
				return fmt.Errorf("delete lifecycle cursor: %w", err)
			}
		}
	default:
		upd := store.LifecycleCursorUpdate{
			ProjectID:        after.row.ProjectID,
			MetadataRevision: metaRev,
			MetadataDigest:   digest,
			LastFingerprint:  Fingerprint(after.row, after.digest),
			LastDeleted:      facts.AfterDeleted,
			Now:              w.Now,
			Run:              run,
			Attribution:      att.State,
		}
		// The skip compares every attribution column and never applies
		// after a normalisation (attribution design 4.5, review r1 R2).
		unchanged := cursor != nil && n == 0 && upd.LastFingerprint == cursor.LastFingerprint &&
			upd.LastDeleted == cursor.LastDeleted && upd.MetadataRevision == cursor.MetadataRevision &&
			upd.MetadataDigest == cursor.MetadataDigest && run.Equal(cursor.RunState()) &&
			!att.Normalized && att.State == cursor.Attribution
		if !unchanged {
			if staleCursor {
				if err := tx.DeleteLifecycleCursor(ctx, agentID); err != nil {
					return fmt.Errorf("delete stale lifecycle cursor: %w", err)
				}
			}
			if cursor == nil {
				upd.Create = true
				cr := creatorOf(after.row, nil, createCtx, w.Op == store.AgentWriteCreate)
				upd.CreatorPrincipalType, upd.CreatorPrincipalID = cr.Type, cr.ID
				upd.RequestedTemplate = cr.RequestedTemplate
			}
			got, err := tx.BumpLifecycleCursor(ctx, agentID, n, upd)
			if err != nil {
				return fmt.Errorf("bump lifecycle cursor: %w", err)
			}
			if got == nil || got.TransitionRevision != rev {
				// The cursor moved under the lock: refuse rather than emit
				// revisions that do not match commit order.
				return fmt.Errorf("lifecycle cursor for agent %s is at revision %v, want %d", agentID, revisionOf(got), rev)
			}
		}
	}

	if len(rows) > 0 {
		if err := tx.InsertLifecycleOutbox(ctx, rows); err != nil {
			return fmt.Errorf("insert lifecycle outbox: %w", err)
		}
	}
	if reason := att.StaleReason; reason != "" {
		counters, logger := o.cfg.Counters, o.cfg.Logger
		tx.AfterCommit(func() {
			counters.AttributionStale(reason)
			logger.Info("lifecycle: run attribution normalised to unattributed until the next qualifying fresh mint",
				"agent_id", agentID, "reason", reason)
		})
	}
	ambiguous, unconfirmed := plan.Ambiguous, plan.UnconfirmedStart
	if ambiguous != "" || unconfirmed {
		counters, logger := o.cfg.Counters, o.cfg.Logger
		row := w.After
		if row == nil {
			row = w.Before
		}
		agent, runID, now := row.ID, row.RunID, w.Now
		tx.AfterCommit(func() {
			if ambiguous != "" {
				counters.AmbiguousTransition(ambiguous)
				logger.Info("lifecycle: ambiguous transition counted, no event",
					"agent_id", agent, "run_id", runID, "kind", ambiguous, "at", now)
			}
			if unconfirmed {
				counters.UnconfirmedStart()
				logger.Info("lifecycle: start outcome unconfirmed; no event until it settles",
					"agent_id", agent, "run_id", runID, "at", now)
			}
		})
	}
	if len(rows) > 0 || len(resyncCauses) > 0 {
		counters, wake := o.cfg.Counters, o.cfg.OnEnqueued
		tx.AfterCommit(func() {
			for _, t := range transitions {
				counters.Enqueued(t)
			}
			for _, c := range resyncCauses {
				counters.Resync(c)
			}
			if wake != nil && len(rows) > 0 {
				wake()
			}
		})
	}
	return nil
}

func revisionOf(c *store.LifecycleCursor) any {
	if c == nil {
		return "none"
	}
	return c.TransitionRevision
}

func newRowState(a *store.Agent, creator Creator) (*rowState, error) {
	m := Snapshot(a, creator)
	d, err := MetadataDigest(m)
	if err != nil {
		return nil, err
	}
	cd, err := ConfigDigest(m)
	if err != nil {
		return nil, err
	}
	// The stored echo's run is a separate digest component (addendum v12
	// section 4), in both digests.
	run := echoRunID(a)
	return &rowState{row: a, meta: m, digest: withEchoRun(d, run), configDigest: withEchoRun(cd, run)}, nil
}

// buildFailed logs a derivation or serialization failure and counts it
// once the write commits (a rolled-back write lost nothing).
func (o *Observer) buildFailed(tx store.LifecycleTx, w store.AgentWrite, err error) {
	reason := buildFailureReason(err)
	o.cfg.Logger.Error("lifecycle event build failed; no event recorded for this write (the next observed write resyncs)",
		"agent_id", w.AgentID(), "op", string(w.Op), "reason", reason, "error", err)
	counters := o.cfg.Counters
	tx.AfterCommit(func() { counters.BuildFailure(reason) })
}

// writeFacts fills the write kind, evidence and hints from the writer's
// hints and the context annotations (design 2.4a inputs K, H, E).
func writeFacts(ctx context.Context, w store.AgentWrite, f *Facts) {
	h := w.Hints
	f.AdoptionSource = h.AdoptionSource
	f.LaunchAccepted, f.LaunchOwner = h.LaunchAccepted, h.LaunchOwner
	afterRun := f.AfterRunID
	switch w.Op {
	case store.AgentWriteStatus:
		switch {
		case h.StartWrite:
			f.Evidence = EvidenceStartWrite
		case h.AgentReport && h.ReportRunID != "" && h.ReportRunID == afterRun:
			f.Evidence = EvidenceReportRun
		case h.AgentReport && h.ReportRunID == "":
			f.Evidence = EvidenceReportNoRun
		}
		switch StartOutcomeFrom(ctx) {
		case StartOutcomeRestartFailed:
			f.Hint = Hint{Kind: HintRestartFailed}
		case StartOutcomeUnconfirmed:
			if h.Restore {
				f.Hint = Hint{Kind: HintRestoreUnconfirmed}
			}
		default:
			if h.Restore {
				f.Hint = Hint{Kind: HintRestore}
			}
		}
	case store.AgentWriteUpdate:
		if r, ok := DispatchResultFrom(ctx); ok {
			f.ConfigResolution = true
			switch {
			case r.RunID == "" || r.RunID != afterRun:
				f.Evidence = EvidenceDispatchStale
				f.ConfigResolution = false
			case r.BrokerEvidence:
				f.Evidence = EvidenceDispatchResult
			case r.NoBrokerRun:
				f.Evidence = EvidenceDispatchNoBrokerRun
			case r.BrokerNamedRun:
				f.Evidence = EvidenceDispatchNotRunning
			default:
				// A legacy answer that was not running (the Hub forced
				// running), or one naming another run: no evidence (F2).
			}
		}
	case store.AgentWriteLaunch:
		if h.LaunchReport {
			f.Evidence = EvidenceLaunchReport
			f.ConfigResolution = true
		}
	case store.AgentWriteRestartStopLeg:
		f.Hint = Hint{Kind: HintRestartStopLeg, StopLegRunID: h.StopLegRunID}
	case store.AgentWriteEffectiveConfig:
		// The broker's effective-configuration echo (WP1 slice 3): a
		// configuration change it commits is configured.
		f.ConfigResolution = true
	}
	if h.LaunchEnd != nil {
		f.Hint = Hint{Kind: HintLaunchEnd, EndReason: h.LaunchEnd.Reason, ErrorCode: h.LaunchEnd.Code,
			Cause: h.LaunchEnd.Cause, OmitLaunchID: h.LaunchEnd.OmitLaunchID, SkipIfOpen: h.LaunchEnd.SkipIfOpen}
	}
	if ConfigResolutionFrom(ctx) {
		f.ConfigResolution = true
	}
}
