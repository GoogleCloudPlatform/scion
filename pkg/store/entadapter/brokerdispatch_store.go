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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerdispatch"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// BrokerDispatchStore is the Ent-backed store for the broker_dispatch durable
// intent table plus the message dispatch-state CAS helpers. Exactly-once
// execution across nodes is enforced by conditional (compare-and-swap) updates
// on the state column — no SELECT ... FOR UPDATE, correct on SQLite + Postgres.
type BrokerDispatchStore struct {
	client *ent.Client
}

// NewBrokerDispatchStore creates a new Ent-backed BrokerDispatchStore.
func NewBrokerDispatchStore(client *ent.Client) *BrokerDispatchStore {
	return &BrokerDispatchStore{client: client}
}

func entBrokerDispatchToStore(e *ent.BrokerDispatch) store.BrokerDispatch {
	d := store.BrokerDispatch{
		ID:        e.ID.String(),
		BrokerID:  e.BrokerID.String(),
		AgentSlug: e.AgentSlug,
		Op:        e.Op,
		Args:      e.Args,
		State:     e.State,
		Result:    e.Result,
		ClaimedBy: e.ClaimedBy,
		Attempts:  e.Attempts,
		Error:     e.Error,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
	}
	if e.AgentID != nil {
		d.AgentID = e.AgentID.String()
	}
	if e.ProjectID != nil {
		d.ProjectID = e.ProjectID.String()
	}
	if e.DeadlineAt != nil {
		d.DeadlineAt = e.DeadlineAt
	}
	return d
}

// InsertBrokerDispatch persists a new durable dispatch intent. State defaults to
// pending. The generated id and timestamps are written back into d.
func (s *BrokerDispatchStore) InsertBrokerDispatch(ctx context.Context, d *store.BrokerDispatch) error {
	if d.BrokerID == "" || d.Op == "" {
		return store.ErrInvalidInput
	}
	brokerUID, err := parseUUID(d.BrokerID)
	if err != nil {
		return err
	}

	create := s.client.BrokerDispatch.Create().
		SetBrokerID(brokerUID).
		SetOp(d.Op)

	if d.ID != "" {
		uid, err := parseUUID(d.ID)
		if err != nil {
			return err
		}
		create.SetID(uid)
	}
	if d.AgentID != "" {
		agentUID, err := parseUUID(d.AgentID)
		if err != nil {
			return err
		}
		create.SetAgentID(agentUID)
	}
	if d.AgentSlug != "" {
		create.SetAgentSlug(d.AgentSlug)
	}
	if d.ProjectID != "" {
		projUID, err := parseUUID(d.ProjectID)
		if err != nil {
			return err
		}
		create.SetProjectID(projUID)
	}
	if d.Args != "" {
		create.SetArgs(d.Args)
	}
	if d.State != "" {
		create.SetState(d.State)
	}
	if d.DeadlineAt != nil {
		create.SetDeadlineAt(*d.DeadlineAt)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	d.ID = created.ID.String()
	d.State = created.State
	d.CreatedAt = created.CreatedAt
	d.UpdatedAt = created.UpdatedAt
	return nil
}

// ClaimBrokerDispatch atomically transitions a dispatch from pending to
// in_progress, recording the claiming hub instance. It is a CAS keyed on
// state='pending', so exactly one node wins for a given row (design §7). Returns
// claimed=false if the row was not pending (already claimed/done/failed/absent).
func (s *BrokerDispatchStore) ClaimBrokerDispatch(ctx context.Context, id, hubInstanceID string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	affected, err := s.client.BrokerDispatch.Update().
		Where(brokerdispatch.IDEQ(uid), brokerdispatch.StateEQ(store.DispatchStatePending)).
		SetState(store.DispatchStateInProgress).
		SetClaimedBy(hubInstanceID).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected == 1, nil
}

// CompleteBrokerDispatch marks a dispatch done and records its result JSON.
// The update is guarded by state=in_progress (CAS) so a done or failed
// dispatch cannot be flipped by a stale or duplicate completion call.
func (s *BrokerDispatchStore) CompleteBrokerDispatch(ctx context.Context, id, result string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	upd := s.client.BrokerDispatch.Update().
		Where(brokerdispatch.IDEQ(uid), brokerdispatch.StateEQ(store.DispatchStateInProgress)).
		SetState(store.DispatchStateDone).
		SetUpdatedAt(time.Now())
	if result != "" {
		upd.SetResult(result)
	}
	affected, err := upd.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// FailBrokerDispatch marks a dispatch failed, records the error, and bumps the
// attempt counter (so a reaper/retry can bound re-drives). The update is
// guarded by state=in_progress (CAS) so a completed or already-failed dispatch
// cannot be overwritten by a stale failure call.
func (s *BrokerDispatchStore) FailBrokerDispatch(ctx context.Context, id, errMsg string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	affected, err := s.client.BrokerDispatch.Update().
		Where(brokerdispatch.IDEQ(uid), brokerdispatch.StateEQ(store.DispatchStateInProgress)).
		SetState(store.DispatchStateFailed).
		SetError(errMsg).
		AddAttempts(1).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetBrokerDispatch returns a single dispatch row by ID. Used by the originator
// to read the result/state after the owner completes the dispatch.
func (s *BrokerDispatchStore) GetBrokerDispatch(ctx context.Context, id string) (*store.BrokerDispatch, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.client.BrokerDispatch.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	d := entBrokerDispatchToStore(row)
	return &d, nil
}

// ListPendingDispatch returns the pending dispatch intents for a broker, oldest
// first — the reconcile-drain query (design §5.3).
func (s *BrokerDispatchStore) ListPendingDispatch(ctx context.Context, brokerID string) ([]store.BrokerDispatch, error) {
	brokerUID, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.BrokerDispatch.Query().
		Where(brokerdispatch.BrokerIDEQ(brokerUID), brokerdispatch.StateEQ(store.DispatchStatePending)).
		Order(ent.Asc(brokerdispatch.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.BrokerDispatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, entBrokerDispatchToStore(r))
	}
	return out, nil
}

// MarkMessageDispatched CAS-flips a message from dispatch_state=pending to
// dispatched and stamps dispatched_at. Returns dispatched=false if the row was
// not pending (already dispatched/failed/absent) — dedupes concurrent drains.
func (s *BrokerDispatchStore) MarkMessageDispatched(ctx context.Context, id string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	affected, err := s.client.Message.Update().
		Where(message.IDEQ(uid), message.DispatchStateEQ(store.MessageDispatchPending)).
		SetDispatchState(store.MessageDispatchDispatched).
		SetDispatchedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected == 1, nil
}

// MarkMessageFailed sets a message's dispatch_state to "failed" and records the reason.
func (s *BrokerDispatchStore) MarkMessageFailed(ctx context.Context, id string, reason string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = s.client.Message.Update().
		Where(message.IDEQ(uid), message.DispatchStateNEQ(store.MessageDispatchFailed)).
		SetDispatchState(store.MessageDispatchFailed).
		SetNillableDispatchFailureReason(&reason).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// CountStuckPendingMessages returns the number of messages still in
// dispatch_state='pending' whose created timestamp is before the given cutoff.
func (s *BrokerDispatchStore) CountStuckPendingMessages(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.Message.Query().
		Where(message.DispatchStateEQ(store.MessageDispatchPending), message.CreatedLT(before)).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// ExpireStuckPendingMessages transitions messages stuck in pending state past
// the given cutoff to failed, recording the reason. Returns the number expired.
func (s *BrokerDispatchStore) ExpireStuckPendingMessages(ctx context.Context, before time.Time, reason string) (int, error) {
	affected, err := s.client.Message.Update().
		Where(message.DispatchStateEQ(store.MessageDispatchPending), message.CreatedLT(before)).
		SetDispatchState(store.MessageDispatchFailed).
		SetNillableDispatchFailureReason(&reason).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// ListPendingMessages returns messages still pending delivery whose target agent
// lives on the given broker (messages have no broker_id; the association is via
// the recipient agent's runtime_broker_id).
func (s *BrokerDispatchStore) ListPendingMessages(ctx context.Context, brokerID string) ([]store.Message, error) {
	agents, err := s.client.Agent.Query().
		Where(agent.RuntimeBrokerIDEQ(brokerID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(agents) == 0 {
		return nil, nil
	}
	agentIDs := make([]string, 0, len(agents))
	for _, a := range agents {
		agentIDs = append(agentIDs, a.ID.String())
	}
	rows, err := s.client.Message.Query().
		Where(message.AgentIDIn(agentIDs...), message.DispatchStateEQ(store.MessageDispatchPending)).
		Order(ent.Asc(message.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entMessageToStore(r))
	}
	return out, nil
}

// ReapStuckDispatch re-drives or fails in_progress dispatches that have gone
// stale. Dispatches with attempts < maxAttempts are reset to pending; those at
// or above the limit are marked failed.
func (s *BrokerDispatchStore) ReapStuckDispatch(ctx context.Context, stuckBefore time.Time, maxAttempts int) (requeued, failed int, err error) {
	now := time.Now()

	stuckPred := brokerdispatch.And(
		brokerdispatch.StateEQ(store.DispatchStateInProgress),
		brokerdispatch.Or(
			brokerdispatch.UpdatedAtLT(stuckBefore),
			brokerdispatch.And(
				brokerdispatch.DeadlineAtNotNil(),
				brokerdispatch.DeadlineAtLT(now),
			),
		),
	)

	requeued, err = s.client.BrokerDispatch.Update().
		Where(stuckPred, brokerdispatch.AttemptsLT(maxAttempts)).
		SetState(store.DispatchStatePending).
		ClearClaimedBy().
		AddAttempts(1).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return 0, 0, mapError(err)
	}

	failed, err = s.client.BrokerDispatch.Update().
		Where(stuckPred, brokerdispatch.AttemptsGTE(maxAttempts)).
		SetState(store.DispatchStateFailed).
		SetError("reaper: max attempts exceeded").
		AddAttempts(1).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return requeued, 0, mapError(err)
	}

	return requeued, failed, nil
}
