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
	"strconv"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	metricPrefix   = "workload.googleapis.com/"
	cacheTTL       = 5 * time.Minute
	maxPeriodDays  = 90
	defaultPeriod  = 7
	alignmentDay   = 86400 // seconds in a day
	alignmentHour  = 3600
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

// QuerySummary returns aggregate metric counts for the given period.
func (s *MetricsDashboardService) QuerySummary(ctx context.Context, periodDays int) (*DashboardSummary, error) {
	cacheKey := fmt.Sprintf("summary:%d", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*DashboardSummary), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	summary := &DashboardSummary{PeriodDays: periodDays}
	var queryErrors []string

	sessions, err := s.querySum(ctx, "agent.session.count", start, now, nil)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("session count: %v", err))
	} else {
		summary.TotalSessions = sessions
	}

	apiCalls, err := s.querySum(ctx, "gen_ai.api.calls", start, now, nil)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("API calls: %v", err))
	} else {
		summary.TotalAPICalls = apiCalls
	}

	inputTokens, err := s.querySum(ctx, "gen_ai.tokens.input", start, now, nil)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("input tokens: %v", err))
	}
	outputTokens, err := s.querySum(ctx, "gen_ai.tokens.output", start, now, nil)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("output tokens: %v", err))
	}
	summary.TotalTokens = inputTokens + outputTokens

	agents, err := s.queryUniqueLabels(ctx, "agent.session.count", "metric.labels.agent_id", start, now)
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
func (s *MetricsDashboardService) QuerySessions(ctx context.Context, periodDays int) (*SessionsView, error) {
	cacheKey := fmt.Sprintf("sessions:%d", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*SessionsView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	view := &SessionsView{PeriodDays: periodDays}
	var queryErrors []string

	dailyCounts, err := s.queryDailyTimeSeries(ctx, "agent.session.count", start, now, nil)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("daily sessions: %v", err))
	} else {
		view.DailyCounts = dailyCounts
	}

	activeAgents, err := s.queryDailyUniqueCount(ctx, "agent.session.count", "metric.labels.agent_id", start, now)
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
func (s *MetricsDashboardService) QueryModelCalls(ctx context.Context, periodDays int) (*ModelCallsView, error) {
	cacheKey := fmt.Sprintf("model-calls:%d", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*ModelCallsView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	view := &ModelCallsView{PeriodDays: periodDays}
	var queryErrors []string

	byModel, err := s.queryGroupedTimeSeries(ctx, "gen_ai.api.calls", "metric.labels.model", start, now)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("by model: %v", err))
	} else {
		view.ByModel = byModel
	}

	byHarness, err := s.queryGroupedTimeSeries(ctx, "gen_ai.api.calls", "metric.labels.harness", start, now)
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
func (s *MetricsDashboardService) QueryTokens(ctx context.Context, periodDays int) (*TokensView, error) {
	cacheKey := fmt.Sprintf("tokens:%d", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*TokensView), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -periodDays)

	view := &TokensView{PeriodDays: periodDays}
	var queryErrors []string

	input, err := s.queryGroupedTimeSeries(ctx, "gen_ai.tokens.input", "metric.labels.model", start, now)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("input tokens: %v", err))
	} else {
		view.Input = input
	}

	output, err := s.queryGroupedTimeSeries(ctx, "gen_ai.tokens.output", "metric.labels.model", start, now)
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
func (s *MetricsDashboardService) queryGroupedTimeSeries(ctx context.Context, metricName, groupByLabel string, start, end time.Time) ([]LabeledTimeSeries, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
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
	return result, nil
}

// queryUniqueLabels returns unique values for a label within a metric's time series.
func (s *MetricsDashboardService) queryUniqueLabels(ctx context.Context, metricName, groupByLabel string, start, end time.Time) (map[string]bool, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
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
func (s *MetricsDashboardService) queryDailyUniqueCount(ctx context.Context, metricName, groupByLabel string, start, end time.Time) ([]TimeSeriesPoint, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
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
	return points, nil
}

// handleAdminMetricsDashboard serves the metrics dashboard API.
func (s *Server) handleAdminMetricsDashboard(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

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
		data, err := s.metricsDashboard.QuerySummary(ctx, periodDays)
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
		data, err := s.metricsDashboard.QuerySessions(ctx, periodDays)
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
		data, err := s.metricsDashboard.QueryModelCalls(ctx, periodDays)
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
		data, err := s.metricsDashboard.QueryTokens(ctx, periodDays)
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
