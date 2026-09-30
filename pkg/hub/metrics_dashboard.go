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
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Design references in this file and the metrics_dashboard_*_test.go files
// (section N, Dn) are to .design/hosted/usage-telemetry.md (ptone/scion#2053).

const (
	metricPrefix  = "workload.googleapis.com/"
	cacheTTL      = 5 * time.Minute
	maxPeriodDays = 90
	defaultPeriod = 7

	// cumulativeLookback bounds how far before the requested window
	// seriesIncreases looks for a series' prior point, so it can compute a
	// delta instead of double-counting a running total (design §3.6).
	cumulativeLookback = 24 * time.Hour
)

// timeSeriesIterator is the subset of *monitoring.TimeSeriesIterator this
// package needs, so tests can supply a fake without depending on the SDK's
// unexported iterator internals (design §7.2).
type timeSeriesIterator interface {
	Next() (*monitoringpb.TimeSeries, error)
}

// metricsClient is a narrow interface over monitoring.MetricClient's
// ListTimeSeries. The dashboard contract test injects a fake that records
// every request filter.
type metricsClient interface {
	ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest, opts ...gax.CallOption) timeSeriesIterator
}

// gcpMetricsClient adapts *monitoring.MetricClient to metricsClient.
type gcpMetricsClient struct{ *monitoring.MetricClient }

func (c gcpMetricsClient) ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest, opts ...gax.CallOption) timeSeriesIterator {
	return c.MetricClient.ListTimeSeries(ctx, req, opts...)
}

// MetricsDashboardService queries Google Cloud Monitoring for Scion telemetry metrics.
type MetricsDashboardService struct {
	client    metricsClient
	rawClient *monitoring.MetricClient // non-nil only for the real GCP client; owns Close.
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
		client:    gcpMetricsClient{client},
		rawClient: client,
		projectID: projectID,
		cache:     make(map[string]*cacheEntry),
	}, nil
}

// Close releases resources.
func (s *MetricsDashboardService) Close() error {
	if s.rawClient == nil {
		return nil
	}
	return s.rawClient.Close()
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
	CacheRead  []LabeledTimeSeries `json:"cacheRead"`
	CacheWrite []LabeledTimeSeries `json:"cacheWrite"`
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

type metricsQueryWindow struct {
	start       time.Time
	end         time.Time
	extraFilter []string
}

func metricsQueryWindowFor(now time.Time, periodDays int, cfg *queryConfig) metricsQueryWindow {
	now = now.UTC()
	window := metricsQueryWindow{
		start: now.AddDate(0, 0, -periodDays),
		end:   now,
	}
	if cfg.ProjectID != "" {
		window.extraFilter = []string{projectFilter(cfg.ProjectID)}
	}
	return window
}

type groupedTimeSeriesQuery struct {
	metricName string
	groupBy    string
	errorLabel string
	// extraFilter is appended on top of the window's own extraFilter (the
	// project filter, if any) — used for a fixed token_type value.
	extraFilter []string
}

func queryGroupedTimeSeriesSet(
	queries []groupedTimeSeriesQuery,
	run func(metricName, groupBy string, extraFilter []string) ([]LabeledTimeSeries, error),
) ([][]LabeledTimeSeries, error) {
	results := make([][]LabeledTimeSeries, len(queries))
	var queryErrors []string
	for i, query := range queries {
		series, err := run(query.metricName, query.groupBy, query.extraFilter)
		if err != nil {
			queryErrors = append(queryErrors, fmt.Sprintf("%s: %v", query.errorLabel, err))
			continue
		}
		results[i] = series
	}
	if len(queryErrors) > 0 {
		return results, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}
	return results, nil
}

func queryGroupedMetricsView[T any](
	s *MetricsDashboardService,
	ctx context.Context,
	cachePrefix string,
	periodDays int,
	opts []QueryOption,
	queries []groupedTimeSeriesQuery,
	build func([][]LabeledTimeSeries) *T,
) (*T, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("%s:%d%s", cachePrefix, periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*T), nil
	}

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)
	combined := make([]groupedTimeSeriesQuery, len(queries))
	for i, q := range queries {
		combined[i] = q
		combined[i].extraFilter = append(append([]string{}, window.extraFilter...), q.extraFilter...)
	}
	series, err := queryGroupedTimeSeriesSet(combined, func(metricName, groupBy string, extraFilter []string) ([]LabeledTimeSeries, error) {
		return s.queryGroupedTimeSeries(ctx, metricName, groupBy, window.start, window.end, extraFilter)
	})
	view := build(series)
	if err != nil {
		return view, err
	}

	s.setCache(cacheKey, view)
	return view, nil
}

// projectFilter returns the Cloud Monitoring filter clause for the
// canonical, exporter-stamped project identity label (design D3). This is
// deliberately not the producer point label "project_id": that name is
// overloaded with the GCP project, and only hook metrics used to set it.
func projectFilter(projectID string) string {
	return fmt.Sprintf(`metric.labels.%s = "%s"`, telemetrycontract.ProjectLabel, projectID)
}

// notSummableTokenTypeFilter excludes TokenTypeReasoning, an informational
// subset of "output" that would double count if summed alongside it (design
// §3.2, §3.6 "DashboardSummary.TotalTokens").
func notSummableTokenTypeFilter() string {
	return fmt.Sprintf(`metric.labels.%s != "%s"`, telemetrycontract.TokenTypeLabel, telemetrycontract.TokenTypeReasoning)
}

// tokenTypeFilter restricts a query to one token_type value.
func tokenTypeFilter(tokenType string) string {
	return fmt.Sprintf(`metric.labels.%s = "%s"`, telemetrycontract.TokenTypeLabel, tokenType)
}

// QuerySummary returns aggregate metric counts for the given period.
func (s *MetricsDashboardService) QuerySummary(ctx context.Context, periodDays int, opts ...QueryOption) (*DashboardSummary, error) {
	cfg := applyQueryOptions(opts)
	cacheKey := fmt.Sprintf("summary:%d%s", periodDays, cfg.cacheKeySuffix())
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*DashboardSummary), nil
	}

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)

	summary := &DashboardSummary{PeriodDays: periodDays}
	var queryErrors []string

	sessions, err := s.querySum(ctx, telemetrycontract.MetricSessionCount, window.start, window.end, window.extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("session count: %v", err))
	} else {
		summary.TotalSessions = sessions
	}

	apiCalls, err := s.querySum(ctx, telemetrycontract.MetricAPICalls, window.start, window.end, window.extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("API calls: %v", err))
	} else {
		summary.TotalAPICalls = apiCalls
	}

	tokenFilter := append(append([]string{}, window.extraFilter...), notSummableTokenTypeFilter())
	tokens, err := s.querySum(ctx, telemetrycontract.MetricUsageTokens, window.start, window.end, tokenFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("tokens: %v", err))
	} else {
		summary.TotalTokens = tokens
	}

	agents, err := s.queryUniqueLabels(ctx, telemetrycontract.MetricSessionCount, "metric.labels."+telemetrycontract.AgentLabel, window.start, window.end, window.extraFilter)
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

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)

	view := &SessionsView{PeriodDays: periodDays}
	var queryErrors []string

	// DailyCounts is a sum of session-count deltas, so it goes through
	// queryDailyTimeSeries -> seriesIncreases, the cumulative-math fix
	// (design §3.6). ActiveAgents below is presence, not a sum, so it stays
	// on queryDailyUniqueCount instead (see that function's comment).
	dailyCounts, err := s.queryDailyTimeSeries(ctx, telemetrycontract.MetricSessionCount, window.start, window.end, window.extraFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("daily sessions: %v", err))
	} else {
		view.DailyCounts = dailyCounts
	}

	activeAgents, err := s.queryDailyUniqueCount(ctx, telemetrycontract.MetricSessionCount, "metric.labels."+telemetrycontract.AgentLabel, window.start, window.end, window.extraFilter)
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
	return queryGroupedMetricsView(s, ctx, "model-calls", periodDays, opts, []groupedTimeSeriesQuery{
		{metricName: telemetrycontract.MetricAPICalls, groupBy: "metric.labels.model", errorLabel: "by model"},
		{metricName: telemetrycontract.MetricAPICalls, groupBy: "metric.labels.harness", errorLabel: "by harness"},
	}, func(series [][]LabeledTimeSeries) *ModelCallsView {
		return &ModelCallsView{PeriodDays: periodDays, ByModel: series[0], ByHarness: series[1]}
	})
}

// QueryTokens returns token usage data grouped by model, one series set per
// token_type (design §3.6: "grouped by metric.labels.model and filtered by
// token_type"). TokenTypeReasoning is not queried here: it is an
// informational subset of TokenTypeOutput, not an additive view.
func (s *MetricsDashboardService) QueryTokens(ctx context.Context, periodDays int, opts ...QueryOption) (*TokensView, error) {
	return queryGroupedMetricsView(s, ctx, "tokens", periodDays, opts, []groupedTimeSeriesQuery{
		{metricName: telemetrycontract.MetricUsageTokens, groupBy: "metric.labels.model", errorLabel: "input tokens", extraFilter: []string{tokenTypeFilter(telemetrycontract.TokenTypeInput)}},
		{metricName: telemetrycontract.MetricUsageTokens, groupBy: "metric.labels.model", errorLabel: "output tokens", extraFilter: []string{tokenTypeFilter(telemetrycontract.TokenTypeOutput)}},
		{metricName: telemetrycontract.MetricUsageTokens, groupBy: "metric.labels.model", errorLabel: "cache read tokens", extraFilter: []string{tokenTypeFilter(telemetrycontract.TokenTypeCacheRead)}},
		{metricName: telemetrycontract.MetricUsageTokens, groupBy: "metric.labels.model", errorLabel: "cache write tokens", extraFilter: []string{tokenTypeFilter(telemetrycontract.TokenTypeCacheWrite)}},
	}, func(series [][]LabeledTimeSeries) *TokensView {
		return &TokensView{PeriodDays: periodDays, Input: series[0], Output: series[1], CacheRead: series[2], CacheWrite: series[3]}
	})
}

// fetchTimeSeries lists every raw point for metricName within
// [fetchStart, windowEnd], with no Cloud Monitoring aggregation (ALIGN_SUM is
// invalid for CUMULATIVE; ALIGN_DELTA yields 0 for sparse data). A NotFound
// error — no descriptor yet, for example scion.usage.tokens before its first
// write — is treated as zero series, not a partial failure (design §3.6
// "Absent metrics").
func (s *MetricsDashboardService) fetchTimeSeries(ctx context.Context, metricName string, fetchStart, windowEnd time.Time, extraFilter []string) ([]*monitoringpb.TimeSeries, error) {
	filter := fmt.Sprintf(`metric.type = "%s%s"`, metricPrefix, metricName)
	for _, f := range extraFilter {
		filter += " AND " + f
	}

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", s.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(fetchStart),
			EndTime:   timestamppb.New(windowEnd),
		},
	}

	var result []*monitoringpb.TimeSeries
	it := s.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("listing time series for %s: %w", metricName, err)
		}
		result = append(result, ts)
	}
	return result, nil
}

// seriesIncrement is one flush's contribution to a cumulative counter,
// attributed to the day its collection interval ended.
type seriesIncrement struct {
	End   time.Time
	Value int64
}

// seriesIncreases converts one time series' raw cumulative points into
// per-flush increments — the cumulative-math fix (design §3.6). sciontool
// exports every counter as CUMULATIVE from a collector epoch, re-sending the
// running total on every flush (metric_streams.go; documented in
// metrics.md's "collector observation epoch"), so summing raw points
// inflates the total by the flush count. Points are partitioned by their
// interval's start time (each start is one collector epoch; a new start is a
// reset), sorted by end time within a partition, and each point's increment
// is the delta from the previous point in that partition. A partition's
// first point contributes its full value only if its epoch began at or after
// fetchStart (the epoch began inside the fetched window); otherwise it is a
// pre-window baseline and contributes 0. A stream is exported only when
// dirty (metric_streams.go), so an epoch that began before fetchStart and
// stayed idle through the whole lookback window has no point before its
// first in-window flush — that first flush is then treated as the baseline
// and its own increment is the one that goes uncounted, not some proportional
// share of the epoch's value. This undercount is bounded to one increment
// per idle-then-active stream — accepted, and documented here rather than
// hidden. Only Int64Value and DoubleValue (rounded) points are read;
// canonical usage metrics are never distributions.
func seriesIncreases(points []*monitoringpb.Point, fetchStart time.Time) []seriesIncrement {
	type observedPoint struct {
		start, end time.Time
		value      int64
	}
	byStart := make(map[int64][]observedPoint)
	for _, p := range points {
		// p == nil or a nil Value can't panic (the getters are nil-safe), but
		// a nil Interval is a real fix: GetStartTime().AsTime() on it would
		// bucket the point at the Unix epoch instead of skipping it.
		if p == nil || p.GetValue() == nil || p.GetInterval() == nil {
			continue
		}
		value, ok := pointValue(p.GetValue())
		if !ok {
			continue
		}
		start := p.GetInterval().GetStartTime().AsTime()
		end := p.GetInterval().GetEndTime().AsTime()
		key := start.UnixNano()
		byStart[key] = append(byStart[key], observedPoint{start: start, end: end, value: value})
	}
	starts := make([]int64, 0, len(byStart))
	for k := range byStart {
		starts = append(starts, k)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	var out []seriesIncrement
	for _, k := range starts {
		group := byStart[k]
		sort.Slice(group, func(i, j int) bool { return group[i].end.Before(group[j].end) })
		var previous int64
		for i, point := range group {
			var increment int64
			switch {
			case i == 0:
				if !point.start.Before(fetchStart) {
					increment = point.value
				}
			case point.value >= previous:
				increment = point.value - previous
			default:
				// A monotonic counter should never decrease; admission
				// already rejects that. Defensive floor only.
				increment = 0
			}
			previous = point.value
			out = append(out, seriesIncrement{End: point.end, Value: increment})
		}
	}
	return out
}

// pointValue reads a monitoring point's numeric value. Distribution and
// other kinds return ok=false: canonical usage metrics are never
// distributions, and this is defensive against a future double producer.
// The nil check below is explicit-for-clarity only: v.GetValue() is already
// nil-safe and would reach the same ok=false default on its own.
func pointValue(v *monitoringpb.TypedValue) (int64, bool) {
	if v == nil {
		return 0, false
	}
	switch v.GetValue().(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return v.GetInt64Value(), true
	case *monitoringpb.TypedValue_DoubleValue:
		return int64(math.Round(v.GetDoubleValue())), true
	default:
		return 0, false
	}
}

// querySum queries a metric and returns the cumulative-corrected total
// across every time series in the window.
func (s *MetricsDashboardService) querySum(ctx context.Context, metricName string, start, end time.Time, extraFilter []string) (int64, error) {
	fetchStart := start.Add(-cumulativeLookback)
	series, err := s.fetchTimeSeries(ctx, metricName, fetchStart, end, extraFilter)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ts := range series {
		for _, increment := range seriesIncreases(ts.GetPoints(), fetchStart) {
			if increment.End.Before(start) || increment.End.After(end) {
				continue
			}
			total += increment.Value
		}
	}
	return total, nil
}

// queryDailyTimeSeries returns daily, cumulative-corrected totals for a metric.
func (s *MetricsDashboardService) queryDailyTimeSeries(ctx context.Context, metricName string, start, end time.Time, extraFilter []string) ([]TimeSeriesPoint, error) {
	fetchStart := start.Add(-cumulativeLookback)
	series, err := s.fetchTimeSeries(ctx, metricName, fetchStart, end, extraFilter)
	if err != nil {
		return nil, err
	}

	dayTotals := make(map[string]int64)
	for _, ts := range series {
		for _, increment := range seriesIncreases(ts.GetPoints(), fetchStart) {
			if increment.End.Before(start) || increment.End.After(end) {
				continue
			}
			dayTotals[increment.End.Format("2006-01-02")] += increment.Value
		}
	}

	points := make([]TimeSeriesPoint, 0, len(dayTotals))
	for day, total := range dayTotals {
		points = append(points, TimeSeriesPoint{Timestamp: day, Value: total})
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp < points[j].Timestamp
	})
	return points, nil
}

// labelKeyFromGroupBy extracts the short label key from a Cloud Monitoring
// groupByLabel like "metric.labels.model" → "model".
func labelKeyFromGroupBy(groupByLabel string) string {
	parts := strings.Split(groupByLabel, ".")
	return parts[len(parts)-1]
}

// queryGroupedTimeSeries returns daily, cumulative-corrected totals grouped
// by a label.
func (s *MetricsDashboardService) queryGroupedTimeSeries(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) ([]LabeledTimeSeries, error) {
	fetchStart := start.Add(-cumulativeLookback)
	series, err := s.fetchTimeSeries(ctx, metricName, fetchStart, end, extraFilter)
	if err != nil {
		return nil, err
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	// label -> day -> total
	seriesDayTotals := make(map[string]map[string]int64)
	for _, ts := range series {
		label := "(unknown)"
		if labels := ts.GetMetric().GetLabels(); labels != nil {
			if v, ok := labels[labelKey]; ok && v != "" {
				label = v
			}
		}
		if seriesDayTotals[label] == nil {
			seriesDayTotals[label] = make(map[string]int64)
		}
		for _, increment := range seriesIncreases(ts.GetPoints(), fetchStart) {
			if increment.End.Before(start) || increment.End.After(end) {
				continue
			}
			seriesDayTotals[label][increment.End.Format("2006-01-02")] += increment.Value
		}
	}

	var result []LabeledTimeSeries
	for label, dayTotals := range seriesDayTotals {
		points := make([]TimeSeriesPoint, 0, len(dayTotals))
		for day, total := range dayTotals {
			points = append(points, TimeSeriesPoint{Timestamp: day, Value: total})
		}
		sort.Slice(points, func(i, j int) bool {
			return points[i].Timestamp < points[j].Timestamp
		})
		result = append(result, LabeledTimeSeries{Label: label, Points: points})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Label < result[j].Label
	})
	return result, nil
}

// queryUniqueLabels returns unique values for a label within a metric's time
// series in the window (presence only — not cumulative-corrected, since
// membership doesn't need a delta).
func (s *MetricsDashboardService) queryUniqueLabels(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) (map[string]bool, error) {
	series, err := s.fetchTimeSeries(ctx, metricName, start, end, extraFilter)
	if err != nil {
		return nil, err
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	unique := make(map[string]bool)
	for _, ts := range series {
		if labels := ts.GetMetric().GetLabels(); labels != nil {
			if v, ok := labels[labelKey]; ok && v != "" {
				unique[v] = true
			}
		}
	}
	return unique, nil
}

// queryDailyUniqueCount returns per-day counts of unique label values —
// presence, like queryUniqueLabels, not seriesIncreases' delta math: an
// agent counts as active on a day if its stream has any positive point that
// day, regardless of the stream's running total. It is not cumulative-
// corrected for a second reason beyond queryUniqueLabels' "membership
// doesn't need a delta": a hook-sourced stream exports only when dirty
// (metric_streams.go), so presence already approximates "had activity" as
// well as a delta would, without needing a baseline point before the
// window.
func (s *MetricsDashboardService) queryDailyUniqueCount(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string) ([]TimeSeriesPoint, error) {
	series, err := s.fetchTimeSeries(ctx, metricName, start, end, extraFilter)
	if err != nil {
		return nil, err
	}
	labelKey := labelKeyFromGroupBy(groupByLabel)

	// Count unique label values per day
	dayAgents := make(map[string]map[string]bool) // date -> set of label values
	for _, ts := range series {
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

	sessions, err := s.querySum(ctx, telemetrycontract.MetricSessionCount, start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("sessions: %v", err))
	} else {
		summary.SessionsCount24h = sessions
	}

	apiCalls, err := s.querySum(ctx, telemetrycontract.MetricAPICalls, start, now, filter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("API calls: %v", err))
	} else {
		summary.APICalls24h = apiCalls
	}

	tokenFilter := append(append([]string{}, filter...), notSummableTokenTypeFilter())
	tokens, err := s.querySum(ctx, telemetrycontract.MetricUsageTokens, start, now, tokenFilter)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("tokens: %v", err))
	} else {
		summary.TokenUsage24h = tokens
	}

	agents, err := s.queryUniqueLabels(ctx, telemetrycontract.MetricSessionCount, "metric.labels."+telemetrycontract.AgentLabel, start, now, filter)
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

		}
		writeJSON(w, http.StatusOK, data)

	default:
		writeError(w, http.StatusBadRequest, "invalid_view",
			fmt.Sprintf("Unknown view: %s. Valid views: summary, sessions, model-calls, tokens", view), nil)
	}
}
