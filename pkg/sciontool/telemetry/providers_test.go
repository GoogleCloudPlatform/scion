/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"net"
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
