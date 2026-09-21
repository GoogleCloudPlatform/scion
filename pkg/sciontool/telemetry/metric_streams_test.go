package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func testMetricResource(service, scope, version, schema string, metric *metricpb.Metric) *metricpb.ResourceMetrics {
	return &metricpb.ResourceMetrics{
		Resource:     &resourcepb.Resource{Attributes: []*commonpb.KeyValue{metricStringLabel("service.name", service), metricStringLabel("service.instance.id", "agent-native"), metricStringLabel("scion.agent.id", "agent"), metricStringLabel("scion.project.id", "project")}},
		SchemaUrl:    "resource:" + schema,
		ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: scope, Version: version}, SchemaUrl: "scope:" + schema, Metrics: []*metricpb.Metric{metric}}},
	}
}

func testNumber(name string, temporal metricpb.AggregationTemporality, start, end uint64, value int64, attrs ...*commonpb.KeyValue) *metricpb.Metric {
	return &metricpb.Metric{Name: name, Unit: "{call}", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{AggregationTemporality: temporal, IsMonotonic: true, DataPoints: []*metricpb.NumberDataPoint{{StartTimeUnixNano: start, TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: value}, Attributes: attrs}}}}}
}

func testHist(temporal metricpb.AggregationTemporality, start, end uint64, count uint64, sum float64, bounds []float64, buckets []uint64, attrs ...*commonpb.KeyValue) *metricpb.Metric {
	return &metricpb.Metric{Name: "duration", Unit: "ms", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{AggregationTemporality: temporal, DataPoints: []*metricpb.HistogramDataPoint{{StartTimeUnixNano: start, TimeUnixNano: end, Count: count, Sum: &sum, ExplicitBounds: bounds, BucketCounts: buckets, Attributes: attrs}}}}}
}

func TestMetricStreamsTenHooksAcrossWindows(t *testing.T) {
	s := newMetricStreams()
	for i := range 10 {
		input := testMetricResource("sciontool", hookMetricScope, "", "hook", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, uint64(i+1), uint64(i+2), 1, metricStringLabel("tool_name", "Bash")))
		if err := s.add([]*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatalf("hook %d: %v", i, err)
		}
		if i == 4 {
			first := s.snapshot()
			if got := first[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt(); got != 5 {
				t.Fatalf("first window = %d", got)
			}
			s.clearPendingMarker()
		}
	}
	second := s.snapshot()
	if len(second) != 1 {
		t.Fatalf("streams = %d", len(second))
	}
	point := second[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	if point.GetAsInt() != 10 || point.StartTimeUnixNano != 1 || point.TimeUnixNano != 11 {
		t.Fatalf("hook output = %v", point)
	}
	if second[0].ScopeMetrics[0].Metrics[0].GetSum().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatal("hook output is not cumulative")
	}
	if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("native", "native", "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 12, 7))}); err != nil {
		t.Fatal(err)
	}
	if len(s.streams) != 2 {
		t.Fatalf("hook/native stream count = %d", len(s.streams))
	}
}

func TestHookTokenNamespaceIsSeparateFromNativeUsage(t *testing.T) {
	s := newMetricStreams()
	hook := testMetricResource("sciontool", hookMetricScope, "", "", testNumber("scion.hook.tokens.input", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, 5))
	native := testMetricResource("native", "native.scope", "", "", testNumber("gen_ai.tokens.input", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 7))
	for _, input := range []*metricpb.ResourceMetrics{hook, native} {
		if err := s.add([]*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.streams) != 2 {
		t.Fatalf("hook and native usage collapsed: %d", len(s.streams))
	}
	oldHook := testMetricResource("sciontool", hookMetricScope, "", "", testNumber("gen_ai.tokens.input", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 3, 4, 1))
	if err := s.add([]*metricpb.ResourceMetrics{oldHook}); err == nil || err.Error() != "unsupported normalized hook token name" {
		t.Fatalf("old hook token name admission = %v", err)
	}
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{oldHook}); err == nil || err.Error() != "unsupported normalized hook token name" {
		t.Fatalf("old hook token name Cloud adapter = %v", err)
	}
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{native}); err != nil {
		t.Fatalf("genuine native token Cloud adapter = %v", err)
	}
}

func TestCloudRejectsDifferentTemporalityWritersBeforeStateCommit(t *testing.T) {
	for _, kind := range []string{"sum", "histogram"} {
		for _, first := range []metricpb.AggregationTemporality{
			metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		} {
			t.Run(fmt.Sprintf("%s/%s", kind, first), func(t *testing.T) {
				other := metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
				if first == other {
					other = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
				}
				input := func(temporal metricpb.AggregationTemporality, start, end uint64) *metricpb.ResourceMetrics {
					var m *metricpb.Metric
					if kind == "sum" {
						m = testNumber("same.name", temporal, start, end, 1)
					} else {
						m = testHist(temporal, start, end, 1, 1, []float64{2}, []uint64{1, 0})
					}
					return testMetricResource("native", "same.scope", "", "", m)
				}
				p := newTestPipelineWithExporter(nil, &mockMetricClient{}, nil)
				p.config.CloudProvider = "gcp"
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first, 1, 2)}); err != nil {
					t.Fatal(err)
				}
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(other, 3, 4)}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "incompatible Cloud Monitoring series writers") {
					t.Fatalf("second writer admission = %v", err)
				}
				if len(p.metricStreams.streams) != 1 || len(p.metricStreams.cloudIdentities) != 1 || p.metricRejectedPoints.Load() != 1 {
					t.Fatalf("rejection mutated admitted state: streams=%d identities=%d rejected=%d", len(p.metricStreams.streams), len(p.metricStreams.cloudIdentities), p.metricRejectedPoints.Load())
				}
				healthyStart := uint64(2)
				if first == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
					healthyStart = 1
				}
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first, healthyStart, 3)}); err != nil {
					t.Fatalf("healthy writer poisoned: %v", err)
				}
				if len(p.metricStreams.streams) != 1 {
					t.Fatal("failed writer committed a second stream")
				}
				fresh := newTestPipelineWithExporter(nil, &mockMetricClient{}, nil)
				fresh.config.CloudProvider = "gcp"
				if err := fresh.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first, 1, 2), input(other, 3, 4)}); status.Code(err) != codes.InvalidArgument || len(fresh.metricStreams.streams) != 0 || len(fresh.metricStreams.cloudIdentities) != 0 {
					t.Fatalf("whole conflicting request was committed: streams=%d identities=%d err=%v", len(fresh.metricStreams.streams), len(fresh.metricStreams.cloudIdentities), err)
				}
				// Generic OTLP retains the distinct processed temporalities.
				generic := newMetricStreams()
				if err := generic.add([]*metricpb.ResourceMetrics{input(first, 1, 2), input(other, 3, 4)}); err != nil || len(generic.streams) != 2 {
					t.Fatalf("generic writer admission: streams=%d err=%v", len(generic.streams), err)
				}
			})
		}
	}
}

func TestMetricStreamIdentityAndCanonicalAttributes(t *testing.T) {
	attrs := [][]*commonpb.KeyValue{
		{metricStringLabel("a;b", "c=d")}, {metricStringLabel("a", "b;c=d")},
		{{Key: "typed", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}},
		{metricStringLabel("typed", "1")},
		{{Key: "nested", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("x", "1"), metricStringLabel("y", "2")}}}}}},
		{{Key: "nested", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("y", "2"), metricStringLabel("x", "1")}}}}}},
	}
	keys := make([]string, len(attrs))
	for i, a := range attrs {
		var err error
		keys[i], err = canonicalAttrs(a)
		if err != nil {
			t.Fatal(err)
		}
	}
	if keys[0] == keys[1] || keys[2] == keys[3] || keys[4] != keys[5] {
		t.Fatal("typed/delimiter/nested identity collision or order dependence")
	}
	s := newMetricStreams()
	for _, tc := range []struct {
		service, scope, version, schema string
		attr                            *commonpb.KeyValue
	}{
		{"a", "one", "1", "a", metricStringLabel("p", "x")},
		{"b", "one", "1", "a", metricStringLabel("p", "x")},
		{"a", "two", "1", "a", metricStringLabel("p", "x")},
		{"a", "one", "2", "a", metricStringLabel("p", "x")},
		{"a", "one", "1", "b", metricStringLabel("p", "x")},
		{"a", "one", "1", "a", metricStringLabel("p", "y")},
	} {
		if err := s.add([]*metricpb.ResourceMetrics{testMetricResource(tc.service, tc.scope, tc.version, tc.schema, testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, tc.attr))}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.streams) != 6 || len(s.snapshot()) != 6 {
		t.Fatalf("identity collapsed: %d", len(s.streams))
	}
}

func TestMetricSumIntervalsAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		temporal metricpb.AggregationTemporality
		samples  [][3]uint64
		want     int64
		rejects  int
	}{
		{"delta ordered gap replay overlap", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, [][3]uint64{{1, 2, 2}, {3, 4, 3}, {3, 4, 3}, {3, 5, 9}}, 5, 1},
		{"cumulative newer and older", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, [][3]uint64{{1, 4, 7}, {1, 3, 5}, {1, 5, 8}, {1, 5, 9}}, 8, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetricStreams()
			var rejected int
			for _, sample := range tc.samples {
				err := s.add([]*metricpb.ResourceMetrics{testMetricResource("native", "native", "1", "a", testNumber("calls", tc.temporal, sample[0], sample[1], int64(sample[2])))})
				if err != nil {
					rejected++
				}
			}
			if rejected != tc.rejects {
				t.Fatalf("rejections = %d, want %d", rejected, tc.rejects)
			}
			output := s.snapshot()
			if got := output[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt(); got != tc.want {
				t.Fatalf("value = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMonotonicCumulativeRejectsSameEpochDecrease(t *testing.T) {
	for _, valueType := range []string{"int64", "double"} {
		for _, delivered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delivered=%t", valueType, delivered), func(t *testing.T) {
				s := newMetricStreams()
				input := func(end uint64, value int64) *metricpb.ResourceMetrics {
					m := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, end, value)
					if valueType == "double" {
						m.GetSum().DataPoints[0].Value = &metricpb.NumberDataPoint_AsDouble{AsDouble: float64(value)}
					}
					return testMetricResource("native", "scope", "", "", m)
				}
				if err := s.add([]*metricpb.ResourceMetrics{input(2, 7)}); err != nil {
					t.Fatal(err)
				}
				if delivered {
					_ = s.snapshot()
					s.clearPendingMarker()
				}
				if err := s.add([]*metricpb.ResourceMetrics{input(3, 3)}); err == nil || err.Error() != "decreasing cumulative sum" {
					t.Fatalf("same-epoch decrease = %v", err)
				}
				if err := s.add([]*metricpb.ResourceMetrics{input(3, 8)}); err != nil {
					t.Fatalf("healthy increment after rejection: %v", err)
				}
				point := s.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
				if valueType == "double" && point.GetAsDouble() != 8 || valueType == "int64" && point.GetAsInt() != 8 {
					t.Fatalf("cumulative value after rejection = %v", point)
				}
			})
		}
	}
}

func TestMetricCumulativeResetAndBounds(t *testing.T) {
	s := newMetricStreams()
	input := func(start, end uint64, value int64) *metricpb.ResourceMetrics {
		return testMetricResource("native", "scope", "", "", testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, value))
	}
	if err := s.add([]*metricpb.ResourceMetrics{input(1, 2, 3)}); err != nil {
		t.Fatal(err)
	}
	if err := s.add([]*metricpb.ResourceMetrics{input(3, 4, 1)}); err == nil {
		t.Fatal("unexported reset was accepted")
	}
	_ = s.snapshot()
	if err := s.add([]*metricpb.ResourceMetrics{input(3, 4, 1)}); err == nil {
		t.Fatal("pending reset was accepted")
	}
	s.clearPendingMarker()
	if err := s.add([]*metricpb.ResourceMetrics{input(3, 4, 1)}); err != nil {
		t.Fatal(err)
	}
	if got := s.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].StartTimeUnixNano; got != 3 {
		t.Fatalf("reset start = %d", got)
	}

	s = newMetricStreams()
	clock := time.Unix(0, 0)
	s.now = func() time.Time { return clock }
	for i := range maxActiveMetricStreams {
		input := testMetricResource(fmt.Sprint(i), "scope", "", "", testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))
		if err := s.add([]*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.add([]*metricpb.ResourceMetrics{input(1, 2, 1)}); err == nil {
		t.Fatal("active stream limit not enforced")
	}
	_ = s.snapshot()
	s.clearPendingMarker()
	clock = clock.Add(metricStreamIdleTTL)
	if err := s.add([]*metricpb.ResourceMetrics{input(1, 2, 1)}); err != nil {
		t.Fatalf("idle expiry: %v", err)
	}
	if len(s.streams) != 1 {
		t.Fatalf("expired stream count = %d", len(s.streams))
	}
}

func TestMetricIdleExpiryStartsNewHookEpochAndDuplicateWindowIsBounded(t *testing.T) {
	s := newMetricStreams()
	clock := time.Unix(100, 0)
	s.now = func() time.Time { return clock }
	input := func(start uint64) *metricpb.ResourceMetrics {
		return testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, start, start+1, 1))
	}
	for i := range maxDuplicateIntervals + 1 {
		if err := s.add([]*metricpb.ResourceMetrics{input(uint64(i + 1))}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range s.streams {
		if len(entry.seen) != maxDuplicateIntervals || len(entry.order) != maxDuplicateIntervals {
			t.Fatalf("duplicate state = %d/%d", len(entry.seen), len(entry.order))
		}
	}
	if err := s.add([]*metricpb.ResourceMetrics{input(1)}); err == nil {
		t.Fatal("replay outside duplicate window was re-added")
	}
	first := s.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	if first.GetAsInt() != maxDuplicateIntervals+1 {
		t.Fatalf("first epoch = %d", first.GetAsInt())
	}
	s.clearPendingMarker()
	clock = clock.Add(metricStreamIdleTTL)
	if err := s.add([]*metricpb.ResourceMetrics{input(1000)}); err != nil {
		t.Fatal(err)
	}
	second := s.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	if second.GetAsInt() != 1 || second.StartTimeUnixNano != 1000 {
		t.Fatalf("new epoch = %v", second)
	}
}

func TestMetricGaugesHistogramsAndUnsupported(t *testing.T) {
	s := newMetricStreams()
	gauge := func(value int64, end uint64, attr string) *metricpb.ResourceMetrics {
		return testMetricResource("native", "scope", "", "", &metricpb.Metric{Name: "temperature", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: value}, Attributes: []*commonpb.KeyValue{metricStringLabel("sensor", attr)}}}}}})
	}
	for _, item := range []*metricpb.ResourceMetrics{gauge(1, 2, "a"), gauge(2, 3, "b"), gauge(9, 1, "a")} {
		if err := s.add([]*metricpb.ResourceMetrics{item}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.snapshot()) != 2 {
		t.Fatal("gauge attribute streams collapsed")
	}
	h := newMetricStreams()
	for _, item := range []*metricpb.Metric{testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, 2, 3, []float64{1}, []uint64{1, 1}, metricStringLabel("sensor", "a")), testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 2, 3, 3, 5, []float64{1}, []uint64{2, 1}, metricStringLabel("sensor", "a")), testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, 1, 1, []float64{1}, []uint64{1, 0}, metricStringLabel("sensor", "b"))} {
		if err := h.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", item)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 3, 4, 1, 1, []float64{2}, []uint64{1, 0}, metricStringLabel("sensor", "a")))}); err == nil {
		t.Fatal("incompatible histogram accepted")
	}
	for _, rm := range h.snapshot() {
		point := rm.ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[0]
		if attrValue(point.Attributes, "sensor") == "a" && (point.Count != 5 || point.GetSum() != 8 || point.BucketCounts[0] != 3) {
			t.Fatalf("histogram = %v", point)
		}
	}
	if err := h.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", &metricpb.Metric{Name: "summary", Data: &metricpb.Metric_Summary{Summary: &metricpb.Summary{}}})}); err == nil {
		t.Fatal("unsupported summary accepted")
	}
}

func TestMetricRetrySnapshotImmutable(t *testing.T) {
	s := newMetricStreams()
	for _, n := range []int64{1, 2} {
		if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, uint64(n), uint64(n+1), 1))}); err != nil {
			t.Fatal(err)
		}
	}
	first := s.snapshot()
	if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 3, 4, 1))}); err != nil {
		t.Fatal(err)
	}
	if first[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt() != 2 {
		t.Fatal("retry snapshot mutated")
	}
	s.clearPendingMarker()
	if got := s.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt(); got != 3 {
		t.Fatalf("next window = %d", got)
	}
	if _, ok := s.rejected["overlapping delta intervals"]; ok {
		t.Fatal("hook intervals were treated as native")
	}
}

func TestPipelineMetricFailedSnapshotDoesNotReaddHooks(t *testing.T) {
	var values []int64
	failed := true
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		value := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt()
		values = append(values, value)
		if failed {
			failed = false
			return nil, status.Error(codes.Unavailable, "test transient")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}, nil)
	add := func(start uint64) {
		t.Helper()
		input := testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, start, start+1, 1))
		if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(1); i <= 7; i++ {
		add(i)
	}
	p.flushMetricBuffer(context.Background(), true)
	for i := uint64(8); i <= 10; i++ {
		add(i)
	}
	p.flushMetricBuffer(context.Background(), true)
	p.flushMetricBuffer(context.Background(), true)
	if fmt.Sprint(values) != "[7 7 10]" {
		t.Fatalf("retry/window output = %v", values)
	}
}

func TestPipelineStopReportsNewerMetricResidualAfterPendingRetryWithTightDeadline(t *testing.T) {
	failures := 4
	var totals []int64
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		totals = append(totals, req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt())
		if failures > 0 {
			failures--
			return nil, status.Error(codes.Unavailable, "test transient")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}, nil)
	for i := uint64(1); i <= 7; i++ {
		if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, i, i+1, 1))}); err != nil {
			t.Fatal(err)
		}
	}
	p.flushMetricBuffer(context.Background(), true)
	for i := uint64(8); i <= 10; i++ {
		if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, i, i+1, 1))}); err != nil {
			t.Fatal(err)
		}
	}
	p.flushMetricBuffer(context.Background(), true)
	p.running = true
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Stop(stopCtx); err == nil || !strings.Contains(err.Error(), "metric shutdown residual") {
		t.Fatalf("Stop residual = %v", err)
	}
	if fmt.Sprint(totals) != "[7 7]" {
		t.Fatalf("shutdown wrote early same series: %v", totals)
	}
}

func TestPipelineStopFlushesAcceptedMetricsWithoutPriorFailure(t *testing.T) {
	var totals []int64
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		totals = append(totals, req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsInt())
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}, nil)
	for i := uint64(1); i <= 3; i++ {
		input := testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, i, i+1, 1))
		if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatal(err)
		}
	}
	p.running = true
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(totals) != "[3]" {
		t.Fatalf("shutdown totals = %v", totals)
	}
}

func TestMetricDiagnosticExportFailureDoesNotRecordAnotherDiagnostic(t *testing.T) {
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "diagnostic export failed")
	}}, nil)
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = meterProvider.Shutdown(context.Background()) }()
	p.exportErrors, _ = meterProvider.Meter("test").Int64Counter("test.export.errors")
	input := testMetricResource("sciontool", pipelineMetricScope, "", "", testNumber("scion.telemetry.export.errors", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("signal", "metrics"), metricStringLabel("error_type", "auth")))
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input}); err != nil {
		t.Fatal(err)
	}
	p.flushMetricBuffer(context.Background(), true)
	var captured metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &captured); err != nil {
		t.Fatal(err)
	}
	if len(captured.ScopeMetrics) != 0 {
		t.Fatalf("diagnostic failure recursively recorded %d metric scopes", len(captured.ScopeMetrics))
	}
}

func TestCloudMetricPolicyRejectsForbiddenDimensionBeforeCommit(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-authority")
	t.Setenv("SCION_PROJECT_ID", "project-authority")
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		t.Fatal("rejected metric reached exporter")
		return nil, nil
	}}, nil)
	p.config.CloudProvider = "gcp"
	p.config.Redaction.Hash = []string{"session_id"}
	p.policy = newReceiverPolicy(p.config)
	input := testMetricResource("native", "scope", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("conversation.id", "PRIVATE-MARKER")))
	err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input})
	if status.Code(err) != codes.InvalidArgument || strings.Contains(err.Error(), "PRIVATE-MARKER") {
		t.Fatalf("producer-visible rejection = %v", err)
	}
	if p.metricRejectedPoints.Load() != 1 || len(p.metricStreams.streams) != 0 || len(p.metricStreams.descriptors) != 0 {
		t.Fatal("rejected metric was counted incorrectly or committed state")
	}
	nested := testMetricResource("native", "scope", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, &commonpb.KeyValue{Key: "operation", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("conversation.id", "PRIVATE-MARKER")}}}}}))
	decision := p.policy.processMetrics([]*metricpb.ResourceMetrics{nested})
	if decision.Reason != "" {
		t.Fatalf("Phase 1 policy rejected supported generic structure: %s", decision.Reason)
	}
	processedNested := decision.Data[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes[0].GetValue().GetKvlistValue().Values[0].GetValue().GetStringValue()
	if processedNested != HashValue("PRIVATE-MARKER") {
		t.Fatalf("generic nested value was not policy processed: %q", processedNested)
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nested}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nested forbidden producer-visible rejection = %v", err)
	}
	if p.metricRejectedPoints.Load() != 2 || len(p.metricStreams.streams) != 0 {
		t.Fatal("nested rejected metric changed Cloud state")
	}
}

func TestCloudMetricAdmissionRejectsForbiddenAndCollidingDimensions(t *testing.T) {
	s := newMetricStreams()
	s.gcp = true
	base := testMetricResource("native-a", "scope", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("operation", "read")))
	if err := s.add([]*metricpb.ResourceMetrics{base}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"prompt", "conversation.id", "session_id", "tool_output", "payload"} {
		input := proto.Clone(base).(*metricpb.ResourceMetrics)
		input.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = append(input.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, metricStringLabel(key, "SECRET"))
		if err := s.add([]*metricpb.ResourceMetrics{input}); err == nil {
			t.Fatalf("forbidden %s admitted", key)
		}
	}
	unsupported := proto.Clone(base).(*metricpb.ResourceMetrics)
	unsupported.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = append(unsupported.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, metricStringLabel("unmapped", "one"))
	if err := s.add([]*metricpb.ResourceMetrics{unsupported}); err == nil {
		t.Fatal("collapsed unsupported point dimension admitted")
	}
	if len(s.streams) != 1 {
		t.Fatalf("rejected admission poisoned streams: %d", len(s.streams))
	}
	second := testMetricResource("native-b", "scope", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 2, metricStringLabel("operation", "read")))
	if err := s.add([]*metricpb.ResourceMetrics{second}); err != nil {
		t.Fatalf("distinct allowed resource: %v", err)
	}
	third := testMetricResource("native-a", "scope", "2", "b", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 3, metricStringLabel("operation", "read")))
	if err := s.add([]*metricpb.ResourceMetrics{third}); err != nil {
		t.Fatalf("distinct allowed scope: %v", err)
	}
	if len(s.streams) != 3 {
		t.Fatalf("accepted streams = %d", len(s.streams))
	}
}

func TestCloudMetricDescriptorShapeRejectsIncompatibleInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*metricpb.ResourceMetrics)
	}{
		{"point label shape", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = []*commonpb.KeyValue{metricStringLabel("sensor", "x")}
		}},
		{"unit", func(rm *metricpb.ResourceMetrics) { rm.ScopeMetrics[0].Metrics[0].Unit = "ms" }},
		{"kind", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Metrics[0].Data = &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: 2, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}, Attributes: []*commonpb.KeyValue{metricStringLabel("operation", "read")}}}}}
		}},
		{"normalized label collision", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = []*commonpb.KeyValue{metricStringLabel("operation", "read"), metricStringLabel("scion.metric.point.id", "x")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetricStreams()
			s.gcp = true
			base := testMetricResource("native", "scope", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("operation", "read")))
			if err := s.add([]*metricpb.ResourceMetrics{base}); err != nil {
				t.Fatal(err)
			}
			changed := proto.Clone(base).(*metricpb.ResourceMetrics)
			tc.change(changed)
			if err := s.add([]*metricpb.ResourceMetrics{changed}); err == nil {
				t.Fatal("incompatible descriptor admitted")
			}
			if len(s.streams) != 1 {
				t.Fatalf("existing stream poisoned: %d", len(s.streams))
			}
		})
	}
}

func TestCloudMetricDescriptorRegistryLimit(t *testing.T) {
	s := newMetricStreams()
	s.gcp = true
	for i := range maxActiveMetricStreams {
		s.descriptors[fmt.Sprintf("existing.%d", i)] = metricDescriptorShape{kind: "sum"}
	}
	input := testMetricResource("native", "scope", "", "", testNumber("new.metric", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))
	if err := s.add([]*metricpb.ResourceMetrics{input}); err == nil || err.Error() != "Cloud Monitoring descriptor limit" {
		t.Fatalf("descriptor cap = %v", err)
	}
	if len(s.streams) != 0 || len(s.descriptors) != maxActiveMetricStreams {
		t.Fatal("over-limit admission changed state")
	}
}

func TestCloudRegistriesTurnOverOnlyDeliveredIdleStreams(t *testing.T) {
	s := newMetricStreams()
	s.gcp = true
	clock := time.Unix(100, 0)
	s.now = func() time.Time { return clock }
	input := func(name, unit string) *metricpb.ResourceMetrics {
		metric := testNumber(name, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1)
		metric.Unit = unit
		return testMetricResource("native", "scope", "", "", metric)
	}
	for i := range maxActiveMetricStreams - 1 {
		if err := s.add([]*metricpb.ResourceMetrics{input(fmt.Sprintf("old.%d", i), "{call}")}); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(metricStreamIdleTTL / 2)
	if err := s.add([]*metricpb.ResourceMetrics{input("still.active", "{call}")}); err != nil {
		t.Fatal(err)
	}
	if len(s.streams) != maxActiveMetricStreams || len(s.descriptors) != maxActiveMetricStreams || len(s.cloudIdentities) != maxActiveMetricStreams {
		t.Fatal("did not reach all active and Cloud registry caps")
	}
	_ = s.snapshot()
	s.clearPendingMarker()
	clock = clock.Add(metricStreamIdleTTL / 2)
	if err := s.add([]*metricpb.ResourceMetrics{input("after.expiry", "{call}")}); err != nil {
		t.Fatalf("delivered idle turnover: %v", err)
	}
	if len(s.streams) != 2 || len(s.descriptors) != 2 || len(s.cloudIdentities) != 2 {
		t.Fatalf("retained state streams=%d descriptors=%d identities=%d", len(s.streams), len(s.descriptors), len(s.cloudIdentities))
	}
	if err := s.add([]*metricpb.ResourceMetrics{input("still.active", "ms")}); err == nil || err.Error() != "incompatible Cloud Monitoring descriptor" {
		t.Fatalf("active descriptor conflict = %v", err)
	}
}

func TestMetricRejectsUnsupportedPointDetailsAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*metricpb.Metric)
	}{
		{"flags", func(m *metricpb.Metric) { m.GetSum().DataPoints[0].Flags = 1 }},
		{"delta exemplar", func(m *metricpb.Metric) { m.GetSum().DataPoints[0].Exemplars = []*metricpb.Exemplar{{}} }},
		{"negative monotonic", func(m *metricpb.Metric) { m.GetSum().DataPoints[0].Value = &metricpb.NumberDataPoint_AsInt{AsInt: -1} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetricStreams()
			m := testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, 1)
			tc.change(m)
			if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", m)}); err == nil {
				t.Fatal("unsupported detail admitted")
			}
		})
	}
	s := newMetricStreams()
	first := testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, int64(^uint64(0)>>1))
	if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", first)}); err != nil {
		t.Fatal(err)
	}
	if err := s.add([]*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 2, 3, 1))}); err == nil {
		t.Fatal("integer overflow admitted")
	}
}
