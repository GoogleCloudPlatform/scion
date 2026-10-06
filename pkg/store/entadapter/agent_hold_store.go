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

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agenthold"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentHoldInsertBatch bounds the rows sent in one INSERT.
const agentHoldInsertBatch = 500

// maxAgentHoldClearReasonLen bounds the stored clear_reason text, like
// last_error on membership loss checks.
const maxAgentHoldClearReasonLen = maxMembershipLossCheckErrorLen

// Page bounds of ListActiveAgentHoldsByProject.
const (
	defaultAgentHoldListLimit = 100
	maxAgentHoldListLimit     = 1000
)

// AgentHoldStore implements store.AgentHoldStore using Ent ORM.
type AgentHoldStore struct {
	client *ent.Client
}

// NewAgentHoldStore creates a new Ent-backed AgentHoldStore.
func NewAgentHoldStore(client *ent.Client) *AgentHoldStore {
	return &AgentHoldStore{client: client}
}

var _ store.AgentHoldStore = (*AgentHoldStore)(nil)

// entAgentHoldToStore converts an Ent AgentHold entity to a store model.
func entAgentHoldToStore(h *ent.AgentHold) *store.AgentHold {
	out := &store.AgentHold{
		ID:                h.ID.String(),
		AgentID:           h.AgentID.String(),
		ProjectID:         h.ProjectID.String(),
		Cause:             store.AgentHoldCause(h.Cause),
		RootPrincipalType: h.RootPrincipalType,
		RootPrincipalID:   h.RootPrincipalID,
		Trigger:           store.MembershipLossTrigger(h.Trigger),
		ActorKind:         h.ActorKind,
		ActorID:           h.ActorID,
		CorrelationID:     h.CorrelationID,
		CreatedAt:         h.CreatedAt,
		ClearedAt:         h.ClearedAt,
		ClearedByKind:     h.ClearedByKind,
		ClearedByID:       h.ClearedByID,
		ClearReason:       h.ClearReason,
	}
	if h.ViaAgentID != nil {
		out.ViaAgentID = h.ViaAgentID.String()
	}
	return out
}

// agentHoldCreate validates h and builds its insert. It fills in an empty ID
// and a zero CreatedAt on h, and stores the root principal ID in canonical
// form.
func (s *AgentHoldStore) agentHoldCreate(h *store.AgentHold) (*ent.AgentHoldCreate, uuid.UUID, error) {
	if h == nil {
		return nil, uuid.Nil, fmt.Errorf("%w: nil agent hold", store.ErrInvalidInput)
	}
	if !store.ValidAgentHoldCause(h.Cause) {
		return nil, uuid.Nil, fmt.Errorf("%w: unknown agent hold cause %q", store.ErrInvalidInput, h.Cause)
	}
	if !store.ValidMembershipLossTrigger(h.Trigger) {
		return nil, uuid.Nil, fmt.Errorf("%w: unknown agent hold trigger %q", store.ErrInvalidInput, h.Trigger)
	}
	if h.RootPrincipalType != store.AgentHoldRootUser {
		return nil, uuid.Nil, fmt.Errorf("%w: agent hold root principal type must be %q", store.ErrInvalidInput, store.AgentHoldRootUser)
	}
	rootID, err := parseUUID(h.RootPrincipalID)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("%w: agent hold requires a root principal UUID", store.ErrInvalidInput)
	}
	if h.ClearedAt != nil || h.ClearedByKind != "" || h.ClearedByID != "" || h.ClearReason != "" {
		return nil, uuid.Nil, fmt.Errorf("%w: a new agent hold cannot be cleared", store.ErrInvalidInput)
	}
	agentID, err := parseUUID(h.AgentID)
	if err != nil {
		return nil, uuid.Nil, err
	}
	projectID, err := parseUUID(h.ProjectID)
	if err != nil {
		return nil, uuid.Nil, err
	}
	id := uuid.New()
	if h.ID != "" {
		if id, err = parseUUID(h.ID); err != nil {
			return nil, uuid.Nil, err
		}
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}
	b := s.client.AgentHold.Create().
		SetID(id).
		SetAgentID(agentID).
		SetProjectID(projectID).
		SetCause(agenthold.Cause(h.Cause)).
		SetRootPrincipalType(h.RootPrincipalType).
		SetRootPrincipalID(rootID.String()).
		SetTrigger(agenthold.Trigger(h.Trigger)).
		SetActorKind(h.ActorKind).
		SetActorID(h.ActorID).
		SetCorrelationID(h.CorrelationID).
		SetCreatedAt(h.CreatedAt)
	if h.ViaAgentID != "" {
		via, err := parseUUID(h.ViaAgentID)
		if err != nil {
			return nil, uuid.Nil, err
		}
		b.SetViaAgentID(via)
	}
	return b, id, nil
}

// CreateAgentHolds inserts holds with INSERT ... ON CONFLICT (agent_id,
// root_principal_id) WHERE cleared_at IS NULL DO NOTHING, so a hold whose
// (agent, root principal) already has an active row is skipped, including
// one inserted concurrently by another hub instance. The rows this call
// inserted are then counted by the IDs it assigned.
func (s *AgentHoldStore) CreateAgentHolds(ctx context.Context, holds []*store.AgentHold) (int, error) {
	builders := make([]*ent.AgentHoldCreate, 0, len(holds))
	ids := make([]uuid.UUID, 0, len(holds))
	for _, h := range holds {
		b, id, err := s.agentHoldCreate(h)
		if err != nil {
			return 0, err
		}
		h.ID = id.String()
		builders = append(builders, b)
		ids = append(ids, id)
	}
	inserted := 0
	for start := 0; start < len(builders); start += agentHoldInsertBatch {
		end := min(start+agentHoldInsertBatch, len(builders))
		// Exec, never Save: when DO NOTHING skips rows, RETURNING yields
		// fewer IDs than builders (see SeedMaintenanceOperations).
		err := s.client.AgentHold.CreateBulk(builders[start:end]...).
			OnConflict(
				entsql.ConflictColumns(agenthold.FieldAgentID, agenthold.FieldRootPrincipalID),
				entsql.ConflictWhere(entsql.IsNull(agenthold.FieldClearedAt)),
			).
			DoNothing().
			Exec(ctx)
		if err != nil {
			return inserted, mapError(err)
		}
		n, err := s.client.AgentHold.Query().
			Where(agenthold.IDIn(ids[start:end]...)).
			Count(ctx)
		if err != nil {
			return inserted, mapError(err)
		}
		inserted += n
	}
	return inserted, nil
}

// HasActiveAgentHold reports whether the agent has an active hold.
func (s *AgentHoldStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	ok, err := s.client.AgentHold.Query().
		Where(agenthold.AgentIDEQ(uid), agenthold.ClearedAtIsNil()).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}

// ListActiveAgentHolds returns the agent's active holds, oldest first.
func (s *AgentHoldStore) ListActiveAgentHolds(ctx context.Context, agentID string) ([]*store.AgentHold, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.AgentHold.Query().
		Where(agenthold.AgentIDEQ(uid), agenthold.ClearedAtIsNil()).
		Order(ent.Asc(agenthold.FieldCreatedAt), ent.Asc(agenthold.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.AgentHold, len(rows))
	for i, r := range rows {
		out[i] = entAgentHoldToStore(r)
	}
	return out, nil
}

// ListActiveAgentHoldsByProject returns one page of the project's active
// holds ordered by ID; the cursor is the last ID of the previous page.
func (s *AgentHoldStore) ListActiveAgentHoldsByProject(ctx context.Context, projectID string, opts store.ListOptions) (*store.ListResult[store.AgentHold], error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultAgentHoldListLimit
	}
	if limit > maxAgentHoldListLimit {
		limit = maxAgentHoldListLimit
	}
	q := s.client.AgentHold.Query().
		Where(agenthold.ProjectIDEQ(pid), agenthold.ClearedAtIsNil())
	if opts.Cursor != "" {
		after, err := uuid.Parse(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cursor", store.ErrInvalidInput)
		}
		q = q.Where(agenthold.IDGT(after))
	}
	rows, err := q.Order(ent.Asc(agenthold.FieldID)).Limit(limit + 1).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	res := &store.ListResult[store.AgentHold]{}
	if len(rows) > limit {
		rows = rows[:limit]
		res.NextCursor = rows[limit-1].ID.String()
	}
	res.Items = make([]store.AgentHold, len(rows))
	for i, r := range rows {
		res.Items[i] = *entAgentHoldToStore(r)
	}
	return res, nil
}

// ClearAgentHolds clears the agent's active holds. Only a user principal may
// clear holds; any other actor is refused here, independently of the
// caller's own checks.
func (s *AgentHoldStore) ClearAgentHolds(ctx context.Context, agentID string, by store.ClearActor, reason string) (int, error) {
	if by.Kind != store.ClearActorUser {
		return 0, store.ErrInvalidActor
	}
	actorID, err := uuid.Parse(by.ID)
	if err != nil {
		return 0, store.ErrInvalidActor
	}
	uid, err := parseUUID(agentID)
	if err != nil {
		return 0, err
	}
	reason = truncateUTF8(reason, maxAgentHoldClearReasonLen)
	n, err := s.client.AgentHold.Update().
		Where(agenthold.AgentIDEQ(uid), agenthold.ClearedAtIsNil()).
		SetClearedAt(time.Now()).
		SetClearedByKind(by.Kind).
		SetClearedByID(actorID.String()).
		SetClearReason(reason).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
