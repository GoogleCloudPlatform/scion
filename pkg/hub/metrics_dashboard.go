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
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
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

	// dayKeyLayout is the calendar-day bucket key, formatted in the
	// dashboard's resolved zone (see resolveDashboardTimeZone).
	dayKeyLayout = "2006-01-02"

	// maxTimeZoneParamLen caps the tz query parameter before it reaches
	// time.LoadLocation. The longest IANA name is about 30 bytes.
	maxTimeZoneParamLen = 64

	// maxCachedZones bounds how many distinct non-UTC zones the dashboard
	// cache holds at once. A viewer's zone is client-supplied, so without a
	// bound a client cycling through every IANA name could multiply the
	// cache by ~600. Past the cap, results for a new zone are computed but
	// not cached until older zones' entries expire (cacheTTL).
	maxCachedZones = 16
)

// timeZoneParamShape is the conservative shape a tz query parameter must
// have before time.LoadLocation sees it: one to three '/'-separated segments,
// each starting with an ASCII letter and otherwise letters, digits, '_', '+'
// or '-' ("UTC", "Asia/Kathmandu", "Etc/GMT+5",
// "America/Argentina/Buenos_Aires", "America/Port-au-Prince"). It rules out
// '.', a leading '/', and '\', so a value can never name a file path:
// time.LoadLocation opens the name under the zoneinfo directory, and it
// also honours the ZONEINFO environment variable.
var timeZoneParamShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z][A-Za-z0-9_+-]*){0,2}$`)

// utcZoneAliases are the tzdata names that are exactly UTC (offset zero, no
// DST, ever). They resolve to time.UTC, so they share the UTC cache entries,
// do not use a slot under maxCachedZones, and get the "(UTC)" labels. Some
// browsers report "Etc/UTC" or "Etc/GMT" for a UTC host.
var utcZoneAliases = map[string]bool{
	"UTC":           true,
	"Etc/UTC":       true,
	"UCT":           true,
	"Etc/UCT":       true,
	"Universal":     true,
	"Etc/Universal": true,
	"Zulu":          true,
	"Etc/Zulu":      true,
	"GMT":           true,
	"Etc/GMT":       true,
	"GMT0":          true,
	"Etc/GMT0":      true,
	"GMT+0":         true,
	"GMT-0":         true,
	"Etc/GMT+0":     true,
	"Etc/GMT-0":     true,
	"Greenwich":     true,
	"Etc/Greenwich": true,
}

// resolveDashboardTimeZone turns the dashboard's tz query parameter into the
// zone whose calendar days the time series are bucketed by. It never fails:
// an empty, "Local", oversized, malformed, non-portable or unknown value
// resolves to UTC, so a bad tz degrades to the pre-existing UTC buckets
// instead of failing the request. "Local" is rejected because it would be
// the hub process's own zone, not the viewer's.
//
// The length, path and shape checks come first because the value is
// untrusted request input; the portability rules are the package's shared
// validateIANATimezone (timezone_validate.go).
func resolveDashboardTimeZone(tz string) *time.Location {
	if tz == "" || tz == "Local" || len(tz) > maxTimeZoneParamLen {
		return time.UTC
	}
	if strings.Contains(tz, "..") || strings.HasPrefix(tz, "/") || strings.ContainsRune(tz, '\\') {
		return time.UTC
	}
	if !timeZoneParamShape.MatchString(tz) {
		return time.UTC
	}
	if utcZoneAliases[tz] {
		return time.UTC
	}
	if validateIANATimezone(tz) != nil {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// dayKey is the calendar day t falls on in loc.
func dayKey(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(dayKeyLayout)
}

// startOfLocalDay returns the first instant of the calendar day (y, m, d)
// in loc. d may be out of range; it is normalised like time.Date. In a zone
// whose DST transition skips midnight (for example America/Santiago,
// 00:00 -> 01:00), time.Date may resolve the missing midnight to 23:00 on
// the previous day, so the result is advanced hour by hour until it lands on
// the intended day.
func startOfLocalDay(y int, m time.Month, d int, loc *time.Location) time.Time {
	noon := time.Date(y, m, d, 12, 0, 0, 0, loc)
	want := dayKey(noon, loc)
	start := time.Date(noon.Year(), noon.Month(), noon.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 3 && dayKey(start, loc) != want; i++ {
		start = start.Add(time.Hour)
	}
	return start
}

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

// Metrics dashboard data source identifiers surfaced in response payloads.
const (
	MetricsSourceCloudMonitoring = "cloud_monitoring"
	MetricsSourceHubRollups      = "hub_rollups"
)

// MetricsDashboardSource abstracts the backing telemetry data source for the
// metrics dashboard (CloudMonitoringSource or HubRollupSource).
type MetricsDashboardSource interface {
	Name() string
	QuerySummary(ctx context.Context, periodDays int, opts ...QueryOption) (*DashboardSummary, error)
	QuerySessions(ctx context.Context, periodDays int, opts ...QueryOption) (*SessionsView, error)
	QueryModelCalls(ctx context.Context, periodDays int, opts ...QueryOption) (*ModelCallsView, error)
	QueryTokens(ctx context.Context, periodDays int, opts ...QueryOption) (*TokensView, error)
	QueryProjectSummary(ctx context.Context, projectID string) (*ProjectMetricsSummary, error)
}

// CloudMonitoringSource is an alias for MetricsDashboardService, which queries
// Google Cloud Monitoring and implements MetricsDashboardSource.
type CloudMonitoringSource = MetricsDashboardService

// MetricsDashboardService queries Google Cloud Monitoring for Scion telemetry metrics.
type MetricsDashboardService struct {
	client    metricsClient
	rawClient *monitoring.MetricClient // non-nil only for the real GCP client; owns Close.
	projectID string

	mu    sync.RWMutex
	cache map[string]*cacheEntry
}

// Name returns the data source identifier for Cloud Monitoring.
func (s *MetricsDashboardService) Name() string {
	return MetricsSourceCloudMonitoring
}

type cacheEntry struct {
	data      interface{}
	fetchedAt time.Time
	// zone is the resolved bucketing zone name for zoned dashboard views,
	// and "" for entries that do not depend on a zone.
	zone string
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

// ProjectID returns the GCP project ID currently targeted by this service.
func (s *MetricsDashboardService) ProjectID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.projectID
}

// SetProjectID retargets this service to query Cloud Monitoring in projectID
// and clears any cached responses from the previous project.
func (s *MetricsDashboardService) SetProjectID(projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectID == projectID {
		return
	}
	s.projectID = projectID
	s.cache = make(map[string]*cacheEntry)
}

// Close releases resources.
func (s *MetricsDashboardService) Close() error {
	if s.rawClient == nil {
		return nil
	}
	return s.rawClient.Close()
}

// DashboardTokenTotals holds nullable canonical Track 1 token totals and the
// derived cache hit rate (cache_read / (input + cache_read + cache_write)).
// Unknown buckets serialize as JSON null and are never fabricated as 0.
type DashboardTokenTotals struct {
	Input          *int64   `json:"input"`
	Output         *int64   `json:"output"`
	CacheRead      *int64   `json:"cacheRead"`
	CacheWrite     *int64   `json:"cacheWrite"`
	CacheWrite1h   *int64   `json:"cacheWrite1h"`
	Reasoning      *int64   `json:"reasoning"`
	SummableTokens *int64   `json:"summableTokens"`
	CacheHitRate   *float64 `json:"cacheHitRate"`
	Saturated      bool     `json:"saturated,omitempty"`
}

// DashboardMeasurement summarizes Track 1 measurement coverage for a dashboard
// view (derived through the single deriveMeasurementStatus function).
type DashboardMeasurement struct {
	Status           string     `json:"status"`
	UnmeasuredAgents int        `json:"unmeasuredAgents"`
	StaleAgents      int        `json:"staleAgents"`
	LastReceiptAt    *time.Time `json:"lastReceiptAt,omitempty"`
}

// NullableFloatPoint represents a time-series point whose value may be unknown
// (JSON null), such as daily cache hit rate when tokens are unmeasured or the
// denominator is zero.
type NullableFloatPoint struct {
	Timestamp string   `json:"timestamp"`
	Value     *float64 `json:"value"`
}

// DashboardSummary contains aggregate metric counts for a period.
type DashboardSummary struct {
	PeriodDays int `json:"periodDays"`
	// TimeZone is the resolved IANA zone whose local midnight starts the
	// period window ("UTC" when the request named no valid zone or when
	// HubRollupSource falls back to daily UTC rollups).
	TimeZone string `json:"timeZone"`
	// Source identifies the backing data source ("cloud_monitoring" or "hub_rollups").
	Source string `json:"source,omitempty"`
	// Granularity reports the rollup grain used by HubRollupSource ("hour" or "day").
	Granularity   string `json:"granularity,omitempty"`
	TotalSessions int64  `json:"totalSessions"`
	TotalAPICalls int64  `json:"totalApiCalls"`
	TotalTokens   int64  `json:"totalTokens"`
	UniqueAgents  int    `json:"uniqueAgents"`
	// Tokens carries nullable canonical token buckets when served from Hub rollups.
	Tokens *DashboardTokenTotals `json:"tokens,omitempty"`
	// CacheHitRate is cache_read / (input + cache_read + cache_write), or nil when
	// any component is unknown or the denominator is zero.
	CacheHitRate *float64 `json:"cacheHitRate,omitempty"`
	// Measurement surfaces Track 1 coverage status and agent counts.
	Measurement *DashboardMeasurement `json:"measurement,omitempty"`
	// TelemetryHint surfaces an actionable setup hint when cloud telemetry
	// cannot export under the current hub/project configuration (for example,
	// GCP cloud telemetry on in metadata block mode without credentials).
	TelemetryHint string `json:"telemetryHint,omitempty"`
}

const missingGCPTelemetryCredentialsHint = "Cloud telemetry is enabled for GCP, but the default metadata mode is block and the scion-telemetry-gcp-credentials secret is not configured. Agents without an assigned service account cannot export metrics to Cloud Monitoring, so this dashboard will stay empty. Configure the scion-telemetry-gcp-credentials Hub secret, assign a service account (--service-account), or remove provider: gcp and point telemetry.cloud.endpoint at a local OpenTelemetry Collector."

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
	PeriodDays int `json:"periodDays"`
	// TimeZone is the resolved IANA zone the day buckets are calendar days
	// of ("UTC" when the request named no valid zone or when HubRollupSource
	// falls back to daily UTC rollups).
	TimeZone     string                `json:"timeZone"`
	Source       string                `json:"source,omitempty"`
	Granularity  string                `json:"granularity,omitempty"`
	DailyCounts  []TimeSeriesPoint     `json:"dailyCounts"`
	ActiveAgents []TimeSeriesPoint     `json:"activeAgents"`
	Measurement  *DashboardMeasurement `json:"measurement,omitempty"`
}

// ModelCallsView contains API call data grouped by model, harness, and
// (when served from Hub rollups) optionally by user.
type ModelCallsView struct {
	PeriodDays int `json:"periodDays"`
	// TimeZone is the resolved IANA zone the day buckets are calendar days of.
	TimeZone    string                `json:"timeZone"`
	Source      string                `json:"source,omitempty"`
	Granularity string                `json:"granularity,omitempty"`
	GroupBy     string                `json:"groupBy,omitempty"`
	ByModel     []LabeledTimeSeries   `json:"byModel"`
	ByHarness   []LabeledTimeSeries   `json:"byHarness"`
	ByUser      []LabeledTimeSeries   `json:"byUser,omitempty"`
	Measurement *DashboardMeasurement `json:"measurement,omitempty"`
}

// TokensView contains token usage data grouped by model (or harness/user when
// requested via groupBy on HubRollupSource).
type TokensView struct {
	PeriodDays int `json:"periodDays"`
	// TimeZone is the resolved IANA zone the day buckets are calendar days of.
	TimeZone          string                `json:"timeZone"`
	Source            string                `json:"source,omitempty"`
	Granularity       string                `json:"granularity,omitempty"`
	GroupBy           string                `json:"groupBy,omitempty"`
	Input             []LabeledTimeSeries   `json:"input"`
	Output            []LabeledTimeSeries   `json:"output"`
	CacheRead         []LabeledTimeSeries   `json:"cacheRead"`
	CacheWrite        []LabeledTimeSeries   `json:"cacheWrite"`
	Totals            *DashboardTokenTotals `json:"totals,omitempty"`
	DailyCacheHitRate []NullableFloatPoint  `json:"dailyCacheHitRate,omitempty"`
	Measurement       *DashboardMeasurement `json:"measurement,omitempty"`
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
	s.setZonedCache(key, "", data)
}

func (s *MetricsDashboardService) setCacheForProject(projectID, key string, data interface{}) {
	s.setZonedCacheForProject(projectID, key, "", data)
}

// setZonedCache stores a view computed for the resolved zone name zone. It
// first drops expired entries, so the map does not grow without bound. A
// zone other than UTC is cached only while fewer than maxCachedZones
// distinct non-UTC zones have live entries (or this zone already has one);
// otherwise the view is returned uncached. It reports whether it cached.
func (s *MetricsDashboardService) setZonedCache(key, zone string, data interface{}) bool {
	return s.setZonedCacheForProject(s.ProjectID(), key, zone, data)
}

// setZonedCacheForProject stores a view only if the service is still targeting
// expectedProjectID, preventing an in-flight query started before a
// SetProjectID retarget from populating the new project's cache.
func (s *MetricsDashboardService) setZonedCacheForProject(expectedProjectID, key, zone string, data interface{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectID != expectedProjectID {
		return false
	}
	now := time.Now()
	for k, e := range s.cache {
		if now.Sub(e.fetchedAt) > cacheTTL {
			delete(s.cache, k)
		}
	}
	if zone != "" && zone != "UTC" {
		zones := make(map[string]bool)
		for _, e := range s.cache {
			if e.zone != "" && e.zone != "UTC" {
				zones[e.zone] = true
			}
		}
		if !zones[zone] && len(zones) >= maxCachedZones {
			return false
		}
	}
	s.cache[key] = &cacheEntry{data: data, fetchedAt: now, zone: zone}
	return true
}

// QueryOption configures optional query parameters.
type QueryOption func(*queryConfig)

type queryConfig struct {
	ProjectID string
	// Location is the zone whose calendar days the series are bucketed by.
	// nil means UTC.
	Location *time.Location
	// GroupBy requests a breakdown dimension ("model", "harness", "user") on
	// grouped views (supported by HubRollupSource; empty means default).
	GroupBy string
}

// WithProjectID filters metrics to a specific project.
func WithProjectID(id string) QueryOption {
	return func(c *queryConfig) { c.ProjectID = id }
}

// WithTimeZone buckets daily series by calendar day in loc, and starts the
// period window at local midnight in loc. Pass a zone already resolved by
// resolveDashboardTimeZone; nil means UTC.
func WithTimeZone(loc *time.Location) QueryOption {
	return func(c *queryConfig) { c.Location = loc }
}

// WithGroupBy requests a specific grouping dimension ("model", "harness", or "user").
func WithGroupBy(dim string) QueryOption {
	return func(c *queryConfig) { c.GroupBy = dim }
}

// location returns the bucketing zone, UTC when none was set.
func (c *queryConfig) location() *time.Location {
	if c.Location == nil {
		return time.UTC
	}
	return c.Location
}

// zoneName returns the resolved bucketing zone's name ("UTC" by default).
func (c *queryConfig) zoneName() string {
	return c.location().String()
}

func applyQueryOptions(opts []QueryOption) *queryConfig {
	cfg := &queryConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// cacheKey returns the dashboard cache key for a view: the view prefix, the
// period, the resolved zone (bucket keys and the window start depend on it)
// and, for project-scoped queries, the project ID.
func (c *queryConfig) cacheKey(prefix string, periodDays int) string {
	key := fmt.Sprintf("%s:%d:tz=%s", prefix, periodDays, c.zoneName())
	if c.ProjectID != "" {
		key += ":" + c.ProjectID
	}
	return key
}

type metricsQueryWindow struct {
	start       time.Time
	end         time.Time
	extraFilter []string
}

// metricsQueryWindowFor returns the window for "the last periodDays
// calendar days" in the query's zone: from local midnight periodDays-1 days
// before today (so today is the last of periodDays buckets) to now. The
// start is computed on the local calendar, not by subtracting 24h steps, so
// a window spanning a DST change starts at a real local midnight and its
// transition day is 23 or 25 hours long.
func metricsQueryWindowFor(now time.Time, periodDays int, cfg *queryConfig) metricsQueryWindow {
	if periodDays < 1 {
		periodDays = 1
	}
	loc := cfg.location()
	y, m, d := now.In(loc).Date()
	window := metricsQueryWindow{
		start: startOfLocalDay(y, m, d-(periodDays-1), loc).UTC(),
		end:   now.UTC(),
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
	gcpProject := s.ProjectID()
	cacheKey := cfg.cacheKey(cachePrefix, periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*T), nil
	}
	loc := cfg.location()

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)
	combined := make([]groupedTimeSeriesQuery, len(queries))
	for i, q := range queries {
		combined[i] = q
		combined[i].extraFilter = append(append([]string{}, window.extraFilter...), q.extraFilter...)
	}
	series, err := queryGroupedTimeSeriesSet(combined, func(metricName, groupBy string, extraFilter []string) ([]LabeledTimeSeries, error) {
		return s.queryGroupedTimeSeries(ctx, metricName, groupBy, window.start, window.end, extraFilter, loc)
	})
	view := build(series)
	if err != nil {
		return view, err
	}

	s.setZonedCacheForProject(gcpProject, cacheKey, cfg.zoneName(), view)
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
	gcpProject := s.ProjectID()
	cacheKey := cfg.cacheKey("summary", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*DashboardSummary), nil
	}

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)

	summary := &DashboardSummary{PeriodDays: periodDays, TimeZone: cfg.zoneName(), Source: MetricsSourceCloudMonitoring}
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

	s.setZonedCacheForProject(gcpProject, cacheKey, cfg.zoneName(), summary)
	return summary, nil
}

// QuerySessions returns daily session counts and active agent counts.
func (s *MetricsDashboardService) QuerySessions(ctx context.Context, periodDays int, opts ...QueryOption) (*SessionsView, error) {
	cfg := applyQueryOptions(opts)
	gcpProject := s.ProjectID()
	cacheKey := cfg.cacheKey("sessions", periodDays)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*SessionsView), nil
	}

	window := metricsQueryWindowFor(time.Now(), periodDays, cfg)

	loc := cfg.location()
	view := &SessionsView{PeriodDays: periodDays, TimeZone: cfg.zoneName(), Source: MetricsSourceCloudMonitoring}
	var queryErrors []string

	// DailyCounts is a sum of session-count deltas, so it goes through
	// queryDailyTimeSeries -> seriesIncreases, the cumulative-math fix
	// (design §3.6). ActiveAgents below is presence, not a sum, so it stays
	// on queryDailyUniqueCount instead (see that function's comment).
	dailyCounts, err := s.queryDailyTimeSeries(ctx, telemetrycontract.MetricSessionCount, window.start, window.end, window.extraFilter, loc)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("daily sessions: %v", err))
	} else {
		view.DailyCounts = dailyCounts
	}

	activeAgents, err := s.queryDailyUniqueCount(ctx, telemetrycontract.MetricSessionCount, "metric.labels."+telemetrycontract.AgentLabel, window.start, window.end, window.extraFilter, loc)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("active agents: %v", err))
	} else {
		view.ActiveAgents = activeAgents
	}

	if len(queryErrors) > 0 {
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}

	s.setZonedCacheForProject(gcpProject, cacheKey, cfg.zoneName(), view)
	return view, nil
}

// QueryModelCalls returns API call data grouped by model and harness.
func (s *MetricsDashboardService) QueryModelCalls(ctx context.Context, periodDays int, opts ...QueryOption) (*ModelCallsView, error) {
	return queryGroupedMetricsView(s, ctx, "model-calls", periodDays, opts, []groupedTimeSeriesQuery{
		{metricName: telemetrycontract.MetricAPICalls, groupBy: "metric.labels.model", errorLabel: "by model"},
		{metricName: telemetrycontract.MetricAPICalls, groupBy: "metric.labels.harness", errorLabel: "by harness"},
	}, func(series [][]LabeledTimeSeries) *ModelCallsView {
		return &ModelCallsView{PeriodDays: periodDays, TimeZone: zoneNameFor(opts), Source: MetricsSourceCloudMonitoring, ByModel: series[0], ByHarness: series[1]}
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
		return &TokensView{PeriodDays: periodDays, TimeZone: zoneNameFor(opts), Source: MetricsSourceCloudMonitoring, Input: series[0], Output: series[1], CacheRead: series[2], CacheWrite: series[3]}
	})
}

// zoneNameFor returns the resolved bucketing zone name the options select.
func zoneNameFor(opts []QueryOption) string {
	return applyQueryOptions(opts).zoneName()
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
		Name:   fmt.Sprintf("projects/%s", s.ProjectID()),
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

// queryDailyTimeSeries returns daily, cumulative-corrected totals for a
// metric, keyed by the calendar day in loc that each increment's interval
// ended on.
func (s *MetricsDashboardService) queryDailyTimeSeries(ctx context.Context, metricName string, start, end time.Time, extraFilter []string, loc *time.Location) ([]TimeSeriesPoint, error) {
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
			dayTotals[dayKey(increment.End, loc)] += increment.Value
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
// by a label, keyed by calendar day in loc.
func (s *MetricsDashboardService) queryGroupedTimeSeries(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string, loc *time.Location) ([]LabeledTimeSeries, error) {
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
			seriesDayTotals[label][dayKey(increment.End, loc)] += increment.Value
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
// window. Days are calendar days in loc.
func (s *MetricsDashboardService) queryDailyUniqueCount(ctx context.Context, metricName, groupByLabel string, start, end time.Time, extraFilter []string, loc *time.Location) ([]TimeSeriesPoint, error) {
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
			// A nil Interval would otherwise bucket at the Unix epoch.
			if p.GetInterval() == nil {
				continue
			}
			pointEnd := p.GetInterval().GetEndTime().AsTime()
			if pointEnd.Before(start) || pointEnd.After(end) {
				continue
			}
			day := dayKey(pointEnd, loc)
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
	Source           string                `json:"source,omitempty"`
	Granularity      string                `json:"granularity,omitempty"`
	SessionsCount24h int64                 `json:"sessionsCount24h"`
	APICalls24h      int64                 `json:"apiCalls24h"`
	TokenUsage24h    int64                 `json:"tokenUsage24h"`
	ActiveAgents24h  int                   `json:"activeAgents24h"`
	PeriodLabel      string                `json:"periodLabel"`
	Tokens           *DashboardTokenTotals `json:"tokens,omitempty"`
	CacheHitRate     *float64              `json:"cacheHitRate,omitempty"`
	Measurement      *DashboardMeasurement `json:"measurement,omitempty"`
}

// QueryProjectSummary returns lightweight scalar metrics for a project over the last 24 hours.
func (s *MetricsDashboardService) QueryProjectSummary(ctx context.Context, projectID string) (*ProjectMetricsSummary, error) {
	gcpProject := s.ProjectID()
	cacheKey := fmt.Sprintf("project-summary:%s", projectID)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached.(*ProjectMetricsSummary), nil
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -1) // 24 hours

	filter := []string{projectFilter(projectID)}

	summary := &ProjectMetricsSummary{Source: MetricsSourceCloudMonitoring, PeriodLabel: "Last 24 hours"}
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

	s.setCacheForProject(gcpProject, cacheKey, summary)
	return summary, nil
}

// currentMetricsDashboard returns the active MetricsDashboardService, or nil.
func (s *Server) currentMetricsDashboard() *MetricsDashboardService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metricsDashboard
}

// newMetricsDashboardService constructs a MetricsDashboardService using the
// configured factory (or NewMetricsDashboardService in production).
func (s *Server) newMetricsDashboardService(ctx context.Context, projectID string) (*MetricsDashboardService, error) {
	if s.metricsDashboardFactory != nil {
		return s.metricsDashboardFactory(ctx, projectID)
	}
	return NewMetricsDashboardService(ctx, projectID)
}

// applyMetricsDashboardProject updates s.metricsDashboard to target projectID:
// retargeting an existing service in place (clearing its cache), creating a
// new service when previously unconfigured, or closing and clearing it when
// projectID is empty. Callers hold s.settingsApply.mu and must NOT hold s.mu.
func (s *Server) applyMetricsDashboardProject(projectID string) bool {
	cur := s.currentMetricsDashboard()
	curProject := ""
	if cur != nil {
		curProject = cur.ProjectID()
	}
	if curProject == projectID {
		return false
	}
	if projectID == "" {
		s.mu.Lock()
		old := s.metricsDashboard
		s.metricsDashboard = nil
		s.mu.Unlock()
		if old != nil {
			if err := old.Close(); err != nil {
				slog.Warn("Failed to close metrics dashboard", "error", err)
			}
			slog.Info("Metrics dashboard service disabled (no telemetry project ID)")
			return true
		}
		return false
	}
	if cur != nil {
		cur.SetProjectID(projectID)
		slog.Info("Metrics dashboard service retargeted", "previous_project", curProject, "project", projectID)
		return true
	}
	metricsSvc, err := s.newMetricsDashboardService(context.Background(), projectID)
	if err != nil {
		slog.Warn("Failed to initialize metrics dashboard service", "project", projectID, "error", err)
		return false
	}
	s.mu.Lock()
	s.metricsDashboard = metricsSvc
	s.mu.Unlock()
	slog.Info("Metrics dashboard service initialized", "project", projectID)
	return true
}

// requiresHubRollupBreakdown reports whether a dashboard request asks for a
// breakdown dimension that Cloud Monitoring cannot provide (per-user on any
// grouped view, or per-harness on the tokens view).
func requiresHubRollupBreakdown(view, groupBy string) bool {
	if groupBy == "" {
		return false
	}
	if groupBy == store.UsageGroupByUser {
		return true
	}
	if view == "tokens" && groupBy == store.UsageGroupByHarness {
		return true
	}
	return false
}

// validateDashboardGroupBy validates the optional groupBy query parameter on
// metrics dashboard requests.
func validateDashboardGroupBy(view, raw string) (string, error) {
	g := strings.TrimSpace(raw)
	if g == "" {
		return "", nil
	}
	switch g {
	case store.UsageGroupByModel, store.UsageGroupByHarness, store.UsageGroupByUser:
	default:
		return "", fmt.Errorf("invalid groupBy %q: valid values are model, harness, user", g)
	}
	if view != "model-calls" && view != "tokens" {
		return "", fmt.Errorf("groupBy is only supported on model-calls and tokens views")
	}
	return g, nil
}

// selectMetricsDashboardSource chooses the backing MetricsDashboardSource for
// the given view and optional breakdown dimension:
//   - When tokenomics.usage_ledger is OFF: Cloud Monitoring only (nil if
//     unconfigured; callers preserve pre-existing 503 / unavailable behavior).
//   - When tokenomics.usage_ledger is ON:
//     1. Default to HubRollupSource when Cloud Monitoring is unconfigured.
//     2. Route to HubRollupSource for breakdowns Cloud Monitoring cannot
//     provide (per-user, or per-harness on tokens), even when Cloud Monitoring
//     is configured.
//     3. Otherwise use CloudMonitoringSource.
func (s *Server) selectMetricsDashboardSource(view, groupBy string) MetricsDashboardSource {
	cm := s.currentMetricsDashboard()
	ledgerEnabled := s.experimentEnabled(experiments.TokenomicsUsageLedger)
	if !ledgerEnabled {
		if cm == nil {
			return nil
		}
		return cm
	}
	if cm == nil || requiresHubRollupBreakdown(view, groupBy) {
		if s.store != nil {
			return NewHubRollupSource(s.store)
		}
		if cm == nil {
			return nil
		}
		return cm
	}
	return cm
}

// handleProjectMetricsSummary returns lightweight metrics summary for a project.
func (s *Server) handleProjectMetricsSummary(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
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

	// If no metrics source is available, return unavailable indicator.
	dashboard := s.selectMetricsDashboardSource("summary", "")
	if dashboard == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": false})
		return
	}

	data, err := dashboard.QueryProjectSummary(ctx, projectID)
	if err != nil {
		if data == nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to query project metrics summary", nil)
			return
		}
		slog.Warn("Partial project metrics summary failure", "projectID", projectID, "source", dashboard.Name(), "error", err)
	}
	writeJSON(w, http.StatusOK, data)
}

// handleMetricsDashboard serves the hub-wide metrics dashboard API.
// Authorization: the route guard requires hub.metrics.read on the hub
// (route_metadata.go: admin.metricsDashboard); a user access token needs the
// hub_metrics:read selector on a hub boundary.
func (s *Server) handleMetricsDashboard(w http.ResponseWriter, r *http.Request) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}

	s.serveMetricsDashboard(w, r)
}

// handleAdminMetricsDashboard serves the metrics dashboard API on the legacy
// /api/v1/admin/metrics-dashboard path. It applies the same rule as
// handleMetricsDashboard: the route guard requires hub.metrics.read on the
// hub (route_metadata.go: admin.metricsDashboard.legacy).
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
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	view := r.URL.Query().Get("view")
	if view == "" {
		view = "summary"
	}

	groupBy, err := validateDashboardGroupBy(view, r.URL.Query().Get("groupBy"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_group_by", err.Error(), nil)
		return
	}

	dashboard := s.selectMetricsDashboardSource(view, groupBy)
	if dashboard == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics_unavailable",
			"Metrics dashboard is not configured (no telemetry project ID)", nil)
		return
	}
	if requiresHubRollupBreakdown(view, groupBy) && dashboard.Name() != MetricsSourceHubRollups {
		writeError(w, http.StatusBadRequest, "invalid_group_by",
			fmt.Sprintf("groupBy=%s requires the Hub usage ledger", groupBy), nil)
		return
	}

	periodStr := r.URL.Query().Get("period")
	periodDays := defaultPeriod
	if periodStr != "" {
		if p, err := strconv.Atoi(periodStr); err == nil && p > 0 && p <= maxPeriodDays {
			periodDays = p
		}
	}

	// The viewer's zone decides which calendar day a point belongs to. It is
	// resolved once here; an invalid value falls back to UTC rather than
	// failing the request. The cap on opts keeps the append from writing
	// into the caller's backing array.
	loc := resolveDashboardTimeZone(r.URL.Query().Get("tz"))
	opts = append(opts[:len(opts):len(opts)], WithTimeZone(loc))
	if groupBy != "" {
		opts = append(opts, WithGroupBy(groupBy))
	}

	ctx := r.Context()

	switch view {
	case "summary":
		data, err := dashboard.QuerySummary(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query metrics summary", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "source", dashboard.Name(), "error", err)
		}
		if dashboard.Name() == MetricsSourceCloudMonitoring {
			if hint := s.metricsTelemetryHint(ctx, applyQueryOptions(opts).ProjectID); hint != "" {
				cp := *data
				cp.TelemetryHint = hint
				data = &cp
			}
		}
		writeJSON(w, http.StatusOK, data)

	case "sessions":
		data, err := dashboard.QuerySessions(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query session metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "source", dashboard.Name(), "error", err)
		}
		writeJSON(w, http.StatusOK, data)

	case "model-calls":
		data, err := dashboard.QueryModelCalls(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query model call metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "source", dashboard.Name(), "error", err)
		}
		writeJSON(w, http.StatusOK, data)

	case "tokens":
		data, err := dashboard.QueryTokens(ctx, periodDays, opts...)
		if err != nil {
			if data == nil {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"Failed to query token metrics", nil)
				return
			}
			slog.Warn("Partial metrics query failure", "view", view, "source", dashboard.Name(), "error", err)
		}
		writeJSON(w, http.StatusOK, data)

	default:
		writeError(w, http.StatusBadRequest, "invalid_view",
			fmt.Sprintf("Unknown view: %s. Valid views: summary, sessions, model-calls, tokens", view), nil)
	}
}

// metricsTelemetryHint returns an actionable dashboard hint when GCP cloud
// telemetry is enabled, the effective default metadata mode is block, and the
// scion-telemetry-gcp-credentials secret is not configured.
func (s *Server) metricsTelemetryHint(ctx context.Context, projectID string) string {
	tel := s.hubTelemetryDefault()
	if tel == nil || tel.Cloud == nil || tel.Cloud.Provider != "gcp" {
		return ""
	}
	if tel.Cloud.Enabled != nil && !*tel.Cloud.Enabled {
		return ""
	}

	var project *store.Project
	if projectID != "" && s.store != nil {
		if p, err := s.store.GetProject(ctx, projectID); err == nil {
			project = p
		}
	}
	if policy := projectTelemetryPolicy(project); policy != nil {
		if !*policy {
			return ""
		}
	} else if tel.Enabled != nil && !*tel.Enabled {
		return ""
	}

	mode := s.hubAgentDefaults().DefaultGCPIdentityMode
	if project != nil {
		if pm := projectSettingsFromAnnotations(project).DefaultGCPIdentityMode; pm != "" {
			mode = pm
		}
	}
	if mode != "" && mode != store.GCPMetadataModeBlock {
		return ""
	}
	if mode == "" && s.allBrokersKubernetesOnly(ctx) {
		return ""
	}

	if s.secretBackend == nil {
		return ""
	}
	meta, err := s.secretBackend.GetMeta(ctx, telemetryGCPCredentialsSecretName, store.ScopeHub, s.hubID)
	if err == nil && meta != nil && meta.SecretType == store.SecretTypeFile && secret.HubSecretDeliverable(*meta) {
		return ""
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if projectID != "" {
		pMeta, pErr := s.secretBackend.GetMeta(ctx, telemetryGCPCredentialsSecretName, store.ScopeProject, projectID)
		if pErr == nil && pMeta != nil && pMeta.SecretType == store.SecretTypeFile {
			return ""
		}
		if pErr != nil && !errors.Is(pErr, store.ErrNotFound) {
			return ""
		}
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil && userIdent.ID() != "" {
		uMeta, uErr := s.secretBackend.GetMeta(ctx, telemetryGCPCredentialsSecretName, store.ScopeUser, userIdent.ID())
		if uErr == nil && uMeta != nil && uMeta.SecretType == store.SecretTypeFile {
			return ""
		}
		if uErr != nil && !errors.Is(uErr, store.ErrNotFound) {
			return ""
		}
	}

	return missingGCPTelemetryCredentialsHint
}

// allBrokersKubernetesOnly reports whether the hub has at least one registered
// runtime broker and every registered broker profile is a Kubernetes runtime
// (where an unset GCP identity mode defaults to passthrough rather than block).
func (s *Server) allBrokersKubernetesOnly(ctx context.Context) bool {
	if s.store == nil {
		return false
	}
	res, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{})
	if err != nil || res == nil || len(res.Items) == 0 {
		return false
	}
	sawProfile := false
	for _, b := range res.Items {
		if len(b.Profiles) == 0 {
			return false
		}
		for _, p := range b.Profiles {
			sawProfile = true
			if !isKubernetesRuntimeType(p.Type) {
				return false
			}
		}
	}
	return sawProfile
}
