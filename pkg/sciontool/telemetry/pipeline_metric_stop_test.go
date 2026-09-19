package telemetry

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
)

func stopTestMetric(value int64, end uint64) *metricpb.ResourceMetrics {
	return testMetricResource("native", "scope", "", "", testNumber("stop.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, end, value))
}

func TestPipelineStopPreservesInFlightMetricExport(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	client := &mockMetricClient{exportFunc: func(ctx context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return &colmetricpb.ExportMetricsServiceResponse{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	p := &Pipeline{running: true, exporter: &CloudExporter{metricClient: client}, metricStreams: newMetricStreams()}
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(5, 2)}); err != nil {
		t.Fatal(err)
	}
	p.metricFlushCtx, p.metricFlushCnl = context.WithCancel(context.Background())
	p.metricExportCtx, p.metricExportCnl = context.WithCancel(context.Background())
	p.metricFlushWg.Add(1)
	go func() {
		defer p.metricFlushWg.Done()
		p.flushMetricBuffer(p.metricExportCtx, true)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(stopCtx) }()
	select {
	case <-p.metricFlushCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not signal the periodic loop")
	}
	if err := p.metricExportCtx.Err(); err != nil {
		t.Fatalf("Stop canceled healthy in-flight export: %v", err)
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop after healthy export: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("SDK exports = %d, want one completed call", got)
	}
}

func TestPipelineStopReportsInFlightMetricResidualAtDeadline(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	client := &mockMetricClient{exportFunc: func(ctx context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		defer close(finished)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := &Pipeline{running: true, exporter: &CloudExporter{metricClient: client}, metricStreams: newMetricStreams()}
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(5, 2)}); err != nil {
		t.Fatal(err)
	}
	p.metricFlushCtx, p.metricFlushCnl = context.WithCancel(context.Background())
	p.metricExportCtx, p.metricExportCnl = context.WithCancel(context.Background())
	p.metricFlushWg.Add(1)
	flushDone := make(chan struct{})
	go func() {
		defer p.metricFlushWg.Done()
		defer close(flushDone)
		p.flushMetricBuffer(p.metricExportCtx, true)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stopStarted := time.Now()
	err := p.Stop(stopCtx)
	if err == nil || !strings.Contains(err.Error(), "metric shutdown residual: pending=1 dirty=0") {
		t.Fatalf("Stop at in-flight deadline = %v", err)
	}
	if elapsed := time.Since(stopStarted); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop exceeded bounded cancellation: %s", elapsed)
	}
	select {
	case <-finished:
	default:
		t.Fatal("SDK export still running after Stop returned")
	}
	select {
	case <-flushDone:
	default:
		t.Fatal("periodic export goroutine still running after Stop returned")
	}
}

func TestPipelineStopWaitsForSafeMetricCadence(t *testing.T) {
	var calls atomic.Int32
	client := &mockMetricClient{exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls.Add(1)
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}
	p := &Pipeline{running: true, exporter: &CloudExporter{metricClient: client}, metricStreams: newMetricStreams()}
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(5, 2)}); err != nil {
		t.Fatal(err)
	}
	p.flushMetricBuffer(context.Background(), true)
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(9, 3)}); err != nil {
		t.Fatal(err)
	}
	// Retain the actual successful flush, moving only its clock to keep this
	// regression fast while still proving that Stop waits for the safe slot.
	p.metricLastFlush = time.Now().Add(-metricFlushInterval + 500*time.Millisecond)
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop with sufficient cadence budget: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("SDK exports = %d, want initial and final", got)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("Stop did not wait for cadence: %s", elapsed)
	}
}

func TestPipelineStopReportsResidualWhenCadenceDeadlineIsTight(t *testing.T) {
	var calls atomic.Int32
	client := &mockMetricClient{exportFunc: func(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls.Add(1)
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}
	p := &Pipeline{running: true, exporter: &CloudExporter{metricClient: client}, metricStreams: newMetricStreams(), metricLastFlush: time.Now()}
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(5, 2)}); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := p.Stop(stopCtx)
	if err == nil || !strings.Contains(err.Error(), "metric shutdown residual: pending=0 dirty=1") {
		t.Fatalf("Stop with tight deadline = %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("SDK exports = %d before safe cadence", got)
	}
}
