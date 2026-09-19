package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestTerminalCumulativeRecoveryAcrossMetricKinds(t *testing.T) {
	for _, kind := range []string{"native_sum", "histogram", "gauge"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			var values []int64
			p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
			p.metricNow = func() time.Time { return now }
			p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
				metric := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
				switch kind {
				case "histogram":
					values = append(values, int64(metric.GetHistogram().DataPoints[0].GetCount()))
				case "gauge":
					values = append(values, metric.GetGauge().DataPoints[0].GetAsInt())
				default:
					values = append(values, metric.GetSum().DataPoints[0].GetAsInt())
				}
				if len(values) == 1 {
					return nil, errors.New("ambiguous")
				}
				return &colmetricpb.ExportMetricsServiceResponse{}, nil
			}}}
			input := func(value int64, end uint64) []*metricpb.ResourceMetrics {
				var metric *metricpb.Metric
				switch kind {
				case "histogram":
					metric = testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, end, uint64(value), float64(value), []float64{2}, []uint64{uint64(value), 0})
				case "gauge":
					metric = &metricpb.Metric{Name: "temperature", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: value}}}}}}
				default:
					metric = testNumber("native.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, end, value)
				}
				return []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", metric)}
			}
			if err := p.handleMetrics(context.Background(), input(7, 2)); err != nil {
				t.Fatal(err)
			}
			p.flushMetricBuffer(context.Background(), false)
			now = now.Add(time.Minute)
			if err := p.handleMetrics(context.Background(), input(10, 3)); err != nil {
				t.Fatal(err)
			}
			now = now.Add(4 * time.Minute)
			p.flushMetricBuffer(context.Background(), false)
			if len(values) != 2 || values[0] != 7 || values[1] != 10 {
				t.Fatalf("cumulative recovery = %v", values)
			}
			if got := p.Diagnostics()["metrics"]; got.Accepted != 2 || got.Unconfirmed != 1 || got.Delivered != 1 || got.AgeLimit != 1 {
				t.Fatalf("terminal ownership = %+v", got)
			}
			p.flushMetricBuffer(context.Background(), true)
			if len(values) != 2 {
				t.Fatalf("idle baseline exported again: %v", values)
			}
		})
	}
}

func TestExpiredDirtyStreamCannotRideUnrelatedAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, oldName, newName, oldService, newService, oldScope, newScope string
	}{
		{"distinct_names", "old.calls", "new.calls", "native", "native", "scope", "scope"},
		{"same_name_distinct_identity", "same.calls", "same.calls", "old-service", "new-service", "old-scope", "new-scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
			p.metricNow = func() time.Time { return now }
			var exported []*colmetricpb.ExportMetricsServiceRequest
			p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
				exported = append(exported, proto.Clone(req).(*colmetricpb.ExportMetricsServiceRequest))
				return &colmetricpb.ExportMetricsServiceResponse{}, nil
			}}}
			admit := func(service, scope, name string, value int64, end uint64) {
				t.Helper()
				metric := testNumber(name, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, end, value)
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource(service, scope, "", "", metric)}); err != nil {
					t.Fatal(err)
				}
			}
			admit(tc.oldService, tc.oldScope, tc.oldName, 7, 2)
			now = now.Add(4 * time.Minute)
			admit(tc.newService, tc.newScope, tc.newName, 3, 3)
			now = now.Add(time.Minute)
			if !p.flushMetricBuffer(context.Background(), false) {
				t.Fatal("eligible new stream not exported")
			}
			if len(exported) != 1 || len(exported[0].ResourceMetrics) != 1 {
				t.Fatalf("snapshot includes expired stream: %+v", exported)
			}
			got := exported[0].ResourceMetrics[0]
			if got.GetResource().GetAttributes()[0].GetValue().GetStringValue() != tc.newService || got.ScopeMetrics[0].GetScope().GetName() != tc.newScope || got.ScopeMetrics[0].Metrics[0].GetName() != tc.newName {
				t.Fatalf("wrong eligible stream: %+v", got)
			}
			if d := p.Diagnostics()["metrics"]; d.Accepted != 2 || d.AgeLimit != 1 || d.Unconfirmed != 1 || d.Delivered != 1 || p.QueueDepth() != (QueueDepth{}) {
				t.Fatalf("ownership diagnostics=%+v depth=%+v", d, p.QueueDepth())
			}
		})
	}
}

func TestDuplicateMetricAdmissionDoesNotCreateUnownedQueueWork(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	input := []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}
	if err := p.handleMetrics(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := p.handleMetrics(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if d := p.QueueDepth(); d.Records != 1 || d.Entries != 1 {
		t.Fatalf("duplicate owned reservation: %+v", d)
	}
	if !p.flushMetricBuffer(context.Background(), false) {
		t.Fatal("first point not exported")
	}
	if d := p.Diagnostics()["metrics"]; d.Accepted != 1 || d.Filtered != 1 || d.Delivered != 1 || d.Unconfirmed != 0 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("duplicate conservation diagnostics=%+v depth=%+v", d, p.QueueDepth())
	}
}

func TestMetricTerminalSnapshotKeepsOnlyNewAdmissionsEligible(t *testing.T) {
	now := time.Now()
	var values []int64
	fail := true
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.metricNow = func() time.Time { return now }
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		value := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt()
		values = append(values, value)
		if fail {
			return nil, errors.New("ambiguous write")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	if p.flushMetricBuffer(context.Background(), false) {
		t.Fatal("failed write confirmed")
	}
	now = now.Add(time.Minute)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(10, 3)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	p.metricExportMu.Lock()
	p.expireMetricAdmissions(now)
	p.metricExportMu.Unlock()
	if got := p.Diagnostics()["metrics"]; got.Unconfirmed != 1 || got.AgeLimit != 1 || got.Delivered != 0 {
		t.Fatalf("terminal old admission: %+v", got)
	}
	if depth := p.QueueDepth(); depth.Records != 1 || depth.Entries != 1 {
		t.Fatalf("newer admission lost: %+v", depth)
	}
	fail = false
	if !p.flushMetricBuffer(context.Background(), false) {
		t.Fatal("newer cumulative snapshot did not succeed")
	}
	if len(values) != 2 || values[0] != 7 || values[1] != 10 {
		t.Fatalf("export values = %v", values)
	}
	if got := p.Diagnostics()["metrics"]; got.Accepted != 2 || got.Unconfirmed != 1 || got.Delivered != 1 {
		t.Fatalf("terminal accounting: %+v", got)
	}
	if p.flushMetricBuffer(context.Background(), true) || p.Diagnostics()["metrics"].Attempts != 2 {
		t.Fatal("terminal baseline created work without a new admission")
	}
}

func TestMetricAttemptLimitOneCallPerFlush(t *testing.T) {
	now := time.Now()
	calls := 0
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.metricNow = func() time.Time { return now }
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls++
		return nil, errors.New("transient")
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < metricMaxAttempts; i++ {
		p.flushMetricBuffer(context.Background(), false)
		if calls != i+1 {
			t.Fatalf("attempt %d made %d calls", i+1, calls)
		}
		if i < metricMaxAttempts-1 && p.QueueDepth().Records != 1 {
			t.Fatal("pending ownership released early")
		}
		now = now.Add(metricFlushInterval)
	}
	if got := p.Diagnostics()["metrics"]; got.Attempts != metricMaxAttempts || got.Failed != metricMaxAttempts || got.AttemptLimit != 1 || got.Unconfirmed != 1 || got.Delivered != 0 {
		t.Fatalf("attempt terminal: %+v", got)
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("terminal budget: %+v", depth)
	}
	p.flushMetricBuffer(context.Background(), false)
	if calls != metricMaxAttempts {
		t.Fatal("terminal snapshot retried")
	}
}

func TestMetricNewerAdmissionExpiresAtOriginalAge(t *testing.T) {
	now := time.Now()
	calls := 0
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.metricNow = func() time.Time { return now }
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls++
		return nil, errors.New("ambiguous")
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	p.flushMetricBuffer(context.Background(), false)
	now = now.Add(time.Minute)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(10, 3)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4*time.Minute + time.Second)
	p.flushMetricBuffer(context.Background(), false) // terminal 7, one attempt for 10
	if calls != 2 || p.QueueDepth().Records != 1 {
		t.Fatalf("newer attempt/ownership: calls=%d depth=%+v", calls, p.QueueDepth())
	}
	now = now.Add(time.Minute - time.Second)
	p.flushMetricBuffer(context.Background(), false) // 10's original five-minute age
	if calls != 2 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("aged newer retried or retained: calls=%d depth=%+v", calls, p.QueueDepth())
	}
	if got := p.Diagnostics()["metrics"]; got.Accepted != 2 || got.Unconfirmed != 2 || got.AgeLimit != 2 || got.Delivered != 0 {
		t.Fatalf("original-age accounting: %+v", got)
	}
	p.flushMetricBuffer(context.Background(), true)
	if calls != 2 {
		t.Fatal("terminal cumulative baseline generated dirty work")
	}
}

func TestMetricAgeLimitExactBoundaryWithoutExporterCall(t *testing.T) {
	now := time.Now()
	calls := 0
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.metricNow = func() time.Time { return now }
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls++
		return nil, errors.New("transient")
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	p.flushMetricBuffer(context.Background(), false)
	now = now.Add(metricMaxAge - time.Nanosecond)
	p.metricExportMu.Lock()
	p.expireMetricAdmissions(now)
	p.metricExportMu.Unlock()
	if p.QueueDepth().Records != 1 || calls != 1 {
		t.Fatal("age limit fired before boundary")
	}
	now = now.Add(time.Nanosecond)
	p.flushMetricBuffer(context.Background(), false)
	if p.QueueDepth() != (QueueDepth{}) || calls != 1 || p.Diagnostics()["metrics"].AgeLimit != 1 {
		t.Fatalf("age boundary calls=%d depth=%+v diagnostics=%+v", calls, p.QueueDepth(), p.Diagnostics()["metrics"])
	}
}

func TestMetricPartialSuccessIsTerminalUnconfirmed(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	calls := 0
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls++
		return &colmetricpb.ExportMetricsServiceResponse{PartialSuccess: &colmetricpb.ExportMetricsPartialSuccess{RejectedDataPoints: 1}}, nil
	}}}
	for _, value := range []int64{7, 10} {
		if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(value, uint64(value))}); err != nil {
			t.Fatal(err)
		}
	}
	if p.flushMetricBuffer(context.Background(), false) {
		t.Fatal("partial success reported as full delivery")
	}
	got := p.Diagnostics()["metrics"]
	if calls != 1 || got.Accepted != 2 || got.Partial != 2 || got.Unconfirmed != 2 || got.BackendRejected != 1 || got.Delivered != 0 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("partial accounting: calls=%d diagnostics=%+v depth=%+v", calls, got, p.QueueDepth())
	}
	if p.flushMetricBuffer(context.Background(), true) || calls != 1 {
		t.Fatal("partial snapshot replayed")
	}
}

func TestMetricCadenceStartsAfterExporterReturns(t *testing.T) {
	now := time.Now()
	calls := 0
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.metricNow = func() time.Time { return now }
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		calls++
		if calls == 1 {
			now = now.Add(3 * time.Second) // simulated exporter latency
			return nil, errors.New("transient")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	p.flushMetricBuffer(context.Background(), false)
	now = now.Add(metricFlushInterval - time.Nanosecond)
	if p.flushMetricBuffer(context.Background(), false) || calls != 1 {
		t.Fatalf("early retry calls=%d", calls)
	}
	now = now.Add(time.Nanosecond)
	if !p.flushMetricBuffer(context.Background(), false) || calls != 2 {
		t.Fatalf("eligible retry calls=%d", calls)
	}
}
