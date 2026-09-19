/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestNewProviders_NilConfig(t *testing.T) {
	p, err := NewProviders(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Error("expected nil Providers for nil config")
	}
}

func TestNewProviders_Disabled(t *testing.T) {
	cfg := &Config{
		Enabled:      false,
		CloudEnabled: true,
		Endpoint:     "localhost:4317",
	}
	p, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Error("expected nil Providers when disabled")
	}
}

func TestNewProviders_IgnoresExternalEndpoint(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		CloudEnabled: true,
		Endpoint:     "", // no endpoint
	}
	p, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected loopback Providers when cloud endpoint is empty")
	}
	shutdownProvidersForTest(t, p)
}

func TestLoopbackEndpointNeverUsesCloudDestination(t *testing.T) {
	cfg := &Config{GRPCPort: 55680, Endpoint: "cloudtrace.googleapis.com:443", CloudProvider: "gcp"}
	if got, want := loopbackEndpoint(cfg), "127.0.0.1:55680"; got != want {
		t.Fatalf("loopbackEndpoint() = %q, want %q", got, want)
	}
}

func TestNewProviders_RoutesAllSignalsToLoopback(t *testing.T) {
	grpcPort := availableTCPPort(t)
	var spans, logs, metrics atomic.Int64
	cfg := &Config{
		Enabled:       true,
		CloudEnabled:  true,
		CloudProvider: "gcp",
		ProjectID:     "cloud-project-that-must-not-be-contacted",
		Endpoint:      "cloudtrace.googleapis.com:443",
		GRPCPort:      grpcPort,
		HTTPPort:      0,
	}
	receiver := NewReceiver(cfg,
		func(_ context.Context, batches []*tracepb.ResourceSpans) error {
			spans.Add(int64(len(batches)))
			return nil
		},
		WithLogHandler(func(_ context.Context, batches []*logspb.ResourceLogs) error {
			logs.Add(int64(len(batches)))
			return nil
		}),
		WithMetricHandler(func(_ context.Context, batches []*metricpb.ResourceMetrics) error {
			metrics.Add(int64(len(batches)))
			return nil
		}),
	)
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()

	providers, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("NewProviders: %v", err)
	}
	ctx := context.Background()
	_, span := providers.TracerProvider.Tracer("routing.test").Start(ctx, "allowed.event")
	span.End()
	var record otellog.Record
	record.SetEventName("allowed.event")
	providers.LoggerProvider.Logger("routing.test").Emit(ctx, record)
	counter, err := providers.MeterProvider.Meter("routing.test").Int64Counter("routing.counter")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(ctx, 1)
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown providers: %v", err)
	}
	if spans.Load() == 0 || logs.Load() == 0 || metrics.Load() == 0 {
		t.Fatalf("loopback captures: spans=%d logs=%d metrics=%d", spans.Load(), logs.Load(), metrics.Load())
	}
}

func TestHookProviderEmitsCounterDeltaAndOtherInstrumentTemporalities(t *testing.T) {
	port := availableTCPPort(t)
	cfg := &Config{Enabled: true, GRPCPort: port, HTTPPort: 0, Endpoint: "external.invalid:4317"}
	var mu sync.Mutex
	var captured []*metricpb.ResourceMetrics
	receiver := NewReceiver(cfg, nil, WithMetricHandler(func(_ context.Context, rms []*metricpb.ResourceMetrics) error {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, rms...)
		return nil
	}))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()
	for range 2 {
		providers, err := NewProviders(context.Background(), cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		meter := providers.MeterProvider.Meter(hookMetricScope)
		counter, _ := meter.Int64Counter("agent.tool.calls")
		updown, _ := meter.Int64UpDownCounter("active")
		hist, _ := meter.Float64Histogram("duration")
		gauge, _ := meter.Int64Gauge("status")
		counter.Add(context.Background(), 1)
		updown.Add(context.Background(), 1)
		hist.Record(context.Background(), 2)
		gauge.Record(context.Background(), 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := providers.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	longLived, err := NewProviders(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	longCounter, err := longLived.MeterProvider.Meter("longlived").Int64Counter("longlived.counter")
	if err != nil {
		t.Fatal(err)
	}
	longCounter.Add(context.Background(), 1)
	if err := longLived.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var counts = map[string]int{}
	for _, rm := range captured {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				counts[m.Name]++
				switch m.Name {
				case "agent.tool.calls":
					if m.GetSum().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA || m.GetSum().DataPoints[0].GetAsInt() != 1 {
						t.Fatalf("hook counter = %v", m.GetSum())
					}
				case "active":
					if m.GetSum().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
						t.Fatalf("updown = %v", m.GetSum())
					}
				case "duration":
					if m.GetHistogram().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
						t.Fatalf("histogram = %v", m.GetHistogram())
					}
				case "status":
					if m.GetGauge() == nil {
						t.Fatalf("gauge = %v", m)
					}
				case "longlived.counter":
					if m.GetSum().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
						t.Fatalf("long-lived counter = %v", m.GetSum())
					}
				}
			}
		}
	}
	for _, name := range []string{"agent.tool.calls", "active", "duration", "status"} {
		if counts[name] != 2 {
			t.Fatalf("%s emitted %d points", name, counts[name])
		}
	}
	if counts["longlived.counter"] != 1 {
		t.Fatalf("long-lived counter emitted %d points", counts["longlived.counter"])
	}
}

func TestMetricHookChild(t *testing.T) {
	value := os.Getenv("SCION_TEST_HOOK_PORT")
	if value == "" {
		return
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := NewProviders(context.Background(), &Config{Enabled: true, GRPCPort: port}, false)
	if err != nil {
		t.Fatal(err)
	}
	counter, err := providers.MeterProvider.Meter(hookMetricScope).Int64Counter("agent.tool.calls")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTenIndependentHookProcessesAcrossFlushWindows(t *testing.T) {
	port := availableTCPPort(t)
	cfg := &Config{Enabled: true, GRPCPort: port, HTTPPort: 0}
	var totals []int64
	p := NewWithConfig(cfg)
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == "agent.tool.calls" {
						totals = append(totals, m.GetSum().DataPoints[0].GetAsInt())
					}
				}
			}
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	receiver := NewReceiver(cfg, nil, WithMetricHandler(p.handleMetrics))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()
	for i := range 10 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMetricHookChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), fmt.Sprintf("SCION_TEST_HOOK_PORT=%d", port))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook process %d: %v: %s", i, err, output)
		}
		if i == 4 {
			p.flushMetricBuffer(context.Background(), true)
		}
	}
	p.flushMetricBuffer(context.Background(), true)
	if fmt.Sprint(totals) != "[5 10]" {
		t.Fatalf("hook totals = %v", totals)
	}
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

func TestNewProviders_CloudDisabledStillUsesLocalBoundary(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		CloudEnabled: false,
		Endpoint:     "localhost:4317",
	}
	p, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected loopback Providers when cloud forwarding is disabled")
	}
	shutdownProvidersForTest(t, p)
}

func shutdownProvidersForTest(t *testing.T, providers *Providers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := providers.Shutdown(ctx); err != nil {
		t.Logf("Shutdown returned expected error without a receiver: %v", err)
	}
}

func TestProviders_ShutdownNil(t *testing.T) {
	var p *Providers
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on nil Providers should not error, got: %v", err)
	}
}

func TestNewProviders_SyncMode(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		CloudEnabled: true,
		Endpoint:     "localhost:4317",
		Insecure:     true,
	}
	p, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Providers")
	}
	if p.TracerProvider == nil {
		t.Error("expected non-nil TracerProvider")
	}
	if p.LoggerProvider == nil {
		t.Error("expected non-nil LoggerProvider")
	}
	if p.MeterProvider == nil {
		t.Error("expected non-nil MeterProvider")
	}

	// Shutdown may return export errors when no collector is listening;
	// this is expected in tests and not a provider creation failure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Logf("Shutdown returned expected export error (no collector): %v", err)
	}
}

func TestNewProviders_BatchMode(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		CloudEnabled: true,
		Endpoint:     "localhost:4317",
		Insecure:     true,
	}
	p, err := NewProviders(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Providers")
	}
	if p.TracerProvider == nil {
		t.Error("expected non-nil TracerProvider")
	}
	if p.LoggerProvider == nil {
		t.Error("expected non-nil LoggerProvider")
	}
	if p.MeterProvider == nil {
		t.Error("expected non-nil MeterProvider")
	}

	// Shutdown may return export errors when no collector is listening;
	// this is expected in tests and not a provider creation failure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Logf("Shutdown returned expected export error (no collector): %v", err)
	}
}
