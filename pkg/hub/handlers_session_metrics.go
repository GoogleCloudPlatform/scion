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

package hub

import (
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Response types for session metrics API
// =============================================================================

// toolUsageSummary summarises call statistics for a single tool.
type toolUsageSummary struct {
	Name    string `json:"name"`
	Calls   int    `json:"calls"`
	Success int    `json:"success"`
	Error   int    `json:"error"`
}

// modelUsageSummary counts sessions per model.
type modelUsageSummary struct {
	Model    string `json:"model"`
	Sessions int    `json:"sessions"`
}

// agentMetricsSummaryResponse is returned by the agent metrics summary endpoint.
type agentMetricsSummaryResponse struct {
	AgentID              string              `json:"agentId"`
	TotalSessions        int                 `json:"totalSessions"`
	TotalTokensInput     int64               `json:"totalTokensInput"`
	TotalTokensOutput    int64               `json:"totalTokensOutput"`
	TotalTokensCached    int64               `json:"totalTokensCached"`
	TotalTokensReasoning int64               `json:"totalTokensReasoning"`
	TotalToolCalls       int                 `json:"totalToolCalls"`
	AvgSessionDurationMs int64               `json:"avgSessionDurationMs"`
	AvgTokensPerSession  int64               `json:"avgTokensPerSession"`
	MostUsedTools        []toolUsageSummary  `json:"mostUsedTools"`
	MostUsedModels       []modelUsageSummary `json:"mostUsedModels"`
}

// projectMetricsSummaryResponse is returned by the project metrics summary endpoint.
type projectMetricsSummaryResponse struct {
	ProjectID            string              `json:"projectId"`
	TotalSessions        int                 `json:"totalSessions"`
	TotalTokensInput     int64               `json:"totalTokensInput"`
	TotalTokensOutput    int64               `json:"totalTokensOutput"`
	TotalTokensCached    int64               `json:"totalTokensCached"`
	TotalTokensReasoning int64               `json:"totalTokensReasoning"`
	ActiveAgents         int                 `json:"activeAgents"`
	MostUsedTools        []toolUsageSummary  `json:"mostUsedTools"`
	MostUsedModels       []modelUsageSummary `json:"mostUsedModels"`
}

// =============================================================================
// Aggregation helpers (tool/model breakdowns from list queries)
// =============================================================================

// aggregateToolCalls extracts tool usage statistics from session metrics records.
func aggregateToolCalls(sessions []*store.AgentSessionMetrics) (tools []toolUsageSummary, totalCalls int) {
	toolMap := make(map[string]*toolUsageSummary)
	for _, s := range sessions {
		if s == nil {
			continue
		}
		for name, raw := range s.ToolCalls {
			ts, ok := toolMap[name]
			if !ok {
				ts = &toolUsageSummary{Name: name}
				toolMap[name] = ts
			}
			if m, ok := raw.(map[string]interface{}); ok {
				calls := intFromAny(m["calls"])
				success := intFromAny(m["success"])
				errs := intFromAny(m["error"])
				ts.Calls += calls
				ts.Success += success
				ts.Error += errs
				totalCalls += calls
			}
		}
	}

	tools = make([]toolUsageSummary, 0, len(toolMap))
	for _, t := range toolMap {
		tools = append(tools, *t)
	}
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Calls > tools[j].Calls
	})
	if len(tools) > 10 {
		tools = tools[:10]
	}
	return tools, totalCalls
}

// aggregateModels counts sessions per model.
func aggregateModels(sessions []*store.AgentSessionMetrics) []modelUsageSummary {
	counts := make(map[string]int)
	for _, s := range sessions {
		if s == nil {
			continue
		}
		if s.Model != "" {
			counts[s.Model]++
		}
	}

	models := make([]modelUsageSummary, 0, len(counts))
	for m, c := range counts {
		models = append(models, modelUsageSummary{Model: m, Sessions: c})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].Sessions > models[j].Sessions
	})
	if len(models) > 10 {
		models = models[:10]
	}
	return models
}

// avgSessionDuration computes the average session duration in milliseconds.
func avgSessionDuration(sessions []*store.AgentSessionMetrics) int64 {
	if len(sessions) == 0 {
		return 0
	}
	var total time.Duration
	var count int
	for _, s := range sessions {
		if s == nil {
			continue
		}
		if s.EndedAt != nil {
			total += s.EndedAt.Sub(s.StartedAt)
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total.Milliseconds() / int64(count)
}

// intFromAny extracts an int from an interface{} that may be a float64 (JSON
// numbers) or an int.
func intFromAny(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// =============================================================================
// Authorization helpers
// =============================================================================

// authorizeSessionMetricsAccess checks that the calling identity (user or
// agent) is allowed to read the given agent resource. Returns true if
// access is allowed, false if a response was already written.
func (s *Server) authorizeSessionMetricsAccess(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	if agent == nil {
		NotFound(w, "Agent")
		return false
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}

	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return false
		}
	} else if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ProjectID() != agent.ProjectID {
			Forbidden(w)
			return false
		}
	} else {
		Forbidden(w)
		return false
	}
	return true
}

// =============================================================================
// Handlers
// =============================================================================

// handleAgentMetricsSummary returns aggregate session metrics for an agent.
// Numeric totals (session count, token sums) are computed via SQL-level
// aggregation; tool/model breakdowns are derived from the most recent
// sessions (capped by the store list limit).
//
// GET /api/v1/agents/{id}/metrics/summary
func (s *Server) handleAgentMetricsSummary(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Verify the agent exists and the caller can view it.
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeSessionMetricsAccess(w, r, agent) {
		return
	}

	// SQL-level aggregation for accurate totals.
	agg, err := s.store.AggregateByAgent(ctx, agentID)
	if err != nil {
		s.agentMetricsLog.Error("Failed to aggregate session metrics for agent",
			"agent_id", agentID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	// Tool/model breakdowns and avg duration are derived from the most recent
	// sessions returned by the list query, which is capped at 100 records
	// (defaultMetricsListLimit). For agents with >100 sessions these values
	// reflect only the newest subset — acceptable for MVP. A future iteration
	// could push tool/model aggregation and AVG(duration) into SQL.
	sessions, err := s.store.ListAgentSessionMetricsByAgent(ctx, agentID)
	if err != nil {
		s.agentMetricsLog.Error("Failed to list session metrics for agent",
			"agent_id", agentID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	tools, totalToolCalls := aggregateToolCalls(sessions)
	models := aggregateModels(sessions)
	avgDuration := avgSessionDuration(sessions)

	totalTokens := agg.SumTokensInput + agg.SumTokensOutput
	var avgTokens int64
	if agg.Count > 0 {
		avgTokens = totalTokens / int64(agg.Count)
	}

	resp := agentMetricsSummaryResponse{
		AgentID:              agentID,
		TotalSessions:        agg.Count,
		TotalTokensInput:     agg.SumTokensInput,
		TotalTokensOutput:    agg.SumTokensOutput,
		TotalTokensCached:    agg.SumTokensCached,
		TotalTokensReasoning: agg.SumTokensReason,
		TotalToolCalls:       totalToolCalls,
		AvgSessionDurationMs: avgDuration,
		AvgTokensPerSession:  avgTokens,
		MostUsedTools:        tools,
		MostUsedModels:       models,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleSessionMetrics returns a single session metrics record by ID.
// GET /api/v1/metrics/session/{id}
func (s *Server) handleSessionMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	id := extractID(r, "/api/v1/metrics/session")
	if id == "" {
		NotFound(w, "Session metrics")
		return
	}

	metrics, err := s.store.GetAgentSessionMetrics(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if metrics == nil {
		NotFound(w, "Session metrics")
		return
	}

	// Authorize: verify the caller can view the owning agent.
	agent, err := s.store.GetAgent(ctx, metrics.AgentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeSessionMetricsAccess(w, r, agent) {
		return
	}

	writeJSON(w, http.StatusOK, metrics)
}

// handleProjectSessionMetricsSummary returns aggregate session metrics for a project.
// Numeric totals are computed via SQL-level aggregation; tool/model breakdowns
// are derived from the most recent sessions (capped by the store list limit).
//
// GET /api/v1/projects/{id}/metrics/summary
func (s *Server) handleProjectSessionMetricsSummary(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Verify the project exists.
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: any authenticated identity with view access.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	// SQL-level aggregation for accurate totals.
	agg, err := s.store.AggregateByProject(ctx, projectID)
	if err != nil {
		s.agentMetricsLog.Error("Failed to aggregate session metrics for project",
			"project_id", projectID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	activeAgents, err := s.store.CountDistinctAgentsByProject(ctx, projectID)
	if err != nil {
		s.agentMetricsLog.Error("Failed to count active agents for project",
			"project_id", projectID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	// Tool/model breakdowns are derived from the most recent sessions returned
	// by the list query, which is capped at 100 records (defaultMetricsListLimit).
	// For projects with >100 sessions these values reflect only the newest
	// subset — acceptable for MVP. A future iteration could push tool/model
	// aggregation into SQL.
	sessions, err := s.store.ListAgentSessionMetricsByProject(ctx, projectID)
	if err != nil {
		s.agentMetricsLog.Error("Failed to list session metrics for project",
			"project_id", projectID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	tools, _ := aggregateToolCalls(sessions)
	models := aggregateModels(sessions)

	resp := projectMetricsSummaryResponse{
		ProjectID:            projectID,
		TotalSessions:        agg.Count,
		TotalTokensInput:     agg.SumTokensInput,
		TotalTokensOutput:    agg.SumTokensOutput,
		TotalTokensCached:    agg.SumTokensCached,
		TotalTokensReasoning: agg.SumTokensReason,
		ActiveAgents:         activeAgents,
		MostUsedTools:        tools,
		MostUsedModels:       models,
	}

	writeJSON(w, http.StatusOK, resp)
}
