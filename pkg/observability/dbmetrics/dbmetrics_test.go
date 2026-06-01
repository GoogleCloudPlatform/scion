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
	r.ObservePoolStats(ctx, PoolStats{Active: 2, Idle: 8, Waiting: 0, Max: 10}, attrs...)
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
	r.ObservePoolStats(ctx, PoolStats{Active: 2, Idle: 8, Waiting: 1, Max: 10}, attrs...)

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
		MetricPoolConnectionsWaiting,
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
