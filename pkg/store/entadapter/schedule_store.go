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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/schedule"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/scheduledevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ScheduleStore implements store.ScheduleStore and store.ScheduledEventStore
// using the Ent ORM. A single type serves both sub-interfaces, mirroring the
// SQLite backend where schedules and scheduled events share one store.
type ScheduleStore struct {
	client *ent.Client
}

// NewScheduleStore creates a new Ent-backed ScheduleStore.
func NewScheduleStore(client *ent.Client) *ScheduleStore {
	return &ScheduleStore{client: client}
}

// defaultListLimit / maxListLimit mirror the pagination bounds used by the
// SQLite backend so the two backends paginate identically.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

// ============================================================================
// Schedule <-> store conversion
// ============================================================================

func entScheduleToStore(e *ent.Schedule) *store.Schedule {
	sc := &store.Schedule{
		ID:            e.ID.String(),
		ProjectID:     e.ProjectID.String(),
		Name:          e.Name,
		CronExpr:      e.CronExpr,
		EventType:     e.EventType,
		Payload:       e.Payload,
		Status:        e.Status,
		NextRunAt:     e.NextRunAt,
		LastRunAt:     e.LastRunAt,
		LastRunStatus: e.LastRunStatus,
		LastRunError:  e.LastRunError,
		RunCount:      e.RunCount,
		ErrorCount:    e.ErrorCount,
		CreatedAt:     e.Created,
		CreatedBy:     e.CreatedBy,
		UpdatedAt:     e.Updated,
	}
	return sc
}

// ============================================================================
// Schedule Operations (Recurring Schedules)
// ============================================================================

// CreateSchedule creates a new recurring schedule.
func (s *ScheduleStore) CreateSchedule(ctx context.Context, sc *store.Schedule) error {
	if sc.ID == "" || sc.ProjectID == "" || sc.Name == "" || sc.CronExpr == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(sc.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(sc.ProjectID)
	if err != nil {
		return err
	}
	if sc.Status == "" {
		sc.Status = store.ScheduleStatusActive
	}

	create := s.client.Schedule.Create().
		SetID(uid).
		SetProjectID(pid).
		SetName(sc.Name).
		SetCronExpr(sc.CronExpr).
		SetEventType(sc.EventType).
		SetStatus(sc.Status).
		SetRunCount(sc.RunCount).
		SetErrorCount(sc.ErrorCount)

	if sc.Payload != "" {
		create.SetPayload(sc.Payload)
	}
	if sc.NextRunAt != nil {
		create.SetNextRunAt(*sc.NextRunAt)
	}
	if sc.LastRunAt != nil {
		create.SetLastRunAt(*sc.LastRunAt)
	}
	if sc.LastRunStatus != "" {
		create.SetLastRunStatus(sc.LastRunStatus)
	}
	if sc.LastRunError != "" {
		create.SetLastRunError(sc.LastRunError)
	}
	if sc.CreatedBy != "" {
		create.SetCreatedBy(sc.CreatedBy)
	}
	if !sc.CreatedAt.IsZero() {
		create.SetCreated(sc.CreatedAt)
	}
	if !sc.UpdatedAt.IsZero() {
		create.SetUpdated(sc.UpdatedAt)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	sc.CreatedAt = created.Created
	sc.UpdatedAt = created.Updated
	sc.Status = created.Status
	return nil
}

// GetSchedule retrieves a schedule by ID.
func (s *ScheduleStore) GetSchedule(ctx context.Context, id string) (*store.Schedule, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Schedule.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entScheduleToStore(e), nil
}

// ListSchedules returns schedules matching the filter criteria.
func (s *ScheduleStore) ListSchedules(ctx context.Context, filter store.ScheduleFilter, opts store.ListOptions) (*store.ListResult[store.Schedule], error) {
	query := s.client.Schedule.Query()

	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(schedule.ProjectIDEQ(pid))
	}
	if filter.Status != "" {
		query.Where(schedule.StatusEQ(filter.Status))
	} else {
		// By default, exclude deleted schedules.
		query.Where(schedule.StatusNEQ(store.ScheduleStatusDeleted))
	}
	if filter.Name != "" {
		query.Where(schedule.NameEQ(filter.Name))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(schedule.ByCreated(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	schedules := make([]store.Schedule, 0, len(entities))
	for _, e := range entities {
		schedules = append(schedules, *entScheduleToStore(e))
	}

	result := &store.ListResult[store.Schedule]{TotalCount: totalCount}
	if len(schedules) > limit {
		result.Items = schedules[:limit]
		result.NextCursor = schedules[limit-1].ID
	} else {
		result.Items = schedules
	}
	return result, nil
}

// UpdateSchedule updates an existing schedule.
func (s *ScheduleStore) UpdateSchedule(ctx context.Context, sc *store.Schedule) error {
	uid, err := parseUUID(sc.ID)
	if err != nil {
		return err
	}

	update := s.client.Schedule.UpdateOneID(uid).
		SetName(sc.Name).
		SetCronExpr(sc.CronExpr).
		SetEventType(sc.EventType).
		SetPayload(sc.Payload).
		SetStatus(sc.Status)

	if sc.NextRunAt != nil {
		update.SetNextRunAt(*sc.NextRunAt)
	} else {
		update.ClearNextRunAt()
	}

	updated, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	sc.UpdatedAt = updated.Updated
	return nil
}

// UpdateScheduleStatus updates only the status of a schedule.
func (s *ScheduleStore) UpdateScheduleStatus(ctx context.Context, id string, status string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	err = s.client.Schedule.UpdateOneID(uid).SetStatus(status).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// UpdateScheduleAfterRun updates a schedule after a run completes. It performs a
// read-modify-write of the run/error counters atomically via Ent's Add* setters.
func (s *ScheduleStore) UpdateScheduleAfterRun(ctx context.Context, id string, ranAt time.Time, nextRunAt time.Time, errMsg string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	update := s.client.Schedule.UpdateOneID(uid).
		SetLastRunAt(ranAt).
		SetNextRunAt(nextRunAt).
		AddRunCount(1)

	if errMsg != "" {
		update.
			SetLastRunStatus(store.ScheduleRunError).
			SetLastRunError(errMsg).
			AddErrorCount(1)
	} else {
		update.
			SetLastRunStatus(store.ScheduleRunSuccess).
			ClearLastRunError()
	}

	if err := update.Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteSchedule removes a schedule by ID (hard delete).
func (s *ScheduleStore) DeleteSchedule(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.Schedule.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListDueSchedules returns active schedules whose next_run_at has passed.
//
// This is a JOB-CLAIM PATH (§2.A.3): multiple hub replicas poll it concurrently.
// The dialect-aware claim helper applies SELECT ... FOR UPDATE SKIP LOCKED on
// Postgres so two replicas never pick up the same schedule, and falls back to a
// plain SELECT on SQLite (single writer, no SKIP LOCKED support).
func (s *ScheduleStore) ListDueSchedules(ctx context.Context, now time.Time) ([]store.Schedule, error) {
	ids, err := s.skipLockedIDs(ctx, schedule.Table, func(sel *entsql.Selector) {
		sel.Where(entsql.And(
			entsql.EQ(schedule.FieldStatus, store.ScheduleStatusActive),
			entsql.NotNull(schedule.FieldNextRunAt),
			entsql.LTE(schedule.FieldNextRunAt, now),
		)).OrderBy(entsql.Asc(schedule.FieldNextRunAt))
	})
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	entities, err := s.client.Schedule.Query().
		Where(schedule.IDIn(ids...)).
		Order(schedule.ByNextRunAt()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Schedule, 0, len(entities))
	for _, e := range entities {
		out = append(out, *entScheduleToStore(e))
	}
	return out, nil
}

// ============================================================================
// ScheduledEvent <-> store conversion
// ============================================================================

func entScheduledEventToStore(e *ent.ScheduledEvent) *store.ScheduledEvent {
	return &store.ScheduledEvent{
		ID:         e.ID.String(),
		ProjectID:  e.ProjectID.String(),
		EventType:  e.EventType,
		FireAt:     e.FireAt,
		Payload:    e.Payload,
		Status:     e.Status,
		CreatedAt:  e.Created,
		CreatedBy:  e.CreatedBy,
		FiredAt:    e.FiredAt,
		Error:      e.Error,
		ScheduleID: e.ScheduleID,
	}
}

// ============================================================================
// Scheduled Event Operations
// ============================================================================

// CreateScheduledEvent creates a new scheduled event.
func (s *ScheduleStore) CreateScheduledEvent(ctx context.Context, event *store.ScheduledEvent) error {
	if event.ID == "" || event.ProjectID == "" || event.EventType == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(event.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(event.ProjectID)
	if err != nil {
		return err
	}
	if event.Status == "" {
		event.Status = store.ScheduledEventPending
	}

	create := s.client.ScheduledEvent.Create().
		SetID(uid).
		SetProjectID(pid).
		SetEventType(event.EventType).
		SetFireAt(event.FireAt).
		SetPayload(event.Payload).
		SetStatus(event.Status)

	if event.CreatedBy != "" {
		create.SetCreatedBy(event.CreatedBy)
	}
	if event.FiredAt != nil {
		create.SetFiredAt(*event.FiredAt)
	}
	if event.Error != "" {
		create.SetError(event.Error)
	}
	if event.ScheduleID != "" {
		create.SetScheduleID(event.ScheduleID)
	}
	if !event.CreatedAt.IsZero() {
		create.SetCreated(event.CreatedAt)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	event.CreatedAt = created.Created
	event.Status = created.Status
	return nil
}

// GetScheduledEvent retrieves a scheduled event by ID.
func (s *ScheduleStore) GetScheduledEvent(ctx context.Context, id string) (*store.ScheduledEvent, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.ScheduledEvent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entScheduledEventToStore(e), nil
}

// ListPendingScheduledEvents returns all events with status "pending",
// ordered by fire_at ASC.
//
// Like ListDueSchedules this is a JOB-CLAIM PATH and uses the dialect-aware
// SKIP LOCKED helper for safe multi-replica polling.
func (s *ScheduleStore) ListPendingScheduledEvents(ctx context.Context) ([]store.ScheduledEvent, error) {
	ids, err := s.skipLockedIDs(ctx, scheduledevent.Table, func(sel *entsql.Selector) {
		sel.Where(entsql.EQ(scheduledevent.FieldStatus, store.ScheduledEventPending)).
			OrderBy(entsql.Asc(scheduledevent.FieldFireAt))
	})
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	entities, err := s.client.ScheduledEvent.Query().
		Where(scheduledevent.IDIn(ids...)).
		Order(scheduledevent.ByFireAt()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.ScheduledEvent, 0, len(entities))
	for _, e := range entities {
		out = append(out, *entScheduledEventToStore(e))
	}
	return out, nil
}

// UpdateScheduledEventStatus updates the status and optional error for an event.
// Mirroring the SQLite backend, a missing event is a no-op (not ErrNotFound).
func (s *ScheduleStore) UpdateScheduledEventStatus(ctx context.Context, id string, status string, firedAt *time.Time, errMsg string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	update := s.client.ScheduledEvent.Update().
		Where(scheduledevent.IDEQ(uid)).
		SetStatus(status)

	if firedAt != nil {
		update.SetFiredAt(*firedAt)
	} else {
		update.ClearFiredAt()
	}
	if errMsg != "" {
		update.SetError(errMsg)
	} else {
		update.ClearError()
	}

	_, err = update.Save(ctx)
	return err
}

// ClaimScheduledEvent atomically transitions a scheduled event from "pending" to
// claimedStatus, returning whether this caller won the claim. It is the
// multi-replica dedup primitive (store.ScheduledEventClaimer): several hub
// replicas may each recover the same pending event from the database on startup
// and arm an in-memory timer for it, but the conditional
// UPDATE ... WHERE status = 'pending' is atomic, so exactly one replica observes
// affected == 1 and is allowed to execute the event's side effect. Losers
// observe affected == 0 and skip execution.
//
// The same atomicity holds on SQLite (a conditional UPDATE is atomic there too);
// it is simply never contended because there is a single writer.
func (s *ScheduleStore) ClaimScheduledEvent(ctx context.Context, id string, claimedStatus string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	if claimedStatus == "" {
		claimedStatus = store.ScheduledEventFired
	}
	affected, err := s.client.ScheduledEvent.Update().
		Where(
			scheduledevent.IDEQ(uid),
			scheduledevent.StatusEQ(store.ScheduledEventPending),
		).
		SetStatus(claimedStatus).
		SetFiredAt(time.Now()).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected == 1, nil
}

// CancelScheduledEvent marks a pending event as cancelled. Returns ErrNotFound
// if the event doesn't exist or is not pending.
func (s *ScheduleStore) CancelScheduledEvent(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	n, err := s.client.ScheduledEvent.Update().
		Where(
			scheduledevent.IDEQ(uid),
			scheduledevent.StatusEQ(store.ScheduledEventPending),
		).
		SetStatus(store.ScheduledEventCancelled).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListScheduledEvents returns events matching the filter criteria.
func (s *ScheduleStore) ListScheduledEvents(ctx context.Context, filter store.ScheduledEventFilter, opts store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	query := s.client.ScheduledEvent.Query()

	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(scheduledevent.ProjectIDEQ(pid))
	}
	if filter.EventType != "" {
		query.Where(scheduledevent.EventTypeEQ(filter.EventType))
	}
	if filter.Status != "" {
		query.Where(scheduledevent.StatusEQ(filter.Status))
	}
	if filter.ScheduleID != "" {
		query.Where(scheduledevent.ScheduleIDEQ(filter.ScheduleID))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	if opts.Cursor != "" {
		cursorUID, err := parseUUID(opts.Cursor)
		if err != nil {
			return nil, err
		}
		query.Where(scheduledevent.IDLT(cursorUID))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(scheduledevent.ByCreated(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	events := make([]store.ScheduledEvent, 0, len(entities))
	for _, e := range entities {
		events = append(events, *entScheduledEventToStore(e))
	}

	result := &store.ListResult[store.ScheduledEvent]{TotalCount: totalCount}
	if len(events) > limit {
		result.Items = events[:limit]
		result.NextCursor = events[limit-1].ID
	} else {
		result.Items = events
	}
	return result, nil
}

// PurgeOldScheduledEvents removes non-pending events older than cutoff.
func (s *ScheduleStore) PurgeOldScheduledEvents(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.client.ScheduledEvent.Delete().
		Where(
			scheduledevent.StatusNEQ(store.ScheduledEventPending),
			scheduledevent.CreatedLT(cutoff),
		).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ============================================================================
// Dialect-aware claim helper
// ============================================================================

// skipLockedIDs runs the claim SELECT described by apply and returns the ids of
// the matching rows. On Postgres it issues `SELECT id ... FOR UPDATE SKIP
// LOCKED` inside a short transaction so concurrent replicas receive disjoint
// row sets; the caller then transitions the claimed rows to their next state.
// On SQLite (and any non-Postgres dialect) it degrades to a plain `SELECT id`,
// which is correct for the single-writer backend that has no SKIP LOCKED
// support.
func (s *ScheduleStore) skipLockedIDs(ctx context.Context, table string, apply func(*entsql.Selector)) ([]uuid.UUID, error) {
	drv := s.client.Driver()
	d := drv.Dialect()

	builder := entsql.Dialect(d)
	selector := builder.Select(genericIDColumn).From(builder.Table(table))
	apply(selector)
	if d == dialect.Postgres {
		selector.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}

	query, args := selector.Query()

	if d == dialect.Postgres {
		return s.queryIDsTx(ctx, drv, query, args)
	}
	return s.queryIDs(ctx, drv, query, args)
}

// genericIDColumn is the primary-key column shared by all ported tables.
const genericIDColumn = "id"

// queryIDs runs the claim SELECT directly (no surrounding transaction). Used for
// dialects that do not support row-level locking.
func (s *ScheduleStore) queryIDs(ctx context.Context, drv interface {
	Query(context.Context, string, any, any) error
}, query string, args []any) ([]uuid.UUID, error) {
	rows := &entsql.Rows{}
	if err := drv.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUUIDRows(rows)
}

// queryIDsTx runs the SKIP LOCKED claim SELECT within a transaction so the row
// locks are held while the disjoint id set is materialized.
func (s *ScheduleStore) queryIDsTx(ctx context.Context, drv interface {
	Tx(context.Context) (dialect.Tx, error)
}, query string, args []any) ([]uuid.UUID, error) {
	tx, err := drv.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rows := &entsql.Rows{}
	if err := tx.Query(ctx, query, args, rows); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	ids, scanErr := scanUUIDRows(rows)
	_ = rows.Close()
	if scanErr != nil {
		_ = tx.Rollback()
		return nil, scanErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// scanUUIDRows scans a single-column id result set into a slice of UUIDs.
func scanUUIDRows(rows *entsql.Rows) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
