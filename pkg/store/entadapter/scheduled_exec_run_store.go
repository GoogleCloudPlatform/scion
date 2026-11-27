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

package entadapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/execrunoutput"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/schedule"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledexecpollcursor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledexecrun"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggerfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggersubjectclaim"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Scheduled exec runs (ptone/scion#7883, ptone/scion#8241). The methods live
// on ScheduleStore so a run's outcome and its owner (scheduled_events or
// trigger_fires) share one transaction, and so purges can remove runs in the
// same transaction.

var _ store.ScheduledExecRunStore = (*ScheduleStore)(nil)

func entScheduledExecRunToStore(e *ent.ScheduledExecRun) store.ScheduledExecRun {
	run := store.ScheduledExecRun{
		OverlapKey:              e.OverlapKey,
		ProjectID:               e.ProjectID.String(),
		TargetAgentID:           e.TargetAgentID.String(),
		InitiatorPrincipalKind:  e.InitiatorPrincipalKind,
		InitiatorPrincipalID:    e.InitiatorPrincipalID,
		InitiatorCredentialKind: e.InitiatorCredentialKind,
		InitiatorCredentialID:   e.InitiatorCredentialID,
		AuthorizationRevision:   e.AuthorizationRevision,
		ExecutorKind:            e.ExecutorKind,
		ExecutorID:              e.ExecutorID,
		CommandSHA256:           e.CommandSha256,
		Argc:                    e.Argc,
		TimeoutSeconds:          e.TimeoutSeconds,
		StartedAt:               e.StartedAt,
		NotAfter:                e.NotAfter,
		LeaseUntil:              e.LeaseUntil,
		FinishedAt:              e.FinishedAt,
		Outcome:                 string(e.Outcome),
		ExitCode:                e.ExitCode,
		DurationMS:              e.DurationMs,
		OutputBytes:             e.OutputBytes,
		CaptureMode:             e.CaptureMode,
		OutputStatus:            e.OutputStatus,
		Runtime:                 e.Runtime,
		LaunchID:                e.LaunchID,
		LastPolledAt:            e.LastPolledAt,
	}
	if e.EventID != nil {
		run.EventID = e.EventID.String()
	}
	if e.FireID != nil {
		run.FireID = e.FireID.String()
	}
	if e.ScheduleID != nil {
		run.ScheduleID = e.ScheduleID.String()
	}
	return run
}

// finalExecOutcome reports whether o is a final outcome of a run.
func finalExecOutcome(o string) bool {
	switch o {
	case store.ScheduledExecOutcomeOK, store.ScheduledExecOutcomeNonzero, store.ScheduledExecOutcomeTimeout,
		store.ScheduledExecOutcomeSkipped, store.ScheduledExecOutcomeError, store.ScheduledExecOutcomeAbandoned:
		return true
	}
	return false
}

// scheduledExecOverlapLockKey returns the Postgres advisory-lock key for run
// overlap serialization. Schedule-owned runs keep the legacy hash(scheduleID)
// key so mixed-version replicas still serialize during a rolling upgrade;
// trigger-owned runs hash the "trigger:<id>" overlapKey.
func scheduledExecOverlapLockKey(scheduleID, overlapKey string) (int32, bool) {
	if scheduleID != "" {
		return store.StableProjectHash(scheduleID), true
	}
	if overlapKey != "" {
		return store.StableProjectHash(overlapKey), true
	}
	return 0, false
}

// CreateScheduledExecRun inserts the run record of an admitted exec event or
// trigger fire inside a transaction that verifies the owner is still open
// (scheduled_events status fired/expired or trigger_fires status firing;
// ptone/scion#8210, ptone/scion#8241). Exactly one of EventID or FireID must
// be set; trigger-owned runs must never set ScheduleID. When an overlap scope
// (OverlapKey or ScheduleID) is set and run.FinishedAt is nil, the transaction
// checks under a per-key advisory lock that no other unfinished run of that
// scope holds an unexpired DB lease. For trigger-owned runs, the transaction
// also transitions trigger_fires to fired (result_ref = fire_id,
// target_agent_id = target) and binds any active unbound trigger_subject_claims
// row held by fire_id.
func (s *ScheduleStore) CreateScheduledExecRun(ctx context.Context, run *store.ScheduledExecRun) error {
	if run == nil || run.ProjectID == "" || run.TargetAgentID == "" || run.CommandSHA256 == "" {
		return store.ErrInvalidInput
	}
	hasEvent := run.EventID != ""
	hasFire := run.FireID != ""
	if hasEvent == hasFire {
		return store.ErrInvalidInput
	}
	if hasFire && run.ScheduleID != "" {
		return store.ErrInvalidInput
	}
	var eid, fid *uuid.UUID
	if hasEvent {
		parsed, err := parseUUID(run.EventID)
		if err != nil {
			return err
		}
		eid = &parsed
	}
	if hasFire {
		parsed, err := parseUUID(run.FireID)
		if err != nil {
			return err
		}
		fid = &parsed
	}
	pid, err := parseUUID(run.ProjectID)
	if err != nil {
		return err
	}
	aid, err := parseUUID(run.TargetAgentID)
	if err != nil {
		return err
	}
	var sid *uuid.UUID
	if run.ScheduleID != "" {
		parsed, err := parseUUID(run.ScheduleID)
		if err != nil {
			return err
		}
		sid = &parsed
	}
	overlapKey := run.EffectiveOverlapKey()
	if run.Outcome == "" {
		run.Outcome = store.ScheduledExecOutcomeStarting
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	if run.NotAfter.IsZero() {
		run.NotAfter = run.StartedAt
	}
	insertOn := func(c *ent.Client) error {
		create := c.ScheduledExecRun.Create().
			SetLastPolledAt(run.StartedAt.UTC()).
			SetProjectID(pid).
			SetTargetAgentID(aid).
			SetOverlapKey(overlapKey).
			SetInitiatorPrincipalKind(run.InitiatorPrincipalKind).
			SetInitiatorPrincipalID(run.InitiatorPrincipalID).
			SetInitiatorCredentialKind(run.InitiatorCredentialKind).
			SetInitiatorCredentialID(run.InitiatorCredentialID).
			SetAuthorizationRevision(run.AuthorizationRevision).
			SetExecutorKind(run.ExecutorKind).
			SetExecutorID(run.ExecutorID).
			SetCommandSha256(run.CommandSHA256).
			SetArgc(run.Argc).
			SetTimeoutSeconds(run.TimeoutSeconds).
			SetStartedAt(run.StartedAt.UTC()).
			SetNotAfter(run.NotAfter.UTC()).
			SetLeaseUntil(run.LeaseUntil.UTC()).
			SetOutcome(scheduledexecrun.Outcome(run.Outcome)).
			SetRuntime(run.Runtime).
			SetLaunchID(run.LaunchID).
			SetNillableExitCode(run.ExitCode).
			SetNillableDurationMs(run.DurationMS).
			SetNillableOutputBytes(run.OutputBytes).
			SetCaptureMode(run.CaptureMode).
			SetOutputStatus(run.OutputStatus)
		if eid != nil {
			create.SetEventID(*eid)
		}
		if fid != nil {
			create.SetFireID(*fid)
		}
		if sid != nil {
			create.SetScheduleID(*sid)
		}
		if run.FinishedAt != nil {
			create.SetFinishedAt(run.FinishedAt.UTC())
		}
		if _, err := create.Save(ctx); err != nil {
			return mapError(err)
		}
		return nil
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		if lockKey, ok := scheduledExecOverlapLockKey(run.ScheduleID, overlapKey); ok && isPostgres(c) {
			var res sql.Result
			if err := c.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
				[]any{int32(store.LockScheduledExecOverlap), lockKey}, &res); err != nil {
				return fmt.Errorf("lock scheduled exec overlap: %w", err)
			}
		}
		if eid != nil {
			evtQuery := c.ScheduledEvent.Query().Where(scheduledevent.IDEQ(*eid))
			if isPostgres(c) {
				evtQuery = evtQuery.ForUpdate()
			}
			evt, err := evtQuery.Only(ctx)
			if err != nil {
				return mapError(err)
			}
			if evt.Status != store.ScheduledEventFired && evt.Status != store.ScheduledEventExpired {
				hasRun, err := c.ScheduledExecRun.Query().Where(scheduledexecrun.EventIDEQ(*eid)).Exist(ctx)
				if err != nil {
					return mapError(err)
				}
				if hasRun {
					return store.ErrAlreadyExists
				}
				return store.ErrScheduledExecEventClosed
			}
		} else {
			fireQuery := c.TriggerFire.Query().Where(triggerfire.IDEQ(*fid))
			if isPostgres(c) {
				fireQuery = fireQuery.ForUpdate()
			}
			tf, err := fireQuery.Only(ctx)
			if err != nil {
				return mapError(err)
			}
			if tf.Status != store.TriggerFireStatusFiring {
				hasRun, err := c.ScheduledExecRun.Query().Where(scheduledexecrun.FireIDEQ(*fid)).Exist(ctx)
				if err != nil {
					return mapError(err)
				}
				if hasRun {
					return store.ErrAlreadyExists
				}
				return store.ErrScheduledExecEventClosed
			}
		}
		if overlapKey != "" && run.FinishedAt == nil {
			scopePred := scheduledexecrun.OverlapKeyEQ(overlapKey)
			if sid != nil {
				scopePred = scheduledexecrun.Or(scopePred, scheduledexecrun.ScheduleIDEQ(*sid))
			}
			preds := []predicate.ScheduledExecRun{
				scopePred,
				scheduledexecrun.FinishedAtIsNil(),
				scheduledexecrun.LeaseUntilGT(run.StartedAt.UTC()),
			}
			if eid != nil {
				preds = append(preds, scheduledexecrun.Or(scheduledexecrun.EventIDIsNil(), scheduledexecrun.EventIDNEQ(*eid)))
			} else {
				preds = append(preds, scheduledexecrun.Or(scheduledexecrun.FireIDIsNil(), scheduledexecrun.FireIDNEQ(*fid)))
			}
			overlaps, err := c.ScheduledExecRun.Query().Where(preds...).Exist(ctx)
			if err != nil {
				return mapError(err)
			}
			if overlaps {
				return store.ErrScheduledExecOverlap
			}
		}
		if err := insertOn(c); err != nil {
			return err
		}
		if fid != nil {
			if _, err := c.TriggerFire.Update().
				Where(triggerfire.IDEQ(*fid), triggerfire.StatusEQ(store.TriggerFireStatusFiring)).
				SetStatus(store.TriggerFireStatusFired).
				SetResultRef(run.FireID).
				SetTargetAgentID(aid).
				Save(ctx); err != nil {
				return mapError(err)
			}
			// Bind any active unbound subject claim held by this fire so
			// skip-mode claims stay held for the lifetime of the exec run.
			if _, err := c.TriggerSubjectClaim.Update().
				Where(
					triggersubjectclaim.FireIDEQ(*fid),
					triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
					triggersubjectclaim.AgentIDIsNil(),
				).
				SetAgentID(aid).
				SetBoundAt(run.StartedAt.UTC().Truncate(time.Microsecond)).
				Save(ctx); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
}

// GetScheduledExecRun returns the run of an event or trigger fire.
func (s *ScheduleStore) GetScheduledExecRun(ctx context.Context, runID string) (*store.ScheduledExecRun, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return nil, err
	}
	e, err := s.client.ScheduledExecRun.Query().
		Where(scheduledexecrun.Or(scheduledexecrun.EventIDEQ(uid), scheduledexecrun.FireIDEQ(uid))).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	run := entScheduledExecRunToStore(e)
	return &run, nil
}

// scheduledExecRunsByEventChunk bounds the IN list of one lookup.
const scheduledExecRunsByEventChunk = 500

// ListScheduledExecRunsByEvent returns the existing runs of eventIDs. IDs
// that are not UUIDs have no run and are skipped.
func (s *ScheduleStore) ListScheduledExecRunsByEvent(ctx context.Context, eventIDs []string) (map[string]store.ScheduledExecRun, error) {
	out := make(map[string]store.ScheduledExecRun)
	ids := make([]uuid.UUID, 0, len(eventIDs))
	for _, id := range eventIDs {
		if uid, err := uuid.Parse(id); err == nil {
			ids = append(ids, uid)
		}
	}
	for start := 0; start < len(ids); start += scheduledExecRunsByEventChunk {
		end := min(start+scheduledExecRunsByEventChunk, len(ids))
		rows, err := s.client.ScheduledExecRun.Query().
			Where(scheduledexecrun.EventIDIn(ids[start:end]...)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, e := range rows {
			run := entScheduledExecRunToStore(e)
			out[run.EventID] = run
		}
	}
	return out, nil
}

// ListScheduledExecRunsByFire returns the existing runs of fireIDs, keyed by
// fire ID. IDs that are not UUIDs have no run and are skipped.
func (s *ScheduleStore) ListScheduledExecRunsByFire(ctx context.Context, fireIDs []string) (map[string]store.ScheduledExecRun, error) {
	out := make(map[string]store.ScheduledExecRun)
	ids := make([]uuid.UUID, 0, len(fireIDs))
	for _, id := range fireIDs {
		if uid, err := uuid.Parse(id); err == nil {
			ids = append(ids, uid)
		}
	}
	for start := 0; start < len(ids); start += scheduledExecRunsByEventChunk {
		end := min(start+scheduledExecRunsByEventChunk, len(ids))
		rows, err := s.client.ScheduledExecRun.Query().
			Where(scheduledexecrun.FireIDIn(ids[start:end]...)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, e := range rows {
			run := entScheduledExecRunToStore(e)
			out[run.FireID] = run
		}
	}
	return out, nil
}

// MarkScheduledExecRunRunning moves an unfinished starting run to running.
func (s *ScheduleStore) MarkScheduledExecRunRunning(ctx context.Context, runID string) (bool, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return false, err
	}
	n, err := s.client.ScheduledExecRun.Update().
		Where(
			scheduledexecrun.Or(scheduledexecrun.EventIDEQ(uid), scheduledexecrun.FireIDEQ(uid)),
			scheduledexecrun.FinishedAtIsNil(),
			scheduledexecrun.OutcomeEQ(scheduledexecrun.OutcomeStarting),
		).
		SetOutcome(scheduledexecrun.OutcomeRunning).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

func scheduleRunStatusForFinish(fin store.ScheduledExecRunFinish) string {
	switch {
	case fin.EventStatus == store.ScheduledEventSkipped || fin.Outcome == store.ScheduledExecOutcomeSkipped:
		return store.ScheduleRunSkipped
	case fin.EventStatus == store.ScheduledEventFailed ||
		fin.Outcome == store.ScheduledExecOutcomeNonzero ||
		fin.Outcome == store.ScheduledExecOutcomeTimeout ||
		fin.Outcome == store.ScheduledExecOutcomeError ||
		fin.Outcome == store.ScheduledExecOutcomeAbandoned:
		return store.ScheduleRunError
	default:
		return store.ScheduleRunSuccess
	}
}

func triggerFireStatusForFinish(fin store.ScheduledExecRunFinish) string {
	if fin.FireStatus != "" {
		return fin.FireStatus
	}
	switch fin.Outcome {
	case store.ScheduledExecOutcomeOK:
		return store.TriggerFireStatusFired
	case store.ScheduledExecOutcomeSkipped:
		return store.TriggerFireStatusSkipped
	default:
		return store.TriggerFireStatusFailed
	}
}

func recordTriggerFireOnClient(ctx context.Context, c *ent.Client, triggerID uuid.UUID, firedAt time.Time, status, errMsg string) error {
	now := time.Now().UTC()
	firedAt = firedAt.UTC()
	counter := c.Trigger.UpdateOneID(triggerID).
		AddFireCount(1).
		SetUpdated(now)
	if status == store.TriggerFireStatusFailed {
		counter.AddErrorCount(1)
	}
	if _, err := counter.Save(ctx); err != nil {
		return mapError(err)
	}
	last := c.Trigger.Update().
		Where(trigger.ID(triggerID), trigger.Or(trigger.LastFireAtIsNil(), trigger.LastFireAtLT(firedAt))).
		SetLastFireAt(firedAt).
		SetLastFireStatus(status).
		SetUpdated(now)
	if errMsg != "" {
		last.SetLastFireError(errMsg)
	} else {
		last.ClearLastFireError()
	}
	_, err := last.Save(ctx)
	return mapError(err)
}

// FinishScheduledExecRun records a run's final outcome, first writer wins,
// and updates its owner in the same transaction:
//   - for event-owned runs: scheduled_events status (while fired/expired) and
//     recurring schedule counters;
//   - for trigger-fire-owned runs: trigger_fires status/error/finished_at
//     (while firing/fired), trigger counters, and releasing any active
//     trigger_subject_claims row held by fire_id.
func (s *ScheduleStore) FinishScheduledExecRun(ctx context.Context, fin store.ScheduledExecRunFinish) (bool, error) {
	if !finalExecOutcome(fin.Outcome) {
		return false, store.ErrInvalidInput
	}
	if len(fin.Output) > store.MaxExecRunOutputBytes {
		return false, store.ErrInvalidInput
	}
	hasEvent := fin.EventID != ""
	hasFire := fin.FireID != ""
	if hasEvent == hasFire {
		return false, store.ErrInvalidInput
	}
	var ownerPred predicate.ScheduledExecRun
	var ownerUID uuid.UUID
	if hasEvent {
		eid, err := parseUUID(fin.EventID)
		if err != nil {
			return false, err
		}
		ownerUID = eid
		ownerPred = scheduledexecrun.EventIDEQ(eid)
	} else {
		fid, err := parseUUID(fin.FireID)
		if err != nil {
			return false, err
		}
		ownerUID = fid
		ownerPred = scheduledexecrun.FireIDEQ(fid)
	}
	finishedAt := fin.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	finishedUTC := finishedAt.UTC().Truncate(time.Microsecond)
	var won bool
	err := runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		runRow, err := c.ScheduledExecRun.Query().
			Where(ownerPred, scheduledexecrun.FinishedAtIsNil()).
			Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mapError(err)
		}
		n, err := c.ScheduledExecRun.Update().
			Where(scheduledexecrun.IDEQ(runRow.ID), scheduledexecrun.FinishedAtIsNil()).
			SetFinishedAt(finishedUTC).
			SetOutcome(scheduledexecrun.Outcome(fin.Outcome)).
			SetNillableExitCode(fin.ExitCode).
			SetNillableDurationMs(fin.DurationMS).
			SetNillableOutputBytes(fin.OutputBytes).
			SetOutputStatus(fin.OutputStatus).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return nil
		}
		won = true
		if len(fin.Output) > 0 {
			if _, err := c.ExecRunOutput.Create().
				SetID(ownerUID).
				SetOwnerKind(execrunoutput.OwnerKindScheduledExec).
				SetProjectID(runRow.ProjectID).
				SetCaptureMode(runRow.CaptureMode).
				SetContent(fin.Output).
				SetCreatedAt(finishedAt.UTC()).
				Save(ctx); err != nil {
				return mapError(err)
			}
		}
		if runRow.EventID != nil {
			if fin.EventStatus != "" {
				upd := c.ScheduledEvent.Update().
					Where(
						scheduledevent.IDEQ(ownerUID),
						scheduledevent.StatusIn(store.ScheduledEventFired, store.ScheduledEventExpired),
					).
					SetStatus(fin.EventStatus)
				if fin.EventError != "" {
					upd.SetError(fin.EventError)
				} else {
					upd.ClearError()
				}
				if _, err := upd.Save(ctx); err != nil {
					return mapError(err)
				}
			}
			if runRow.ScheduleID != nil {
				ranAt := runRow.StartedAt
				if evt, getErr := c.ScheduledEvent.Get(ctx, ownerUID); getErr == nil && evt.FiredAt != nil {
					ranAt = *evt.FiredAt
				}
				runStatus := scheduleRunStatusForFinish(fin)
				if err := recordScheduleRunStatusOnClient(ctx, c, *runRow.ScheduleID, ranAt, runStatus, fin.EventError); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
		} else if runRow.FireID != nil {
			// Lock order (I-1): always lock the triggers row before updating
			// trigger_fires, matching FlushTriggerEventCounters and
			// DeleteTriggersByProject to prevent PG 40P01 deadlocks.
			tf, tfGetErr := c.TriggerFire.Get(ctx, *runRow.FireID)
			if tfGetErr != nil && !ent.IsNotFound(tfGetErr) {
				return mapError(tfGetErr)
			}
			if tfGetErr == nil {
				if lErr := c.Trigger.UpdateOneID(tf.TriggerID).SetUpdated(time.Now().UTC()).Exec(ctx); lErr != nil && !ent.IsNotFound(lErr) {
					return mapError(lErr)
				}
			}
			fireStatus := triggerFireStatusForFinish(fin)
			fireErr := fin.FireError
			if fireErr == "" {
				fireErr = fin.EventError
			}
			upd := c.TriggerFire.Update().
				Where(
					triggerfire.IDEQ(*runRow.FireID),
					triggerfire.StatusIn(store.TriggerFireStatusFiring, store.TriggerFireStatusFired),
				).
				SetStatus(fireStatus).
				SetResultRef(runRow.FireID.String()).
				SetTargetAgentID(runRow.TargetAgentID).
				SetFinishedAt(finishedUTC)
			if fireErr != "" {
				upd.SetError(fireErr)
			} else {
				upd.ClearError()
			}
			if _, err := upd.Save(ctx); err != nil {
				return mapError(err)
			}
			if tfGetErr == nil {
				if err := recordTriggerFireOnClient(ctx, c, tf.TriggerID, finishedUTC, fireStatus, fireErr); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			if _, err := c.TriggerSubjectClaim.Update().
				Where(
					triggersubjectclaim.FireIDEQ(*runRow.FireID),
					triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
				).
				SetState(store.TriggerSubjectClaimStateReleased).
				SetReleasedAt(finishedUTC).
				Save(ctx); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// GetScheduledExecRunOutput returns the captured output bytes for runID from
// exec_run_outputs, or store.ErrNotFound when no output row exists.
func (s *ScheduleStore) GetScheduledExecRunOutput(ctx context.Context, runID string) ([]byte, error) {
	uid, err := parseUUID(runID)
	if err != nil {
		return nil, err
	}
	row, err := s.client.ExecRunOutput.Query().
		Where(
			execrunoutput.IDEQ(uid),
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindScheduledExec),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return row.Content, nil
}

// ListExpiredScheduledExecRuns returns up to limit unfinished runs whose
// lease ended before now, oldest lease first.
func (s *ScheduleStore) ListExpiredScheduledExecRuns(ctx context.Context, now time.Time, limit int) ([]store.ScheduledExecRun, error) {
	rows, err := s.client.ScheduledExecRun.Query().
		Where(scheduledexecrun.FinishedAtIsNil(), scheduledexecrun.LeaseUntilLT(now.UTC())).
		Order(scheduledexecrun.ByLeaseUntil(), scheduledexecrun.ByID()).
		Limit(clampLimit(limit)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entScheduledExecRunsToStore(rows), nil
}

// ClaimScheduledExecRunsForPoll returns up to limit unfinished runs whose
// lease has not ended, skipping excludeEventIDs, chosen round-robin across
// the full eligible set of target agents on their persisted per-agent turn
// (ptone/scion#8096):
//
//  1. Distinct eligible target_agent_id values are queried across the full
//     unfinished, in-lease set (never capped by a candidate-row window).
//  2. Eligible targets are ordered by their persisted turn_seq in
//     scheduled_exec_poll_cursors (0 when never claimed, ties broken by
//     target_agent_id), never by run timestamps and never rewound when
//     polled runs finish or are purged.
//  3. One least-polled run (by poll_seq, last_polled_at, event_id; FOR
//     UPDATE SKIP LOCKED on Postgres) is chosen per target per round until
//     limit runs are claimed or all eligible runs are exhausted.
//  4. Claimed runs update poll_seq and last_polled_at, and each claimed
//     target's cursor row is upserted with its latest monotonic turn_seq.
func (s *ScheduleStore) ClaimScheduledExecRunsForPoll(ctx context.Context, now time.Time, limit int, excludeEventIDs []string) ([]store.ScheduledExecRun, error) {
	limit = clampLimit(limit)
	excludeSet := make(map[uuid.UUID]struct{}, len(excludeEventIDs))
	var excludeSQL []uuid.UUID
	for _, id := range excludeEventIDs {
		if uid, err := uuid.Parse(id); err == nil {
			if _, dup := excludeSet[uid]; !dup {
				excludeSet[uid] = struct{}{}
				excludeSQL = append(excludeSQL, uid)
			}
		}
	}
	nowUTC := now.UTC()
	var out []store.ScheduledExecRun
	err := runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		if isPostgres(c) {
			var res sql.Result
			if err := c.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)",
				[]any{int64(store.LockScheduledExecPollClaim)}, &res); err != nil {
				return fmt.Errorf("lock scheduled exec poll claim: %w", err)
			}
		}

		basePreds := []predicate.ScheduledExecRun{
			scheduledexecrun.FinishedAtIsNil(),
			scheduledexecrun.LeaseUntilGTE(nowUTC),
		}
		if len(excludeSQL) > 0 {
			basePreds = append(basePreds,
				scheduledexecrun.Or(scheduledexecrun.EventIDIsNil(), scheduledexecrun.EventIDNotIn(excludeSQL...)),
				scheduledexecrun.Or(scheduledexecrun.FireIDIsNil(), scheduledexecrun.FireIDNotIn(excludeSQL...)),
			)
		}

		var targets []uuid.UUID
		if err := c.ScheduledExecRun.Query().
			Where(basePreds...).
			GroupBy(scheduledexecrun.FieldTargetAgentID).
			Scan(ctx, &targets); err != nil {
			return mapError(err)
		}
		if len(targets) == 0 {
			return nil
		}

		turnByTarget := make(map[uuid.UUID]int64, len(targets))
		for start := 0; start < len(targets); start += scheduledExecRunsByEventChunk {
			end := min(start+scheduledExecRunsByEventChunk, len(targets))
			cursors, err := c.ScheduledExecPollCursor.Query().
				Where(scheduledexecpollcursor.TargetAgentIDIn(targets[start:end]...)).
				All(ctx)
			if err != nil {
				return mapError(err)
			}
			for _, cur := range cursors {
				turnByTarget[cur.TargetAgentID] = cur.TurnSeq
			}
		}

		var maxSeq int64
		topCursor, err := c.ScheduledExecPollCursor.Query().
			Order(scheduledexecpollcursor.ByTurnSeq(entsql.OrderDesc())).
			First(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return mapError(err)
		}
		if topCursor != nil {
			maxSeq = topCursor.TurnSeq
		}

		slices.SortFunc(targets, func(a, b uuid.UUID) int {
			ta, tb := turnByTarget[a], turnByTarget[b]
			if ta < tb {
				return -1
			}
			if ta > tb {
				return 1
			}
			sa, sb := a.String(), b.String()
			if sa < sb {
				return -1
			}
			if sa > sb {
				return 1
			}
			return 0
		})

		chosen := make([]*ent.ScheduledExecRun, 0, limit)
		pickedRunIDs := make(map[uuid.UUID][]uuid.UUID, len(targets))
		exhausted := make(map[uuid.UUID]bool, len(targets))

		for len(chosen) < limit {
			activeTargets := len(targets) - len(exhausted)
			if activeTargets == 0 {
				break
			}
			progressed := false
			for _, target := range targets {
				if len(chosen) == limit {
					break
				}
				if exhausted[target] {
					continue
				}
				need := 1
				if activeTargets == 1 {
					need = limit - len(chosen)
				}
				preds := make([]predicate.ScheduledExecRun, 0, len(basePreds)+2)
				preds = append(preds, basePreds...)
				preds = append(preds, scheduledexecrun.TargetAgentIDEQ(target))
				if picked := pickedRunIDs[target]; len(picked) > 0 {
					preds = append(preds, scheduledexecrun.IDNotIn(picked...))
				}
				q := c.ScheduledExecRun.Query().
					Where(preds...).
					Order(
						scheduledexecrun.ByPollSeq(),
						scheduledexecrun.ByLastPolledAt(),
						scheduledexecrun.ByID(),
					).
					Limit(need)
				if isPostgres(c) {
					q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
				}
				rows, err := q.All(ctx)
				if err != nil {
					return mapError(err)
				}
				for _, r := range rows {
					chosen = append(chosen, r)
					pickedRunIDs[target] = append(pickedRunIDs[target], r.ID)
					progressed = true
					if len(chosen) == limit {
						break
					}
				}
				if len(rows) < need || activeTargets == 1 {
					exhausted[target] = true
					activeTargets = len(targets) - len(exhausted)
				}
			}
			if !progressed {
				break
			}
		}

		if len(chosen) == 0 {
			return nil
		}

		latestTurn := make(map[uuid.UUID]int64, len(chosen))
		var touchedTargets []uuid.UUID
		for i, r := range chosen {
			seq := maxSeq + int64(i) + 1
			if err := c.ScheduledExecRun.UpdateOneID(r.ID).
				SetLastPolledAt(nowUTC).
				SetPollSeq(seq).
				Exec(ctx); err != nil {
				return mapError(err)
			}
			r.LastPolledAt = nowUTC
			r.PollSeq = seq
			if _, seen := latestTurn[r.TargetAgentID]; !seen {
				touchedTargets = append(touchedTargets, r.TargetAgentID)
			}
			latestTurn[r.TargetAgentID] = seq
		}

		builders := make([]*ent.ScheduledExecPollCursorCreate, 0, len(touchedTargets))
		for _, target := range touchedTargets {
			builders = append(builders, c.ScheduledExecPollCursor.Create().
				SetTargetAgentID(target).
				SetTurnSeq(latestTurn[target]).
				SetLastPolledAt(nowUTC))
		}
		if err := c.ScheduledExecPollCursor.CreateBulk(builders...).
			OnConflictColumns(scheduledexecpollcursor.FieldTargetAgentID).
			Update(func(u *ent.ScheduledExecPollCursorUpsert) {
				u.UpdateTurnSeq()
				u.UpdateLastPolledAt()
			}).
			Exec(ctx); err != nil {
			return mapError(err)
		}

		out = entScheduledExecRunsToStore(chosen)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func entScheduledExecRunsToStore(rows []*ent.ScheduledExecRun) []store.ScheduledExecRun {
	out := make([]store.ScheduledExecRun, 0, len(rows))
	for _, e := range rows {
		out = append(out, entScheduledExecRunToStore(e))
	}
	return out
}

// CreateScheduledEventChecked inserts event after check accepts the
// pending events of its project and event type (at most limit of them),
// read in the same transaction. On Postgres the transaction first takes the
// per-project scheduled-event quota lock (store.LockScheduledEventQuota,
// pg_advisory_xact_lock), so concurrent authors, on any replica, are
// checked and inserted one at a time; SQLite serializes writers already.
func loadScheduledExecQuotaSnapshot(
	ctx context.Context,
	c *ent.Client,
	projectID string,
	pid uuid.UUID,
	eventType string,
	limit int,
	excludeScheduleID *uuid.UUID,
) (store.ScheduledExecQuotaSnapshot, error) {
	if isPostgres(c) {
		var res sql.Result
		if err := c.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
			[]any{int32(store.LockScheduledEventQuota), store.StableProjectHash(projectID)}, &res); err != nil {
			return store.ScheduledExecQuotaSnapshot{}, fmt.Errorf("lock scheduled event quota: %w", err)
		}
	}
	eventRows, err := c.ScheduledEvent.Query().
		Where(
			scheduledevent.ProjectIDEQ(pid),
			scheduledevent.EventTypeEQ(eventType),
			scheduledevent.StatusEQ(store.ScheduledEventPending),
		).
		Order(scheduledevent.ByCreated(), scheduledevent.ByID()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return store.ScheduledExecQuotaSnapshot{}, mapError(err)
	}
	pending := make([]store.ScheduledEvent, 0, len(eventRows))
	for _, e := range eventRows {
		pending = append(pending, *entScheduledEventToStore(e))
	}

	schedPreds := []predicate.Schedule{
		schedule.ProjectIDEQ(pid),
		schedule.EventTypeEQ(eventType),
		schedule.StatusEQ(store.ScheduleStatusActive),
	}
	if excludeScheduleID != nil {
		schedPreds = append(schedPreds, schedule.IDNEQ(*excludeScheduleID))
	}
	schedRows, err := c.Schedule.Query().
		Where(schedPreds...).
		Order(schedule.ByCreated(), schedule.ByID()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return store.ScheduledExecQuotaSnapshot{}, mapError(err)
	}
	active := make([]store.Schedule, 0, len(schedRows))
	for _, sc := range schedRows {
		active = append(active, *entScheduleToStore(sc))
	}

	// Every kind, for the hub's per-project recurring schedule cap.
	allPreds := []predicate.Schedule{
		schedule.ProjectIDEQ(pid),
		schedule.StatusEQ(store.ScheduleStatusActive),
	}
	if excludeScheduleID != nil {
		allPreds = append(allPreds, schedule.IDNEQ(*excludeScheduleID))
	}
	allKinds, err := c.Schedule.Query().Where(allPreds...).Count(ctx)
	if err != nil {
		return store.ScheduledExecQuotaSnapshot{}, mapError(err)
	}
	return store.ScheduledExecQuotaSnapshot{
		PendingEvents:           pending,
		ActiveSchedules:         active,
		ActiveSchedulesAllKinds: allKinds,
	}, nil
}

// CreateScheduledEventChecked inserts event after check accepts the active
// exec items (pending events + active recurring schedules) of its project and
// event type, read in the same transaction under store.LockScheduledEventQuota.
func (s *ScheduleStore) CreateScheduledEventChecked(ctx context.Context, event *store.ScheduledEvent, limit int, check func(snap store.ScheduledExecQuotaSnapshot) error) error {
	pid, err := parseUUID(event.ProjectID)
	if err != nil {
		return err
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		snap, err := loadScheduledExecQuotaSnapshot(ctx, c, event.ProjectID, pid, event.EventType, limit, nil)
		if err != nil {
			return err
		}
		if err := check(snap); err != nil {
			return err
		}
		tx := &ScheduleStore{client: c, inTx: true}
		return tx.CreateScheduledEvent(ctx, event)
	})
}

// CreateScheduleChecked inserts sc after check accepts the active exec items
// (pending events + active recurring schedules) of its project and event type,
// read in the same transaction under store.LockScheduledEventQuota.
func (s *ScheduleStore) CreateScheduleChecked(ctx context.Context, sc *store.Schedule, limit int, check func(snap store.ScheduledExecQuotaSnapshot) error) error {
	pid, err := parseUUID(sc.ProjectID)
	if err != nil {
		return err
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		snap, err := loadScheduledExecQuotaSnapshot(ctx, c, sc.ProjectID, pid, sc.EventType, limit, nil)
		if err != nil {
			return err
		}
		if err := check(snap); err != nil {
			return err
		}
		tx := &ScheduleStore{client: c, inTx: true}
		return tx.CreateSchedule(ctx, sc)
	})
}

// UpdateScheduleChecked updates sc after check accepts the other active exec
// items (pending events + active recurring schedules excluding sc.ID) of its
// project and event type, read in the same transaction under
// store.LockScheduledEventQuota.
func (s *ScheduleStore) UpdateScheduleChecked(
	ctx context.Context, sc *store.Schedule, fields store.ScheduleFieldMask,
	prevRevision int, prevRevisionKnown bool, attribution *store.InitiatorAttribution,
	limit int, check func(snap store.ScheduledExecQuotaSnapshot) error,
) error {
	uid, err := parseUUID(sc.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(sc.ProjectID)
	if err != nil {
		return err
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		snap, err := loadScheduledExecQuotaSnapshot(ctx, c, sc.ProjectID, pid, sc.EventType, limit, &uid)
		if err != nil {
			return err
		}
		if err := check(snap); err != nil {
			return err
		}
		tx := &ScheduleStore{client: c, inTx: true}
		return tx.UpdateSchedule(ctx, sc, fields, prevRevision, prevRevisionKnown, attribution)
	})
}

// CompleteUnadmittedExecEvent atomically transitions an unadmitted exec event
// (status fired or expired, with no ScheduledExecRun row) to status with
// errMsg, and when scheduleID is non-empty records the corresponding run
// status on that schedule in the same transaction.
func (s *ScheduleStore) CompleteUnadmittedExecEvent(ctx context.Context, id string, status string, errMsg string, scheduleID string, ranAt time.Time) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	var schedUID uuid.UUID
	if scheduleID != "" {
		schedUID, err = parseUUID(scheduleID)
		if err != nil {
			return false, err
		}
	}
	var updated bool
	err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		// Lock the event row first on Postgres before checking ScheduledExecRun
		// so concurrent CreateScheduledExecRun and CompleteUnadmittedExecEvent
		// serialize on the event row rather than evaluating HasExecRun under a
		// pre-lock statement snapshot (ptone/scion#8210).
		evtQuery := c.ScheduledEvent.Query().Where(
			scheduledevent.IDEQ(uid),
			scheduledevent.StatusIn(store.ScheduledEventFired, store.ScheduledEventExpired),
		)
		if isPostgres(c) {
			evtQuery = evtQuery.ForUpdate()
		}
		if _, err := evtQuery.Only(ctx); ent.IsNotFound(err) {
			return nil
		} else if err != nil {
			return mapError(err)
		}
		admitted, err := c.ScheduledExecRun.Query().Where(scheduledexecrun.EventIDEQ(uid)).Exist(ctx)
		if err != nil {
			return mapError(err)
		}
		if admitted {
			return nil
		}
		upd := c.ScheduledEvent.Update().
			Where(
				scheduledevent.IDEQ(uid),
				scheduledevent.StatusIn(store.ScheduledEventFired, store.ScheduledEventExpired),
				scheduledevent.Not(scheduledevent.HasExecRun()),
			).
			SetStatus(status)
		if errMsg != "" {
			upd.SetError(errMsg)
		} else {
			upd.ClearError()
		}
		n, err := upd.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return nil
		}
		updated = true
		if scheduleID != "" {
			runStatus := store.ScheduleRunError
			if status == store.ScheduledEventSkipped {
				runStatus = store.ScheduleRunSkipped
			}
			if err := recordScheduleRunStatusOnClient(ctx, c, schedUID, ranAt, runStatus, errMsg); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// unadmittedPredicates select events of eventType claimed (fired or
// expired) before firedBefore that have no exec run.
func unadmittedPredicates(eventType string, firedBefore time.Time) []predicate.ScheduledEvent {
	return []predicate.ScheduledEvent{
		scheduledevent.EventTypeEQ(eventType),
		scheduledevent.StatusIn(store.ScheduledEventFired, store.ScheduledEventExpired),
		scheduledevent.FiredAtLT(firedBefore.UTC()),
		scheduledevent.Not(scheduledevent.HasExecRun()),
	}
}

// ListUnadmittedScheduledEvents returns up to limit events of eventType
// claimed (fired or expired) before firedBefore that have no exec run,
// oldest claim first.
func (s *ScheduleStore) ListUnadmittedScheduledEvents(ctx context.Context, eventType string, firedBefore time.Time, limit int) ([]store.ScheduledEvent, error) {
	rows, err := s.client.ScheduledEvent.Query().
		Where(unadmittedPredicates(eventType, firedBefore)...).
		Order(scheduledevent.ByFiredAt(), scheduledevent.ByID()).
		Limit(clampLimit(limit)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.ScheduledEvent, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entScheduledEventToStore(e))
	}
	return out, nil
}

// FailUnadmittedScheduledEvent records event id failed with errMsg while it
// still matches unadmittedPredicates (same event type, claimed before
// firedBefore, no exec run), and, for a recurring event, records the failed
// run on its schedule in the same transaction. It reports whether the event
// changed.
func (s *ScheduleStore) FailUnadmittedScheduledEvent(ctx context.Context, id, eventType string, firedBefore time.Time, errMsg string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	var changed bool
	err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		// Lock the event row first on Postgres before checking ScheduledExecRun
		// so concurrent CreateScheduledExecRun and FailUnadmittedScheduledEvent
		// serialize on the event row rather than evaluating HasExecRun under a
		// pre-lock statement snapshot (ptone/scion#8210).
		evtQuery := c.ScheduledEvent.Query().Where(
			scheduledevent.IDEQ(uid),
			scheduledevent.EventTypeEQ(eventType),
			scheduledevent.StatusIn(store.ScheduledEventFired, store.ScheduledEventExpired),
			scheduledevent.FiredAtLT(firedBefore.UTC()),
		)
		if isPostgres(c) {
			evtQuery = evtQuery.ForUpdate()
		}
		evt, err := evtQuery.Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return mapError(err)
		}
		admitted, err := c.ScheduledExecRun.Query().Where(scheduledexecrun.EventIDEQ(uid)).Exist(ctx)
		if err != nil {
			return mapError(err)
		}
		if admitted {
			return nil
		}
		n, err := c.ScheduledEvent.Update().
			Where(append([]predicate.ScheduledEvent{scheduledevent.IDEQ(uid)}, unadmittedPredicates(eventType, firedBefore)...)...).
			SetStatus(store.ScheduledEventFailed).
			SetError(errMsg).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return nil
		}
		changed = true
		if evt.ScheduleID != "" {
			schedUID, err := parseUUID(evt.ScheduleID)
			if err != nil {
				return nil
			}
			ranAt := evt.FireAt
			if evt.FiredAt != nil {
				ranAt = *evt.FiredAt
			}
			if err := recordScheduleRunStatusOnClient(ctx, c, schedUID, ranAt, store.ScheduleRunError, errMsg); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
