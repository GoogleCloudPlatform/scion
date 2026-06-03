/*
Copyright 2026 The Scion Authors.
*/

package dispatchmetrics

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
	attrs := []attribute.KeyValue{attribute.String("op", "start")}

	r.IncPublished(ctx, 1, attrs...)
	r.IncClaimed(ctx, 1, attrs...)
	r.IncDone(ctx, 1, attrs...)
	r.IncFailed(ctx, 1, attrs...)
	r.RecordDispatchLatency(ctx, 42.5, attrs...)
	r.IncMessageDispatched(ctx, 1, attrs...)
	r.ObserveMessageStuck(ctx, 3, attrs...)
	r.IncCmdBusReconnects(ctx, 1)
	r.RecordReconcileDrainDuration(ctx, 10.0)
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
	attrs := []attribute.KeyValue{attribute.String("op", "start")}

	r.IncPublished(ctx, 2, attrs...)
	r.IncClaimed(ctx, 1, attrs...)
	r.IncDone(ctx, 1, attrs...)
	r.IncFailed(ctx, 1, attrs...)
	r.RecordDispatchLatency(ctx, 42.5, attrs...)
	r.IncMessageDispatched(ctx, 1, attrs...)
	r.ObserveMessageStuck(ctx, 3, attrs...)
	r.IncCmdBusReconnects(ctx, 1)
	r.RecordReconcileDrainDuration(ctx, 10.0)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	got := collectedNames(&rm)

	want := []string{
		MetricDispatchPublished,
		MetricDispatchClaimed,
		MetricDispatchDone,
		MetricDispatchFailed,
		MetricDispatchLatency,
		MetricMessageDispatched,
		MetricMessageStuck,
		MetricCmdBusReconnects,
		MetricReconcileDrainDur,
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
