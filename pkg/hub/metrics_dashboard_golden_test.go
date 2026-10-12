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
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// usageGoldenRelPath is pkg/sciontool/telemetry's captured-emitter-output
// fixture (design §7.6), read here as a plain file rather than imported
// as a Go type — hub must not depend on sciontool. Keep this path in sync
// with pkg/sciontool/telemetry/usage_golden_test.go's usageGoldenPath; if
// that fixture is regenerated (`go test ./pkg/sciontool/telemetry/... -run
// TestPipelineDerivesClaudeUsageEndToEnd -update`), this test picks up the
// new file automatically.
const usageGoldenRelPath = "../sciontool/telemetry/testdata/usage/claude-2.1.280.timeseries.json"

// usageGoldenFixture mirrors the on-disk shape written by
// pkg/sciontool/telemetry/usage_golden_test.go: one entry in Flushes per GCP
// export call captured, each a protojson-marshaled monitoringpb.TimeSeries.
type usageGoldenFixture struct {
	Source           string              `json:"source"`
	ClaudeCLIVersion string              `json:"claude_cli_version"`
	Flushes          [][]json.RawMessage `json:"flushes"`
}

// usageGoldenRecency is how far before "now" the fixture's latest point
// lands after shiftUsageGoldenTimestamps, well inside the dashboard's
// default lookback window regardless of when this test runs.
const usageGoldenRecency = time.Hour

// loadUsageGoldenFlushes loads every flush from the shared golden fixture,
// then shifts every Interval timestamp in the whole fixture by one constant
// offset so the latest end time lands usageGoldenRecency before the real
// wall clock (the dashboard windows against time.Now(), and there is no
// "now" override on the production QueryOption API). A single constant
// offset applied uniformly preserves every relative spacing the fixture
// pins — the collector-epoch start within a series, and the gap between the
// two captured flushes — so this only defeats staleness, not the two-flush
// shape the golden fixture exists to pin.
func loadUsageGoldenFlushes(t *testing.T) [][]*monitoringpb.TimeSeries {
	t.Helper()
	data, err := os.ReadFile(usageGoldenRelPath)
	require.NoError(t, err, "reading golden fixture %s (produced by pkg/sciontool/telemetry's TestPipelineDerivesClaudeUsageEndToEnd; rerun that test with -update if it's missing or stale)", usageGoldenRelPath)
	var fixture usageGoldenFixture
	require.NoError(t, json.Unmarshal(data, &fixture))

	flushes := make([][]*monitoringpb.TimeSeries, len(fixture.Flushes))
	var latestEnd time.Time
	for i, raw := range fixture.Flushes {
		flushes[i] = make([]*monitoringpb.TimeSeries, len(raw))
		for j, r := range raw {
			var ts monitoringpb.TimeSeries
			require.NoError(t, protojson.Unmarshal(r, &ts))
			flushes[i][j] = &ts
			for _, p := range ts.GetPoints() {
				if end := p.GetInterval().GetEndTime().AsTime(); end.After(latestEnd) {
					latestEnd = end
				}
			}
		}
	}
	require.False(t, latestEnd.IsZero(), "golden fixture %s has no points with an end time", usageGoldenRelPath)

	offset := time.Now().Add(-usageGoldenRecency).Sub(latestEnd)
	for _, series := range flushes {
		for _, ts := range series {
			for _, p := range ts.GetPoints() {
				interval := p.GetInterval()
				if interval == nil {
					continue
				}
				if interval.StartTime != nil {
					interval.StartTime = timestamppb.New(interval.StartTime.AsTime().Add(offset))
				}
				if interval.EndTime != nil {
					interval.EndTime = timestamppb.New(interval.EndTime.AsTime().Add(offset))
				}
			}
		}
	}
	return flushes
}

// canonicalSeriesLabelKey identifies a TimeSeries by its metric type and its
// full label set (sorted), independent of point order or values. Two
// captured flushes of the same identity carry identical labels, including
// scion_metric_point_id (a digest of the point's own label values), so this
// is exactly "the same series" in GCP's sense — good enough to merge on,
// without needing to special-case which labels are identity-bearing.
func canonicalSeriesLabelKey(ts *monitoringpb.TimeSeries) string {
	labels := ts.GetMetric().GetLabels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(ts.GetMetric().GetType())
	for _, k := range keys {
		b.WriteByte('\x00')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

// mergeUsageGoldenFlushes merges every flush's points into one TimeSeries
// per identity: each captured flush is its own
// CreateTimeSeries wire call — one point each — mirroring what sciontool
// sends, but a dashboard query reads back one ListTimeSeries response per
// identity carrying every point across the queried window. Without this
// merge, the second flush's cumulative point is simply never seen, and a
// naive sum-of-flush-0-only fixture can't catch a regression that
// over-counts by re-summing every flush instead of taking deltas.
func mergeUsageGoldenFlushes(flushes [][]*monitoringpb.TimeSeries) []*monitoringpb.TimeSeries {
	var order []string
	merged := map[string]*monitoringpb.TimeSeries{}
	for _, flush := range flushes {
		for _, ts := range flush {
			key := canonicalSeriesLabelKey(ts)
			existing, ok := merged[key]
			if !ok {
				merged[key] = proto.Clone(ts).(*monitoringpb.TimeSeries)
				order = append(order, key)
				continue
			}
			existing.Points = append(existing.Points, ts.GetPoints()...)
		}
	}
	out := make([]*monitoringpb.TimeSeries, len(order))
	for i, key := range order {
		out[i] = merged[key]
	}
	return out
}

// lastPointValue returns the value of ts's point with the latest interval
// end time — the cumulative total a real Cloud Monitoring series converges
// to, which seriesIncreases' per-flush deltas must sum back up to.
func lastPointValue(t *testing.T, ts *monitoringpb.TimeSeries) int64 {
	t.Helper()
	require.NotEmpty(t, ts.GetPoints(), "series has no points: %v", ts)
	latest := ts.GetPoints()[0]
	for _, p := range ts.GetPoints()[1:] {
		if p.GetInterval().GetEndTime().AsTime().After(latest.GetInterval().GetEndTime().AsTime()) {
			latest = p
		}
	}
	return latest.GetValue().GetInt64Value()
}

// TestDashboardGoldenClaudeUsagePoints pins emitter → dashboard (design
// §7.6). Unlike a hand-built fixture, every series here is loaded, unedited
// except for the timestamp shift loadUsageGoldenFlushes applies and the
// merge mergeUsageGoldenFlushes applies, from the golden file
// pkg/sciontool/telemetry's own end-to-end test
// (TestPipelineDerivesClaudeUsageEndToEnd) captured and checked in — so a
// rename of a metric, a label, or a token_type value on either side shows up
// as a diff here or a failure there, not as two hand-maintained fixtures that
// silently drift apart.
//
// Both captured flushes are used, merged into one TimeSeries per
// identity so each carries both cumulative points the way a real
// ListTimeSeries response would, not the one-point-per-CreateTimeSeries-call
// shape sciontool's own export wire format captures them in. The dashboard
// totals asserted below are each series' *last* cumulative value — what
// seriesIncreases' per-flush deltas must telescope back up to — not the sum
// of both flushes' raw points, which would double-count.
//
// The fixture's checked-in Interval timestamps are a fixed date near its
// capture time (see that test's own comment, needed there for a
// byte-for-byte golden comparison), but this test's queries window against
// the real wall clock (metricsQueryWindowFor(time.Now(), ...) in
// metrics_dashboard.go), and there is no "now" override on the production
// QueryOption API. loadUsageGoldenFlushes shifts every timestamp by one
// constant offset so the latest point always lands inside that window,
// regardless of how long ago the fixture was captured — this test does not
// go stale.
func TestDashboardGoldenClaudeUsagePoints(t *testing.T) {
	flushes := loadUsageGoldenFlushes(t)
	require.GreaterOrEqual(t, len(flushes), 2, "golden fixture must carry at least two flushes to pin the cumulative shape")
	series := mergeUsageGoldenFlushes(flushes)

	var callsSeries, tokenSeries []*monitoringpb.TimeSeries
	for _, ts := range series {
		switch ts.GetMetric().GetType() {
		case metricPrefix + telemetrycontract.MetricAPICalls:
			callsSeries = append(callsSeries, ts)
		case metricPrefix + telemetrycontract.MetricUsageTokens:
			tokenSeries = append(tokenSeries, ts)
		}
	}
	require.Len(t, callsSeries, 2, "golden fixture gen_ai.api.calls series (one success, one error)")
	require.Len(t, tokenSeries, 3, "golden fixture scion.usage.tokens series (input, output, cache_write; cache_read was 0)")

	// The success-calls and all three token series were re-exported in the
	// second flush (a genuinely new event, not a duplicate — see the
	// sciontool test); the error-calls series was not (its request was a
	// duplicate). Confirms the merge actually merged, rather than every
	// series accidentally having exactly one point regardless.
	var multiPointSeries int
	for _, ts := range series {
		if len(ts.GetPoints()) > 1 {
			multiPointSeries++
		}
	}
	require.Equal(t, 4, multiPointSeries, "expected 4 series (success calls + 3 token types) to carry both flushes' points after the merge")

	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	svc.projectID = "test-project"

	callsFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricAPICalls + `"`
	client.seriesByFilter[callsFilter] = callsSeries

	tokensAllFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` != "` + telemetrycontract.TokenTypeReasoning + `"`
	client.seriesByFilter[tokensAllFilter] = tokenSeries

	var wantTotalCalls, wantTotalTokens int64
	for _, ts := range callsSeries {
		wantTotalCalls += lastPointValue(t, ts)
	}
	byTokenType := map[string]int64{}
	for _, ts := range tokenSeries {
		tokenType := ts.GetMetric().GetLabels()[telemetrycontract.TokenTypeLabel]
		require.NotEmpty(t, tokenType, "golden token series missing token_type label: %v", ts)
		filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` = "` + tokenType + `"`
		client.seriesByFilter[filter] = []*monitoringpb.TimeSeries{ts}
		last := lastPointValue(t, ts)
		byTokenType[tokenType] = last
		wantTotalTokens += last
	}

	ctx := context.Background()
	summary, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, wantTotalCalls, summary.TotalAPICalls, "QuerySummary must sum each series' last cumulative value, not the raw point sum")
	assert.Equal(t, wantTotalTokens, summary.TotalTokens, "QuerySummary must sum each series' last cumulative value, not the raw point sum")

	model := callsSeries[0].GetMetric().GetLabels()[telemetrycontract.ModelLabel]
	require.NotEmpty(t, model, "golden calls series missing model label")

	calls, err := svc.QueryModelCalls(ctx, 7)
	require.NoError(t, err)
	require.Len(t, calls.ByModel, 1)
	assert.Equal(t, model, calls.ByModel[0].Label)
	var callsTotal int64
	for _, p := range calls.ByModel[0].Points {
		callsTotal += p.Value
	}
	assert.Equal(t, wantTotalCalls, callsTotal, "QueryModelCalls must sum to the last cumulative value, not the raw point sum")

	tokens, err := svc.QueryTokens(ctx, 7)
	require.NoError(t, err)
	require.Len(t, tokens.Input, 1)
	require.Len(t, tokens.Output, 1)
	require.Empty(t, tokens.CacheRead, "cache_read was never emitted (its value was 0)")
	require.Len(t, tokens.CacheWrite, 1)
	// Sum every day bucket, not just Points[0]: queryGroupedTimeSeries
	// buckets increments by the end time's UTC calendar day, and the
	// fixture's two flushes are only ~5s apart (loadUsageGoldenFlushes
	// shifts both by the same offset), so a run that happens to start in the
	// few seconds before UTC midnight puts them in different day buckets.
	// QueryModelCalls' callsTotal above already sums this way; token_type
	// totals need the same treatment to avoid a rare, real day-boundary
	// flake.
	sumPoints := func(series LabeledTimeSeries) int64 {
		var total int64
		for _, p := range series.Points {
			total += p.Value
		}
		return total
	}
	dashboardByType := map[string]int64{
		telemetrycontract.TokenTypeInput:      sumPoints(tokens.Input[0]),
		telemetrycontract.TokenTypeOutput:     sumPoints(tokens.Output[0]),
		telemetrycontract.TokenTypeCacheWrite: sumPoints(tokens.CacheWrite[0]),
	}
	for tokenType, want := range byTokenType {
		assert.Equal(t, want, dashboardByType[tokenType], "dashboard token_type=%s value must equal the last cumulative value, not the raw point sum", tokenType)
	}
}
