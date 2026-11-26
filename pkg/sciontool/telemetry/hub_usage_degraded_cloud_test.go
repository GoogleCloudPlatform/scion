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

package telemetry

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func sendClaudeUsageLogGRPC(t *testing.T, grpcPort int, model string, input, output, cacheRead int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(
		"127.0.0.1:"+strconv.Itoa(grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := collogspb.NewLogsServiceClient(conn)
	_, err = client.Export(ctx, &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope},
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: uint64(time.Now().UTC().UnixNano()),
					Attributes: []*commonpb.KeyValue{
						{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
						{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: model}}},
						{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: input}}},
						{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: output}}},
						{Key: "cache_read_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: cacheRead}}},
					},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("OTLP gRPC Export logs: %v", err)
	}
}

// TestPipeline_HubUsageSinkStartsWhenExplicitCloudMissingDestination verifies
// that when Hub usage metering is active and cloud export was explicitly enabled
// without a destination, the pipeline still starts the Hub usage sink, outbox,
// and loopback receiver, surfaces the cloud misconfiguration via
// DeliveryState()=="degraded", and delivers native usage events to the Hub.
func TestPipeline_HubUsageSinkStartsWhenExplicitCloudMissingDestination(t *testing.T) {
	cloudDefaultEnv(t, "true") // explicit SCION_TELEMETRY_CLOUD_ENABLED=true, no destination
	t.Setenv("SCION_AGENT_ID", "agent-degraded-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotEvents []telemetrycontract.UsageEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotEvents = append(gotEvents, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	p := New()
	if p == nil {
		t.Fatal("New() = nil, want pipeline")
	}
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:          httptestPostFunc(srv.URL, "agent-degraded-1", "tok-1"),
		RunID:         "run-degraded-1",
		HubID:         "hub-1",
		DrainInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil when Hub usage sink is active", err)
	}
	if !p.IsRunning() {
		t.Fatal("pipeline not running after Start")
	}
	if p.exporter != nil {
		t.Error("expected nil cloud exporter when destination is missing")
	}
	if p.Config().CloudEnabled {
		t.Error("CloudEnabled must be cleared so loopback intake does not reject batches")
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() = %q, want %q to surface cloud misconfiguration", got, "degraded")
	}
	if _, ok, reason := p.AnalyticsDiagnostics(); !ok || reason != "" {
		t.Fatalf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:true reason:\"\"", ok, reason)
	}

	sendClaudeUsageLogGRPC(t, p.Config().GRPCPort, "claude-haiku-4-5-20251001", 100, 25, 400)

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() after Stop = %q, want %q", got, "degraded")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotEvents) != 1 {
		t.Fatalf("received %d usage events at Hub, want 1", len(gotEvents))
	}
	ev := gotEvents[0]
	if ev.Input == nil || *ev.Input != 100 || ev.Output == nil || *ev.Output != 25 || ev.CacheRead == nil || *ev.CacheRead != 400 {
		t.Errorf("unexpected token counts: input=%v output=%v cache_read=%v", ev.Input, ev.Output, ev.CacheRead)
	}
}

// TestPipeline_HubUsageSinkStartsWhenCloudExporterFails verifies that when
// NewCloudExporter fails (e.g. unreadable CA file), a pipeline with an active
// Hub usage sink degrades cloud export cleanly, reports DeliveryState()=="degraded",
// and continues metering native usage events to the Hub.
func TestPipeline_HubUsageSinkStartsWhenCloudExporterFails(t *testing.T) {
	cloudDefaultEnv(t, "true")
	t.Setenv(EnvEndpoint, "127.0.0.1:4317")
	t.Setenv(EnvCAFile, "/nonexistent/scion-test-ca.pem")
	t.Setenv("SCION_AGENT_ID", "agent-degraded-2")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotEvents []telemetrycontract.UsageEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotEvents = append(gotEvents, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	p := New()
	if p == nil {
		t.Fatal("New() = nil, want pipeline")
	}
	if !p.Config().IsCloudConfigured() {
		t.Fatal("precondition: expected IsCloudConfigured() == true so NewCloudExporter is attempted")
	}
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:          httptestPostFunc(srv.URL, "agent-degraded-2", "tok-1"),
		RunID:         "run-degraded-2",
		HubID:         "hub-1",
		DrainInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil when Hub usage sink is active despite cloud exporter failure", err)
	}
	if !p.IsRunning() {
		t.Fatal("pipeline not running after Start")
	}
	if p.exporter != nil {
		t.Error("expected nil cloud exporter after NewCloudExporter failure")
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() = %q, want %q", got, "degraded")
	}

	sendClaudeUsageLogGRPC(t, p.Config().GRPCPort, "claude-haiku-4-5-20251001", 42, 13, 0)

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotEvents) != 1 {
		t.Fatalf("received %d usage events at Hub, want 1", len(gotEvents))
	}
	if gotEvents[0].Input == nil || *gotEvents[0].Input != 42 || gotEvents[0].Output == nil || *gotEvents[0].Output != 13 {
		t.Errorf("unexpected token counts: input=%v output=%v", gotEvents[0].Input, gotEvents[0].Output)
	}
}

// TestNew_ExplicitDisabledVsDefaultUnset distinguishes an explicit
// SCION_TELEMETRY_ENABLED=false (pipeline nil) from unset default (pipeline
// created with Enabled=true, CloudEnabledExplicit=false).
func TestNew_ExplicitDisabledVsDefaultUnset(t *testing.T) {
	t.Run("unset default creates pipeline", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(SetTelemetryTestSandboxed())
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvEnabled, "")
		if err := os.Unsetenv(EnvEnabled); err != nil {
			t.Fatal(err)
		}
		p := New()
		if p == nil {
			t.Fatal("New() = nil on unset default, want non-nil pipeline")
		}
		if !p.Config().Enabled || p.Config().CloudEnabledExplicit {
			t.Errorf("unexpected default config: Enabled=%v CloudEnabledExplicit=%v", p.Config().Enabled, p.Config().CloudEnabledExplicit)
		}
	})

	t.Run("explicit false returns nil", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(SetTelemetryTestSandboxed())
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvEnabled, "false")
		if p := New(); p != nil {
			t.Fatal("New() != nil on explicit SCION_TELEMETRY_ENABLED=false, want nil")
		}
	})
}

type fakeCloudCollector struct {
	collogspb.UnimplementedLogsServiceServer
	colmetricpb.UnimplementedMetricsServiceServer
	coltracepb.UnimplementedTraceServiceServer

	mu      sync.Mutex
	logs    []*logspb.ResourceLogs
	metrics []*metricpb.ResourceMetrics
	spans   []*tracepb.ResourceSpans
}

func (f *fakeCloudCollector) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	f.logs = append(f.logs, req.ResourceLogs...)
	f.mu.Unlock()
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type fakeCloudMetricsCollector struct {
	colmetricpb.UnimplementedMetricsServiceServer
	parent *fakeCloudCollector
}

func (f *fakeCloudMetricsCollector) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	f.parent.mu.Lock()
	f.parent.metrics = append(f.parent.metrics, req.ResourceMetrics...)
	f.parent.mu.Unlock()
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

type fakeCloudTraceCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	parent *fakeCloudCollector
}

func (f *fakeCloudTraceCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	f.parent.mu.Lock()
	f.parent.spans = append(f.parent.spans, req.ResourceSpans...)
	f.parent.mu.Unlock()
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func startFakeCloudCollector(t *testing.T) (string, *fakeCloudCollector) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	fc := &fakeCloudCollector{}
	srv := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(srv, fc)
	colmetricpb.RegisterMetricsServiceServer(srv, &fakeCloudMetricsCollector{parent: fc})
	coltracepb.RegisterTraceServiceServer(srv, &fakeCloudTraceCollector{parent: fc})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fc
}

// TestPipeline_MeteringOnlySuppressesNativeCloudExport verifies R1 (ptone/scion#9492):
// when SetNativeMeteringOnly(true) is active (SCION_NATIVE_TELEMETRY_POLICY=metering),
// sciontool still derives canonical usage events from native harness logs/metrics
// for the Hub usage sink, does NOT forward any native harness logs or metrics to
// an active cloud exporter, and continues forwarding sciontool's own spans.
func TestPipeline_MeteringOnlySuppressesNativeCloudExport(t *testing.T) {
	collectorAddr, fc := startFakeCloudCollector(t)

	clearTelemetryEnv()
	t.Cleanup(SetTelemetryTestSandboxed())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_AGENT_ID", "agent-metering-r1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotHubEvents []telemetrycontract.UsageEvent
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotHubEvents = append(gotHubEvents, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer hubSrv.Close()

	cfg := &Config{
		Enabled:      true,
		CloudEnabled: true,
		Endpoint:     collectorAddr,
		Protocol:     "grpc",
		Insecure:     true,
		GRPCPort:     0,
		HTTPPort:     0,
	}
	p := NewWithConfig(cfg)
	p.SetAnalyticsOutbox(t.TempDir(), 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:          httptestPostFunc(hubSrv.URL, "agent-metering-r1", "tok-1"),
		RunID:         "run-metering-r1",
		HubID:         "hub-1",
		DrainInterval: 50 * time.Millisecond,
	})
	p.SetNativeMeteringOnly(true)

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	if p.exporter == nil {
		t.Fatal("precondition: expected active cloud exporter")
	}

	conn, err := grpc.NewClient(
		"127.0.0.1:"+strconv.Itoa(p.Config().GRPCPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 1. Send a native Claude api_request log (service.name="claude-code").
	logClient := collogspb.NewLogsServiceClient(conn)
	if _, err := logClient.Export(ctx, &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-code"}}},
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope},
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: uint64(time.Now().UTC().UnixNano()),
					Attributes: []*commonpb.KeyValue{
						{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
						{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-haiku-4-5-20251001"}}},
						{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 210}}},
						{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 45}}},
					},
				}},
			}},
		}},
	}); err != nil {
		t.Fatalf("Export native log: %v", err)
	}

	// 2. Send a native Gemini CLI metric (service.name="gemini-cli").
	metricClient := colmetricpb.NewMetricsServiceClient(conn)
	if _, err := metricClient.Export(ctx, &colmetricpb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricpb.ResourceMetrics{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gemini-cli"}}},
				},
			},
			ScopeMetrics: []*metricpb.ScopeMetrics{{
				Scope: &commonpb.InstrumentationScope{Name: "gemini_cli"},
				Metrics: []*metricpb.Metric{{
					Name: "gemini_cli.token.usage",
					Data: &metricpb.Metric_Sum{
						Sum: &metricpb.Sum{
							AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
							IsMonotonic:            true,
							DataPoints: []*metricpb.NumberDataPoint{{
								StartTimeUnixNano: uint64(time.Now().Add(-time.Second).UnixNano()),
								TimeUnixNano:      uint64(time.Now().UnixNano()),
								Value:             &metricpb.NumberDataPoint_AsInt{AsInt: 99},
							}},
						},
					},
				}},
			}},
		}},
	}); err != nil {
		t.Fatalf("Export native metric: %v", err)
	}

	// 3. Send a native harness span (service.name="copilot-cli") — must be dropped before cloud export (O2).
	traceClient := coltracepb.NewTraceServiceClient(conn)
	if _, err := traceClient.Export(ctx, &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "copilot-cli"}}},
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "copilot"},
				Spans: []*tracepb.Span{{
					TraceId:           []byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					SpanId:            []byte{2, 2, 3, 4, 5, 6, 7, 8},
					Name:              "copilot.native.turn",
					StartTimeUnixNano: uint64(time.Now().Add(-time.Millisecond).UnixNano()),
					EndTimeUnixNano:   uint64(time.Now().UnixNano()),
				}},
			}},
		}},
	}); err != nil {
		t.Fatalf("Export native span: %v", err)
	}

	// 4. Send a sciontool-owned span (service.name="sciontool") — must still reach cloud exporter.
	if _, err := traceClient.Export(ctx, &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "sciontool"}}},
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "sciontool"},
				Spans: []*tracepb.Span{{
					TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
					Name:              "agent.lifecycle.pre_start",
					StartTimeUnixNano: uint64(time.Now().Add(-time.Millisecond).UnixNano()),
					EndTimeUnixNano:   uint64(time.Now().UnixNano()),
				}},
			}},
		}},
	}); err != nil {
		t.Fatalf("Export sciontool span: %v", err)
	}

	// Verify O5: metering-filtered records are counted in delivery diagnostics.
	diags := p.Diagnostics()
	if diags["logs"].Filtered < 1 {
		t.Errorf("logs.Filtered = %d, want >= 1 (metering-filtered native log)", diags["logs"].Filtered)
	}
	if diags["metrics"].Filtered < 1 {
		t.Errorf("metrics.Filtered = %d, want >= 1 (metering-filtered native metric)", diags["metrics"].Filtered)
	}
	if diags["spans"].Filtered < 1 {
		t.Errorf("spans.Filtered = %d, want >= 1 (metering-filtered native span)", diags["spans"].Filtered)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Pipeline.Stop: %v", err)
	}

	// Hub usage sink MUST have received the derived Claude usage event.
	mu.Lock()
	hubCount := len(gotHubEvents)
	var firstEv telemetrycontract.UsageEvent
	if hubCount > 0 {
		firstEv = gotHubEvents[0]
	}
	mu.Unlock()
	if hubCount != 1 {
		t.Fatalf("Hub received %d usage events, want 1", hubCount)
	}
	if firstEv.Input == nil || *firstEv.Input != 210 || firstEv.Output == nil || *firstEv.Output != 45 {
		t.Errorf("unexpected Hub token counts: input=%v output=%v", firstEv.Input, firstEv.Output)
	}

	// Fake cloud collector MUST have received ZERO native harness logs, native
	// metrics, or native spans, while still receiving sciontool's own span.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.logs) != 0 {
		t.Errorf("cloud exporter received %d native log batches in metering-only mode, want 0", len(fc.logs))
	}
	for _, rm := range fc.metrics {
		if !isSciontoolResource(rm.GetResource()) {
			t.Errorf("cloud exporter received native harness metric batch in metering-only mode: %+v", rm.GetResource())
		}
	}
	if len(fc.spans) == 0 {
		t.Error("cloud exporter received 0 sciontool spans, want >= 1 (sciontool spans must keep flowing)")
	}
	for _, rs := range fc.spans {
		if !isSciontoolResource(rs.GetResource()) {
			t.Errorf("cloud exporter received native harness span batch in metering-only mode: %+v", rs.GetResource())
		}
	}
}

// TestPipeline_MeteringOnlyNoCloudDoesNotDropWarn verifies that in metering-only
// mode with no cloud exporter configured, native harness logs do not increment
// Dropped or trigger the "cloud exporter is not configured — logs will be dropped"
// ERROR log.
func TestPipeline_MeteringOnlyNoCloudDoesNotDropWarn(t *testing.T) {
	cloudDefaultEnv(t, "")
	t.Setenv("SCION_AGENT_ID", "agent-metering-nocloud")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	p := New()
	if p == nil {
		t.Fatal("New() = nil")
	}
	p.SetAnalyticsOutbox(t.TempDir(), 0, 0)
	p.SetNativeMeteringOnly(true)

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sendClaudeUsageLogGRPC(t, p.Config().GRPCPort, "claude-haiku-4-5-20251001", 10, 5, 0)

	if got := p.Diagnostics()["logs"].Dropped; got != 0 {
		t.Errorf("logs.Dropped = %d in metering-only mode, want 0", got)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
