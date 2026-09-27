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
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// usageGoldenRelPath is pkg/sciontool/telemetry's captured-emitter-output
// fixture (design §7.6, F7), read here as a plain file rather than imported
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

// loadUsageGoldenFlush loads one flush (a []*monitoringpb.TimeSeries) from
// the shared golden fixture.
func loadUsageGoldenFlush(t *testing.T, flushIndex int) []*monitoringpb.TimeSeries {
	t.Helper()
	data, err := os.ReadFile(usageGoldenRelPath)
	require.NoError(t, err, "reading golden fixture %s (produced by pkg/sciontool/telemetry's TestPipelineDerivesClaudeUsageEndToEnd; rerun that test with -update if it's missing or stale)", usageGoldenRelPath)
	var fixture usageGoldenFixture
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Greater(t, len(fixture.Flushes), flushIndex, "golden fixture %s has %d flushes, want more than %d", usageGoldenRelPath, len(fixture.Flushes), flushIndex)
	out := make([]*monitoringpb.TimeSeries, len(fixture.Flushes[flushIndex]))
	for i, raw := range fixture.Flushes[flushIndex] {
		var ts monitoringpb.TimeSeries
		require.NoError(t, protojson.Unmarshal(raw, &ts))
		out[i] = &ts
	}
	return out
}

// TestDashboardGoldenClaudeUsagePoints pins emitter → dashboard (design
// §7.6). Unlike a hand-built fixture, every series here is loaded, unedited,
// from the golden file pkg/sciontool/telemetry's own end-to-end test
// (TestPipelineDerivesClaudeUsageEndToEnd) captured and checked in — so a
// rename of a metric, a label, or a token_type value on either side shows up
// as a diff here or a failure there, not as two hand-maintained fixtures that
// silently drift apart (design §7.6, F7).
//
// Staleness caveat: the fixture's Interval timestamps are a fixed date near
// its capture time (see that test's own comment), while this test's queries
// window against the real wall clock (metricsQueryWindowFor(time.Now(), ...)
// in metrics_dashboard.go — there is no "now" override on the production
// QueryOption API, and adding one only for this test is not this fix's
// scope). The fixture must be regenerated (`go test
// ./pkg/sciontool/telemetry/... -run TestPipelineDerivesClaudeUsageEndToEnd
// -update`) if it ever falls outside the dashboard's default lookback
// window relative to whenever CI actually runs this test.
func TestDashboardGoldenClaudeUsagePoints(t *testing.T) {
	series := loadUsageGoldenFlush(t, 0)

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

	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	svc.projectID = "test-project"

	callsFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricAPICalls + `"`
	client.seriesByFilter[callsFilter] = callsSeries

	tokensAllFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` != "` + telemetrycontract.TokenTypeReasoning + `"`
	client.seriesByFilter[tokensAllFilter] = tokenSeries

	var wantTotalTokens int64
	for _, ts := range tokenSeries {
		tokenType := ts.GetMetric().GetLabels()[telemetrycontract.TokenTypeLabel]
		require.NotEmpty(t, tokenType, "golden token series missing token_type label: %v", ts)
		filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` = "` + tokenType + `"`
		client.seriesByFilter[filter] = []*monitoringpb.TimeSeries{ts}
		require.NotEmpty(t, ts.Points, "golden token series has no points: %v", ts)
		wantTotalTokens += ts.Points[0].GetValue().GetInt64Value()
	}

	ctx := context.Background()
	summary, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, int64(len(callsSeries)), summary.TotalAPICalls)
	assert.Equal(t, wantTotalTokens, summary.TotalTokens)

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
	assert.Equal(t, int64(len(callsSeries)), callsTotal)

	tokens, err := svc.QueryTokens(ctx, 7)
	require.NoError(t, err)
	require.Len(t, tokens.Input, 1)
	require.Len(t, tokens.Output, 1)
	require.Empty(t, tokens.CacheRead, "cache_read was never emitted (its value was 0)")
	require.Len(t, tokens.CacheWrite, 1)
	byType := map[string]int64{
		telemetrycontract.TokenTypeInput:      tokens.Input[0].Points[0].Value,
		telemetrycontract.TokenTypeOutput:     tokens.Output[0].Points[0].Value,
		telemetrycontract.TokenTypeCacheWrite: tokens.CacheWrite[0].Points[0].Value,
	}
	for _, ts := range tokenSeries {
		tokenType := ts.GetMetric().GetLabels()[telemetrycontract.TokenTypeLabel]
		assert.Equal(t, ts.Points[0].GetValue().GetInt64Value(), byType[tokenType], "dashboard token_type=%s value", tokenType)
	}
}
