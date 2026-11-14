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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledexecrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Scheduled exec runs (ptone/scion#7883). The methods live on ScheduleStore
// so a run's outcome and its event's status share one transaction, and so
// the event purge can remove runs in its own transaction.

var _ store.ScheduledExecRunStore = (*ScheduleStore)(nil)

func entScheduledExecRunToStore(e *ent.ScheduledExecRun) store.ScheduledExecRun {
	run := store.ScheduledExecRun{
		EventID:                 e.ID.String(),
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
		LeaseUntil:              e.LeaseUntil,
		FinishedAt:              e.FinishedAt,
		Outcome:                 string(e.Outcome),
		ExitCode:                e.ExitCode,
		DurationMS:              e.DurationMs,
		OutputBytes:             e.OutputBytes,
		Runtime:                 e.Runtime,
		LaunchID:                e.LaunchID,
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

// CreateScheduledExecRun inserts the run record of an admitted exec event.
func (s *ScheduleStore) CreateScheduledExecRun(ctx context.Context, run *store.ScheduledExecRun) error {
	if run == nil || run.EventID == "" || run.ProjectID == "" || run.TargetAgentID == "" || run.CommandSHA256 == "" {
		return store.ErrInvalidInput
	}
	eid, err := parseUUID(run.EventID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(run.ProjectID)
	if err != nil {
		return err
	}
	aid, err := parseUUID(run.TargetAgentID)
	if err != nil {
		return err
	}
	if run.Outcome == "" {
		run.Outcome = store.ScheduledExecOutcomeStarting
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	create := s.client.ScheduledExecRun.Create().
		SetID(eid).
		SetProjectID(pid).
		SetTargetAgentID(aid).
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
		SetLeaseUntil(run.LeaseUntil.UTC()).
		SetOutcome(scheduledexecrun.Outcome(run.Outcome)).
		SetRuntime(run.Runtime).
		SetLaunchID(run.LaunchID).
		SetNillableExitCode(run.ExitCode).
		SetNillableDurationMs(run.DurationMS).
		SetNillableOutputBytes(run.OutputBytes)
	if run.ScheduleID != "" {
		sid, err := parseUUID(run.ScheduleID)
		if err != nil {
			return err
		}
		create.SetScheduleID(sid)
	}
	if run.FinishedAt != nil {
		create.SetFinishedAt(run.FinishedAt.UTC())
	}
	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetScheduledExecRun returns the run of an event.
func (s *ScheduleStore) GetScheduledExecRun(ctx context.Context, eventID string) (*store.ScheduledExecRun, error) {
	eid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	e, err := s.client.ScheduledExecRun.Get(ctx, eid)
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
			Where(scheduledexecrun.IDIn(ids[start:end]...)).
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

// MarkScheduledExecRunRunning moves an unfinished starting run to running.
func (s *ScheduleStore) MarkScheduledExecRunRunning(ctx context.Context, eventID string) (bool, error) {
	eid, err := parseUUID(eventID)
	if err != nil {
		return false, err
	}
	n, err := s.client.ScheduledExecRun.Update().
		Where(
			scheduledexecrun.IDEQ(eid),
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

// FinishScheduledExecRun records a run's final outcome, first writer wins,
// and the event's status in the same transaction when asked.
func (s *ScheduleStore) FinishScheduledExecRun(ctx context.Context, fin store.ScheduledExecRunFinish) (bool, error) {
	if !finalExecOutcome(fin.Outcome) {
		return false, store.ErrInvalidInput
	}
	eid, err := parseUUID(fin.EventID)
	if err != nil {
		return false, err
	}
	finishedAt := fin.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	var won bool
	err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		n, err := c.ScheduledExecRun.Update().
			Where(scheduledexecrun.IDEQ(eid), scheduledexecrun.FinishedAtIsNil()).
			SetFinishedAt(finishedAt.UTC()).
			SetOutcome(scheduledexecrun.Outcome(fin.Outcome)).
			SetNillableExitCode(fin.ExitCode).
			SetNillableDurationMs(fin.DurationMS).
			SetNillableOutputBytes(fin.OutputBytes).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return nil
		}
		won = true
		if fin.EventStatus == "" {
			return nil
		}
		upd := c.ScheduledEvent.Update().
			Where(
				scheduledevent.IDEQ(eid),
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
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// ListUnfinishedScheduledExecRuns pages through unfinished runs by event ID.
func (s *ScheduleStore) ListUnfinishedScheduledExecRuns(ctx context.Context, afterEventID string, limit int) ([]store.ScheduledExecRun, error) {
	limit = clampLimit(limit)
	q := s.client.ScheduledExecRun.Query().
		Where(scheduledexecrun.FinishedAtIsNil())
	if afterEventID != "" {
		after, err := parseUUID(afterEventID)
		if err != nil {
			return nil, err
		}
		q = q.Where(scheduledexecrun.IDGT(after))
	}
	rows, err := q.Order(scheduledexecrun.ByID()).Limit(limit).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.ScheduledExecRun, 0, len(rows))
	for _, e := range rows {
		out = append(out, entScheduledExecRunToStore(e))
	}
	return out, nil
}
