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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agenthold"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// defaultAgentHoldInsertBatch bounds the rows sent in one INSERT.
const defaultAgentHoldInsertBatch = 500

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
	inTx   bool // true when client wraps an ambient WithTx transaction

	// insertBatch overrides defaultAgentHoldInsertBatch when non-zero.
	// Tests set it per instance to run a call over several batches
	// without creating hundreds of rows.
	insertBatch int
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

// agentHoldRow is one validated hold insert with the parsed IDs it names.
type agentHoldRow struct {
	create    *ent.AgentHoldCreate
	id        uuid.UUID
	agentID   uuid.UUID
	projectID uuid.UUID
}

// agentHoldCreate validates h and builds its insert on c under a fresh ID,
// with created_at set to now. Any ID or CreatedAt already set on h is
// ignored, and h is not changed. The root principal ID is stored in
// canonical form.
func agentHoldCreate(c *ent.Client, h *store.AgentHold, now time.Time) (agentHoldRow, error) {
	if h == nil {
		return agentHoldRow{}, fmt.Errorf("%w: nil agent hold", store.ErrInvalidInput)
	}
	if !store.ValidAgentHoldCause(h.Cause) {
		return agentHoldRow{}, fmt.Errorf("%w: unknown agent hold cause %q", store.ErrInvalidInput, h.Cause)
	}
	if !store.ValidMembershipLossTrigger(h.Trigger) {
		return agentHoldRow{}, fmt.Errorf("%w: unknown agent hold trigger %q", store.ErrInvalidInput, h.Trigger)
	}
	if h.RootPrincipalType != store.AgentHoldRootUser {
		return agentHoldRow{}, fmt.Errorf("%w: agent hold root principal type must be %q", store.ErrInvalidInput, store.AgentHoldRootUser)
	}
	rootID, err := parseUUID(h.RootPrincipalID)
	if err != nil {
		return agentHoldRow{}, fmt.Errorf("%w: agent hold requires a root principal UUID", store.ErrInvalidInput)
	}
	if h.ClearedAt != nil || h.ClearedByKind != "" || h.ClearedByID != "" || h.ClearReason != "" {
		return agentHoldRow{}, fmt.Errorf("%w: a new agent hold cannot be cleared", store.ErrInvalidInput)
	}
	agentID, err := parseUUID(h.AgentID)
	if err != nil {
		return agentHoldRow{}, err
	}
	projectID, err := parseUUID(h.ProjectID)
	if err != nil {
		return agentHoldRow{}, err
	}
	// ID and CreatedAt are output-only: every call assigns a fresh ID, so
	// the IDs it counts name only rows this call inserted, and every row
	// it inserts carries the call's own creation time.
	id := uuid.New()
	b := c.AgentHold.Create().
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
		SetCreatedAt(now)
	if h.ViaAgentID != "" {
		via, err := parseUUID(h.ViaAgentID)
		if err != nil {
			return agentHoldRow{}, err
		}
		b.SetViaAgentID(via)
	}
	return agentHoldRow{create: b, id: id, agentID: agentID, projectID: projectID}, nil
}

// CreateAgentHolds inserts holds with INSERT ... ON CONFLICT (agent_id,
// root_principal_id) WHERE cleared_at IS NULL DO NOTHING, so a hold whose
// (agent, root principal) already has an active row is skipped, including
// one inserted concurrently by another hub instance. Every hold gets a fresh
// ID and the call's creation time on every call, and the rows this call
// inserted are counted by those IDs. Before any insert, each hold's agent row
// must exist with project_id equal to the hold's ProjectID. The call runs in
// one transaction (the ambient one when called inside WithTx), so it inserts
// all of its rows or none.
func (s *AgentHoldStore) CreateAgentHolds(ctx context.Context, holds []*store.AgentHold) (int, error) {
	if s.inTx {
		return s.createAgentHolds(ctx, s.client, holds)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	inserted, err := s.createAgentHolds(ctx, tx.Client(), holds)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, mapError(err)
	}
	return inserted, nil
}

// createAgentHolds runs CreateAgentHolds on c, which must be a transactional
// client. It returns 0 with any error.
func (s *AgentHoldStore) createAgentHolds(ctx context.Context, c *ent.Client, holds []*store.AgentHold) (int, error) {
	batchSize := s.insertBatch
	if batchSize <= 0 {
		batchSize = defaultAgentHoldInsertBatch
	}
	now := time.Now()
	rows := make([]agentHoldRow, 0, len(holds))
	for _, h := range holds {
		r, err := agentHoldCreate(c, h, now)
		if err != nil {
			return 0, err
		}
		rows = append(rows, r)
	}
	for start := 0; start < len(rows); start += batchSize {
		if err := checkAgentHoldProjects(ctx, c, rows[start:min(start+batchSize, len(rows))]); err != nil {
			return 0, err
		}
	}
	for i, h := range holds {
		h.ID = rows[i].id.String()
		h.CreatedAt = now
	}
	inserted := 0
	for start := 0; start < len(rows); start += batchSize {
		batch := rows[start:min(start+batchSize, len(rows))]
		builders := make([]*ent.AgentHoldCreate, len(batch))
		ids := make([]uuid.UUID, len(batch))
		for i, r := range batch {
			builders[i] = r.create
			ids[i] = r.id
		}
		// Exec, never Save: when DO NOTHING skips rows, RETURNING yields
		// fewer IDs than builders (see SeedMaintenanceOperations).
		err := c.AgentHold.CreateBulk(builders...).
			OnConflict(
				entsql.ConflictColumns(agenthold.FieldAgentID, agenthold.FieldRootPrincipalID),
				entsql.ConflictWhere(entsql.IsNull(agenthold.FieldClearedAt)),
			).
			DoNothing().
			Exec(ctx)
		if err != nil {
			return 0, mapError(err)
		}
		n, err := c.AgentHold.Query().
			Where(agenthold.IDIn(ids...)).
			Count(ctx)
		if err != nil {
			return 0, mapError(err)
		}
		inserted += n
	}
	return inserted, nil
}

// checkAgentHoldProjects loads the agent rows of batch on c with one IN
// query and returns ErrInvalidInput unless every hold's agent row exists and its
// project_id equals the hold's project ID.
func checkAgentHoldProjects(ctx context.Context, c *ent.Client, batch []agentHoldRow) error {
	want := make(map[uuid.UUID]bool, len(batch))
	ids := make([]uuid.UUID, 0, len(batch))
	for _, r := range batch {
		if !want[r.agentID] {
			want[r.agentID] = true
			ids = append(ids, r.agentID)
		}
	}
	agents, err := c.Agent.Query().
		Where(agent.IDIn(ids...)).
		Select(agent.FieldID, agent.FieldProjectID).
		All(ctx)
	if err != nil {
		return mapError(err)
	}
	projectOf := make(map[uuid.UUID]uuid.UUID, len(agents))
	for _, a := range agents {
		projectOf[a.ID] = a.ProjectID
	}
	for _, r := range batch {
		p, ok := projectOf[r.agentID]
		if !ok {
			return fmt.Errorf("%w: agent hold names agent %s, which has no row", store.ErrInvalidInput, r.agentID)
		}
		if p != r.projectID {
			return fmt.Errorf("%w: agent hold project %s is not the project of agent %s", store.ErrInvalidInput, r.projectID, r.agentID)
		}
	}
	return nil
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
