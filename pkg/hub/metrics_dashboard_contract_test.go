// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeTimeSeriesIterator is a canned timeSeriesIterator.
type fakeTimeSeriesIterator struct {
	series []*monitoringpb.TimeSeries
	err    error
	idx    int
}

func (it *fakeTimeSeriesIterator) Next() (*monitoringpb.TimeSeries, error) {
	if it.err != nil {
		return nil, it.err
	}
	if it.idx >= len(it.series) {
		return nil, iterator.Done
	}
	ts := it.series[it.idx]
	it.idx++
	return ts, nil
}

// fakeMetricsClient is the "narrow interface over monitoring.MetricClient"
// fake used by the dashboard contract test (design §7.2): it records every
// request filter, and returns canned series or errors keyed by the exact
// filter string a query produced.
type fakeMetricsClient struct {
	mu       sync.Mutex
	requests []*monitoringpb.ListTimeSeriesRequest

	seriesByFilter map[string][]*monitoringpb.TimeSeries
	errByFilter    map[string]error
}

func newFakeMetricsClient() *fakeMetricsClient {
	return &fakeMetricsClient{seriesByFilter: map[string][]*monitoringpb.TimeSeries{}, errByFilter: map[string]error{}}
}

func (c *fakeMetricsClient) ListTimeSeries(_ context.Context, req *monitoringpb.ListTimeSeriesRequest, _ ...gax.CallOption) timeSeriesIterator {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	if err, ok := c.errByFilter[req.Filter]; ok {
		return &fakeTimeSeriesIterator{err: err}
	}
	return &fakeTimeSeriesIterator{series: c.seriesByFilter[req.Filter]}
}

func (c *fakeMetricsClient) requestFilters() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	filters := make([]string, len(c.requests))
	for i, r := range c.requests {
		filters[i] = r.Filter
	}
	return filters
}

func newContractTestService(client *fakeMetricsClient) *MetricsDashboardService {
	return &MetricsDashboardService{client: client, projectID: "test-project", cache: make(map[string]*cacheEntry)}
}

// requireExactlyOneFilterContaining asserts exactly one recorded filter
// contains every given substring, and returns it.
func requireExactlyOneFilterContaining(t *testing.T, filters []string, substrs ...string) string {
	t.Helper()
	var matches []string
	for _, f := range filters {
		all := true
		for _, s := range substrs {
			if !strings.Contains(f, s) {
				all = false
				break
			}
		}
		if all {
			matches = append(matches, f)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("filters containing %v: got %d matches %v, want 1; all filters: %v", substrs, len(matches), matches, filters)
	}
	return matches[0]
}

// TestDashboardContractQueriesCanonicalMetricNames pins design §7.2: every
// view queries exactly the contract metric types, never the retired
// gen_ai.tokens.* names.
func TestDashboardContractQueriesCanonicalMetricNames(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	ctx := context.Background()

	_, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	_, err = svc.QueryModelCalls(ctx, 7)
	require.NoError(t, err)
	_, err = svc.QueryTokens(ctx, 7)
	require.NoError(t, err)

	filters := client.requestFilters()
	for _, f := range filters {
		assert.NotContains(t, f, "gen_ai.tokens.", "legacy metric name must never be queried")
		assert.NotContains(t, f, "scion.hook.tokens.", "legacy metric name must never be queried")
	}

	// agent.session.count is queried twice by QuerySummary: once as a sum,
	// once (identically filtered) to group unique agents.
	sessionType := `metric.type = "workload.googleapis.com/` + telemetrycontract.MetricSessionCount + `"`
	sessionCount := 0
	for _, f := range filters {
		if f == sessionType {
			sessionCount++
		}
	}
	assert.Equal(t, 2, sessionCount, "agent.session.count queries: %v", filters)

	// gen_ai.api.calls is queried by QuerySummary (a sum) and twice more by
	// QueryModelCalls (grouped by model, by harness): three occurrences.
	callsType := `metric.type = "workload.googleapis.com/` + telemetrycontract.MetricAPICalls + `"`
	callsCount := 0
	for _, f := range filters {
		if strings.Contains(f, callsType) {
			callsCount++
		}
	}
	assert.Equal(t, 3, callsCount, "gen_ai.api.calls queries: %v", filters)

	// scion.usage.tokens is queried once by QuerySummary (a sum excluding
	// "reasoning") and once per token_type by QueryTokens (input, output,
	// cache_read, cache_write): five occurrences.
	tokensType := `metric.type = "workload.googleapis.com/` + telemetrycontract.MetricUsageTokens + `"`
	tokensCount := 0
	for _, f := range filters {
		if strings.Contains(f, tokensType) {
			tokensCount++
		}
	}
	assert.Equal(t, 5, tokensCount, "scion.usage.tokens queries: %v", filters)

	requireExactlyOneFilterContaining(t, filters, tokensType, `!= "`+telemetrycontract.TokenTypeReasoning+`"`)
	for _, tt := range []string{telemetrycontract.TokenTypeInput, telemetrycontract.TokenTypeOutput, telemetrycontract.TokenTypeCacheRead, telemetrycontract.TokenTypeCacheWrite} {
		requireExactlyOneFilterContaining(t, filters, tokensType, `token_type = "`+tt+`"`)
	}
}

// TestDashboardContractProjectViewsFilterOnCanonicalProjectLabel pins design
// §7.2: project-scoped views filter on metric.labels.scion_project_id
// (contract.ProjectLabel), never the producer's overloaded "project_id".
func TestDashboardContractProjectViewsFilterOnCanonicalProjectLabel(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	ctx := context.Background()

	_, err := svc.QuerySummary(ctx, 7, WithProjectID("proj-42"))
	require.NoError(t, err)
	_, err = svc.QueryModelCalls(ctx, 7, WithProjectID("proj-42"))
	require.NoError(t, err)
	_, err = svc.QueryTokens(ctx, 7, WithProjectID("proj-42"))
	require.NoError(t, err)

	filters := client.requestFilters()
	require.NotEmpty(t, filters)
	for _, f := range filters {
		assert.Contains(t, f, `metric.labels.`+telemetrycontract.ProjectLabel+` = "proj-42"`)
		assert.NotContains(t, f, `metric.labels.project_id`, "must not filter on the non-canonical producer label")
	}

	_, err = svc.QueryProjectSummary(ctx, "proj-99")
	require.NoError(t, err)
	for _, f := range client.requestFilters()[len(filters):] {
		assert.Contains(t, f, `metric.labels.`+telemetrycontract.ProjectLabel+` = "proj-99"`)
	}
}

// TestDashboardContractAgentGroupingUsesCanonicalLabel pins design §7.2:
// agent grouping uses metric.labels.scion_agent_id, and the unique-agent
// count reads that label from returned series.
func TestDashboardContractAgentGroupingUsesCanonicalLabel(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	ctx := context.Background()

	sessionType := `metric.type = "workload.googleapis.com/` + telemetrycontract.MetricSessionCount + `"`
	client.seriesByFilter[sessionType] = []*monitoringpb.TimeSeries{
		{
			Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricSessionCount, Labels: map[string]string{telemetrycontract.AgentLabel: "agent-a"}},
			Points: []*monitoringpb.Point{{
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-time.Hour)), EndTime: timestamppb.New(time.Now())},
				Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 1}},
			}},
		},
		{
			Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricSessionCount, Labels: map[string]string{telemetrycontract.AgentLabel: "agent-b"}},
			Points: []*monitoringpb.Point{{
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-time.Hour)), EndTime: timestamppb.New(time.Now())},
				Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 1}},
			}},
		},
	}

	summary, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.UniqueAgents)

	found := false
	for _, f := range client.requestFilters() {
		if f == sessionType {
			found = true
		}
	}
	assert.True(t, found, "expected an unfiltered agent.session.count query for the grouping pass")

	// The label key extracted from the groupBy string must be the canonical
	// one, not the legacy producer label "agent_id".
	assert.Equal(t, telemetrycontract.AgentLabel, labelKeyFromGroupBy("metric.labels."+telemetrycontract.AgentLabel))
}

// TestDashboardContractAbsentMetricCountsAsZero pins the "NotFound-as-zero"
// rule (design §3.6 "Absent metrics"): a metric with no descriptor yet is
// zero, not a partial failure.
func TestDashboardContractAbsentMetricCountsAsZero(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	ctx := context.Background()
	notFound := status.Error(codes.NotFound, "metric descriptor not found")
	for _, name := range []string{telemetrycontract.MetricSessionCount, telemetrycontract.MetricAPICalls, telemetrycontract.MetricUsageTokens} {
		client.errByFilter[`metric.type = "`+metricPrefix+name+`"`] = notFound
	}
	// The grouped/token_type-filtered queries carry a different filter
	// string; register the same NotFound for every filter beginning with the
	// tokens metric type, since QueryTokens issues four distinct filters.
	client.errByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricUsageTokens+`" AND metric.labels.`+telemetrycontract.TokenTypeLabel+` != "`+telemetrycontract.TokenTypeReasoning+`"`] = notFound
	for _, tt := range telemetrycontract.SummableTokenTypes {
		client.errByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricUsageTokens+`" AND metric.labels.`+telemetrycontract.TokenTypeLabel+` = "`+tt+`"`] = notFound
	}
	client.errByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricAPICalls+`"`] = notFound

	summary, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, int64(0), summary.TotalSessions)
	assert.Equal(t, int64(0), summary.TotalAPICalls)
	assert.Equal(t, int64(0), summary.TotalTokens)
	assert.Equal(t, 0, summary.UniqueAgents)

	tokens, err := svc.QueryTokens(ctx, 7)
	require.NoError(t, err)
	assert.Empty(t, tokens.Input)
	assert.Empty(t, tokens.Output)
	assert.Empty(t, tokens.CacheRead)
	assert.Empty(t, tokens.CacheWrite)
}
