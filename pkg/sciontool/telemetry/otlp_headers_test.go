/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type headerCapturingCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	colmetricpb.UnimplementedMetricsServiceServer
	collogspb.UnimplementedLogsServiceServer

	mu        sync.Mutex
	traceMD   []metadata.MD
	metricsMD []metadata.MD
	logsMD    []metadata.MD
}

func (c *headerCapturingCollector) Export(ctx context.Context, _ *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	c.mu.Lock()
	c.traceMD = append(c.traceMD, md.Copy())
	c.mu.Unlock()
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type headerCapturingMetricsServer struct {
	colmetricpb.UnimplementedMetricsServiceServer
	collector *headerCapturingCollector
}

func (s *headerCapturingMetricsServer) Export(ctx context.Context, _ *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.collector.mu.Lock()
	s.collector.metricsMD = append(s.collector.metricsMD, md.Copy())
	s.collector.mu.Unlock()
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

type headerCapturingLogsServer struct {
	collogspb.UnimplementedLogsServiceServer
	collector *headerCapturingCollector
}

func (s *headerCapturingLogsServer) Export(ctx context.Context, _ *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.collector.mu.Lock()
	s.collector.logsMD = append(s.collector.logsMD, md.Copy())
	s.collector.mu.Unlock()
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func startHeaderCapturingCollector(t *testing.T) (string, *headerCapturingCollector) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := &headerCapturingCollector{}
	server := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(server, collector)
	colmetricpb.RegisterMetricsServiceServer(server, &headerCapturingMetricsServer{collector: collector})
	collogspb.RegisterLogsServiceServer(server, &headerCapturingLogsServer{collector: collector})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), collector
}

func assertMetadataHasHeaders(t *testing.T, label string, mds []metadata.MD, want map[string]string) {
	t.Helper()
	if len(mds) == 0 {
		t.Fatalf("%s: expected at least one export request, got 0", label)
	}
	for i, md := range mds {
		for k, wantVal := range want {
			got := md.Get(strings.ToLower(k))
			if len(got) != 1 || got[0] != wantVal {
				t.Errorf("%s[%d] metadata[%q] = %v, want [%q]", label, i, k, got, wantVal)
			}
		}
	}
}

// TestCloudExporter_AppliesOTELHeaders verifies that SCION_OTEL_HEADERS loaded
// via LoadConfig is sent as gRPC metadata on every generic OTLP export path:
// SDK spans (otlptracegrpc), raw proto spans, raw proto metrics, and raw proto logs.
func TestCloudExporter_AppliesOTELHeaders(t *testing.T) {
	defer SetTelemetryTestSandboxed()()

	addr, collector := startHeaderCapturingCollector(t)

	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvCloudEnabled, "true")
	t.Setenv(EnvCloudProvider, "")
	t.Setenv(EnvGCPCredentials, "")
	t.Setenv(EnvEndpoint, addr)
	t.Setenv(EnvProtocol, "grpc")
	t.Setenv(EnvInsecure, "true")
	t.Setenv(EnvHeaders, `{"Authorization":"Bearer test-secret-token","X-Custom-Tenant":"tenant-42"}`)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_AGENT_SLUG", "slug-1")
	t.Setenv("SCION_PROJECT_ID", "proj-1")

	cfg := LoadConfig()
	if cfg.headersErr != nil {
		t.Fatalf("unexpected headersErr: %v", cfg.headersErr)
	}

	exporter, err := NewCloudExporter(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewCloudExporter failed: %v", err)
	}
	defer func() { _ = exporter.Shutdown(context.Background()) }()

	// Mutating cfg.Headers after construction must not affect exported headers.
	cfg.Headers["Authorization"] = "mutated"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exporter.ExportSpans(ctx, tracetest.SpanStubs{{Name: "header-probe-sdk"}}.Snapshots()); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}
	if err := exporter.ExportProtoSpans(ctx, []*tracepb.ResourceSpans{{}}); err != nil {
		t.Fatalf("ExportProtoSpans failed: %v", err)
	}
	if err := exporter.ExportProtoMetrics(ctx, []*metricpb.ResourceMetrics{{}}); err != nil {
		t.Fatalf("ExportProtoMetrics failed: %v", err)
	}
	if err := exporter.ExportProtoLogs(ctx, []*logspb.ResourceLogs{{}}); err != nil {
		t.Fatalf("ExportProtoLogs failed: %v", err)
	}

	want := map[string]string{
		"authorization":   "Bearer test-secret-token",
		"x-custom-tenant": "tenant-42",
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if len(collector.traceMD) != 2 {
		t.Fatalf("expected 2 trace export calls (SDK + proto), got %d", len(collector.traceMD))
	}
	assertMetadataHasHeaders(t, "trace", collector.traceMD, want)
	assertMetadataHasHeaders(t, "metrics", collector.metricsMD, want)
	assertMetadataHasHeaders(t, "logs", collector.logsMD, want)
}

// TestCloudExporter_MalformedOTELHeadersReturnsError verifies that a malformed
// SCION_OTEL_HEADERS value produces a clear error without echoing the raw value.
func TestCloudExporter_MalformedOTELHeadersReturnsError(t *testing.T) {
	defer SetTelemetryTestSandboxed()()

	const secretPayload = "Bearer super-secret-should-not-leak"
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvCloudEnabled, "true")
	t.Setenv(EnvCloudProvider, "")
	t.Setenv(EnvGCPCredentials, "")
	t.Setenv(EnvEndpoint, "127.0.0.1:4317")
	t.Setenv(EnvProtocol, "grpc")
	t.Setenv(EnvInsecure, "true")
	t.Setenv(EnvHeaders, `not-json-`+secretPayload)

	cfg := LoadConfig()
	_, err := NewCloudExporter(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for malformed SCION_OTEL_HEADERS, got nil")
	}
	if !strings.Contains(err.Error(), EnvHeaders) {
		t.Errorf("error %q should mention %s", err.Error(), EnvHeaders)
	}
	if strings.Contains(err.Error(), secretPayload) {
		t.Errorf("error %q must not echo raw header value", err.Error())
	}
}

// TestLoopbackProviders_IgnoreSCIONOTELHeaders verifies that SCION_OTEL_HEADERS
// does not leak onto loopback exports (ptone/scion#2992).
func TestLoopbackProviders_IgnoreSCIONOTELHeaders(t *testing.T) {
	defer SetTelemetryTestSandboxed()()

	addr, collector := startHeaderCapturingCollector(t)
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvGRPCPort, portStr)
	t.Setenv(EnvHeaders, `{"Authorization":"Bearer cloud-only-secret"}`)

	cfg := LoadConfig()
	providers, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("NewProviders failed: %v", err)
	}
	_, span := providers.TracerProvider.Tracer("test").Start(context.Background(), "loopback-probe")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if len(collector.traceMD) == 0 {
		t.Fatal("expected loopback trace export to reach collector")
	}
	for i, md := range collector.traceMD {
		if got := md.Get("authorization"); len(got) > 0 {
			t.Errorf("loopback trace[%d] leaked authorization header: %v", i, got)
		}
	}
}
