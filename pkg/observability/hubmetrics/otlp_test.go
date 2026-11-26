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

package hubmetrics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type fakeOTLPMetricsServer struct {
	colmetricpb.UnimplementedMetricsServiceServer
	mu       sync.Mutex
	requests []*colmetricpb.ExportMetricsServiceRequest
	headers  []metadata.MD
}

func (f *fakeOTLPMetricsServer) Export(ctx context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, md.Copy())
	f.mu.Unlock()
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func startFakeOTLPMetricsServer(t *testing.T) (string, *fakeOTLPMetricsServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeOTLPMetricsServer{}
	colmetricpb.RegisterMetricsServiceServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String(), fake
}

func TestLoadOTLPConfigFromEnv(t *testing.T) {
	t.Run("unset returns disabled", func(t *testing.T) {
		cfg, err := loadOTLPConfig(func(string) string { return "" })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Enabled() {
			t.Fatalf("expected disabled OTLP config, got %+v", cfg)
		}
	})

	t.Run("parses endpoint insecure and headers", func(t *testing.T) {
		env := map[string]string{
			EnvOTelEndpoint: "collector.internal:4317",
			EnvOTelInsecure: "true",
			EnvOTelHeaders:  `{"Authorization":"Bearer tok-123","X-Tenant":"acme"}`,
		}
		cfg, err := loadOTLPConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.Enabled() || cfg.Endpoint != "collector.internal:4317" || !cfg.Insecure {
			t.Fatalf("unexpected config: %+v", cfg)
		}
		if cfg.Headers["Authorization"] != "Bearer tok-123" || cfg.Headers["X-Tenant"] != "acme" {
			t.Fatalf("unexpected headers: %+v", cfg.Headers)
		}
	})

	t.Run("malformed headers error does not echo secret", func(t *testing.T) {
		env := map[string]string{
			EnvOTelEndpoint: "collector.internal:4317",
			EnvOTelHeaders:  "not-json-secret-value-xyz",
		}
		_, err := loadOTLPConfig(func(k string) string { return env[k] })
		if err == nil {
			t.Fatal("expected error for malformed SCION_OTEL_HEADERS")
		}
		if strings.Contains(err.Error(), "secret-value-xyz") {
			t.Fatalf("error echoed raw header value: %v", err)
		}
	})

	t.Run("non-grpc protocol rejected", func(t *testing.T) {
		env := map[string]string{
			EnvOTelEndpoint: "collector.internal:4318",
			EnvOTelProtocol: "http",
		}
		_, err := loadOTLPConfig(func(k string) string { return env[k] })
		if err == nil {
			t.Fatal("expected error for unsupported SCION_OTEL_PROTOCOL=http")
		}
		if !strings.Contains(err.Error(), "grpc") {
			t.Fatalf("expected error to mention grpc, got: %v", err)
		}
	})
}

func TestNewMeterProvider_OTLPHeadersAndDistinctInstanceID(t *testing.T) {
	addr, fake := startFakeOTLPMetricsServer(t)
	ctx := context.Background()

	otlpCfg := OTLPConfig{
		Endpoint: addr,
		Insecure: true,
		Headers: map[string]string{
			"x-api-key": "hub-otlp-secret",
		},
	}

	mp1, err := NewMeterProvider(ctx, "",
		WithHubID("hub-shared"),
		WithInstanceID("hub-replica-1"),
		WithOTLPConfig(otlpCfg),
		WithExportInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewMeterProvider replica 1: %v", err)
	}
	mp2, err := NewMeterProvider(ctx, "",
		WithHubID("hub-shared"),
		WithInstanceID("hub-replica-2"),
		WithOTLPConfig(otlpCfg),
		WithExportInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewMeterProvider replica 2: %v", err)
	}

	c1, err := mp1.Meter("test").Int64Counter("scion.usage.tokens")
	if err != nil {
		t.Fatalf("counter 1: %v", err)
	}
	c2, err := mp2.Meter("test").Int64Counter("scion.usage.tokens")
	if err != nil {
		t.Fatalf("counter 2: %v", err)
	}
	c1.Add(ctx, 10)
	c2.Add(ctx, 20)

	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := mp1.Shutdown(flushCtx); err != nil {
		t.Fatalf("mp1.Shutdown: %v", err)
	}
	if err := mp2.Shutdown(flushCtx); err != nil {
		t.Fatalf("mp2.Shutdown: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 {
		t.Fatalf("expected at least 2 OTLP export requests, got %d", len(fake.requests))
	}

	seenInstances := make(map[string]bool)
	seenHeader := false
	for i, req := range fake.requests {
		if vals := fake.headers[i].Get("x-api-key"); len(vals) > 0 && vals[0] == "hub-otlp-secret" {
			seenHeader = true
		}
		for _, rm := range req.GetResourceMetrics() {
			if id := findResourceAttr(rm, "service.instance.id"); id != "" {
				seenInstances[id] = true
			}
		}
	}
	if !seenHeader {
		t.Fatalf("expected x-api-key header from SCION_OTEL_HEADERS in OTLP metadata, got %+v", fake.headers)
	}
	if !seenInstances["hub-replica-1"] || !seenInstances["hub-replica-2"] {
		t.Fatalf("expected both distinct service.instance.id values, got %+v", seenInstances)
	}
}

func TestNewMeterProvider_ConflictingOTLPTransportFails(t *testing.T) {
	_, err := NewMeterProvider(context.Background(), "",
		WithInstanceID("hub-bad-otlp"),
		WithOTLPConfig(OTLPConfig{
			Endpoint:      "127.0.0.1:4317",
			Insecure:      true,
			SkipTLSVerify: true,
		}),
	)
	if err == nil {
		t.Fatal("expected error when plaintext OTLP conflicts with TLS options")
	}

	_, err = NewMeterProvider(context.Background(), "",
		WithInstanceID("hub-http-otlp"),
		WithOTLPConfig(OTLPConfig{
			Endpoint: "127.0.0.1:4318",
			Protocol: "http",
			Insecure: true,
		}),
	)
	if err == nil {
		t.Fatal("expected error when OTLP protocol is not grpc")
	}
}

func findResourceAttr(rm *metricpb.ResourceMetrics, key string) string {
	for _, kv := range rm.GetResource().GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func TestNewMeterProvider_GCPFailureWithValidOTLPReturnsPartialErrorAndWarns(t *testing.T) {
	addr, _ := startFakeOTLPMetricsServer(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/scion-test-creds.json")

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mp, err := NewMeterProvider(context.Background(), "some-gcp-project",
		WithInstanceID("hub-partial-err"),
		WithOTLPConfig(OTLPConfig{
			Endpoint: addr,
			Insecure: true,
		}),
		WithExportInterval(time.Hour),
	)
	if mp == nil {
		t.Fatalf("expected usable OTLP MeterProvider when GCP exporter fails, got nil (err=%v)", err)
	}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	var partialErr *PartialError
	if !errors.As(err, &partialErr) {
		t.Fatalf("expected *PartialError when GCP exporter fails and OTLP succeeds, got %T (%v)", err, err)
	}
	if partialErr.GCPErr == nil {
		t.Fatal("expected PartialError.GCPErr to be non-nil")
	}
	if partialErr.OTLPErr != nil {
		t.Fatalf("expected PartialError.OTLPErr to be nil, got %v", partialErr.OTLPErr)
	}
	if !strings.Contains(logBuf.String(), "GCP Cloud Monitoring exporter disabled") {
		t.Fatalf("expected warning log naming GCP exporter failure, got %q", logBuf.String())
	}
}

func TestUsageCardinalityLimitAndOverflowDetection(t *testing.T) {
	if got, ok := ParseUsageOTelCardinalityLimit(""); !ok || got != DefaultUsageOTelCardinalityLimit {
		t.Fatalf("empty limit = (%d, %v), want (%d, true)", got, ok, DefaultUsageOTelCardinalityLimit)
	}
	if got, ok := ParseUsageOTelCardinalityLimit("50000"); !ok || got != 50000 {
		t.Fatalf("50000 = (%d, %v), want (50000, true)", got, ok)
	}
	for _, bad := range []string{"0", "-5", "abc", "9999999"} {
		if got, ok := ParseUsageOTelCardinalityLimit(bad); ok || got != DefaultUsageOTelCardinalityLimit {
			t.Fatalf("ParseUsageOTelCardinalityLimit(%q) = (%d, %v), want (%d, false)", bad, got, ok, DefaultUsageOTelCardinalityLimit)
		}
	}

	// Verify overflow detection + rate-limited warning using a small limit of 5.
	reader := sdkmetric.NewManualReader()
	opts := append([]sdkmetric.Option{sdkmetric.WithReader(reader)}, UsageMetricViews(5)...)
	mp := sdkmetric.NewMeterProvider(opts...)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	c, err := mp.Meter(hubUsageInstrumentationScope).Int64Counter("scion.usage.tokens")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	for i := 0; i < 10; i++ {
		c.Add(context.Background(), 1, otelmetric.WithAttributes(attribute.Int("idx", i)))
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	now := time.Now()
	tracker := &usageOverflowTracker{now: func() time.Time { return now }}
	tracker.observe(&rm)
	tracker.observe(&rm) // rate-limited: should not warn twice within 5m
	if got := tracker.count.Load(); got != 2 {
		t.Fatalf("overflow count = %d, want 2", got)
	}
	if warnings := strings.Count(logBuf.String(), "otel.metric.overflow=true"); warnings != 1 {
		t.Fatalf("expected 1 rate-limited overflow warning, got %d in %q", warnings, logBuf.String())
	}
}
