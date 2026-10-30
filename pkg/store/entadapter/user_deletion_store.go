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
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/userdeletion"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/userdeletionagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// UserDeletionStore implements store.UserDeletionStore using Ent
// (ptone/scion#2143). Deletion rows are never deleted.
type UserDeletionStore struct {
	client *ent.Client
}

// NewUserDeletionStore creates an Ent-backed UserDeletionStore.
func NewUserDeletionStore(client *ent.Client) *UserDeletionStore {
	return &UserDeletionStore{client: client}
}

func entUserDeletionToStore(d *ent.UserDeletion) *store.UserDeletion {
	return &store.UserDeletion{
		ID:               d.ID.String(),
		UserID:           d.UserID.String(),
		State:            d.State,
		RequestedByKind:  d.RequestedByKind,
		RequestedByID:    d.RequestedByID,
		CredentialKind:   d.CredentialKind,
		RequestedAt:      d.RequestedAt,
		CompletedAt:      d.CompletedAt,
		LastReconciledAt: d.LastReconciledAt,
		BeforeSummary:    d.BeforeSummary,
		OrphanedByRevoke: d.OrphanedByRevoke,
		LastError:        d.LastError,
	}
}

func entUserDeletionAgentToStore(a *ent.UserDeletionAgent) store.UserDeletionAgent {
	return store.UserDeletionAgent{
		DeletionID: a.DeletionID.String(),
		AgentID:    a.AgentID.String(),
		Slug:       a.Slug,
		ProjectID:  a.ProjectID,
		Outcome:    a.Outcome,
		Attempts:   a.Attempts,
		LastError:  a.LastError,
		UpdatedAt:  a.UpdatedAt,
	}
}

// CreateUserDeletion records a deletion in state stopping.
func (s *UserDeletionStore) CreateUserDeletion(ctx context.Context, d *store.UserDeletion) error {
	if d == nil {
		return fmt.Errorf("%w: nil user deletion", store.ErrInvalidInput)
	}
	userID, err := uuid.Parse(d.UserID)
	if err != nil {
		return fmt.Errorf("%w: a user deletion requires a user ID that is a UUID", store.ErrInvalidInput)
	}
	if d.RequestedAt.IsZero() {
		return fmt.Errorf("%w: a user deletion requires a request time", store.ErrInvalidInput)
	}
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	id, err := parseUUID(d.ID)
	if err != nil {
		return err
	}
	d.State = store.UserDeletionStateStopping
	d.CompletedAt = nil
	_, err = s.client.UserDeletion.Create().
		SetID(id).
		SetUserID(userID).
		SetState(d.State).
		SetRequestedByKind(d.RequestedByKind).
		SetRequestedByID(d.RequestedByID).
		SetCredentialKind(d.CredentialKind).
		SetRequestedAt(d.RequestedAt).
		SetBeforeSummary(d.BeforeSummary).
		SetOrphanedByRevoke(d.OrphanedByRevoke).
		SetLastError(d.LastError).
		Save(ctx)
	return mapError(err)
}

// GetUserDeletion returns a deletion by ID.
func (s *UserDeletionStore) GetUserDeletion(ctx context.Context, id string) (*store.UserDeletion, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, store.ErrNotFound
	}
	row, err := s.client.UserDeletion.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entUserDeletionToStore(row), nil
}

// GetUserDeletionByUser returns the deletion of a user.
func (s *UserDeletionStore) GetUserDeletionByUser(ctx context.Context, userID string) (*store.UserDeletion, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	row, err := s.client.UserDeletion.Query().Where(userdeletion.UserIDEQ(uid)).Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUserDeletionToStore(row), nil
}

// ListUserDeletionsByState returns up to limit deletions in state, least
// recently reconciled first (never reconciled first), then oldest request.
// NULLS FIRST is explicit: PostgreSQL sorts NULL last in ascending order.
func (s *UserDeletionStore) ListUserDeletionsByState(ctx context.Context, state string, limit int) ([]store.UserDeletion, error) {
	q := s.client.UserDeletion.Query().
		Where(userdeletion.StateEQ(state)).
		Order(
			userdeletion.ByLastReconciledAt(sql.OrderAsc(), sql.OrderNullsFirst()),
			userdeletion.ByRequestedAt(sql.OrderAsc()),
			userdeletion.ByID(),
		)
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.UserDeletion, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entUserDeletionToStore(r))
	}
	return out, nil
}

// CompleteUserDeletion moves a deletion from stopping to completed.
func (s *UserDeletionStore) CompleteUserDeletion(ctx context.Context, id string, at time.Time) (bool, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return false, store.ErrNotFound
	}
	n, err := s.client.UserDeletion.Update().
		Where(userdeletion.IDEQ(uid), userdeletion.StateEQ(store.UserDeletionStateStopping)).
		SetState(store.UserDeletionStateCompleted).
		SetCompletedAt(at).
		SetLastError("").
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if n > 0 {
		return true, nil
	}
	exists, err := s.client.UserDeletion.Query().Where(userdeletion.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if !exists {
		return false, store.ErrNotFound
	}
	return false, nil
}

// TouchUserDeletionReconciled records a reconcile and its error on a
// deletion in state stopping; a completed deletion is left unchanged.
func (s *UserDeletionStore) TouchUserDeletionReconciled(ctx context.Context, id string, at time.Time, lastErr string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return store.ErrNotFound
	}
	n, err := s.client.UserDeletion.Update().
		Where(userdeletion.IDEQ(uid), userdeletion.StateEQ(store.UserDeletionStateStopping)).
		SetLastReconciledAt(at).
		SetLastError(lastErr).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n > 0 {
		return nil
	}
	exists, err := s.client.UserDeletion.Query().Where(userdeletion.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return mapError(err)
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// UpsertUserDeletionAgent creates or replaces the record of one agent of a
// deletion, leaving a stopped or removed record unchanged.
func (s *UserDeletionStore) UpsertUserDeletionAgent(ctx context.Context, a *store.UserDeletionAgent) error {
	return s.writeUserDeletionAgent(ctx, a, false)
}

// ReopenUserDeletionAgent creates or replaces the record of one agent of a
// deletion whatever its outcome.
func (s *UserDeletionStore) ReopenUserDeletionAgent(ctx context.Context, a *store.UserDeletionAgent) error {
	return s.writeUserDeletionAgent(ctx, a, true)
}

// writeUserDeletionAgent writes the record of one agent. Unless reopen, a
// record whose outcome is final (stopped or removed) is left unchanged.
//
// Each statement is safe inside a PostgreSQL transaction: the insert is
// ON CONFLICT DO NOTHING, so a concurrent insert of the same record never
// raises a unique violation (which would abort the transaction). The
// statements are: a conditional UPDATE; when it matched nothing, the insert;
// when the insert added nothing (the record exists), the conditional UPDATE
// again, which matches nothing on a final record.
func (s *UserDeletionStore) writeUserDeletionAgent(ctx context.Context, a *store.UserDeletionAgent, reopen bool) error {
	if a == nil {
		return fmt.Errorf("%w: nil user deletion agent", store.ErrInvalidInput)
	}
	if !store.ValidUserDeletionOutcome(a.Outcome) {
		return fmt.Errorf("%w: unknown user deletion outcome %q", store.ErrInvalidInput, a.Outcome)
	}
	if a.Attempts < 0 {
		return fmt.Errorf("%w: negative attempts", store.ErrInvalidInput)
	}
	deletionID, err := uuid.Parse(a.DeletionID)
	if err != nil {
		return store.ErrNotFound
	}
	agentID, err := uuid.Parse(a.AgentID)
	if err != nil {
		return fmt.Errorf("%w: a user deletion agent requires an agent ID that is a UUID", store.ErrInvalidInput)
	}
	exists, err := s.client.UserDeletion.Query().Where(userdeletion.IDEQ(deletionID)).Exist(ctx)
	if err != nil {
		return mapError(err)
	}
	if !exists {
		return store.ErrNotFound
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = time.Now()
	}
	update := func() (int, error) {
		preds := []predicate.UserDeletionAgent{
			userdeletionagent.DeletionIDEQ(deletionID),
			userdeletionagent.AgentIDEQ(agentID),
		}
		if !reopen {
			preds = append(preds, userdeletionagent.OutcomeNotIn(
				store.UserDeletionOutcomeStopped, store.UserDeletionOutcomeRemoved))
		}
		return s.client.UserDeletionAgent.Update().
			Where(preds...).
			SetSlug(a.Slug).
			SetProjectID(a.ProjectID).
			SetOutcome(a.Outcome).
			SetAttempts(a.Attempts).
			SetLastError(a.LastError).
			SetUpdatedAt(a.UpdatedAt).
			Save(ctx)
	}
	n, err := update()
	if err != nil {
		return mapError(err)
	}
	if n > 0 {
		return nil
	}
	// CreateBulk and Exec, not Create and Save: when DO NOTHING skips the
	// row, the single-row read-back fails (see SeedMaintenanceOperations).
	rowID := uuid.New()
	err = s.client.UserDeletionAgent.CreateBulk(
		s.client.UserDeletionAgent.Create().
			SetID(rowID).
			SetDeletionID(deletionID).
			SetAgentID(agentID).
			SetSlug(a.Slug).
			SetProjectID(a.ProjectID).
			SetOutcome(a.Outcome).
			SetAttempts(a.Attempts).
			SetLastError(a.LastError).
			SetUpdatedAt(a.UpdatedAt),
	).
		OnConflictColumns(userdeletionagent.FieldDeletionID, userdeletionagent.FieldAgentID).
		DoNothing().
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	inserted, err := s.client.UserDeletionAgent.Query().Where(userdeletionagent.IDEQ(rowID)).Exist(ctx)
	if err != nil {
		return mapError(err)
	}
	if inserted {
		return nil
	}
	// The record exists: it was final, or a concurrent writer inserted it
	// after the first UPDATE. Replace it unless it is final.
	if _, err := update(); err != nil {
		return mapError(err)
	}
	return nil
}

// ListUserDeletionAgents returns the agents of a deletion, by agent ID.
func (s *UserDeletionStore) ListUserDeletionAgents(ctx context.Context, deletionID string) ([]store.UserDeletionAgent, error) {
	uid, err := uuid.Parse(deletionID)
	if err != nil {
		return []store.UserDeletionAgent{}, nil
	}
	rows, err := s.client.UserDeletionAgent.Query().
		Where(userdeletionagent.DeletionIDEQ(uid)).
		Order(userdeletionagent.ByAgentID()).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.UserDeletionAgent, 0, len(rows))
	for _, r := range rows {
		out = append(out, entUserDeletionAgentToStore(r))
	}
	return out, nil
}

// ListUserDeletionAgentsForAgent returns the records of an agent in
// deletions in state stopping.
func (s *UserDeletionStore) ListUserDeletionAgentsForAgent(ctx context.Context, agentID string) ([]store.UserDeletionAgent, error) {
	uid, err := uuid.Parse(agentID)
	if err != nil {
		return []store.UserDeletionAgent{}, nil
	}
	rows, err := s.client.UserDeletionAgent.Query().
		Where(
			userdeletionagent.AgentIDEQ(uid),
			userdeletionagent.HasDeletionWith(userdeletion.StateEQ(store.UserDeletionStateStopping)),
		).
		Order(userdeletionagent.ByDeletionID()).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.UserDeletionAgent, 0, len(rows))
	for _, r := range rows {
		out = append(out, entUserDeletionAgentToStore(r))
	}
	return out, nil
}
