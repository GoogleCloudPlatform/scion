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
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	metricPrefix  = "workload.googleapis.com/"
	cacheTTL      = 5 * time.Minute
	maxPeriodDays = 90
	defaultPeriod = 7
	alignmentDay  = 86400 // seconds in a day
	alignmentHour = 3600
)

// MetricsDashboardService queries Google Cloud Monitoring for Scion telemetry metrics.
type MetricsDashboardService struct {
	client    *monitoring.MetricClient
	projectID string

	mu    sync.RWMutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	data      interface{}
	fetchedAt time.Time
}

// NewMetricsDashboardService creates a new service for querying Cloud Monitoring.
func NewMetricsDashboardService(ctx context.Context, projectID string) (*MetricsDashboardService, error) {
	if projectID == "" {
		return nil, fmt.Errorf("GCP project ID is required for metrics dashboard")
	}

	client, err := monitoring.NewMetricClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating monitoring client: %w", err)
	}

	return &MetricsDashboardService{
		client:    client,
		projectID: projectID,
		cache:     make(map[string]*cacheEntry),
	}, nil
}

// Close releases resources.
func (s *MetricsDashboardService) Close() error {
	return s.client.Close()
}

// DashboardSummary contains aggregate metric counts for a period.
type DashboardSummary struct {
	PeriodDays    int   `json:"periodDays"`
	TotalSessions int64 `json:"totalSessions"`
	TotalAPICalls int64 `json:"totalApiCalls"`
	TotalTokens   int64 `json:"totalTokens"`
	UniqueAgents  int   `json:"uniqueAgents"`
}

// TimeSeriesPoint represents a single data point in a time series.
type TimeSeriesPoint struct {
	Timestamp string `json:"timestamp"`
	Value     int64  `json:"value"`
}

// LabeledTimeSeries groups time series data by a label value.
type LabeledTimeSeries struct {
	Label  string            `json:"label"`
	Points []TimeSeriesPoint `json:"points"`
}

// SessionsView contains session count and active agent data.
type SessionsView struct {
	PeriodDays   int               `json:"periodDays"`
	DailyCounts  []TimeSeriesPoint `json:"dailyCounts"`
	ActiveAgents []TimeSeriesPoint `json:"activeAgents"`
}

// ModelCallsView contains API call data grouped by model and harness.
type ModelCallsView struct {
	PeriodDays int                 `json:"periodDays"`
	ByModel    []LabeledTimeSeries `json:"byModel"`
	ByHarness  []LabeledTimeSeries `json:"byHarness"`
}

// TokensView contains token usage data grouped by model.
type TokensView struct {
	PeriodDays int                 `json:"periodDays"`
	Input      []LabeledTimeSeries `json:"input"`
	Output     []LabeledTimeSeries `json:"output"`
}

func (s *MetricsDashboardService) getCached(key string) (interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.cache[key]
	if !ok || time.Since(entry.fetchedAt) > cacheTTL {
		return nil, false
	}
	return entry.data, true
}

func (s *MetricsDashboardService) setCache(key string, data interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = &cacheEntry{data: data, fetchedAt: time.Now()}
}

// QueryOption configures optional query parameters.
type QueryOption func(*queryConfig)

type queryConfig struct {
	ProjectID string
}

// WithProjectID filters metrics to a specific project.
func WithProjectID(id string) QueryOption {
	return func(c *queryConfig) { c.ProjectID = id }
}

func applyQueryOptions(opts []QueryOption) *queryConfig {
	cfg := &queryConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// cacheKeySuffix returns a cache key suffix for the query config.
// Returns empty string for global queries, ":projectID" for project-scoped.
func (c *queryConfig) cacheKeySuffix() string {
	if c.ProjectID != "" {
		return ":" + c.ProjectID
	}
	return ""
}

// projectFilter returns the Cloud Monitoring filter clause for a project ID.
func projectFilter(projectID string) string {
	return fmt.Sprintf(`metric.labels.project_id = "%s"`, projectID)
}

// QuerySummary returns aggregate metric counts for the given period.
func (s *MetricsDashboardService) QuerySummary(ctx context.Context, periodDays int, opts ...QueryOption) (*DashboardSummary, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("summary:%d%s", periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*DashboardSummary), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	var extraFilter []string
	if cfg.ProjectID != "" {
		extraFilter = append(extraFilter, projectFilter(cfg.ProjectID))
	}

	summary := &DashboardSummary{PeriodDays: periodDays}
	var queryErrors []string

	sessions, err := s.querySum(ctx, "agent.session.count", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("session count: %v", err))
	} else {
		summary.TotalSessions = sessions
	}

	apiCalls, err := s.querySum(ctx, "gen_ai.api.calls", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("API calls: %v", err))
	} else {
		summary.TotalAPICalls = apiCalls
	}

	inputTokens, err := s.querySum(ctx, "gen_ai.tokens.input", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("input tokens: %v", err))
	}
	outputTokens, err := s.querySum(ctx, "gen_ai.tokens.output", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("output tokens: %v", err))
	}
	summary.TotalTokens = inputTokens + outputTokens

	agents, err := s.queryUniqueLabels(ctx, "agent.session.count", "metric.labels.agent_id", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("unique agents: %v", err))
	} else {
		summary.UniqueAgents = len(agents)
	}

	if len(queryErrors) > 0 {
		return summary, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setCache(cacheKey, summary)
	return summary, nil
}

// QuerySessions returns daily session counts and active agent counts.
func (s *MetricsDashboardService) QuerySessions(ctx context.Context, periodDays int, opts ...QueryOption) (*SessionsView, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("sessions:%d%s", periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*SessionsView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	var extraFilter []string
	if cfg.ProjectID != "" {
		extraFilter = append(extraFilter, projectFilter(cfg.ProjectID))
	}

	view := &SessionsView{PeriodDays: periodDays}
	var queryErrors []string

	dailyCounts, err := s.queryDailyTimeSeries(ctx, "agent.session.count", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("daily sessions: %v", err))
	} else {
		view.DailyCounts = dailyCounts
	}

	activeAgents, err := s.queryDailyUniqueCount(ctx, "agent.session.count", "metric.labels.agent_id", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("active agents: %v", err))
	} else {
		view.ActiveAgents = activeAgents
	}

	if len(queryErrors) > 0 {
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setCache(cacheKey, view)
	return view, nil
}

// QueryModelCalls returns API call data grouped by model and harness.
func (s *MetricsDashboardService) QueryModelCalls(ctx context.Context, periodDays int, opts ...QueryOption) (*ModelCallsView, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("model-calls:%d%s", periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*ModelCallsView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	var extraFilter []string
	if cfg.ProjectID != "" {
		extraFilter = append(extraFilter, projectFilter(cfg.ProjectID))
	}

	view := &ModelCallsView{PeriodDays: periodDays}
	var queryErrors []string

	byModel, err := s.queryGroupedTimeSeries(ctx, "gen_ai.api.calls", "metric.labels.model", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("by model: %v", err))
	} else {
		view.ByModel = byModel
	}

	byHarness, err := s.queryGroupedTimeSeries(ctx, "gen_ai.api.calls", "metric.labels.harness", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("by harness: %v", err))
	} else {
		view.ByHarness = byHarness
	}

	if len(queryErrors) > 0 {
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setCache(cacheKey, view)
	return view, nil
}

// QueryTokens returns token usage data grouped by model.
func (s *MetricsDashboardService) QueryTokens(ctx context.Context, periodDays int, opts ...QueryOption) (*TokensView, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("tokens:%d%s", periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*TokensView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	var extraFilter []string
	if cfg.ProjectID != "" {
		extraFilter = append(extraFilter, projectFilter(cfg.ProjectID))
	}

	view := &TokensView{PeriodDays: periodDays}
	var queryErrors []string

	input, err := s.queryGroupedTimeSeries(ctx, "gen_ai.tokens.input", "metric.labels.model", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("input tokens: %v", err))
	} else {
		view.Input = input
	}

	output, err := s.queryGroupedTimeSeries(ctx, "gen_ai.tokens.output", "metric.labels.model", start, now, extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("output tokens: %v", err))
	} else {
		view.Output = output
	}

	if len(queryErrors) > 0 {
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setCache(cacheKey, view)
	return view, nil
}

// querySum queries a metric and returns the total sum across all time series and points.
func (s *MetricsDashboardService) querySum(ctx context.Context, metricName string, start, end time.Time, extraFilter []string) (int64, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start),
			EndTime:   timestamppb.New(end),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Duration(end.Sub(start).Seconds()) * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
		},
	}

	var total int64
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("listing time series for %s: %w", metricName, err)
		}
		for _, p := range ts.GetPoints() {
			total += p.GetValue().GetInt64Value()
		}
	}
	return total, nil
}

// queryDailyTimeSeries returns daily aggregated data points for a metric.
func (s *MetricsDashboardService) queryDailyTimeSeries(ctx context.Context, metricName string, start, end time.Time, extraFilter []string) ([]TimeSeriesPoint, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start),
			EndTime:   timestamppb.New(end),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Duration(alignmentDay) * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
		},
	}

	var points []TimeSeriesPoint
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing daily time series for %s: %w", metricName, err)
		}
		for _, p := range ts.GetPoints() {
			points = append(points, TimeSeriesPoint{
				Timestamp: p.GetInterval().GetEndTime().AsTime().Format("2006-01-02"),
				Value:     p.GetValue().GetInt64Value(),
			})
		}
	}
	return points, nil
}

// labelKeyFromGroupBy extracts the short label key from a Cloud Monitoring
// groupByLabel like "metric.labels.model" → "model".
func labelKeyFromGroupBy(groupByLabel string) string {
	parts := strings.Split(groupByLabel, ".")
	return parts[len(parts)-1]
}

// queryGroupedTimeSeries returns daily data grouped by a label.
func (s *MetricsDashboardService) queryGroupedTimeSeries(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) ([]LabeledTimeSeries, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start),
			EndTime:   timestamppb.New(end),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Duration(alignmentDay) * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      []string{groupByLabel},
		},
	}

	seriesMap := make(map[string][]TimeSeriesPoint)
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing grouped time series for %s: %w", metricName, err)
		}

		label := "(unknown)"
		if labels := ts.GetMetric().GetLabels(); labels != nil {
			if v, ok := labels[labelKey]; ok && v != "" {
				label = v
			}
		}

		for _, p := range ts.GetPoints() {
			seriesMap[label] = append(seriesMap[label], TimeSeriesPoint{
				Timestamp: p.GetInterval().GetEndTime().AsTime().Format("2006-01-02"),
				Value:     p.GetValue().GetInt64Value(),
			})
		}
	}

	var result []LabeledTimeSeries
	for label, points := range seriesMap {
		result = append(result, LabeledTimeSeries{Label: label, Points: points})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Label < result[j].Label
	})
	return result, nil
}

// queryUniqueLabels returns unique values for a label within a metric's time series.
func (s *MetricsDashboardService) queryUniqueLabels(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) (map[string]bool, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start),
			EndTime:   timestamppb.New(end),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Duration(end.Sub(start).Seconds()) * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      []string{groupByLabel},
		},
	}

	unique := make(map[string]bool)
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing unique labels for %s: %w", metricName, err)
		}
		if labels := ts.GetMetric().GetLabels(); labels != nil {
			if v, ok := labels[labelKey]; ok && v != "" {
				unique[v] = true
			}
		}
	}
	return unique, nil
}

// queryDailyUniqueCount returns per-day counts of unique label values.
func (s *MetricsDashboardService) queryDailyUniqueCount(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) ([]TimeSeriesPoint, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start),
			EndTime:   timestamppb.New(end),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(time.Duration(alignmentDay) * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      []string{groupByLabel},
		},
	}

	// Count unique label values per day
	dayAgents := make(map[string]map[string]bool) // date -> set of label values
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing daily unique for %s: %w", metricName, err)
		}

		label := "(unknown)"
		if labels := ts.GetMetric().GetLabels(); labels != nil {
			if v, ok := labels[labelKey]; ok && v != "" {
				label = v
			}
		}

		for _, p := range ts.GetPoints() {
			day := p.GetInterval().GetEndTime().AsTime().Format("2006-01-02")
			if dayAgents[day] == nil {
				dayAgents[day] = make(map[string]bool)
			}
			if p.GetValue().GetInt64Value() > 0 {
				dayAgents[day][label] = true
			}
		}
	}

	var points []TimeSeriesPoint
	for day, agents := range dayAgents {
		points = append(points, TimeSeriesPoint{
			Timestamp: day,
			Value:     int64(len(agents)),
		})
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp < points[j].Timestamp
	})
	return points, nil
}

// ProjectMetricsSummary contains lightweight scalar metrics for a project's status bar.
type ProjectMetricsSummary struct {
	SessionsCount24h int64  `json:"sessionsCount24h"`
	APICalls24h      int64  `json:"apiCalls24h"`
	TokenUsage24h    int64  `json:"tokenUsage24h"`
	ActiveAgents24h  int    `json:"activeAgents24h"`
	PeriodLabel      string `json:"periodLabel"`
}

// QueryProjectSummary returns lightweight scalar metrics for a project over the last 24 hours.
func (s *MetricsDashboardService) QueryProjectSummary(ctx context.Context, projectID string) (*ProjectMetricsSummary, error) {
	cacheKey := fmt.Sprintf("project-summary:%s", projectID)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*ProjectMetricsSummary), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -1) // 24 hours

	filter := []string{projectFilter(projectID)}

	summary := &ProjectMetricsSummary{PeriodLabel: "Last 24 hours"}
	var queryErrors []string

	sessions, err := s.querySum(ctx, "agent.session.count", start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("sessions: %v", err))
	} else {
		summary.SessionsCount24h = sessions
	}

	apiCalls, err := s.querySum(ctx, "gen_ai.api.calls", start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("API calls: %v", err))
	} else {
		summary.APICalls24h = apiCalls
	}

	inputTokens, err := s.querySum(ctx, "gen_ai.tokens.input", start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("input tokens: %v", err))
	}
	outputTokens, err := s.querySum(ctx, "gen_ai.tokens.output", start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("output tokens: %v", err))
	}
	summary.TokenUsage24h = inputTokens + outputTokens

	agents, err := s.queryUniqueLabels(ctx, "agent.session.count", "metric.labels.agent_id", start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("active agents: %v", err))
	} else {
		summary.ActiveAgents24h = len(agents)
	}

	if len(queryErrors) > 0 {
		return summary, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setCache(cacheKey, summary)
	return summary, nil
}

// handleProjectMetricsSummary returns lightweight metrics summary for a project.
func (s *Server) handleProjectMetricsSummary(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: any authenticated user with view access
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

	// If metrics service is not configured, return unavailable indicator
	if s.metricsDashboard == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": false})
		return
	}

	data, err := s.metricsDashboard.QueryProjectSummary(ctx, projectID)
	if err != nil {
		if data == nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to query project metrics summary", nil)
			return
		}
		slog.Warn("Partial project metrics summary failure", "projectID", projectID, "error", err)
	}
	writeJSON(w, http.StatusOK, data)
}

// handleMetricsDashboard serves the metrics dashboard API to any authenticated user.
func (s *Server) handleMetricsDashboard(w http.ResponseWriter, r *http.Request) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}

	s.serveMetricsDashboard(w, r)
}

// handleAdminMetricsDashboard serves the metrics dashboard API (legacy admin-scoped path).
// Kept for backward compatibility — delegates to the same handler with relaxed auth.
// NOTE: This endpoint intentionally no longer requires admin role. The metrics dashboard
// was moved from admin-only to all-authenticated-users access as part of the metrics
// dashboard refactoring. The old admin-scoped URL is maintained for browser bookmark
// backward compatibility and will be removed in a future release.
func (s *Server) handleAdminMetricsDashboard(w http.ResponseWriter, r *http.Request) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}

	s.serveMetricsDashboard(w, r)
}

// handleProjectMetricsDashboard serves the per-project metrics dashboard.
func (s *Server) handleProjectMetricsDashboard(w http.ResponseWriter, r *http.Request, projectID, _ string) {
	ctx := r.Context()

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: any authenticated user with view access to the project
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

	s.serveMetricsDashboard(w, r, WithProjectID(projectID))
}

// serveMetricsDashboard contains the shared metrics dashboard logic.
func (s *Server) serveMetricsDashboard(w http.ResponseWriter, r *http.Request, opts ...QueryOption) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.metricsDashboard == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics_unavailable",
			"Metrics dashboard is not configured (no telemetry project ID)", nil)
		return
	}

	view := r.URL.Query().Get("view")
	if view == "" {
		view = "summary"
	}

	periodStr := r.URL.Query().Get("period")
	periodDays := defaultPeriod
	if periodStr != "" {
		if p, err := strconv.Atoi(periodStr); err == nil && p > 0 && p <= maxPeriodDays {
			periodDays = p
		}
	}

	ctx := r.Context()

	switch view {
	case "summary":
		data, err := s.metricsDashboard.QuerySummary(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query metrics summary", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "error", err)
			w.Header().Set("X-Metrics-Warning", err.Error())
		}
		writeJSON(w, http.StatusOK, data)

	case "sessions":
		data, err := s.metricsDashboard.QuerySessions(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query session metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "error", err)
			w.Header().Set("X-Metrics-Warning", err.Error())
		}
		writeJSON(w, http.StatusOK, data)

	case "model-calls":
		data, err := s.metricsDashboard.QueryModelCalls(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query model call metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "error", err)
			w.Header().Set("X-Metrics-Warning", err.Error())
		}
		writeJSON(w, http.StatusOK, data)

	case "tokens":
		data, err := s.metricsDashboard.QueryTokens(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query token metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "error", err)
			w.Header().Set("X-Metrics-Warning", err.Error())
		}
		writeJSON(w, http.StatusOK, data)

	default:
		writeError(w, http.StatusBadRequest, "invalid_view",
			fmt.Sprintf("Unknown view: %s. Valid views: summary, sessions, model-calls, tokens", view), nil)
	}
}
