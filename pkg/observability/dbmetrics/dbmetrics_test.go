/*
Copyright 2025 The Scion Authors.
*/

package dbmetrics

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestNewDisabledRegisters verifies that the safe default (no MeterProvider,
// e.g. when no GCP project/exporter is configured) registers all instruments
// without error and reports itself disabled.
func TestNewDisabledRegisters(t *testing.T) {
	r, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) returned error: %v", err)
	}
	if r == nil {
		t.Fatal("New(nil) returned nil Recorder")
	}
	if r.Enabled() {
		t.Error("expected Recorder backed by no-op provider to report Enabled()==false")
	}
}

// TestNewDisabledRecordsAreNoops ensures every method is safe to call when
// metrics are disabled (no panics, no errors).
func TestNewDisabledRecordsAreNoops(t *testing.T) {
	r := NewDisabled()
	ctx := context.Background()
	attrs := []attribute.KeyValue{attribute.String("channel", "events")}

	// None of these should panic.
	r.RecordPublishToDeliverLatency(ctx, 12.5, attrs...)
	r.IncPublished(ctx, 1, attrs...)
	r.IncDelivered(ctx, 1, attrs...)
	r.IncDropped(ctx, 1, attrs...)
	r.ObserveSubscriberLag(ctx, 3, attrs...)
	r.IncListenerReconnects(ctx, 1, attrs...)
	r.RecordPayloadSize(ctx, 256, attrs...)
	r.ObservePoolStats(ctx, PoolStore, PoolStats{Active: 2, Idle: 8, WaitCount: 0, Max: 10}, attrs...)
}

// TestNewWithRealProviderRegisters verifies registration succeeds against a real
// SDK MeterProvider and that the resulting Recorder reports itself enabled.
func TestNewWithRealProviderRegisters(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	r, err := New(mp)
	if err != nil {
		t.Fatalf("New(mp) returned error: %v", err)
	}
	if !r.Enabled() {
		t.Error("expected Recorder backed by real provider to report Enabled()==true")
	}
}

// TestRecordedMetricsAreExported drives every instrument and asserts the
// expected metric names show up in a collected snapshot. This proves the
// registration paths are wired correctly end-to-end.
func TestRecordedMetricsAreExported(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	r, err := New(mp)
	if err != nil {
		t.Fatalf("New(mp) returned error: %v", err)
	}

	ctx := context.Background()
	attrs := []attribute.KeyValue{attribute.String("channel", "events")}

	r.RecordPublishToDeliverLatency(ctx, 12.5, attrs...)
	r.IncPublished(ctx, 2, attrs...)
	r.IncDelivered(ctx, 1, attrs...)
	r.IncDropped(ctx, 1, attrs...)
	r.ObserveSubscriberLag(ctx, 5, attrs...)
	r.IncListenerReconnects(ctx, 1, attrs...)
	r.RecordPayloadSize(ctx, 256, attrs...)
	r.ObservePoolStats(ctx, PoolStore, PoolStats{Active: 2, Idle: 8, WaitCount: 1, Max: 10}, attrs...)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	got := collectedNames(&rm)

	want := []string{
		MetricPublishToDeliverLatency,
		MetricNotificationsPublished,
		MetricNotificationsDelivered,
		MetricNotificationsDropped,
		MetricSubscriberLag,
		MetricListenerReconnects,
		MetricPayloadSize,
		MetricPoolConnectionsActive,
		MetricPoolConnectionsIdle,
		MetricPoolConnectionsWaits,
		MetricPoolConnectionsMax,
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("expected metric %q to be exported, but it was not present", name)
		}
	}
}

// collectedNames flattens the collected metric names into a set for assertion.
func collectedNames(rm *metricdata.ResourceMetrics) map[string]bool {
	names := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

// poolPoints returns the int64 data points of one collected metric keyed by
// their AttrPool value, and whether the metric is a monotonic cumulative sum.
func poolPoints(t *testing.T, rm *metricdata.ResourceMetrics, name string) (map[string]int64, bool) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var dps []metricdata.DataPoint[int64]
			counter := false
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				dps = d.DataPoints
			case metricdata.Sum[int64]:
				dps = d.DataPoints
				counter = d.IsMonotonic && d.Temporality == metricdata.CumulativeTemporality
			default:
				t.Fatalf("%s: unexpected data type %T", name, m.Data)
			}
			out := map[string]int64{}
			for _, dp := range dps {
				v, ok := dp.Attributes.Value(AttrPool)
				if !ok {
					t.Fatalf("%s: data point without %q attribute: %v", name, AttrPool, dp.Attributes)
				}
				out[v.AsString()] = dp.Value
			}
			return out, counter
		}
	}
	t.Fatalf("metric %s not collected", name)
	return nil, false
}

// TestPoolStatsPerPoolAndWaitCounter checks the two hub pools write separate
// series and the cumulative wait count is exported as a counter holding the
// pool's total, not as a gauge (ptone/scion#3618).
func TestPoolStatsPerPoolAndWaitCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	r, err := New(mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	collect := func() *metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return &rm
	}

	r.ObservePoolStats(ctx, PoolStore, PoolStats{Active: 4, Idle: 1, WaitCount: 5, Max: 10})
	r.ObservePoolStats(ctx, PoolEvents, PoolStats{Active: 1, Idle: 2, WaitCount: 2, Max: 4})
	r.ObservePoolStats(ctx, PoolStore, PoolStats{Active: 6, Idle: 0, WaitCount: 8, Max: 10})
	rm := collect()

	active, _ := poolPoints(t, rm, MetricPoolConnectionsActive)
	if active[PoolStore] != 6 || active[PoolEvents] != 1 || len(active) != 2 {
		t.Errorf("active = %v, want store=6 events=1", active)
	}
	maxConns, _ := poolPoints(t, rm, MetricPoolConnectionsMax)
	if maxConns[PoolStore] != 10 || maxConns[PoolEvents] != 4 {
		t.Errorf("max = %v, want store=10 events=4", maxConns)
	}
	waits, counter := poolPoints(t, rm, MetricPoolConnectionsWaits)
	if !counter {
		t.Errorf("%s must be a monotonic cumulative counter", MetricPoolConnectionsWaits)
	}
	if waits[PoolStore] != 8 || waits[PoolEvents] != 2 {
		t.Errorf("wait_count = %v, want store=8 events=2", waits)
	}

	// A wait count that goes down belongs to a new pool: its whole value is
	// new waits, so the counter keeps growing.
	r.ObservePoolStats(ctx, PoolStore, PoolStats{WaitCount: 3, Max: 10})
	waits, _ = poolPoints(t, collect(), MetricPoolConnectionsWaits)
	if waits[PoolStore] != 11 {
		t.Errorf("wait_count after pool reset = %v, want store=11", waits)
	}
}
