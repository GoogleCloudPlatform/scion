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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func collectUsageOTelMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func findUsageOTelMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			if rm.ScopeMetrics[i].Metrics[j].Name == name {
				return &rm.ScopeMetrics[i].Metrics[j]
			}
		}
	}
	return nil
}

func usageOTelAttrMap(set attribute.Set) map[string]string {
	out := make(map[string]string, set.Len())
	iter := set.Iter()
	for iter.Next() {
		kv := iter.Attribute()
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

type failingUsageIngestStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failingUsageIngestStore) IngestUsageBatch(ctx context.Context, in *store.UsageIngestBatchInput) (*store.UsageIngestBatchResult, error) {
	if f.fault.Active() {
		return nil, errors.New("simulated tx rollback")
	}
	return f.Store.IngestUsageBatch(ctx, in)
}

func setupUsageOTelAgent(t *testing.T, srv *Server, s store.Store, suffix string) (*store.Project, *store.Agent, string, string) {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{ID: tid("uotel-proj-" + suffix), Name: "uotel-" + suffix, Slug: "uotel-" + suffix}
	require.NoError(t, s.CreateProject(ctx, proj))
	runID := "run-uotel-" + suffix
	ag := &store.Agent{
		ID:        tid("uotel-ag-" + suffix),
		Slug:      "uotel-slug-" + suffix,
		Name:      "uotel-agent-" + suffix,
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
		RunID:     runID,
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:      runID,
		Generation: 1,
		HubID:      srv.usageLedgerHubID(),
		ProjectID:  proj.ID,
		AgentID:    ag.ID,
		AgentSlug:  ag.Slug,
		Harness:    "claude",
	})
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	return proj, ag, runID, tok
}

func TestUsageOTel_ReemitsOnlyCommittedInsertedRowsAndEnforcesContract(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-otel-reemit")
	setUsageLedgerExperimentForTest(t, srv, true)

	reader := sdkmetric.NewManualReader()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("scion-hub"),
		semconv.ServiceInstanceID("hub-instance-alpha"),
	)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	require.NoError(t, srv.RegisterUsageOTelMetrics(mp,
		WithUsageOTelActiveSink(true),
		WithUsageOTelCardinality(UsageOTelCardinalityAgent),
	))

	proj, ag, runID, tok := setupUsageOTelAgent(t, srv, s, "reemit")
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Event 1: includes all 5 allowed token types + cache_write_1h (must be excluded)
	// and spoofed payload project_id/harness (must be ignored in favor of run_attribution).
	ev1 := makeWireUsageEvent("hub-otel-reemit", ag.ID, "spoofed-proj", runID, "ev-reemit-1", 1, now, 100, 50)
	ev1.Harness = "spoofed-harness"
	ev1.CacheRead = usageInt64p(30)
	ev1.CacheWrite = usageInt64p(20)
	ev1.CacheWrite1h = usageInt64p(15) // MUST NOT be emitted in scion.usage.tokens
	ev1.Reasoning = usageInt64p(10)

	// Event 2: NULL cache_read / cache_write / reasoning (must NOT be emitted as 0).
	ev2 := makeWireUsageEvent("hub-otel-reemit", ag.ID, proj.ID, runID, "ev-reemit-2", 2, now, 25, 0)
	ev2.Status = telemetrycontract.StatusError
	ev2.CacheRead = nil
	ev2.CacheWrite = nil
	ev2.Reasoning = nil

	// Submit batch containing ev1, intra-batch duplicate of ev1, and ev2.
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{ev1, ev1, ev2},
		RunID:                runID,
		ProducerEpoch:        "ep-1",
		ProducerSeq:          2,
		DirectOTelSuppressed: true,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Replay the exact same batch (cross-batch duplicates): MUST NOT increment counters again.
	recDup := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{ev1, ev2},
		RunID:                runID,
		ProducerEpoch:        "ep-1",
		ProducerSeq:          2,
		DirectOTelSuppressed: true,
	})
	require.Equal(t, http.StatusOK, recDup.Code, recDup.Body.String())

	// Batch with DirectOTelSuppressed=false (e.g. older sciontool or pre-switch residual batch):
	// persists to ledger but MUST NOT re-emit to OTel.
	ev3 := makeWireUsageEvent("hub-otel-reemit", ag.ID, proj.ID, runID, "ev-reemit-3", 3, now, 999, 888)
	recUnsuppressed := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{ev3},
		RunID:                runID,
		ProducerEpoch:        "ep-1",
		ProducerSeq:          3,
		DirectOTelSuppressed: false,
	})
	require.Equal(t, http.StatusOK, recUnsuppressed.Code, recUnsuppressed.Body.String())

	rm := collectUsageOTelMetrics(t, reader)
	// Verify resource carries service.instance.id.
	resAttrs := usageOTelAttrMap(*rm.Resource.Set())
	assert.Equal(t, "hub-instance-alpha", resAttrs["service.instance.id"])

	// Verify gen_ai.api.calls.
	callsMetric := findUsageOTelMetric(rm, telemetrycontract.MetricAPICalls)
	require.NotNil(t, callsMetric, "expected gen_ai.api.calls metric")
	callsSum, ok := callsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	assert.Equal(t, metricdata.CumulativeTemporality, callsSum.Temporality)
	assert.True(t, callsSum.IsMonotonic)

	callsByStatus := map[string]int64{}
	for _, dp := range callsSum.DataPoints {
		attrs := usageOTelAttrMap(dp.Attributes)
		assert.Equal(t, "claude", attrs[telemetrycontract.HarnessLabel], "harness must come from run_attribution")
		assert.Equal(t, "claude-sonnet-4-6", attrs[telemetrycontract.ModelLabel])
		assert.Equal(t, proj.ID, attrs[telemetrycontract.ProjectLabel], "scion_project_id must come from run_attribution")
		assert.Equal(t, ag.ID, attrs[telemetrycontract.AgentLabel])
		assert.Equal(t, ag.Slug, attrs[telemetrycontract.AgentSlugLabel])
		callsByStatus[attrs[telemetrycontract.StatusLabel]] += dp.Value
	}
	assert.Equal(t, map[string]int64{
		telemetrycontract.StatusSuccess: 1,
		telemetrycontract.StatusError:   1,
	}, callsByStatus)

	// Verify scion.usage.tokens.
	tokensMetric := findUsageOTelMetric(rm, telemetrycontract.MetricUsageTokens)
	require.NotNil(t, tokensMetric, "expected scion.usage.tokens metric")
	tokensSum, ok := tokensMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	assert.Equal(t, metricdata.CumulativeTemporality, tokensSum.Temporality)
	assert.True(t, tokensSum.IsMonotonic)

	tokensByType := map[string]int64{}
	for _, dp := range tokensSum.DataPoints {
		attrs := usageOTelAttrMap(dp.Attributes)
		tt := attrs[telemetrycontract.TokenTypeLabel]
		assert.True(t, telemetrycontract.ValidTokenType(tt), "unexpected token_type %q", tt)
		assert.NotEqual(t, "cache_write_1h", tt)
		assert.Equal(t, "claude", attrs[telemetrycontract.HarnessLabel])
		assert.Equal(t, proj.ID, attrs[telemetrycontract.ProjectLabel])
		assert.Equal(t, ag.ID, attrs[telemetrycontract.AgentLabel])
		assert.Equal(t, ag.Slug, attrs[telemetrycontract.AgentSlugLabel])
		tokensByType[tt] += dp.Value
	}
	assert.Equal(t, map[string]int64{
		telemetrycontract.TokenTypeInput:      125, // 100 + 25 (ev3 excluded because DirectOTelSuppressed=false)
		telemetrycontract.TokenTypeOutput:     50,  // 50 (ev2 output=0 not added; ev3 excluded)
		telemetrycontract.TokenTypeCacheRead:  30,  // ev2 nil not emitted
		telemetrycontract.TokenTypeCacheWrite: 20,  // ev2 nil not emitted
		telemetrycontract.TokenTypeReasoning:  10,  // ev2 nil not emitted
	}, tokensByType)
}

func TestUsageOTel_RolledBackTransactionEmitsNothing(t *testing.T) {
	ctx := context.Background()
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failingUsageIngestStore {
		return &failingUsageIngestStore{Store: inner, fault: f}
	})
	srv.SetHubID("hub-otel-rollback")
	setUsageLedgerExperimentForTest(t, srv, true)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })
	require.NoError(t, srv.RegisterUsageOTelMetrics(mp, WithUsageOTelActiveSink(true)))

	proj, ag, runID, tok := setupUsageOTelAgent(t, srv, s, "rollback")
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Force IngestUsageBatch to fail (simulating a rolled-back transaction).
	fault.Arm()

	ev := makeWireUsageEvent("hub-otel-rollback", ag.ID, proj.ID, runID, "ev-rb-1", 1, now, 500, 200)
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{ev},
		RunID:                runID,
		ProducerEpoch:        "ep-1",
		ProducerSeq:          1,
		DirectOTelSuppressed: true,
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	rm := collectUsageOTelMetrics(t, reader)
	assert.Nil(t, findUsageOTelMetric(rm, telemetrycontract.MetricAPICalls), "rolled-back tx must not emit gen_ai.api.calls")
	assert.Nil(t, findUsageOTelMetric(rm, telemetrycontract.MetricUsageTokens), "rolled-back tx must not emit scion.usage.tokens")
}

func TestUsageOTel_ProjectCardinalityOmitsAgentLabels(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	m, err := NewUsageOTelMetrics(mp,
		WithUsageOTelActiveSink(true),
		WithUsageOTelCardinality(UsageOTelCardinalityProject),
	)
	require.NoError(t, err)

	attr := &store.RunAttribution{
		RunID:     "run-card-1",
		ProjectID: "proj-card",
		AgentID:   "ag-card",
		AgentSlug: "slug-card",
		Harness:   "claude",
	}
	ev := &store.UsageEvent{
		RunID:  "run-card-1",
		Model:  "claude-sonnet-4-6",
		Status: telemetrycontract.StatusSuccess,
		Calls:  1,
		Input:  usageInt64p(42),
	}
	m.RecordCommittedEvents(ctx, []*store.UsageEvent{ev}, map[string]*store.RunAttribution{attr.RunID: attr})

	rm := collectUsageOTelMetrics(t, reader)
	for _, metricName := range []string{telemetrycontract.MetricAPICalls, telemetrycontract.MetricUsageTokens} {
		met := findUsageOTelMetric(rm, metricName)
		require.NotNil(t, met, "missing metric %s", metricName)
		sum := met.Data.(metricdata.Sum[int64])
		require.Len(t, sum.DataPoints, 1)
		attrs := usageOTelAttrMap(sum.DataPoints[0].Attributes)
		assert.Equal(t, "proj-card", attrs[telemetrycontract.ProjectLabel])
		assert.NotContains(t, attrs, telemetrycontract.AgentLabel, "project cardinality must omit scion_agent_id")
		assert.NotContains(t, attrs, telemetrycontract.AgentSlugLabel, "project cardinality must omit scion_agent_slug")
	}

	mode, ok := ParseUsageOTelCardinality("project")
	assert.True(t, ok)
	assert.Equal(t, UsageOTelCardinalityProject, mode)
	mode, ok = ParseUsageOTelCardinality("bogus")
	assert.False(t, ok)
	assert.Equal(t, UsageOTelCardinalityAgent, mode)
}

func TestUsageOTel_SuppressDirectOTelCapabilityMatrix(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name         string
		expEnabled   bool
		registerSink bool
		activeSink   bool
		wantStatus   int
		wantSuppress bool
	}{
		{
			name:       "experiment off returns 404",
			expEnabled: false,
			wantStatus: http.StatusNotFound,
		},
		{
			name:         "experiment on without metrics sink -> suppress false",
			expEnabled:   true,
			registerSink: false,
			wantStatus:   http.StatusOK,
			wantSuppress: false,
		},
		{
			name:         "experiment on with inactive sink -> suppress false",
			expEnabled:   true,
			registerSink: true,
			activeSink:   false,
			wantStatus:   http.StatusOK,
			wantSuppress: false,
		},
		{
			name:         "experiment on with active sink -> suppress true",
			expEnabled:   true,
			registerSink: true,
			activeSink:   true,
			wantStatus:   http.StatusOK,
			wantSuppress: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetHubID("hub-cap-matrix")
			setUsageLedgerExperimentForTest(t, srv, tc.expEnabled)
			if tc.registerSink {
				reader := sdkmetric.NewManualReader()
				mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
				t.Cleanup(func() { _ = mp.Shutdown(ctx) })
				require.NoError(t, srv.RegisterUsageOTelMetrics(mp, WithUsageOTelActiveSink(tc.activeSink)))
			}
			_, ag, _, tok := setupUsageOTelAgent(t, srv, s, "cap")
			rec := doAgentUsageRequest(t, srv, http.MethodGet, telemetrycontract.UsageCapabilitiesAgentPath(ag.ID), tok, nil)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantStatus == http.StatusOK {
				var resp telemetrycontract.UsageCapabilitiesResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.True(t, resp.UsageIngestEnabled)
				assert.Equal(t, tc.wantSuppress, resp.SuppressDirectOTel)
			}
		})
	}
}

func TestUsageOTel_MeterStaleGaugeAndReplicaIndependence(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-stale-gauge")
	setUsageLedgerExperimentForTest(t, srv, true)

	proj, ag, runID, _ := setupUsageOTelAgent(t, srv, s, "stale")
	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)

	// Seed a stale open meter_health row (> 180s old).
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag.ID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-5 * time.Minute),
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Two independent Hub replicas backed by the same DB must report the exact
	// same per-project stale count (so dashboards take max across replicas).
	readReplica := func(instanceID string) (*sdkmetric.ManualReader, *UsageOTelMetrics) {
		r := sdkmetric.NewManualReader()
		res := resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("scion-hub"),
			semconv.ServiceInstanceID(instanceID),
		)
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(r))
		t.Cleanup(func() { _ = mp.Shutdown(ctx) })
		m, err := NewUsageOTelMetrics(mp,
			WithUsageOTelActiveSink(true),
			WithUsageOTelStaleStore(s),
			withUsageOTelCacheTTL(time.Millisecond),
		)
		require.NoError(t, err)
		return r, m
	}

	r1, _ := readReplica("replica-1")
	r2, _ := readReplica("replica-2")

	gaugeValues := func(r *sdkmetric.ManualReader) map[string]int64 {
		rm := collectUsageOTelMetrics(t, r)
		met := findUsageOTelMetric(rm, MetricUsageMeterStale)
		if met == nil {
			return nil
		}
		g := met.Data.(metricdata.Gauge[int64])
		out := make(map[string]int64, len(g.DataPoints))
		for _, dp := range g.DataPoints {
			out[usageOTelAttrMap(dp.Attributes)["project"]] = dp.Value
		}
		return out
	}

	assert.Equal(t, map[string]int64{proj.ID: 1}, gaugeValues(r1))
	assert.Equal(t, map[string]int64{proj.ID: 1}, gaugeValues(r2))

	// Close the meter_health row (final_state=final) and wait past cache TTL:
	// gauge must drop back to 0 for that project.
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag.ID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow,
			FinalState:    store.MeterHealthFinalStateFinal,
		},
	})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	assert.Equal(t, map[string]int64{proj.ID: 0}, gaugeValues(r1))
}
