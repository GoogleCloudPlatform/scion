/*
Copyright 2025 The Scion Authors.
*/

package dbmetrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
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
	r.ObservePoolStats(ctx, PoolStore, PoolStats{Active: 2, Idle: 8, WaitCount: 1, Max: 10, WaitDuration: time.Second}, attrs...)
	tr := r.(interface {
		RecordTxHold(context.Context, float64, string, string, string)
		RecordTxCommit(context.Context, float64, string, string)
		RecordLockWait(context.Context, float64, string, string)
		IncDiagDropped(context.Context, int64)
	})
	tr.RecordTxHold(ctx, 1, "op", "tx", "commit")
	tr.RecordTxCommit(ctx, 1, "op", "commit")
	tr.RecordLockWait(ctx, 1, "op", "acquired")
	tr.IncDiagDropped(ctx, 1)

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
		MetricPoolWaitDuration,
		MetricTxHoldDuration,
		MetricTxLockWaitDuration,
		MetricTxCommitDuration,
		MetricDiagWarnDropped,
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

// TestPoolStatsCallerCannotOverridePool checks a caller attribute named
// "pool" does not replace the required pool name.
func TestPoolStatsCallerCannotOverridePool(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	r, err := New(mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	r.ObservePoolStats(ctx, PoolEvents, PoolStats{Active: 3, Max: 4}, attribute.String(AttrPool, "other"))
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	active, _ := poolPoints(t, &rm, MetricPoolConnectionsActive)
	if len(active) != 1 || active[PoolEvents] != 3 {
		t.Fatalf("active = %v, want only events=3", active)
	}
}

// TestPoolWaitDurationCounter checks the cumulative pool wait time is
// exported as a counter of milliseconds, per pool, from snapshot deltas.
func TestPoolWaitDurationCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	r, err := New(mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	r.ObservePoolStats(ctx, PoolStore, PoolStats{WaitDuration: 1500 * time.Millisecond})
	r.ObservePoolStats(ctx, PoolStore, PoolStats{WaitDuration: 4 * time.Second})
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got, counter := poolPoints(t, &rm, MetricPoolWaitDuration)
	if !counter {
		t.Errorf("%s must be a monotonic cumulative counter", MetricPoolWaitDuration)
	}
	if got[PoolStore] != 4000 {
		t.Errorf("wait_duration = %v, want store=4000 ms", got)
	}
}

// TestTxDiagnosticsInstruments checks the transaction diagnostics
// histograms carry op/kind/outcome/pool and use buckets reaching 120 s, and
// the drop counter counts.
func TestTxDiagnosticsInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := New(mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := rec.(*recorder)
	ctx := context.Background()
	r.RecordTxHold(ctx, 3000, "hub.f", "tx", "commit")
	r.RecordLockWait(ctx, 90000, "hub.g", "deadline")
	r.RecordTxCommit(ctx, 2500, "hub.h", "commit")
	r.IncDiagDropped(ctx, 2)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricTxHoldDuration, MetricTxLockWaitDuration, MetricTxCommitDuration:
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok || len(h.DataPoints) != 1 {
					t.Fatalf("%s: data %T with %v", m.Name, m.Data, m.Data)
				}
				dp := h.DataPoints[0]
				if b := dp.Bounds; len(b) == 0 || b[len(b)-1] != 120000 {
					t.Errorf("%s: bounds %v, want the last at 120000 ms", m.Name, b)
				}
				want := map[string]string{AttrOp: "hub.f", AttrKind: "tx", AttrOutcome: "commit", AttrPool: PoolStore}
				switch m.Name {
				case MetricTxLockWaitDuration:
					want = map[string]string{AttrOp: "hub.g", AttrOutcome: "deadline", AttrPool: PoolStore}
				case MetricTxCommitDuration:
					want = map[string]string{AttrOp: "hub.h", AttrOutcome: "commit", AttrPool: PoolStore}
				}
				if dp.Attributes.Len() != len(want) {
					t.Errorf("%s: attributes %v, want %v", m.Name, dp.Attributes, want)
				}
				for k, v := range want {
					if got, ok := dp.Attributes.Value(attribute.Key(k)); !ok || got.AsString() != v {
						t.Errorf("%s: %s = %v, want %q", m.Name, k, got, v)
					}
				}
				found[m.Name] = true
			case MetricDiagWarnDropped:
				s, ok := m.Data.(metricdata.Sum[int64])
				if !ok || len(s.DataPoints) != 1 || s.DataPoints[0].Value != 2 {
					t.Errorf("%s: %v", m.Name, m.Data)
				}
				found[m.Name] = true
			}
		}
	}
	for _, n := range []string{MetricTxHoldDuration, MetricTxLockWaitDuration, MetricTxCommitDuration, MetricDiagWarnDropped} {
		if !found[n] {
			t.Errorf("metric %s not collected", n)
		}
	}
}

// The hub attaches the dbmetrics recorder to the transaction diagnostics by
// a type assertion to txdiag.Recorder; fail the build if it stops matching.
var _ txdiag.Recorder = (*recorder)(nil)
