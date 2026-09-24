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
	"encoding/json"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentreincarnation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentReincarnationStore implements store.AgentReincarnationStore using Ent ORM.
type AgentReincarnationStore struct {
	client *ent.Client
}

// NewAgentReincarnationStore creates a new Ent-backed AgentReincarnationStore.
func NewAgentReincarnationStore(client *ent.Client) *AgentReincarnationStore {
	return &AgentReincarnationStore{client: client}
}

// entAgentReincarnationToStore converts an Ent AgentReincarnation entity to a
// store model. Malformed applied-config JSON is logged and dropped rather
// than failing the read, matching entAgentToStore's precedent: a corrupt
// snapshot on one history row must not break the whole list.
func entAgentReincarnationToStore(r *ent.AgentReincarnation) *store.AgentReincarnation {
	out := &store.AgentReincarnation{
		ID:             r.ID.String(),
		AgentID:        r.AgentID,
		FromGeneration: r.FromGeneration,
		ToGeneration:   r.ToGeneration,
		RequestedBy:    r.RequestedBy,
		RequestedAt:    r.RequestedAt,
		UpdatedAt:      r.UpdatedAt,
		CompletedAt:    r.CompletedAt,
		State:          string(r.State),
		Error:          r.Error,
		Handoff:        r.Handoff,
	}
	if r.PreviousAppliedConfig != "" {
		cfg, err := unmarshalAppliedConfigSnapshot(r.PreviousAppliedConfig)
		if err != nil {
			slog.Error("agent reincarnation store: previous_applied_config could not be used as stored",
				"reincarnation_id", out.ID, "error", err)
		}
		out.PreviousAppliedConfig = cfg
	}
	if r.NewAppliedConfig != "" {
		cfg, err := unmarshalAppliedConfigSnapshot(r.NewAppliedConfig)
		if err != nil {
			slog.Error("agent reincarnation store: new_applied_config could not be used as stored",
				"reincarnation_id", out.ID, "error", err)
		}
		out.NewAppliedConfig = cfg
	}
	return out
}

// unmarshalAppliedConfigSnapshot decodes an AgentAppliedConfig JSON blob for
// a history row. Unlike parseAppliedConfig (agent_store.go), it does not
// apply the live-agent GCP-metadata-mode sanitization: a reincarnation
// snapshot is historical record, not a config that will be dispatched as-is.
func unmarshalAppliedConfigSnapshot(raw string) (*store.AgentAppliedConfig, error) {
	var cfg store.AgentAppliedConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// marshalAppliedConfigSnapshot serializes an AgentAppliedConfig snapshot to
// JSON text, returning "" for nil so the column is left empty.
func marshalAppliedConfigSnapshot(cfg *store.AgentAppliedConfig) string {
	if cfg == nil {
		return ""
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return string(data)
}

// CreateAgentReincarnation creates a new reincarnation record.
func (s *AgentReincarnationStore) CreateAgentReincarnation(ctx context.Context, r *store.AgentReincarnation) error {
	state := r.State
	if state == "" {
		state = store.AgentReincarnationStatePending
	}
	builder := s.client.AgentReincarnation.Create().
		SetAgentID(r.AgentID).
		SetFromGeneration(r.FromGeneration).
		SetToGeneration(r.ToGeneration).
		SetRequestedBy(r.RequestedBy).
		SetState(agentreincarnation.State(state)).
		SetError(r.Error).
		SetHandoff(r.Handoff)

	if !r.RequestedAt.IsZero() {
		builder.SetRequestedAt(r.RequestedAt)
	}
	if r.CompletedAt != nil {
		builder.SetCompletedAt(*r.CompletedAt)
	}
	if cfg := marshalAppliedConfigSnapshot(r.PreviousAppliedConfig); cfg != "" {
		builder.SetPreviousAppliedConfig(cfg)
	}
	if cfg := marshalAppliedConfigSnapshot(r.NewAppliedConfig); cfg != "" {
		builder.SetNewAppliedConfig(cfg)
	}
	if r.ID != "" {
		uid, err := parseUUID(r.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	r.ID = created.ID.String()
	r.State = string(created.State)
	r.RequestedAt = created.RequestedAt
	return nil
}

// GetAgentReincarnation retrieves a reincarnation record by ID.
func (s *AgentReincarnationStore) GetAgentReincarnation(ctx context.Context, id string) (*store.AgentReincarnation, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	r, err := s.client.AgentReincarnation.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentReincarnationToStore(r), nil
}

// UpdateAgentReincarnation updates the mutable fields of a reincarnation
// record: state, completion, error, and the new-generation config snapshot
// (written once the worker has resolved it).
func (s *AgentReincarnationStore) UpdateAgentReincarnation(ctx context.Context, r *store.AgentReincarnation) error {
	uid, err := parseGetID(r.ID)
	if err != nil {
		return err
	}
	builder := s.client.AgentReincarnation.UpdateOneID(uid).
		SetState(agentreincarnation.State(r.State)).
		SetError(r.Error)

	if r.CompletedAt != nil {
		builder.SetCompletedAt(*r.CompletedAt)
	} else {
		builder.ClearCompletedAt()
	}
	if cfg := marshalAppliedConfigSnapshot(r.NewAppliedConfig); cfg != "" {
		builder.SetNewAppliedConfig(cfg)
	}
	if cfg := marshalAppliedConfigSnapshot(r.PreviousAppliedConfig); cfg != "" {
		builder.SetPreviousAppliedConfig(cfg)
	}

	if _, err := builder.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListAgentReincarnations returns all reincarnation records for an agent,
// most recent first.
func (s *AgentReincarnationStore) ListAgentReincarnations(ctx context.Context, agentID string) ([]*store.AgentReincarnation, error) {
	rows, err := s.client.AgentReincarnation.Query().
		Where(agentreincarnation.AgentIDEQ(agentID)).
		Order(ent.Desc(agentreincarnation.FieldRequestedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.AgentReincarnation, 0, len(rows))
	for _, r := range rows {
		out = append(out, entAgentReincarnationToStore(r))
	}
	return out, nil
}

// GetPendingAgentReincarnation returns the agent's non-terminal reincarnation
// record, if any (AC-8's 409-concurrency check).
func (s *AgentReincarnationStore) GetPendingAgentReincarnation(ctx context.Context, agentID string) (*store.AgentReincarnation, error) {
	states := make([]agentreincarnation.State, 0, len(store.AgentReincarnationNonTerminalStates))
	for _, st := range store.AgentReincarnationNonTerminalStates {
		states = append(states, agentreincarnation.State(st))
	}
	r, err := s.client.AgentReincarnation.Query().
		Where(
			agentreincarnation.AgentIDEQ(agentID),
			agentreincarnation.StateIn(states...),
		).
		Order(ent.Desc(agentreincarnation.FieldRequestedAt)).
		First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentReincarnationToStore(r), nil
}

// DeleteAgentReincarnationsForAgent hard-deletes all reincarnation records
// for an agent. Called from the agent hard-delete cascade.
func (s *AgentReincarnationStore) DeleteAgentReincarnationsForAgent(ctx context.Context, agentID string) error {
	_, err := s.client.AgentReincarnation.Delete().
		Where(agentreincarnation.AgentIDEQ(agentID)).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListNonTerminalAgentReincarnations returns every non-terminal reincarnation
// record across all agents, for the hub-restart boot sweep (design §3.7 F4).
func (s *AgentReincarnationStore) ListStaleNonTerminalAgentReincarnations(ctx context.Context, olderThan time.Time) ([]*store.AgentReincarnation, error) {
	states := make([]agentreincarnation.State, 0, len(store.AgentReincarnationNonTerminalStates))
	for _, st := range store.AgentReincarnationNonTerminalStates {
		states = append(states, agentreincarnation.State(st))
	}
	rows, err := s.client.AgentReincarnation.Query().
		Where(
			agentreincarnation.StateIn(states...),
			agentreincarnation.UpdatedAtLT(olderThan),
		).
		Order(ent.Desc(agentreincarnation.FieldRequestedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.AgentReincarnation, 0, len(rows))
	for _, r := range rows {
		out = append(out, entAgentReincarnationToStore(r))
	}
	return out, nil
}
