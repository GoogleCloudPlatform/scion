/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestLoopbackProvidersIgnoreOTELResourceEnv pins ptone/scion#2249 for every
// signal: OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME in sciontool's
// environment must not change the resource its loopback providers send.
func TestLoopbackProvidersIgnoreOTELResourceEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,host.name=leak,service.name=env-service,scion.agent.id=env-agent")
	t.Setenv("OTEL_SERVICE_NAME", "env-service-name")
	t.Setenv("SCION_AGENT_ID", "agent")
	t.Setenv("SCION_HARNESS", "claude")

	var mu sync.Mutex
	got := map[string][][]*commonpb.KeyValue{}
	record := func(signal string, attrs []*commonpb.KeyValue) {
		mu.Lock()
		defer mu.Unlock()
		got[signal] = append(got[signal], attrs)
	}
	cfg := &Config{Enabled: true}
	receiver := NewReceiver(cfg,
		func(_ context.Context, batches []*tracepb.ResourceSpans) error {
			for _, b := range batches {
				record("spans", b.GetResource().GetAttributes())
			}
			return nil
		},
		WithLogHandler(func(_ context.Context, batches []*logspb.ResourceLogs) error {
			for _, b := range batches {
				record("logs", b.GetResource().GetAttributes())
			}
			return nil
		}),
		WithMetricHandler(func(_ context.Context, batches []*metricpb.ResourceMetrics) error {
			for _, b := range batches {
				record("metrics", b.GetResource().GetAttributes())
			}
			return nil
		}),
	)
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()
	cfg.GRPCPort, _ = receiver.BoundPorts()

	providers, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, span := providers.TracerProvider.Tracer("resource.test").Start(ctx, "event")
	span.End()
	var rec otellog.Record
	rec.SetEventName("event")
	providers.LoggerProvider.Logger("resource.test").Emit(ctx, rec)
	counter, err := providers.MeterProvider.Meter("resource.test").Int64Counter("resource.counter")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 1)
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, signal := range []string{"spans", "logs", "metrics"} {
		if len(got[signal]) == 0 {
			t.Fatalf("no %s reached the receiver", signal)
		}
		for _, attrs := range got[signal] {
			var keys []string
			for _, kv := range attrs {
				keys = append(keys, kv.Key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				// Every key must come from buildResource, all of which
				// are inside the GCP resource allowlist.
				if !cloudResourceFields[key] {
					t.Fatalf("%s resource keys = %v: %q leaked from the environment", signal, keys, key)
				}
			}
			if v := attrValue(attrs, "service.name"); v != "sciontool" {
				t.Fatalf("%s service.name = %q", signal, v)
			}
			if v := attrValue(attrs, "scion.agent.id"); v != "agent" {
				t.Fatalf("%s scion.agent.id = %q", signal, v)
			}
		}
	}
}

// TestLoopbackResourceNilResourceDropsAllAttributes pins the nil-resource
// choice: no panic, and an empty allowlist rather than a pass-through, so
// nothing from the environment can reach GCP admission.
func TestLoopbackResourceNilResourceDropsAllAttributes(t *testing.T) {
	if opt := loopbackResourceDialOption(nil); opt == nil {
		t.Fatal("loopbackResourceDialOption(nil) returned nil")
	}
	allowed := loopbackResourceAllowlist(nil)
	if len(allowed) != 0 {
		t.Fatalf("allowlist for nil resource = %v, want empty", allowed)
	}
	req := &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "env"}}},
			{Key: "host.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "leak"}}},
		}},
	}}}
	pinLoopbackRequestResource(req, allowed)
	if attrs := req.ResourceMetrics[0].Resource.Attributes; len(attrs) != 0 {
		t.Fatalf("resource attributes after pinning with nil resource = %v, want none", attrs)
	}
}
