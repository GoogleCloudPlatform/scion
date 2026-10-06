/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func newGCPHookTestPipeline(t *testing.T, now *time.Time) (*Pipeline, *captureMetricExporter) {
	t.Helper()
	exp := &captureMetricExporter{}
	p := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp", ProjectID: "test-project"})
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: exp}}
	p.metricNow = func() time.Time { return *now }
	return p, exp
}

func hookToolCall(start, end time.Time) []*metricpb.ResourceMetrics {
	return []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "hook",
		testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			uint64(start.UnixNano()), uint64(end.UnixNano()), 1, metricStringLabel("tool_name", "Bash")))}
}

func exportedSumStarts(t *testing.T, exp *captureMetricExporter) []time.Time {
	t.Helper()
	var starts []time.Time
	for _, rm := range exp.exports {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					starts = append(starts, dp.StartTime)
				}
			}
		}
	}
	return starts
}

// TestPipelineHookCounterStartTimeAfterEarlyFlushTick is the regression test
// for the zero-start-time agent.tool.calls stream seen in AC-1.5
// (ptone/scion#2253). The metric flush loop ticks every second from Start,
// so it normally runs before the first metric arrives. It used to create the
// stream state without the GCP flag, so every hook stream then got a zero
// collector epoch and was exported with a zero start time, which Cloud
// Monitoring rejects for the whole batch on every retry.
func TestPipelineHookCounterStartTimeAfterEarlyFlushTick(t *testing.T) {
	now := time.Date(2026, 10, 6, 13, 30, 0, 0, time.UTC)
	p, exp := newGCPHookTestPipeline(t, &now)

	// The flush ticker wins the race with the first metric.
	p.flushMetricBuffer(context.Background(), false)
	if !p.metricStreams.gcp {
		t.Fatal("stream state created by the flush loop must carry the GCP provider mode")
	}

	now = now.Add(time.Second)
	if err := p.handleMetrics(context.Background(), hookToolCall(now.Add(-time.Second), now)); err != nil {
		t.Fatal(err)
	}
	epoch := now
	now = now.Add(time.Second)
	p.flushMetricBuffer(context.Background(), true)

	starts := exportedSumStarts(t, exp)
	if len(starts) != 1 {
		t.Fatalf("exported %d sum points, want 1", len(starts))
	}
	if starts[0].IsZero() || !starts[0].Equal(epoch) {
		t.Fatalf("hook start time = %v, want collector epoch %v", starts[0], epoch)
	}
}

// TestSnapshotGCPNeverEmitsZeroHookStart pins the guard: a hook stream that
// somehow has no collector epoch starts one at the next observation and is
// exported on a later flush, never with a zero start time.
func TestSnapshotGCPNeverEmitsZeroHookStart(t *testing.T) {
	now := time.Date(2026, 10, 6, 13, 30, 0, 0, time.UTC)
	s := newMetricStreams()
	s.gcp = true
	s.now = func() time.Time { return now }
	if err := s.add(hookToolCall(now.Add(-time.Second), now)); err != nil {
		t.Fatal(err)
	}
	for _, entry := range s.streams {
		entry.collectorEpoch = 0
	}

	now = now.Add(time.Second)
	if batch, _, ok := s.snapshotGCP(now, nil); ok || batch != nil {
		t.Fatal("a hook stream without a collector epoch must not be exported")
	}
	epoch := now

	now = now.Add(time.Second)
	batch, _, ok := s.snapshotGCP(now, nil)
	if !ok || len(batch) != 1 {
		t.Fatalf("expected one exported stream on the next flush, got ok=%v len=%d", ok, len(batch))
	}
	point := batch[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	if point.StartTimeUnixNano != uint64(epoch.UnixNano()) {
		t.Fatalf("start = %d, want epoch %d", point.StartTimeUnixNano, epoch.UnixNano())
	}
}
