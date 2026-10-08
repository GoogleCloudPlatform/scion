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
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestDecisionAuditRecorder(t *testing.T, depth func() int64) (*OTelDecisionAuditMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelDecisionAuditMetrics(mp, depth)
	require.NoError(t, err)
	return rec, reader
}

func collectDecisionAuditMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		assert.Equal(t, instrumentationScope, sm.Scope.Name)
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func decisionAuditGaugeValue(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "queue_depth is an int64 gauge, got %T", m.Data)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

func TestNewOTelDecisionAuditMetrics_RejectsNilInputs(t *testing.T) {
	_, err := NewOTelDecisionAuditMetrics(nil, func() int64 { return 0 })
	assert.Error(t, err)
	mp := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	_, err = NewOTelDecisionAuditMetrics(mp, nil)
	assert.Error(t, err)
}

func TestOTelDecisionAuditMetrics_DropsAndWrites(t *testing.T) {
	rec, reader := newTestDecisionAuditRecorder(t, func() int64 { return 0 })

	rec.RecordDecisionAuditDrop(DecisionAuditDropQueueFull, "allow")
	rec.RecordDecisionAuditDrop(DecisionAuditDropQueueFull, "allow")
	rec.RecordDecisionAuditDrop(DecisionAuditDropShutdown, "deny")
	rec.RecordDecisionAuditWrite(3*time.Millisecond, DecisionAuditWriteOK)
	rec.RecordDecisionAuditWrite(7*time.Millisecond, DecisionAuditWriteError)
	rec.RecordDecisionAuditWrite(time.Millisecond, DecisionAuditWriteDuplicate)

	metrics := collectDecisionAuditMetrics(t, reader)

	dropped, ok := metrics["scion.hub.decision_audit.dropped"]
	require.True(t, ok)
	assert.Equal(t, "{record}", dropped.Unit)
	sum, ok := dropped.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		reason, _ := dp.Attributes.Value(attribute.Key("reason"))
		decision, _ := dp.Attributes.Value(attribute.Key("decision"))
		assert.Equal(t, 2, dp.Attributes.Len(), "labels are reason and decision only")
		got[reason.AsString()+"/"+decision.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"queue_full/allow": 2, "shutdown/deny": 1}, got)

	dur, ok := metrics["scion.hub.decision_audit.write.duration"]
	require.True(t, ok)
	assert.Equal(t, "ms", dur.Unit)
	hist, ok := dur.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	outcomes := map[string]uint64{}
	for _, dp := range hist.DataPoints {
		outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
		assert.Equal(t, 1, dp.Attributes.Len(), "the only label is outcome")
		outcomes[outcome.AsString()] = dp.Count
	}
	assert.Equal(t, map[string]uint64{"ok": 1, "error": 1, "duplicate": 1}, outcomes)
}

// TestOTelDecisionAuditMetrics_QueueDepthTracksWriter checks the gauge end
// to end: it reads the writer's live depth at each collection, so it shows
// the backlog while the store is blocked and zero once the writer is idle,
// without waiting for Close.
func TestOTelDecisionAuditMetrics_QueueDepthTracksWriter(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := newStoreDecisionAuditEmitter(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), testDecisionAuditConfig())
	t.Cleanup(func() { e.Close(context.Background()) })
	rec, reader := newTestDecisionAuditRecorder(t, func() int64 { return int64(e.QueueDepth()) })
	e.SetMetrics(rec)
	ctx := context.Background()

	depth := func() int64 {
		m, ok := collectDecisionAuditMetrics(t, reader)["scion.hub.decision_audit.queue_depth"]
		require.True(t, ok, "queue_depth is exported even before any record")
		assert.Equal(t, "{record}", m.Unit)
		return decisionAuditGaugeValue(t, m)
	}
	assert.Equal(t, int64(0), depth())

	e.EmitDecisionAudit(ctx, auditRec("deny", "d0"))
	waitEntered(t, fs)
	for _, tag := range []string{"d1", "d2", "a1"} {
		result := "deny"
		if tag == "a1" {
			result = "allow"
		}
		e.EmitDecisionAudit(ctx, auditRec(result, tag))
	}
	assert.Equal(t, int64(3), depth())

	close(fs.gate)
	waitFor(t, "all records written", func() bool {
		written, _ := fs.snapshot()
		return len(written) == 4
	})
	assert.Equal(t, int64(0), depth(), "an idle writer reports an empty queue")
}
