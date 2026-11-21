/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scionlog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMetricFlush_StandsDownAfterPermanentAuthFailureAndRecoversOnRecheck(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "standdown.log")
	scionlog.SetLogPath(logPath)
	t.Cleanup(func() { scionlog.SetLogPath("/tmp/agent.log") })

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var metricCalls atomic.Int32
	var failAuth atomic.Bool
	failAuth.Store(true)

	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.metricNow = func() time.Time { return now }
	p.authStandDown.interval = 5 * time.Minute
	p.exporter = &CloudExporter{
		metricClient: &mockMetricClient{
			exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
				metricCalls.Add(1)
				if failAuth.Load() {
					return nil, status.Error(codes.PermissionDenied, "caller does not have monitoring.timeSeries.create")
				}
				return &colmetricpb.ExportMetricsServiceResponse{}, nil
			},
		},
	}

	ctx := context.Background()
	pushMetric := func(endSec uint64, val int64) {
		t.Helper()
		rm := []*metricpb.ResourceMetrics{
			testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, endSec, val)),
		}
		if err := p.handleMetrics(ctx, rm); err != nil {
			t.Fatalf("handleMetrics(%d): %v", endSec, err)
		}
	}

	// 1. First flush hits permanent auth failure -> enters stand-down with one error log.
	pushMetric(10, 1)
	if p.flushMetricBuffer(ctx, true) {
		t.Fatal("first flush succeeded despite auth failure")
	}
	if got := metricCalls.Load(); got != 1 {
		t.Fatalf("metricCalls after first flush = %d, want 1", got)
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("queue depth after first flush = %+v, want empty", depth)
	}

	// 2. Subsequent flushes inside the stand-down window do not call the exporter or log errors.
	for i := 1; i <= 3; i++ {
		now = now.Add(30 * time.Second)
		pushMetric(uint64(10+i*30), int64(i+1))
		if p.flushMetricBuffer(ctx, true) {
			t.Fatalf("flush %d succeeded while standing down", i)
		}
	}
	if got := metricCalls.Load(); got != 1 {
		t.Fatalf("metricCalls during stand-down = %d, want 1 (no retries across flushes)", got)
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("queue depth during stand-down = %+v, want empty", depth)
	}
	if diag := p.Diagnostics()["metrics"]; diag.Dropped != 3 {
		t.Fatalf("metrics dropped during stand-down = %d, want 3", diag.Dropped)
	}

	// 3. Advance past the stand-down interval while auth is still failing -> one re-check probe,
	// still no duplicate error log, and stand-down extends for another interval.
	now = now.Add(5 * time.Minute)
	pushMetric(500, 10)
	if p.flushMetricBuffer(ctx, true) {
		t.Fatal("re-check flush succeeded while auth still failing")
	}
	if got := metricCalls.Load(); got != 2 {
		t.Fatalf("metricCalls after failed re-check = %d, want 2", got)
	}

	// Immediate next flush is suppressed again.
	now = now.Add(30 * time.Second)
	pushMetric(530, 11)
	if p.flushMetricBuffer(ctx, true) {
		t.Fatal("flush after failed re-check should be suppressed")
	}
	if got := metricCalls.Load(); got != 2 {
		t.Fatalf("metricCalls after suppressed post-recheck flush = %d, want 2", got)
	}

	// 4. Credentials are fixed; advance past stand-down interval -> re-check succeeds and resumes export.
	failAuth.Store(false)
	now = now.Add(5 * time.Minute)
	pushMetric(900, 20)
	if !p.flushMetricBuffer(ctx, true) {
		t.Fatal("re-check flush failed after auth recovered")
	}
	if got := metricCalls.Load(); got != 3 {
		t.Fatalf("metricCalls after recovery = %d, want 3", got)
	}

	// Next flush within the old interval now proceeds normally because stand-down cleared.
	now = now.Add(20 * time.Second)
	pushMetric(920, 21)
	if !p.flushMetricBuffer(ctx, true) {
		t.Fatal("normal flush after recovery failed")
	}
	if got := metricCalls.Load(); got != 4 {
		t.Fatalf("metricCalls after resumed flush = %d, want 4", got)
	}

	// Verify log file: exactly one stand-down error, zero per-batch "Failed to export" errors,
	// and one recovery info message.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	logText := string(raw)
	if got := strings.Count(logText, "standing down after permanent authentication failure"); got != 1 {
		t.Fatalf("stand-down error log count = %d, want 1:\n%s", got, logText)
	}
	if got := strings.Count(logText, "Failed to export"); got != 0 {
		t.Fatalf("unexpected per-batch export error logs = %d, want 0:\n%s", got, logText)
	}
	if !strings.Contains(logText, "recovered after authentication re-check") {
		t.Fatalf("missing recovery log line:\n%s", logText)
	}
}

func TestAuthStandDown_SuppressesSpansAndLogsUntilRecheck(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var spanCalls, logCalls atomic.Int32
	var failAuth atomic.Bool
	failAuth.Store(true)

	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.metricNow = func() time.Time { return now }
	p.authStandDown.interval = 5 * time.Minute
	p.exporter = &CloudExporter{
		grpcClient: &mockTraceClient{
			exportFunc: func(context.Context, *coltracepb.ExportTraceServiceRequest, ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
				spanCalls.Add(1)
				if failAuth.Load() {
					return nil, status.Error(codes.Unauthenticated, "invalid credentials")
				}
				return &coltracepb.ExportTraceServiceResponse{}, nil
			},
		},
		logClient: &mockLogClient{
			exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
				logCalls.Add(1)
				if failAuth.Load() {
					return nil, status.Error(codes.Unauthenticated, "invalid credentials")
				}
				return &collogspb.ExportLogsServiceResponse{}, nil
			},
		},
	}

	ctx := context.Background()
	spans := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "s1"}}}}}}
	logs := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "e1"}}}}}}

	// First span export hits Unauthenticated and triggers stand-down for spans only.
	if err := p.handleSpans(ctx, spans); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first handleSpans = %v, want Unauthenticated", err)
	}
	if spanCalls.Load() != 1 {
		t.Fatalf("spanCalls = %d, want 1", spanCalls.Load())
	}

	// Subsequent span export during stand-down does not call the backend and returns nil.
	if err := p.handleSpans(ctx, spans); err != nil {
		t.Fatalf("handleSpans during stand-down = %v, want nil", err)
	}
	if spanCalls.Load() != 1 {
		t.Fatalf("spanCalls during stand-down = %d, want 1", spanCalls.Load())
	}

	// Logs have independent stand-down state: first log call runs and enters stand-down on its own failure.
	if err := p.handleLogs(ctx, logs); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first handleLogs = %v, want Unauthenticated", err)
	}
	if err := p.handleLogs(ctx, logs); err != nil {
		t.Fatalf("handleLogs during stand-down = %v, want nil", err)
	}
	if spanCalls.Load() != 1 || logCalls.Load() != 1 {
		t.Fatalf("calls during stand-down: spans=%d logs=%d, want 1 and 1", spanCalls.Load(), logCalls.Load())
	}

	// After advancing past stand-down and fixing auth, each signal probes and recovers independently.
	failAuth.Store(false)
	now = now.Add(5 * time.Minute)
	if err := p.handleLogs(ctx, logs); err != nil {
		t.Fatalf("handleLogs re-check = %v, want nil", err)
	}
	if err := p.handleSpans(ctx, spans); err != nil {
		t.Fatalf("handleSpans re-check = %v, want nil", err)
	}
	if spanCalls.Load() != 2 || logCalls.Load() != 2 {
		t.Fatalf("calls after recovery: spans=%d logs=%d, want 2 and 2", spanCalls.Load(), logCalls.Load())
	}
}

func TestAuthStandDown_PerSignalIsolation(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var spanCalls, metricCalls, logCalls atomic.Int32

	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.metricNow = func() time.Time { return now }
	p.authStandDown.interval = 5 * time.Minute
	p.exporter = &CloudExporter{
		grpcClient: &mockTraceClient{
			exportFunc: func(context.Context, *coltracepb.ExportTraceServiceRequest, ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
				spanCalls.Add(1)
				return nil, status.Error(codes.PermissionDenied, "missing roles/cloudtrace.agent")
			},
		},
		metricClient: &mockMetricClient{
			exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
				metricCalls.Add(1)
				return &colmetricpb.ExportMetricsServiceResponse{}, nil
			},
		},
		logClient: &mockLogClient{
			exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
				logCalls.Add(1)
				return &collogspb.ExportLogsServiceResponse{}, nil
			},
		},
	}

	ctx := context.Background()
	spans := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "s1"}}}}}}
	logs := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "e1"}}}}}}
	rm := []*metricpb.ResourceMetrics{
		testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 10, 1)),
	}

	// Trace-only 403 triggers stand-down for spans.
	if err := p.handleSpans(ctx, spans); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("first handleSpans = %v, want PermissionDenied", err)
	}
	if err := p.handleSpans(ctx, spans); err != nil {
		t.Fatalf("suppressed handleSpans = %v, want nil", err)
	}
	if spanCalls.Load() != 1 {
		t.Fatalf("spanCalls = %d, want 1", spanCalls.Load())
	}

	// Metrics and logs must continue exporting without being silenced by the trace stand-down.
	if err := p.handleMetrics(ctx, rm); err != nil {
		t.Fatalf("handleMetrics: %v", err)
	}
	if !p.flushMetricBuffer(ctx, true) {
		t.Fatal("flushMetricBuffer failed; trace-only auth failure must not suppress metrics")
	}
	if metricCalls.Load() != 1 {
		t.Fatalf("metricCalls = %d, want 1", metricCalls.Load())
	}
	if diag := p.Diagnostics()["metrics"]; diag.Dropped != 0 {
		t.Fatalf("metrics dropped = %d, want 0", diag.Dropped)
	}

	if err := p.handleLogs(ctx, logs); err != nil {
		t.Fatalf("handleLogs failed: %v", err)
	}
	if logCalls.Load() != 1 {
		t.Fatalf("logCalls = %d, want 1", logCalls.Load())
	}
}

func TestMetricFlush_ExpiredProbeReleasesProbeFlag(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var metricCalls atomic.Int32
	var failAuth atomic.Bool
	failAuth.Store(true)

	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.metricNow = func() time.Time { return now }
	p.authStandDown.interval = 5 * time.Minute
	p.exporter = &CloudExporter{
		metricClient: &mockMetricClient{
			exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
				metricCalls.Add(1)
				if failAuth.Load() {
					return nil, status.Error(codes.PermissionDenied, "denied")
				}
				return &colmetricpb.ExportMetricsServiceResponse{}, nil
			},
		},
	}

	ctx := context.Background()
	pushMetric := func(endSec uint64, val int64) {
		t.Helper()
		rm := []*metricpb.ResourceMetrics{
			testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, endSec, val)),
		}
		if err := p.handleMetrics(ctx, rm); err != nil {
			t.Fatalf("handleMetrics(%d): %v", endSec, err)
		}
	}

	// Enter stand-down.
	pushMetric(10, 1)
	if p.flushMetricBuffer(ctx, true) {
		t.Fatal("first flush succeeded despite auth failure")
	}

	// Admit a metric at t=12:00, then advance past both stand-down (5m) and metricMaxAge
	// so the probe is granted by allowCloudExport("metrics") but flushMetricBuffer exits
	// early on remaining <= 0 before calling the exporter.
	pushMetric(20, 2)
	failAuth.Store(false)
	now = now.Add(metricMaxAge + 6*time.Minute)
	if p.flushMetricBuffer(ctx, true) {
		t.Fatal("expired batch should not succeed")
	}
	if metricCalls.Load() != 1 {
		t.Fatalf("metricCalls after expired probe = %d, want 1", metricCalls.Load())
	}

	// The probe flag must have been released so a fresh metric can immediately probe and recover.
	pushMetric(30, 3)
	if !p.flushMetricBuffer(ctx, true) {
		t.Fatal("subsequent flush failed; probe flag leaked on early return")
	}
	if metricCalls.Load() != 2 {
		t.Fatalf("metricCalls after recovery = %d, want 2", metricCalls.Load())
	}
}

func TestNonAuthPermanentFailureDoesNotStandDown(t *testing.T) {
	var calls atomic.Int32
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.exporter = &CloudExporter{
		logClient: &mockLogClient{
			exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
				calls.Add(1)
				return nil, status.Error(codes.InvalidArgument, "bad payload")
			},
		},
	}

	ctx := context.Background()
	logs := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "e1"}}}}}}
	_ = p.handleLogs(ctx, logs)
	_ = p.handleLogs(ctx, logs)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (non-auth errors must not trigger auth stand-down)", calls.Load())
	}
}
