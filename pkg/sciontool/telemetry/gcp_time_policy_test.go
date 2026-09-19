package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTimePolicyPipeline() (*Pipeline, *captureMetricExporter) {
	sink := &captureMetricExporter{}
	p := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp"})
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sink}}
	return p, sink
}

func nativeTimePoint(name string, start, end uint64) *metricpb.ResourceMetrics {
	return testMetricResource("sciontool", "native.scope", "1", "native", testNumber(name, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, 7))
}

func TestGCPNativeKnownSpacingRejectedBeforeAdmission(t *testing.T) {
	p, sink := newTimePolicyPipeline()
	const start = uint64(1000000000)
	const first = start + uint64(10*time.Second)
	firstPoint := nativeTimePoint("native.calls", start, first)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{firstPoint}); err != nil {
		t.Fatal(err)
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first export")
	}
	if len(sink.exports) != 1 {
		t.Fatalf("exports=%d", len(sink.exports))
	}
	before := p.QueueDepth()
	for _, gap := range []time.Duration{4 * time.Second, 5*time.Second - time.Nanosecond} {
		invalid := nativeTimePoint("native.calls", start, first+uint64(gap))
		err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{invalid})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "unsupported Cloud Monitoring sampling interval") {
			t.Fatalf("gap %s error=%v", gap, err)
		}
		if p.QueueDepth() != before {
			t.Fatalf("rejection changed depth: %+v", p.QueueDepth())
		}
	}
	// A replay of an unchanged point remains a filtered no-op.
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{firstPoint}); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	valid := nativeTimePoint("native.calls", start, first+uint64(5*time.Second))
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{valid}); err != nil {
		t.Fatalf("exact five seconds: %v", err)
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("second export")
	}
	d := p.Diagnostics()["metrics"]
	if d.Accepted != 2 || d.Delivered != 2 || d.Rejected != 2 {
		t.Fatalf("diagnostics=%+v", d)
	}
}

func TestGCPNativeMixedRequestRejectsWholeBatch(t *testing.T) {
	p, _ := newTimePolicyPipeline()
	const start = uint64(1000000000)
	const end = start + uint64(10*time.Second)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", start, end)}); err != nil {
		t.Fatal(err)
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first export")
	}
	invalid := nativeTimePoint("native.calls", start, end+uint64(time.Second))
	valid := nativeTimePoint("unrelated.calls", start, end+uint64(time.Second))
	err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{invalid, valid})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mixed error=%v", err)
	}
	if p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("mixed request retained data: %+v", p.QueueDepth())
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{valid}); err != nil {
		t.Fatalf("independent identity rejected: %v", err)
	}
}

func TestGCPNativeKindsKnownSpacing(t *testing.T) {
	for _, kind := range []string{"sum", "histogram", "gauge"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := newTimePolicyPipeline()
			const start = uint64(1000000000)
			const first = start + uint64(10*time.Second)
			input := func(end uint64) *metricpb.ResourceMetrics {
				var metric *metricpb.Metric
				switch kind {
				case "sum":
					metric = testNumber("native.kind.sum", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, int64(end))
				case "histogram":
					metric = testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, 1, 1, []float64{10}, []uint64{1, 0})
					metric.Name = "native.kind.histogram"
				default:
					metric = &metricpb.Metric{Name: "native.kind.gauge", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: int64(end)}}}}}}
				}
				return testMetricResource("sciontool", "native.scope", "1", "native", metric)
			}
			if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first)}); err != nil {
				t.Fatal(err)
			}
			if !p.flushMetricBuffer(context.Background(), true) {
				t.Fatal("first export")
			}
			if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first + uint64(5*time.Second-time.Nanosecond))}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("below boundary %v", err)
			}
			if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(first + uint64(5*time.Second))}); err != nil {
				t.Fatalf("exact boundary %v", err)
			}
		})
	}
}

func TestGCPHookSelectorAndGenericSourceTime(t *testing.T) {
	makeInput := func(scope, name, unit string, temporal metricpb.AggregationTemporality) *metricpb.ResourceMetrics {
		metric := testNumber(name, temporal, 1000000000, 2000000000, 1)
		metric.Unit = unit
		return testMetricResource("sciontool", scope, "", "", metric)
	}
	for _, tc := range []struct {
		scope, name, unit string
		temporal          metricpb.AggregationTemporality
		reject            bool
	}{
		{hookMetricScope, "agent.tool.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "agent.session.count", "{session}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "gen_ai.api.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "scion.hook.tokens.input", "{token}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "scion.hook.tokens.output", "{token}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "scion.hook.tokens.cached", "{token}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{hookMetricScope, "agent.tool.calls", "wrong", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, true},
		{hookMetricScope, "agent.tool.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, true},
		{LifecycleMetricScope, "agent.session.count", "{session}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
		{"native.scope", "agent.tool.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, false},
	} {
		s := newMetricStreams()
		s.gcp = true
		err := s.add([]*metricpb.ResourceMetrics{makeInput(tc.scope, tc.name, tc.unit, tc.temporal)})
		if (err != nil) != tc.reject {
			t.Fatalf("selector %+v error=%v", tc, err)
		}
	}
	double := makeInput(hookMetricScope, "agent.tool.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA)
	double.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Value = &metricpb.NumberDataPoint_AsDouble{AsDouble: 1}
	doubleState := newMetricStreams()
	doubleState.gcp = true
	if err := doubleState.add([]*metricpb.ResourceMetrics{double}); err == nil {
		t.Fatal("reserved hook double point accepted")
	}
	generic := newMetricStreams()
	if err := generic.add([]*metricpb.ResourceMetrics{makeInput(hookMetricScope, "agent.tool.calls", "{call}", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA)}); err != nil {
		t.Fatal(err)
	}
	point := generic.snapshot()[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	if point.StartTimeUnixNano != 1000000000 || point.TimeUnixNano != 2000000000 {
		t.Fatalf("generic timestamps changed: %+v", point)
	}
}

func TestGCPHookSnapshotObservationClockAndRollbackHold(t *testing.T) {
	p, sink := newTimePolicyPipeline()
	now := time.Unix(100, 0)
	p.metricNow = func() time.Time { return now }
	input := func(start, end uint64) *metricpb.ResourceMetrics {
		return testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, start, end, 1))
	}
	// Use the actual handler unit; source timestamps can be arbitrarily skewed.
	setUnit := func(rm *metricpb.ResourceMetrics) *metricpb.ResourceMetrics {
		rm.ScopeMetrics[0].Metrics[0].Unit = "{call}"
		return rm
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{setUnit(input(1000000000, 2000000000))}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first snapshot")
	}
	first := sink.exports[0].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if !first.StartTime.Equal(time.Unix(100, 0)) || !first.Time.Equal(time.Unix(101, 0)) {
		t.Fatalf("first collector time: %+v", first)
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{setUnit(input(3000000000, 4000000000))}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Second)
	depth := p.QueueDepth()
	if p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("rollback exported")
	}
	if p.QueueDepth() != depth || len(sink.exports) != 1 || len(p.metricPending) != 0 {
		t.Fatalf("rollback transferred ownership: depth=%+v pending=%d exports=%d", p.QueueDepth(), len(p.metricPending), len(sink.exports))
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	started := time.Now()
	p.flushMetricsOnStop(stopCtx)
	cancel()
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond || p.QueueDepth() != depth {
		t.Fatalf("rollback Stop wait elapsed=%s depth=%+v", elapsed, p.QueueDepth())
	}
	now = now.Add(6 * time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("recovered clock did not export")
	}
	if len(sink.exports) != 2 {
		t.Fatalf("exports=%d", len(sink.exports))
	}
	second := sink.exports[1].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if !second.StartTime.Equal(first.StartTime) || !second.Time.Equal(time.Unix(106, 0)) || second.Value != 2 {
		t.Fatalf("second collector observation: %+v", second)
	}
}

func TestGCPHookEpochRecreatedAfterIdleAndNoDuplicateCredit(t *testing.T) {
	p, sink := newTimePolicyPipeline()
	now := time.Unix(100, 0)
	p.metricNow = func() time.Time { return now }
	input := func(start, end uint64) *metricpb.ResourceMetrics {
		m := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, start, end, 1)
		m.Unit = "{call}"
		return testMetricResource("sciontool", hookMetricScope, "", "", m)
	}
	first := input(1000000000, 2000000000)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{first}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first export")
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{first}); err != nil {
		t.Fatal(err)
	}
	if p.QueueDepth() != (QueueDepth{}) {
		t.Fatal("duplicate owns retained work")
	}
	// A distinct backdated source addition still belongs to a later actual observation.
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(500000000, 1500000000)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("backdated addition export")
	}
	second := sink.exports[1].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if second.Value != 2 || !second.StartTime.Equal(time.Unix(100, 0)) || !second.Time.Equal(now) {
		t.Fatalf("backdated observation %+v", second)
	}
	now = now.Add(31 * time.Minute)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(3000000000, 4000000000)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("new epoch export")
	}
	third := sink.exports[2].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if third.Value != 1 || !third.StartTime.Equal(now.Add(-time.Second)) || !third.Time.Equal(now) {
		t.Fatalf("new epoch %+v", third)
	}
}

func TestGCPHookSnapshotWaitsForActualTwoMillisecondObservation(t *testing.T) {
	p, sink := newTimePolicyPipeline()
	now := time.Unix(100, 0)
	p.metricNow = func() time.Time { return now }
	m := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1000000000, 2000000000, 1)
	m.Unit = "{call}"
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", m)}); err != nil {
		t.Fatal(err)
	}
	owned := p.QueueDepth()
	if owned.Records != 1 || owned.Entries != 1 {
		t.Fatalf("unexpected admitted ownership: %+v", owned)
	}
	for _, gap := range []time.Duration{500 * time.Microsecond, time.Millisecond, 2*time.Millisecond - time.Nanosecond} {
		now = time.Unix(100, 0).Add(gap)
		if p.flushMetricBuffer(context.Background(), true) || len(p.metricPending) != 0 || len(sink.exports) != 0 {
			t.Fatalf("premature point at %s", gap)
		}
		if depth, diag := p.QueueDepth(), p.Diagnostics()["metrics"]; depth != owned || len(p.metricDirtyAdmissions) != 1 || len(p.metricPendingAdmissions) != 0 || !p.metricStreams.hasDirty() || len(p.metricPossibleEnds) != 0 || p.metricBatchSequence != 0 || diag.Accepted != 1 || diag.Queued != 1 || diag.Attempts != 0 || diag.Delivered != 0 || diag.Unconfirmed != 0 {
			t.Fatalf("short snapshot transferred ownership at %s: depth=%+v diag=%+v dirty=%d pending=%d sequence=%d watermarks=%d", gap, depth, diag, len(p.metricDirtyAdmissions), len(p.metricPendingAdmissions), p.metricBatchSequence, len(p.metricPossibleEnds))
		}
	}
	now = time.Unix(100, 0).Add(2 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("2ms actual observation not exported")
	}
	pt := sink.exports[0].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if !pt.Time.Equal(now) {
		t.Fatalf("fabricated observation end %+v", pt)
	}
	if depth, diag := p.QueueDepth(), p.Diagnostics()["metrics"]; depth != (QueueDepth{}) || diag.Accepted != 1 || diag.Delivered != 1 || diag.Queued != 1 || diag.Attempts != 1 || len(p.metricDirtyAdmissions) != 0 || len(p.metricPendingAdmissions) != 0 || p.metricBatchSequence != 1 || len(p.metricPossibleEnds) != 1 {
		t.Fatalf("eligible snapshot did not settle ownership: depth=%+v diag=%+v dirty=%d pending=%d sequence=%d watermarks=%d", depth, diag, len(p.metricDirtyAdmissions), len(p.metricPendingAdmissions), p.metricBatchSequence, len(p.metricPossibleEnds))
	}
}

func TestGCPHookFutureSourceIntervalKeepsCollectorTime(t *testing.T) {
	p, sink := newTimePolicyPipeline()
	now := time.Unix(100, 0)
	p.metricNow = func() time.Time { return now }
	m := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, uint64(time.Unix(1000, 0).UnixNano()), uint64(time.Unix(1001, 0).UnixNano()), 1)
	m.Unit = "{call}"
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", m)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("source-skewed hook export")
	}
	point := sink.exports[0].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if !point.StartTime.Equal(time.Unix(100, 0)) || !point.Time.Equal(time.Unix(101, 0)) {
		t.Fatalf("source skew dictated output time: %+v", point)
	}
}

func TestGCPPairedCounterAndCloseHistogramRejectWholeRequest(t *testing.T) {
	p, _ := newTimePolicyPipeline()
	makePair := func(counterStart, counterEnd, histEnd uint64, count uint64) *metricpb.ResourceMetrics {
		counter := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, counterStart, counterEnd, 1)
		counter.Unit = "{call}"
		hist := testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1000000000, histEnd, count, float64(count), []float64{10}, []uint64{count, 0})
		hist.Name = "agent.tool.duration"
		rm := testMetricResource("sciontool", hookMetricScope, "", "", counter)
		rm.ScopeMetrics[0].Metrics = append(rm.ScopeMetrics[0].Metrics, hist)
		return rm
	}
	first := makePair(1000000000, 2000000000, 11000000000, 1)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{first}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond) // The collector observation must be SDK-eligible.
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first paired export")
	}
	second := makePair(3000000000, 4000000000, 12000000000, 2)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{second}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("paired close histogram must reject whole request: %v", err)
	}
	d := p.Diagnostics()["metrics"]
	if d.Accepted != 2 || d.Delivered != 2 || d.Rejected != 2 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("paired accounting %+v depth=%+v", d, p.QueueDepth())
	}
}

type partialCauseSink struct {
	captureMetricExporter
	calls int
	fail  error
}

type retryCaptureSink struct {
	captureMetricExporter
	calls int
}

type blockedMetricSink struct {
	captureMetricExporter
	entered chan struct{}
	release chan struct{}
}

func (s *blockedMetricSink) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.captureMetricExporter.Export(ctx, rm)
}

func TestGCPNativeAdmissionAndFreezeLockOrders(t *testing.T) {
	const start = uint64(1000000000)
	const first = start + uint64(10*time.Second)
	// Admission wins before the exporter can acquire metricExportMu: both
	// accepted values become one latest immutable snapshot.
	p, sink := newTimePolicyPipeline()
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", start, first)}); err != nil {
		t.Fatal(err)
	}
	p.metricExportMu.Lock()
	result := make(chan bool, 1)
	go func() { result <- p.flushMetricBuffer(context.Background(), true) }()
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", start, first+uint64(time.Second))}); err != nil {
		t.Fatalf("pre-freeze admission: %v", err)
	}
	p.metricExportMu.Unlock()
	if !<-result || len(sink.exports) != 1 || p.Diagnostics()["metrics"].Delivered != 2 {
		t.Fatalf("pre-freeze coalescing exports=%d diagnostics=%+v", len(sink.exports), p.Diagnostics()["metrics"])
	}
	// Freeze wins: the exporter blocks after watermark publication, so the
	// next same-series close point must be rejected before ACK.
	blocked := &blockedMetricSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p2 := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp"})
	p2.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: blocked}}
	if err := p2.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", start, first)}); err != nil {
		t.Fatal(err)
	}
	result2 := make(chan bool, 1)
	go func() { result2 <- p2.flushMetricBuffer(context.Background(), true) }()
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("exporter not entered")
	}
	if err := p2.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", start, first+uint64(time.Second))}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("post-freeze admission: %v", err)
	}
	close(blocked.release)
	if !<-result2 || p2.Diagnostics()["metrics"].Accepted != 1 || p2.Diagnostics()["metrics"].Rejected != 1 {
		t.Fatalf("post-freeze diagnostics %+v", p2.Diagnostics()["metrics"])
	}
}

func (s *retryCaptureSink) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	s.calls++
	_ = s.captureMetricExporter.Export(ctx, rm)
	if s.calls <= 2 {
		return status.Error(codes.Unavailable, "local transient")
	}
	return nil
}

func TestGCPHookPendingRetryKeepsTimeAndNewerCohort(t *testing.T) {
	sink := &retryCaptureSink{}
	p := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp"})
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sink}}
	now := time.Unix(100, 0)
	p.metricNow = func() time.Time { return now }
	input := func(start, end uint64, value int64) *metricpb.ResourceMetrics {
		m := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, start, end, value)
		m.Unit = "{call}"
		return testMetricResource("sciontool", hookMetricScope, "", "", m)
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(1000000000, 2000000000, 7)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first transient confirmed")
	}
	digest := metricBatchDigest(p.metricPending)
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{input(3000000000, 4000000000, 3)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	if p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("second transient confirmed")
	}
	if digest != metricBatchDigest(p.metricPending) {
		t.Fatal("immutable pending retry changed")
	}
	for i := 0; i < 2; i++ {
		point := sink.exports[i].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
		if point.Value != 7 || !point.StartTime.Equal(time.Unix(100, 0)) || !point.Time.Equal(time.Unix(101, 0)) {
			t.Fatalf("retry %d changed point: %+v", i, point)
		}
	}
	now = now.Add(15 * time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("pending recovery")
	}
	now = now.Add(15 * time.Second)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("newer cohort recovery")
	}
	last := sink.exports[3].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints[0]
	if last.Value != 10 || !last.StartTime.Equal(time.Unix(100, 0)) || !last.Time.Equal(now) {
		t.Fatalf("newer cumulative point %+v", last)
	}
	d := p.Diagnostics()["metrics"]
	if d.Accepted != 2 || d.Delivered != 2 || d.Failed != 2 || d.Attempts != 4 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("recovery %+v depth=%+v", d, p.QueueDepth())
	}
}

func (s *partialCauseSink) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	s.calls++
	if s.calls == 2 {
		return s.fail
	}
	return s.captureMetricExporter.Export(ctx, rm)
}

func TestGCPPartialFailureKeepsSafeCauseAndTerminalDisposition(t *testing.T) {
	if cause, code := classifyMonitoringFailures([]error{status.Error(codes.PermissionDenied, "PRIVATE_A"), status.Error(codes.Unavailable, "PRIVATE_B")}); cause != "mixed" || code != "mixed" {
		t.Fatalf("mixed classes = %q/%q", cause, code)
	}
	for _, tc := range []struct {
		name        string
		err         error
		cause, code string
	}{
		{"invalid", status.Error(codes.InvalidArgument, "PRIVATE_PAYLOAD"), "other", "InvalidArgument"},
		{"unavailable", status.Error(codes.Unavailable, "PRIVATE_PAYLOAD"), "other", "Unavailable"},
		{"deadline", context.DeadlineExceeded, "timeout", "Unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &partialCauseSink{fail: tc.err}
			p := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp"})
			p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sink}}
			first := nativeTimePoint("native.calls", 1000000000, 11000000000)
			second := nativeTimePoint("native.other", 1000000000, 11000000000)
			if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{first, second}); err != nil {
				t.Fatal(err)
			}
			if p.flushMetricBuffer(context.Background(), true) {
				t.Fatal("partial export confirmed whole batch")
			}
			d := p.Diagnostics()["metrics"]
			if d.Accepted != 2 || d.Unconfirmed != 2 || d.Partial != 2 || d.Delivered != 0 || d.Failed != 1 || sink.calls != 2 {
				t.Fatalf("partial accounting %+v calls=%d", d, sink.calls)
			}
			if p.metricPending != nil || p.QueueDepth() != (QueueDepth{}) {
				t.Fatalf("partial retained ownership: %+v", p.QueueDepth())
			}
			if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{nativeTimePoint("native.calls", 1000000000, 12000000000)}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("partial terminal lost possible-write watermark: %v", err)
			}
			sink2 := &partialCauseSink{fail: tc.err}
			err := (&GCPExporter{metricExporter: sink2}).ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{first, second})
			var partial *partialSuccessError
			if !errors.As(err, &partial) {
				t.Fatalf("missing partial error: %v", err)
			}
			if partial.succeededGroups != 1 || partial.failedGroups != 1 || partial.causeClass != tc.cause || partial.statusCode != tc.code || strings.Contains(err.Error(), "PRIVATE_PAYLOAD") {
				t.Fatalf("unsafe/incorrect partial: %+v %v", partial, err)
			}
		})
	}
}
