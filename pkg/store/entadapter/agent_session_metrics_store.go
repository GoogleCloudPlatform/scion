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

	entasm "github.com/GoogleCloudPlatform/scion/pkg/ent/agentsessionmetrics"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// AgentSessionMetricsStore implements store.AgentSessionMetricsStore using the
// Ent ORM.
type AgentSessionMetricsStore struct {
	client *ent.Client
}

// NewAgentSessionMetricsStore creates a new Ent-backed AgentSessionMetricsStore.
func NewAgentSessionMetricsStore(client *ent.Client) *AgentSessionMetricsStore {
	return &AgentSessionMetricsStore{client: client}
}

// ============================================================================
// Conversion helpers
// ============================================================================

func entAgentSessionMetricsToStore(e *ent.AgentSessionMetrics) *store.AgentSessionMetrics {
	return &store.AgentSessionMetrics{
		ID:              e.ID.String(),
		AgentID:         e.AgentID,
		ProjectID:       e.GroveID,
		SessionID:       e.SessionID,
		StartedAt:       e.StartedAt,
		EndedAt:         e.EndedAt,
		Status:          e.Status,
		TurnCount:       e.TurnCount,
		Model:           e.Model,
		TokensInput:     e.TokensInput,
		TokensOutput:    e.TokensOutput,
		TokensCached:    e.TokensCached,
		TokensReasoning: e.TokensReasoning,
		ToolCalls:       e.ToolCalls,
		Languages:       e.Languages,
		CreatedAt:       e.CreatedAt,
	}
}

// ============================================================================
// CRUD operations
// ============================================================================

// CreateAgentSessionMetrics persists a new session metrics record.
func (s *AgentSessionMetricsStore) CreateAgentSessionMetrics(ctx context.Context, m *store.AgentSessionMetrics) error {
	if m.AgentID == "" || m.ProjectID == "" || m.SessionID == "" {
		return store.ErrInvalidInput
	}

	builder := s.client.AgentSessionMetrics.Create().
		SetAgentID(m.AgentID).
		SetGroveID(m.ProjectID).
		SetSessionID(m.SessionID).
		SetStartedAt(m.StartedAt).
		SetNillableEndedAt(m.EndedAt).
		SetTurnCount(m.TurnCount).
		SetTokensInput(m.TokensInput).
		SetTokensOutput(m.TokensOutput).
		SetTokensCached(m.TokensCached).
		SetTokensReasoning(m.TokensReasoning)

	if m.ID != "" {
		uid, err := uuid.Parse(m.ID)
		if err != nil {
			return store.ErrInvalidInput
		}
		builder = builder.SetID(uid)
	}
	if m.Status != "" {
		builder = builder.SetStatus(m.Status)
	}
	if m.Model != "" {
		builder = builder.SetModel(m.Model)
	}
	if m.ToolCalls != "" {
		builder = builder.SetToolCalls(m.ToolCalls)
	}
	if m.Languages != "" {
		builder = builder.SetLanguages(m.Languages)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	// Write back the generated ID and timestamp.
	m.ID = created.ID.String()
	m.CreatedAt = created.CreatedAt
	return nil
}

// GetAgentSessionMetrics retrieves a session metrics record by ID.
func (s *AgentSessionMetricsStore) GetAgentSessionMetrics(ctx context.Context, id string) (*store.AgentSessionMetrics, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	e, err := s.client.AgentSessionMetrics.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentSessionMetricsToStore(e), nil
}

// defaultMetricsListLimit caps unbounded list queries to prevent runaway
// responses for long-lived agents. Pagination via ListOptions can be added
// in a future milestone.
const defaultMetricsListLimit = 100

// ListAgentSessionMetricsByAgent returns session metrics for an agent,
// ordered by started_at descending, capped at defaultMetricsListLimit.
func (s *AgentSessionMetricsStore) ListAgentSessionMetricsByAgent(ctx context.Context, agentID string) ([]*store.AgentSessionMetrics, error) {
	entities, err := s.client.AgentSessionMetrics.
		Query().
		Where(entasm.AgentIDEQ(agentID)).
		Order(ent.Desc(entasm.FieldStartedAt)).
		Limit(defaultMetricsListLimit).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AgentSessionMetrics, 0, len(entities))
	for _, e := range entities {
		result = append(result, entAgentSessionMetricsToStore(e))
	}
	return result, nil
}
