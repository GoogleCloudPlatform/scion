/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// --- Mock gRPC clients for testing ---

type mockTraceClient struct {
	exportFunc func(ctx context.Context, in *coltracepb.ExportTraceServiceRequest, opts ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error)
}

func (m *mockTraceClient) Export(ctx context.Context, in *coltracepb.ExportTraceServiceRequest, opts ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
	return m.exportFunc(ctx, in, opts...)
}

type mockLogClient struct {
	exportFunc func(ctx context.Context, in *collogspb.ExportLogsServiceRequest, opts ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error)
}

func (m *mockLogClient) Export(ctx context.Context, in *collogspb.ExportLogsServiceRequest, opts ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
	return m.exportFunc(ctx, in, opts...)
}

type mockMetricClient struct {
	exportFunc func(ctx context.Context, in *colmetricpb.ExportMetricsServiceRequest, opts ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error)
}

func (m *mockMetricClient) Export(ctx context.Context, in *colmetricpb.ExportMetricsServiceRequest, opts ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
	return m.exportFunc(ctx, in, opts...)
}

// newTestPipelineWithExporter creates a Pipeline with a CloudExporter backed
// by mock gRPC clients for testing.
func newTestPipelineWithExporter(
	traceClient *mockTraceClient,
	metricClient *mockMetricClient,
	logClient *mockLogClient,
) *Pipeline {
	cfg := &Config{Enabled: true, GRPCPort: 0, HTTPPort: 0}
	p := NewWithConfig(cfg)
	p.exporter = &CloudExporter{
		grpcClient:   traceClient,
		metricClient: metricClient,
		logClient:    logClient,
	}
	return p
}

// fastRetryConfig returns a retry config with minimal backoff for testing.
func fastRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        3,
		InitialBackoff:    1 * time.Millisecond,
		MaxBackoff:        10 * time.Millisecond,
		BackoffMultiplier: 2.0,
		JitterMin:         0.0,
		JitterMax:         0.0,
	}
}

// --- isRetryable tests ---

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil error", nil, false},
		{"timeout error", context.DeadlineExceeded, true},
		{"canceled error", context.Canceled, true},
		{"auth 401 error", &googleapi.Error{Code: 401}, false},
		{"auth 403 error", &googleapi.Error{Code: 403}, false},
		{"quota 429 error", &googleapi.Error{Code: 429}, true},
		{"auth string match", errors.New("unauthenticated"), false},
		{"permission denied string", errors.New("permission denied"), false},
		{"quota string match", errors.New("resource exhausted"), true},
		{"timeout string match", errors.New("deadline exceeded"), true},
		{"generic error", errors.New("something went wrong"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryable(tt.err)
			if got != tt.retryable {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.retryable)
			}
		})
	}
}

// --- retryExport tests ---

func TestRetryExport_SuccessOnFirstCall(t *testing.T) {
	calls := 0
	err := retryExport(context.Background(), fastRetryConfig(), "test", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryExport_SuccessAfterRetries(t *testing.T) {
	calls := 0
	err := retryExport(context.Background(), fastRetryConfig(), "test", func() error {
		calls++
		if calls < 3 {
			return context.DeadlineExceeded // retryable
		}
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRetryExport_ExhaustRetries(t *testing.T) {
	calls := 0
	retryErr := errors.New("persistent failure")
	err := retryExport(context.Background(), fastRetryConfig(), "test", func() error {
		calls++
		return retryErr
	})
	if err == nil {
		t.Error("expected error after exhausting retries")
	}
	// 1 initial + 3 retries = 4 calls
	if calls != 4 {
		t.Errorf("expected 4 calls (1 initial + 3 retries), got %d", calls)
	}
}

func TestRetryExport_NonRetryableError(t *testing.T) {
	calls := 0
	authErr := &googleapi.Error{Code: 403, Message: "forbidden"}
	err := retryExport(context.Background(), fastRetryConfig(), "test", func() error {
		calls++
		return authErr
	})
	if err == nil {
		t.Error("expected error for non-retryable failure")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry for auth error), got %d", calls)
	}
}

func TestRetryExport_NonRetryableAfterRetries(t *testing.T) {
	calls := 0
	authErr := &googleapi.Error{Code: 401, Message: "unauthorized"}
	err := retryExport(context.Background(), fastRetryConfig(), "test", func() error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded // first call: retryable
		}
		return authErr // second call: non-retryable
	})
	if err == nil {
		t.Error("expected error")
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestRetryExport_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryExport(ctx, fastRetryConfig(), "test", func() error {
		calls++
		cancel() // cancel after first attempt
		return errors.New("transient")
	})
	if err == nil {
		t.Error("expected error when context cancelled")
	}
	if calls != 1 {
		t.Errorf("expected 1 call before context cancellation, got %d", calls)
	}
}

func TestRetryExport_BackoffTiming(t *testing.T) {
	cfg := RetryConfig{
		MaxRetries:        2,
		InitialBackoff:    20 * time.Millisecond,
		MaxBackoff:        1 * time.Second,
		BackoffMultiplier: 2.0,
		JitterMin:         0.0,
		JitterMax:         0.0,
	}
	start := time.Now()
	calls := 0
	_ = retryExport(context.Background(), cfg, "test", func() error {
		calls++
		return errors.New("fail")
	})
	elapsed := time.Since(start)

	// With 0 jitter: 20ms + 40ms = 60ms minimum
	if elapsed < 50*time.Millisecond {
		t.Errorf("backoff too fast: %v (expected >= 60ms)", elapsed)
	}
	// Sanity upper bound: shouldn't take more than 500ms
	if elapsed > 500*time.Millisecond {
		t.Errorf("backoff too slow: %v", elapsed)
	}
}

func TestRetryExport_MaxBackoffCap(t *testing.T) {
	cfg := RetryConfig{
		MaxRetries:        3,
		InitialBackoff:    50 * time.Millisecond,
		MaxBackoff:        60 * time.Millisecond,
		BackoffMultiplier: 10.0, // aggressive multiplier
		JitterMin:         0.0,
		JitterMax:         0.0,
	}
	start := time.Now()
	_ = retryExport(context.Background(), cfg, "test", func() error {
		return errors.New("fail")
	})
	elapsed := time.Since(start)

	// Without cap: 50 + 500 + 5000 = 5550ms
	// With cap at 60ms: 50 + 60 + 60 = 170ms
	if elapsed > 500*time.Millisecond {
		t.Errorf("max backoff cap not applied: %v", elapsed)
	}
}

func TestRetryExport_NoOverheadOnSuccess(t *testing.T) {
	start := time.Now()
	err := retryExport(context.Background(), DefaultRetryConfig(), "test", func() error {
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Should complete nearly instantly (well under 10ms) on success
	if elapsed > 10*time.Millisecond {
		t.Errorf("unexpected overhead on success: %v", elapsed)
	}
}

// --- Handler-level retry tests ---

func TestHandleSpans_RetriesOnTransientError(t *testing.T) {
	calls := 0
	traceClient := &mockTraceClient{
		exportFunc: func(_ context.Context, _ *coltracepb.ExportTraceServiceRequest, _ ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
			calls++
			if calls < 3 {
				return nil, context.DeadlineExceeded
			}
			return &coltracepb.ExportTraceServiceResponse{}, nil
		},
	}

	p := newTestPipelineWithExporter(traceClient, nil, nil)
	p.retryConfig = fastRetryConfig()

	err := p.handleSpans(context.Background(), []*tracepb.ResourceSpans{
		{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "test"}}}}},
	})
	if err != nil {
		t.Errorf("expected success after retries, got: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 export calls, got %d", calls)
	}
}

func TestHandleSpans_NoRetryOnAuthError(t *testing.T) {
	calls := 0
	traceClient := &mockTraceClient{
		exportFunc: func(_ context.Context, _ *coltracepb.ExportTraceServiceRequest, _ ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
			calls++
			return nil, &googleapi.Error{Code: 403, Message: "forbidden"}
		},
	}

	p := newTestPipelineWithExporter(traceClient, nil, nil)
	p.retryConfig = fastRetryConfig()

	err := p.handleSpans(context.Background(), []*tracepb.ResourceSpans{
		{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "test"}}}}},
	})
	if err == nil {
		t.Error("expected auth error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry for auth), got %d", calls)
	}
}

func TestHandleLogs_RetriesOnTransientError(t *testing.T) {
	calls := 0
	logClient := &mockLogClient{
		exportFunc: func(_ context.Context, _ *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
			calls++
			if calls < 2 {
				return nil, errors.New("resource exhausted")
			}
			return &collogspb.ExportLogsServiceResponse{}, nil
		},
	}

	p := newTestPipelineWithExporter(nil, nil, logClient)
	p.retryConfig = fastRetryConfig()

	err := p.handleLogs(context.Background(), []*logspb.ResourceLogs{
		{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}},
	})
	if err != nil {
		t.Errorf("expected success after retries, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 export calls, got %d", calls)
	}
}

func TestHandleLogs_NoRetryOnAuthError(t *testing.T) {
	calls := 0
	logClient := &mockLogClient{
		exportFunc: func(_ context.Context, _ *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
			calls++
			return nil, errors.New("permission denied")
		},
	}

	p := newTestPipelineWithExporter(nil, nil, logClient)
	p.retryConfig = fastRetryConfig()

	err := p.handleLogs(context.Background(), []*logspb.ResourceLogs{
		{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}},
	})
	if err == nil {
		t.Error("expected auth error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry for auth), got %d", calls)
	}
}

// --- Metric re-buffer tests ---

func TestFlushMetricBuffer_RebufferOnExhaustion(t *testing.T) {
	metricClient := &mockMetricClient{
		exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
			return nil, errors.New("temporary failure")
		},
	}

	p := newTestPipelineWithExporter(nil, metricClient, nil)
	p.retryConfig = fastRetryConfig()

	// Add metrics to buffer
	testMetrics := []*metricpb.ResourceMetrics{
		{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "test.metric"}}}}},
	}
	p.metricBuf = testMetrics

	// Flush (should fail and re-buffer)
	p.flushMetricBuffer(context.Background(), true)

	p.metricBufMu.Lock()
	bufLen := len(p.metricBuf)
	p.metricBufMu.Unlock()

	if bufLen == 0 {
		t.Error("expected metrics to be re-buffered after export failure")
	}
}

func TestFlushMetricBuffer_RebufferCapped(t *testing.T) {
	// makeDistinct creates n ResourceMetrics with unique scope+metric names
	// so deduplicateMetrics does not collapse them.
	makeDistinct := func(n int, prefix string) []*metricpb.ResourceMetrics {
		out := make([]*metricpb.ResourceMetrics, n)
		for i := range out {
			out[i] = &metricpb.ResourceMetrics{
				ScopeMetrics: []*metricpb.ScopeMetrics{{
					Scope:   &commonpb.InstrumentationScope{Name: fmt.Sprintf("%s.scope.%d", prefix, i)},
					Metrics: []*metricpb.Metric{{Name: fmt.Sprintf("%s.metric.%d", prefix, i)}},
				}},
			}
		}
		return out
	}

	// The mock export function simulates concurrent handleMetrics calls
	// flooding the buffer during the retry window. flushMetricBuffer sets
	// metricBuf = nil before calling the exporter, so entries added here
	// accumulate in the buffer while the export retries. After retries
	// exhaust, the re-buffer appends deduped data on top of these entries.
	var p *Pipeline
	metricClient := &mockMetricClient{
		exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
			// Simulate concurrent handleMetrics adding entries during retry.
			p.metricBufMu.Lock()
			p.metricBuf = append(p.metricBuf, makeDistinct(maxMetricBufCap/2, "concurrent")...)
			p.metricBufMu.Unlock()
			return nil, errors.New("temporary failure")
		},
	}

	p = newTestPipelineWithExporter(nil, metricClient, nil)
	p.retryConfig = fastRetryConfig()

	// Seed the buffer with one batch.
	p.metricBuf = makeDistinct(10, "initial")

	// Flush — export fails on every attempt. Each attempt adds
	// maxMetricBufCap/2 entries to the buffer (simulating concurrent
	// handleMetrics calls). After 4 attempts (1 + 3 retries) the buffer
	// holds 4*(maxMetricBufCap/2) = 2*maxMetricBufCap entries, plus the
	// re-buffered deduped batch. The cap should trim it.
	p.flushMetricBuffer(context.Background(), true)

	p.metricBufMu.Lock()
	bufLen := len(p.metricBuf)
	p.metricBufMu.Unlock()

	if bufLen > maxMetricBufCap {
		t.Errorf("re-buffer exceeded cap: got %d, max %d", bufLen, maxMetricBufCap)
	}
	if bufLen == 0 {
		t.Error("expected re-buffered metrics, got empty buffer")
	}
}

func TestFlushMetricBuffer_SuccessfulExport(t *testing.T) {
	exported := false
	metricClient := &mockMetricClient{
		exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
			exported = true
			return &colmetricpb.ExportMetricsServiceResponse{}, nil
		},
	}

	p := newTestPipelineWithExporter(nil, metricClient, nil)
	p.retryConfig = fastRetryConfig()

	p.metricBuf = []*metricpb.ResourceMetrics{
		{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "test"}}}}},
	}

	p.flushMetricBuffer(context.Background(), true)

	if !exported {
		t.Error("expected metrics to be exported")
	}

	p.metricBufMu.Lock()
	bufLen := len(p.metricBuf)
	p.metricBufMu.Unlock()

	if bufLen != 0 {
		t.Errorf("expected empty buffer after successful export, got %d", bufLen)
	}
}

func TestFlushMetricBuffer_SuccessAfterRetry(t *testing.T) {
	calls := 0
	metricClient := &mockMetricClient{
		exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
			calls++
			if calls < 2 {
				return nil, context.DeadlineExceeded
			}
			return &colmetricpb.ExportMetricsServiceResponse{}, nil
		},
	}

	p := newTestPipelineWithExporter(nil, metricClient, nil)
	p.retryConfig = fastRetryConfig()

	p.metricBuf = []*metricpb.ResourceMetrics{
		{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "test"}}}}},
	}

	p.flushMetricBuffer(context.Background(), true)

	p.metricBufMu.Lock()
	bufLen := len(p.metricBuf)
	p.metricBufMu.Unlock()

	if bufLen != 0 {
		t.Errorf("expected empty buffer after successful retry, got %d", bufLen)
	}
	if calls != 2 {
		t.Errorf("expected 2 export calls, got %d", calls)
	}
}
