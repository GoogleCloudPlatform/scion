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

// rowState is one row's snapshot and digest.
type rowState struct {
	row    *store.Agent
	meta   telemetrycontract.LifecycleMetadata
	digest string
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
		before, err = newRowState(w.Before, creatorOf(w.Before, cursor, nil))
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
		after, err = newRowState(w.After, creatorOf(w.After, c, createCtx))
		if err != nil {
			o.buildFailed(tx, w, err)
			return nil
		}
	}

	facts := Facts{
		Op:        w.Op,
		HasBefore: before != nil,
		HasAfter:  after != nil,
		Cause:     cause,
	}
	if before != nil {
		facts.BeforeLive = before.row.DeletedAt.IsZero()
	}
	if after != nil {
		facts.AfterDeleted = !after.row.DeletedAt.IsZero()
	}
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
		}
		if before != nil {
			facts.Cursor.FingerprintMatches = Fingerprint(before.row, before.digest) == cursor.LastFingerprint
		}
	}
	plan := Derive(facts)

	rowOf := func(r Row) *rowState {
		if r == RowAfter {
			return after
		}
		return before
	}

	// Every metadata revision has a snapshot: a write that changes the
	// digest without a snapshot transition of the committed row gets
	// metadata_changed (design section 4).
	metaRev, digest := int64(0), ""
	if cursor != nil {
		metaRev, digest = cursor.MetadataRevision, cursor.MetadataDigest
	}
	cur := digest
	for _, p := range plan.Events {
		if telemetrycontract.IsSnapshotTransition(p.Transition) && rowOf(p.Row) != nil {
			cur = rowOf(p.Row).digest
		}
	}
	if after != nil && !plan.DeleteCursor && after.digest != cur {
		plan.Events = append(plan.Events, Planned{Transition: telemetrycontract.TransitionMetadataChanged, Row: RowAfter})
		sortPlanned(plan.Events)
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
		}
		unchanged := cursor != nil && n == 0 && upd.LastFingerprint == cursor.LastFingerprint &&
			upd.LastDeleted == cursor.LastDeleted && upd.MetadataRevision == cursor.MetadataRevision &&
			upd.MetadataDigest == cursor.MetadataDigest
		if !unchanged {
			if staleCursor {
				if err := tx.DeleteLifecycleCursor(ctx, agentID); err != nil {
					return fmt.Errorf("delete stale lifecycle cursor: %w", err)
				}
			}
			if cursor == nil {
				upd.Create = true
				cr := creatorOf(after.row, nil, createCtx)
				upd.CreatorPrincipalType, upd.CreatorPrincipalID = cr.Type, cr.ID
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
	return &rowState{row: a, meta: m, digest: d}, nil
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
