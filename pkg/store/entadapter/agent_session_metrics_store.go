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
	if len(m.ToolCalls) > 0 {
		builder = builder.SetToolCalls(m.ToolCalls)
	}
	if len(m.Languages) > 0 {
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

// ListAgentSessionMetricsByProject returns session metrics for all agents
// in a project, ordered by started_at descending, capped at defaultMetricsListLimit.
func (s *AgentSessionMetricsStore) ListAgentSessionMetricsByProject(ctx context.Context, projectID string) ([]*store.AgentSessionMetrics, error) {
	entities, err := s.client.AgentSessionMetrics.
		Query().
		Where(entasm.GroveIDEQ(projectID)).
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

// ============================================================================
// Aggregation queries (SQL-level COUNT/SUM)
// ============================================================================

// AggregateByAgent returns SQL-level aggregate totals for an agent's sessions.
func (s *AgentSessionMetricsStore) AggregateByAgent(ctx context.Context, agentID string) (*store.AgentSessionMetricsAggregates, error) {
	var agg []struct {
		Count           int    `json:"count"`
		SumTokensInput  *int64 `json:"sum_tokens_input"`
		SumTokensOutput *int64 `json:"sum_tokens_output"`
		SumTokensCached *int64 `json:"sum_tokens_cached"`
		SumTokensReason *int64 `json:"sum_tokens_reasoning"`
		SumTurnCount    *int64 `json:"sum_turn_count"`
	}

	err := s.client.AgentSessionMetrics.
		Query().
		Where(entasm.AgentIDEQ(agentID)).
		Aggregate(
			ent.As(ent.Count(), "count"),
			ent.As(ent.Sum(entasm.FieldTokensInput), "sum_tokens_input"),
			ent.As(ent.Sum(entasm.FieldTokensOutput), "sum_tokens_output"),
			ent.As(ent.Sum(entasm.FieldTokensCached), "sum_tokens_cached"),
			ent.As(ent.Sum(entasm.FieldTokensReasoning), "sum_tokens_reasoning"),
			ent.As(ent.Sum(entasm.FieldTurnCount), "sum_turn_count"),
		).
		Scan(ctx, &agg)
	if err != nil {
		return nil, mapError(err)
	}

	if len(agg) == 0 {
		return &store.AgentSessionMetricsAggregates{}, nil
	}

	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return &store.AgentSessionMetricsAggregates{
		Count:           agg[0].Count,
		SumTokensInput:  deref(agg[0].SumTokensInput),
		SumTokensOutput: deref(agg[0].SumTokensOutput),
		SumTokensCached: deref(agg[0].SumTokensCached),
		SumTokensReason: deref(agg[0].SumTokensReason),
		SumTurnCount:    deref(agg[0].SumTurnCount),
	}, nil
}

// AggregateByProject returns SQL-level aggregate totals for a project's sessions.
func (s *AgentSessionMetricsStore) AggregateByProject(ctx context.Context, projectID string) (*store.AgentSessionMetricsAggregates, error) {
	var agg []struct {
		Count           int    `json:"count"`
		SumTokensInput  *int64 `json:"sum_tokens_input"`
		SumTokensOutput *int64 `json:"sum_tokens_output"`
		SumTokensCached *int64 `json:"sum_tokens_cached"`
		SumTokensReason *int64 `json:"sum_tokens_reasoning"`
		SumTurnCount    *int64 `json:"sum_turn_count"`
	}

	err := s.client.AgentSessionMetrics.
		Query().
		Where(entasm.GroveIDEQ(projectID)).
		Aggregate(
			ent.As(ent.Count(), "count"),
			ent.As(ent.Sum(entasm.FieldTokensInput), "sum_tokens_input"),
			ent.As(ent.Sum(entasm.FieldTokensOutput), "sum_tokens_output"),
			ent.As(ent.Sum(entasm.FieldTokensCached), "sum_tokens_cached"),
			ent.As(ent.Sum(entasm.FieldTokensReasoning), "sum_tokens_reasoning"),
			ent.As(ent.Sum(entasm.FieldTurnCount), "sum_turn_count"),
		).
		Scan(ctx, &agg)
	if err != nil {
		return nil, mapError(err)
	}

	if len(agg) == 0 {
		return &store.AgentSessionMetricsAggregates{}, nil
	}

	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return &store.AgentSessionMetricsAggregates{
		Count:           agg[0].Count,
		SumTokensInput:  deref(agg[0].SumTokensInput),
		SumTokensOutput: deref(agg[0].SumTokensOutput),
		SumTokensCached: deref(agg[0].SumTokensCached),
		SumTokensReason: deref(agg[0].SumTokensReason),
		SumTurnCount:    deref(agg[0].SumTurnCount),
	}, nil
}

// CountDistinctAgentsByProject returns the number of distinct agents that have
// at least one session metrics record in the given project.
func (s *AgentSessionMetricsStore) CountDistinctAgentsByProject(ctx context.Context, projectID string) (int, error) {
	var groups []struct {
		AgentID string `json:"agent_id"`
	}

	err := s.client.AgentSessionMetrics.
		Query().
		Where(entasm.GroveIDEQ(projectID)).
		GroupBy(entasm.FieldAgentID).
		Scan(ctx, &groups)
	if err != nil {
		return 0, mapError(err)
	}
	return len(groups), nil
}
