package telemetry

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scionlog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestUnsupportedMetricKindsRejectAtReceiverWithoutAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		metric *metricpb.Metric
	}{
		{"summary", &metricpb.Metric{Name: "summary", Data: &metricpb.Metric_Summary{Summary: &metricpb.Summary{DataPoints: []*metricpb.SummaryDataPoint{{TimeUnixNano: 2}}}}}},
		{"exponential_histogram", &metricpb.Metric{Name: "exp", Data: &metricpb.Metric_ExponentialHistogram{ExponentialHistogram: &metricpb.ExponentialHistogram{DataPoints: []*metricpb.ExponentialHistogramDataPoint{{TimeUnixNano: 2}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
			request := &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", tc.metric)}}
			body, err := proto.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil, WithMetricHandler(p.handleMetrics))
			httpResult := httptest.NewRecorder()
			r.handleHTTPMetrics(httpResult, otlpHTTPRequest("/v1/metrics", bytes.NewReader(body)))
			if httpResult.Code != http.StatusBadRequest {
				t.Fatalf("HTTP status=%d", httpResult.Code)
			}
			if err := r.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Stop(context.Background()) }()
			conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = colmetricpb.NewMetricsServiceClient(conn).Export(ctx, request)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("gRPC result=%v", err)
			}
			if got := p.Diagnostics()["metrics"]; got.Rejected != 2 || got.Accepted != 0 || got.Queued != 0 || got.Delivered != 0 {
				t.Fatalf("diagnostics=%+v", got)
			}
			if depth := p.QueueDepth(); depth != (QueueDepth{}) {
				t.Fatalf("unsupported data retained: %+v", depth)
			}
		})
	}
}

func TestStopRetriesTransientMetricWithinCallerBudget(t *testing.T) {
	var calls atomic.Int32
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.running = true
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("adequate-budget Stop=%v", err)
	}
	if elapsed := time.Since(start); elapsed < metricFlushInterval-time.Second || calls.Load() != 2 {
		t.Fatalf("cadence retry elapsed=%v calls=%d", elapsed, calls.Load())
	}
	if got := p.Diagnostics()["metrics"]; got.Accepted != 1 || got.Delivered != 1 || got.Unconfirmed != 0 || got.Attempts != 2 || got.Failed != 1 {
		t.Fatalf("drain accounting=%+v", got)
	}
	if p.QueueDepth() != (QueueDepth{}) {
		t.Fatal("successful Stop retained queue")
	}
}

func TestStopShortBudgetReportsPendingMetricResidual(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.log")
	scionlog.SetLogPath(path)
	t.Cleanup(func() { scionlog.SetLogPath("/tmp/agent.log") })
	var calls atomic.Int32
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.running = true
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls.Add(1)
		return nil, errors.New("transient")
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := p.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "metric shutdown residual") || calls.Load() != 1 {
		t.Fatalf("short Stop=%v calls=%d", err, calls.Load())
	}
	if got := p.Diagnostics()["metrics"]; got.Accepted != 1 || got.Unconfirmed != 1 || got.Canceled != 1 || got.Delivered != 0 {
		t.Fatalf("short-budget accounting=%+v", got)
	}
	if p.QueueDepth() != (QueueDepth{}) {
		t.Fatal("terminal residual retained queue")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Telemetry delivery snapshot state=degraded") || !strings.Contains(string(content), "Unconfirmed:1") {
		t.Fatalf("incomplete shutdown lacks local residual snapshot: %s", content)
	}
}
