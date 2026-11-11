/*
Copyright 2026 The Scion Authors.
*/

package reapermetrics

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

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

func TestNewDisabledRecordsAreNoops(t *testing.T) {
	r := NewDisabled()
	ctx := context.Background()
	attrs := []attribute.KeyValue{attribute.String("outcome", "completed")}

	r.IncTicks(ctx, 1, attrs...)
	r.IncRowErrors(ctx, 1)
	r.RecordDisarmedFor(ctx, 42.5)
}

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

func TestRecordedMetricsAreExported(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	r, err := New(mp)
	if err != nil {
		t.Fatalf("New(mp) returned error: %v", err)
	}

	ctx := context.Background()
	attrs := []attribute.KeyValue{attribute.String("outcome", "completed")}

	r.IncTicks(ctx, 1, attrs...)
	r.IncRowErrors(ctx, 2)
	r.RecordDisarmedFor(ctx, 42.5)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	got := collectedNames(&rm)

	want := []string{
		MetricLaunchReaperTicks,
		MetricLaunchReaperRowErrors,
		MetricLaunchReaperDisarmedFor,
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("expected metric %q to be exported, but it was not present", name)
		}
	}
}

func collectedNames(rm *metricdata.ResourceMetrics) map[string]bool {
	names := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}
