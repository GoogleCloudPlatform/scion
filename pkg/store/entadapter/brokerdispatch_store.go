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
	"errors"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerdispatch"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
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
	if e.InitiatorPrincipalKind != nil {
		d.InitiatorPrincipalKind = *e.InitiatorPrincipalKind
	}
	if e.InitiatorPrincipalID != nil {
		d.InitiatorPrincipalID = *e.InitiatorPrincipalID
	}
	if e.InitiatorCredentialKind != nil {
		d.InitiatorCredentialKind = *e.InitiatorCredentialKind
	}
	if e.InitiatorCredentialID != nil {
		d.InitiatorCredentialID = *e.InitiatorCredentialID
	}
	if e.CorrelationID != nil {
		d.CorrelationID = *e.CorrelationID
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
	if d.InitiatorPrincipalKind != "" {
		create.SetInitiatorPrincipalKind(d.InitiatorPrincipalKind)
	}
	if d.InitiatorPrincipalID != "" {
		create.SetInitiatorPrincipalID(d.InitiatorPrincipalID)
	}
	if d.InitiatorCredentialKind != "" {
		create.SetInitiatorCredentialKind(d.InitiatorCredentialKind)
	}
	if d.InitiatorCredentialID != "" {
		create.SetInitiatorCredentialID(d.InitiatorCredentialID)
	}
	if d.CorrelationID != "" {
		create.SetCorrelationID(d.CorrelationID)
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
// attempt counter (so a reaper/retry can bound re-drives). A non-empty result
// is written in the same update, so a reader that sees the failed state also
// sees its result. The update is guarded by state=in_progress (CAS) so a
// completed or already-failed dispatch cannot be overwritten by a stale
// failure call.
func (s *BrokerDispatchStore) FailBrokerDispatch(ctx context.Context, id, errMsg, result string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	upd := s.client.BrokerDispatch.Update().
		Where(brokerdispatch.IDEQ(uid), brokerdispatch.StateEQ(store.DispatchStateInProgress)).
		SetState(store.DispatchStateFailed).
		SetError(errMsg).
		AddAttempts(1).
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

// CountBrokerDispatchHealth returns the number of in_progress dispatches whose
// updated_at is before stuckBefore, and the number of failed dispatches whose
// updated_at is at or after failedSince. Each is a COUNT over the
// (state, updated_at) index: an equality on state plus a range on updated_at.
func (s *BrokerDispatchStore) CountBrokerDispatchHealth(ctx context.Context, stuckBefore, failedSince time.Time) (stuck, failed int, err error) {
	stuck, err = s.client.BrokerDispatch.Query().
		Where(
			brokerdispatch.StateEQ(store.DispatchStateInProgress),
			brokerdispatch.UpdatedAtLT(stuckBefore),
		).
		Count(ctx)
	if err != nil {
		return 0, 0, mapError(err)
	}
	failed, err = s.client.BrokerDispatch.Query().
		Where(
			brokerdispatch.StateEQ(store.DispatchStateFailed),
			brokerdispatch.UpdatedAtGTE(failedSince),
		).
		Count(ctx)
	if err != nil {
		return 0, 0, mapError(err)
	}
	return stuck, failed, nil
}

// stuckPendingMessage returns the predicates every stuck-pending sweep query
// shares: dispatch_state 'pending', created before the cutoff, and an agent
// recipient. dispatch_state is compared with the literal 'pending' rather
// than a bound parameter so the predicate textually implies the WHERE of the
// partial index on (created, id) WHERE dispatch_state = 'pending'. SQLite
// and a Postgres generic plan can only use a partial index when the query
// proves its WHERE without knowing parameter values.
func stuckPendingMessage(before time.Time) []predicate.Message {
	return []predicate.Message{
		messageDispatchPendingLiteral(),
		message.CreatedLT(before),
		message.RecipientHasPrefix("agent:"),
	}
}

// messageDispatchPendingLiteral matches dispatch_state = 'pending' with the
// value inlined as a literal, so the query can use the partial pending index
// (see stuckPendingMessage).
func messageDispatchPendingLiteral() predicate.Message {
	return func(s *entsql.Selector) {
		s.Where(entsql.ExprP(s.C(message.FieldDispatchState) + " = '" + store.MessageDispatchPending + "'"))
	}
}

// CountStuckPendingMessages returns the number of messages still in
// dispatch_state='pending' whose created timestamp is before the given
// cutoff. Scoped to agent recipients only (message.RecipientHasPrefix
// "agent:") — only a message addressed to an agent is actually dispatched
// through the broker/runtime, so only that row can be genuinely "stuck".
// A "user:" recipient row reaching this state is always a bug in the writer
// (e.g. nc-promote-busy), not a stalled dispatch, and must not be counted or
// expired here.
func (s *BrokerDispatchStore) CountStuckPendingMessages(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.Message.Query().
		Where(stuckPendingMessage(before)...).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// ListStuckPendingMessageIDs returns the IDs of up to limit messages matched
// by CountStuckPendingMessages, oldest first. A non-positive limit returns
// no IDs without querying.
func (s *BrokerDispatchStore) ListStuckPendingMessageIDs(ctx context.Context, before time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	ids, err := s.client.Message.Query().
		Where(stuckPendingMessage(before)...).
		Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
		Limit(limit).
		IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out, nil
}

// ExpireStuckPendingMessages transitions messages stuck in pending state past
// the given cutoff to failed, recording the reason. Returns the number
// expired. Scoped to agent recipients only — see CountStuckPendingMessages.
func (s *BrokerDispatchStore) ExpireStuckPendingMessages(ctx context.Context, before time.Time, reason string) (int, error) {
	affected, err := s.client.Message.Update().
		Where(stuckPendingMessage(before)...).
		SetDispatchState(store.MessageDispatchFailed).
		SetNillableDispatchFailureReason(&reason).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// FailPendingMessagesWithMissingRecipient transitions pending messages to
// failed early when their recipient agent has been deleted (soft- or
// hard-deleted), instead of waiting for ExpireStuckPendingMessages' TTL.
// Non-agent recipients (e.g. "user:...") and messages with no recipient_id
// are left untouched — a lookup miss there does not mean the recipient is
// gone. Returns the number of messages transitioned.
func (s *BrokerDispatchStore) FailPendingMessagesWithMissingRecipient(ctx context.Context, reason string) (int, error) {
	// The literal lets this read scan the partial pending index instead of
	// the whole table.
	pending, err := s.client.Message.Query().
		Where(
			messageDispatchPendingLiteral(),
			message.RecipientHasPrefix("agent:"),
			message.RecipientIDNEQ(""),
		).
		Select(message.FieldID, message.FieldRecipientID).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	recipientIDSet := make(map[string]struct{}, len(pending))
	for _, m := range pending {
		recipientIDSet[m.RecipientID] = struct{}{}
	}
	recipientIDs := make([]string, 0, len(recipientIDSet))
	for id := range recipientIDSet {
		recipientIDs = append(recipientIDs, id)
	}

	existingAgents, err := s.client.Agent.Query().
		Where(agent.IDIn(parseUUIDList(recipientIDs)...), agent.DeletedAtIsNil()).
		Select(agent.FieldID).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	existingIDs := make(map[string]struct{}, len(existingAgents))
	for _, a := range existingAgents {
		existingIDs[a.ID.String()] = struct{}{}
	}

	orphanedIDs := make([]uuid.UUID, 0)
	for _, m := range pending {
		if _, ok := existingIDs[m.RecipientID]; ok {
			continue
		}
		orphanedIDs = append(orphanedIDs, m.ID)
	}
	if len(orphanedIDs) == 0 {
		return 0, nil
	}

	affected, err := s.client.Message.Update().
		Where(message.IDIn(orphanedIDs...), message.DispatchStateEQ(store.MessageDispatchPending)).
		SetDispatchState(store.MessageDispatchFailed).
		SetNillableDispatchFailureReason(&reason).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// expirePageSize bounds how many rows one ExpireAndListStuckPendingMessages
// call expires, so a large backlog cannot outlast the sweep's context and
// roll back every sweep; later sweeps drain the rest. A var (not const) so
// tests can shrink it.
var expirePageSize = 500

// ExpireAndListStuckPendingMessages transitions up to expirePageSize messages
// stuck in pending state past the given cutoff to failed, recording the
// reason, and returns the rows it transitioned, oldest first. It matches the
// same rows as ExpireStuckPendingMessages (agent recipients only). Each row
// is flipped by its own compare-and-swap on dispatch_state inside one
// transaction, so a row that a concurrent writer dispatched after the select
// is neither expired nor returned. Inside WithTx it runs in the caller's
// transaction.
func (s *BrokerDispatchStore) ExpireAndListStuckPendingMessages(ctx context.Context, before time.Time, reason string) ([]store.ExpiredPendingMessage, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		if !errors.Is(err, ent.ErrTxStarted) {
			return nil, mapError(err)
		}
		// s.client already wraps the caller's transaction (store.WithTx);
		// the caller owns commit and rollback.
		return expireAndListStuckPending(ctx, s.client, before, reason)
	}
	defer func() { _ = tx.Rollback() }()
	expired, err := expireAndListStuckPending(ctx, tx.Client(), before, reason)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapError(err)
	}
	return expired, nil
}

// expireAndListStuckPending runs ExpireAndListStuckPendingMessages on c,
// which must be a transactional client.
func expireAndListStuckPending(ctx context.Context, c *ent.Client, before time.Time, reason string) ([]store.ExpiredPendingMessage, error) {
	candidates, err := c.Message.Query().
		Where(stuckPendingMessage(before)...).
		Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
		Limit(expirePageSize).
		Select(message.FieldID, message.FieldRecipient, message.FieldCreated).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	expired := make([]store.ExpiredPendingMessage, 0, len(candidates))
	for _, m := range candidates {
		n, err := c.Message.Update().
			Where(message.IDEQ(m.ID), message.DispatchStateEQ(store.MessageDispatchPending)).
			SetDispatchState(store.MessageDispatchFailed).
			SetNillableDispatchFailureReason(&reason).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n == 0 {
			continue
		}
		expired = append(expired, store.ExpiredPendingMessage{
			ID:        m.ID.String(),
			Recipient: m.Recipient,
			CreatedAt: m.Created,
		})
	}
	return expired, nil
}

// backfillPageSize bounds how many rows BackfillNonAgentDispatchState repairs
// per transaction, so a large legacy population commits in chunks instead of
// one fsync per row. A var (not const) so tests can shrink it to exercise
// multi-page pagination without seeding hundreds of rows.
var backfillPageSize = 500

// BackfillNonAgentDispatchState repairs non-agent-recipient rows left
// "pending", or "failed" with dispatch_failure_reason == expiredReason, by
// the pre-fix nc-promote-busy bug (any writer that built a storeMsg without
// stamping DispatchState). The exact complement of the R1 agent-only
// allow-list: only an agent-addressed row is ever legitimately pending or
// genuinely failed (see ExpireStuckPendingMessages/PurgeFailedMessages), so
// any other row in either state is this writer bug. Sets dispatch_state
// "dispatched", clears the failure reason, and backdates dispatched_at to
// the row's own created time when unset. Returns the number of rows
// repaired; see nc-promote-busy's investigation note for the full rationale.
func (s *BrokerDispatchStore) BackfillNonAgentDispatchState(ctx context.Context, expiredReason string) (int, error) {
	eligible := message.And(
		message.Not(message.RecipientHasPrefix("agent:")),
		message.Or(
			message.DispatchStateEQ(store.MessageDispatchPending),
			message.And(
				message.DispatchStateEQ(store.MessageDispatchFailed),
				message.DispatchFailureReasonEQ(expiredReason),
			),
		),
	)

	total := 0
	for {
		repaired, err := s.backfillNonAgentDispatchStatePage(ctx, eligible)
		if err != nil {
			return total, err
		}
		total += repaired
		if repaired < backfillPageSize {
			return total, nil
		}
	}
}

// backfillNonAgentDispatchStatePage repairs up to backfillPageSize eligible
// rows in a single transaction. Repaired rows drop out of eligible (their
// dispatch_state is no longer pending/failed-with-reason), so each call
// naturally fetches the next page — no offset or cursor bookkeeping needed.
func (s *BrokerDispatchStore) backfillNonAgentDispatchStatePage(ctx context.Context, eligible predicate.Message) (int, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	page, err := tx.Message.Query().
		Where(eligible).
		Limit(backfillPageSize).
		Select(message.FieldID, message.FieldCreated, message.FieldDispatchedAt).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if len(page) == 0 {
		return 0, nil
	}

	ids := make([]uuid.UUID, len(page))
	for i, m := range page {
		ids[i] = m.ID
	}
	if _, err := tx.Message.Update().
		Where(message.IDIn(ids...)).
		SetDispatchState(store.MessageDispatchDispatched).
		ClearDispatchFailureReason().
		Save(ctx); err != nil {
		return 0, mapError(err)
	}

	// dispatched_at is per-row (backdated to each row's own created time),
	// so it cannot join the bulk update above; only touch rows where it is
	// still unset.
	for _, m := range page {
		if m.DispatchedAt != nil {
			continue
		}
		if err := tx.Message.UpdateOneID(m.ID).SetDispatchedAt(m.Created).Exec(ctx); err != nil {
			return 0, mapError(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, mapError(err)
	}
	return len(page), nil
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
