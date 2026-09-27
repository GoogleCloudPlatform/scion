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
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
)

// TestDashboardGoldenClaudeUsagePoints pins emitter → dashboard (design §7.6).
// The fixture below mirrors exactly what pkg/sciontool/telemetry's GCP
// exporter produces for the captured Claude fixture
// (pkg/sciontool/telemetry/testdata/usage/claude-2.1.280.pb.json), verified
// point-for-point by
// TestPipelineDerivesClaudeUsageEndToEnd in that package: one successful
// gen_ai.api.calls point, one failed one, and three scion.usage.tokens
// points (input, output, cache_write; cache_read is absent because its
// value was 0). If either side renames a metric, a label, or a token_type
// value, this test and that one diverge from what's actually exported.
func TestDashboardGoldenClaudeUsagePoints(t *testing.T) {
	const (
		agentID   = "agent-usage-1"
		agentSlug = "usage-agent-slug"
		projectID = "project-usage-1"
		harness   = "claude"
		model     = "claude-sonnet-5"
	)
	epoch := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	end := epoch.Add(time.Minute)

	canonicalLabels := func(extra map[string]string) map[string]string {
		labels := map[string]string{
			telemetrycontract.AgentLabel:     agentID,
			telemetrycontract.ProjectLabel:   projectID,
			telemetrycontract.AgentSlugLabel: agentSlug,
			"scion_metric_resource_id":       "resource-digest",
			"scion_metric_scope_id":          "scope-digest",
			"scion_metric_point_id":          "point-digest",
			"service_name":                   "sciontool",
			"service_instance_id":            agentID,
		}
		for k, v := range extra {
			labels[k] = v
		}
		return labels
	}

	callsSeries := func(status string, digest string) *monitoringpb.TimeSeries {
		labels := canonicalLabels(map[string]string{
			"agent_id":                     agentID,
			"project_id":                   projectID,
			telemetrycontract.HarnessLabel: harness,
			telemetrycontract.ModelLabel:   model,
			telemetrycontract.StatusLabel:  status,
			"scion_metric_point_id":        digest, // distinguish the two series
		})
		return &monitoringpb.TimeSeries{
			Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricAPICalls, Labels: labels},
			Points: []*monitoringpb.Point{intPoint(epoch, end, 1)},
		}
	}
	tokensSeries := func(tokenType string, value int64, digest string) *monitoringpb.TimeSeries {
		labels := canonicalLabels(map[string]string{
			telemetrycontract.HarnessLabel:   harness,
			telemetrycontract.ModelLabel:     model,
			telemetrycontract.TokenTypeLabel: tokenType,
			"scion_metric_point_id":          digest,
		})
		return &monitoringpb.TimeSeries{
			Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricUsageTokens, Labels: labels},
			Points: []*monitoringpb.Point{intPoint(epoch, end, value)},
		}
	}

	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	svc.projectID = "test-project"

	callsFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricAPICalls + `"`
	client.seriesByFilter[callsFilter] = []*monitoringpb.TimeSeries{
		callsSeries(telemetrycontract.StatusSuccess, "digest-success"),
		callsSeries(telemetrycontract.StatusError, "digest-error"),
	}
	tokensAllFilter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` != "` + telemetrycontract.TokenTypeReasoning + `"`
	allTokenSeries := []*monitoringpb.TimeSeries{
		tokensSeries(telemetrycontract.TokenTypeInput, 2, "digest-input"),
		tokensSeries(telemetrycontract.TokenTypeOutput, 41, "digest-output"),
		tokensSeries(telemetrycontract.TokenTypeCacheWrite, 26607, "digest-cache-write"),
	}
	client.seriesByFilter[tokensAllFilter] = allTokenSeries
	for _, tt := range []struct {
		tokenType string
		value     int64
	}{
		{telemetrycontract.TokenTypeInput, 2},
		{telemetrycontract.TokenTypeOutput, 41},
		{telemetrycontract.TokenTypeCacheWrite, 26607},
	} {
		filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `" AND metric.labels.` + telemetrycontract.TokenTypeLabel + ` = "` + tt.tokenType + `"`
		client.seriesByFilter[filter] = []*monitoringpb.TimeSeries{tokensSeries(tt.tokenType, tt.value, "digest-"+tt.tokenType)}
	}

	ctx := context.Background()
	summary, err := svc.QuerySummary(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.TotalAPICalls)
	assert.Equal(t, int64(2+41+26607), summary.TotalTokens)

	calls, err := svc.QueryModelCalls(ctx, 7)
	require.NoError(t, err)
	require.Len(t, calls.ByModel, 1)
	assert.Equal(t, model, calls.ByModel[0].Label)
	var callsTotal int64
	for _, p := range calls.ByModel[0].Points {
		callsTotal += p.Value
	}
	assert.Equal(t, int64(2), callsTotal)

	tokens, err := svc.QueryTokens(ctx, 7)
	require.NoError(t, err)
	require.Len(t, tokens.Input, 1)
	require.Len(t, tokens.Output, 1)
	require.Empty(t, tokens.CacheRead, "cache_read was never emitted (its value was 0)")
	require.Len(t, tokens.CacheWrite, 1)
	assert.Equal(t, int64(2), tokens.Input[0].Points[0].Value)
	assert.Equal(t, int64(41), tokens.Output[0].Points[0].Value)
	assert.Equal(t, int64(26607), tokens.CacheWrite[0].Points[0].Value)
}
