/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
)

// genericIdentityTestResource builds a ResourceMetrics with authoritative
// identity on the resource (as the receiver stamps it, policy.go
// processResource) and one metric of every OTLP point kind — Sum, Gauge,
// Histogram, ExponentialHistogram, Summary — none of them carrying any
// identity label yet.
func genericIdentityTestResource() *metricpb.ResourceMetrics {
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		metricStringLabel("scion.agent.id", "agent-generic-1"),
		metricStringLabel("scion.project.id", "project-generic-1"),
		metricStringLabel("scion.agent.slug", "generic-agent-slug"),
	}}
	sum := &metricpb.Metric{Name: "test.sum", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{
		AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints:             []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}}},
	}}}
	gauge := &metricpb.Metric{Name: "test.gauge", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{
		DataPoints: []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsInt{AsInt: 2}}},
	}}}
	histogram := &metricpb.Metric{Name: "test.histogram", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
		AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints:             []*metricpb.HistogramDataPoint{{Count: 1}},
	}}}
	expHistogram := &metricpb.Metric{Name: "test.exponential_histogram", Data: &metricpb.Metric_ExponentialHistogram{ExponentialHistogram: &metricpb.ExponentialHistogram{
		AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints:             []*metricpb.ExponentialHistogramDataPoint{{Count: 1}},
	}}}
	summary := &metricpb.Metric{Name: "test.summary", Data: &metricpb.Metric_Summary{Summary: &metricpb.Summary{
		DataPoints: []*metricpb.SummaryDataPoint{{Count: 1}},
	}}}
	return &metricpb.ResourceMetrics{
		Resource: resource,
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "test.scope"},
			Metrics: []*metricpb.Metric{sum, gauge, histogram, expHistogram, summary},
		}},
	}
}

// TestGenericOTLPIdentityStampingCoversEveryPointKind is F4/F5: the generic
// OTLP path must stamp scion_agent_id/scion_project_id/scion_agent_slug on
// every exported metric point kind (AC-1.1b "every exported metric point"),
// not only Sum/Gauge/Histogram — ExponentialHistogram and Summary too.
func TestGenericOTLPIdentityStampingCoversEveryPointKind(t *testing.T) {
	var captured *colmetricpb.ExportMetricsServiceRequest
	exporter := &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		captured = req
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	if err := exporter.ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{genericIdentityTestResource()}); err != nil {
		t.Fatal(err)
	}
	if captured == nil || len(captured.ResourceMetrics) != 1 {
		t.Fatal("exporter did not forward the resource metrics")
	}
	metrics := captured.ResourceMetrics[0].ScopeMetrics[0].Metrics
	assertIdentity := func(name string, attrs []*commonpb.KeyValue) {
		t.Helper()
		if metricAttrString(attrs, "scion_agent_id") != "agent-generic-1" ||
			metricAttrString(attrs, "scion_project_id") != "project-generic-1" ||
			metricAttrString(attrs, "scion_agent_slug") != "generic-agent-slug" {
			t.Errorf("%s: point attributes = %+v, want scion_agent_id/scion_project_id/scion_agent_slug stamped", name, attrs)
		}
	}
	seen := map[string]bool{}
	for _, m := range metrics {
		seen[m.Name] = true
		switch m.Name {
		case "test.sum":
			assertIdentity(m.Name, m.GetSum().DataPoints[0].Attributes)
		case "test.gauge":
			assertIdentity(m.Name, m.GetGauge().DataPoints[0].Attributes)
		case "test.histogram":
			assertIdentity(m.Name, m.GetHistogram().DataPoints[0].Attributes)
		case "test.exponential_histogram":
			assertIdentity(m.Name, m.GetExponentialHistogram().DataPoints[0].Attributes)
		case "test.summary":
			assertIdentity(m.Name, m.GetSummary().DataPoints[0].Attributes)
		default:
			t.Fatalf("unexpected metric %q", m.Name)
		}
	}
	for _, name := range []string{"test.sum", "test.gauge", "test.histogram", "test.exponential_histogram", "test.summary"} {
		if !seen[name] {
			t.Fatalf("metric %q missing from captured export", name)
		}
	}
}

// TestGenericOTLPIdentityStampingToleratesNilDataPointEntries is the
// verification for the upstream review of PR 2051 (Gemini finding on
// identity_stamp.go:73, "nil-check point before appendCanonicalIdentity").
// A nil point can't reach appendCanonicalIdentity: proto.Clone(source), which
// stampIdentityLabels runs before ever touching a DataPoints slice, replaces
// a nil element of a repeated message field with a fresh non-nil zero-value
// message rather than preserving the nil (verified with a standalone
// proto.Clone repro during review; this test pins that behavior against this
// package's actual types instead of just asserting it in a comment). So a
// hand-built nil entry becomes an empty, stampable point, and nothing here
// panics -- declined the suggested nil-check as literal dead code, backed by
// this test rather than by the pipeline-admission argument alone (which
// already covered the real pipeline, see the doc comment above the loops).
func TestGenericOTLPIdentityStampingToleratesNilDataPointEntries(t *testing.T) {
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		metricStringLabel("scion.agent.id", "agent-nil-point-1"),
		metricStringLabel("scion.project.id", "project-nil-point-1"),
		metricStringLabel("scion.agent.slug", "nil-point-slug"),
	}}
	sum := &metricpb.Metric{Name: "test.sum", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{
		AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints: []*metricpb.NumberDataPoint{
			nil,
			{Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}},
		},
	}}}
	input := &metricpb.ResourceMetrics{
		Resource: resource,
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "test.scope"},
			Metrics: []*metricpb.Metric{sum},
		}},
	}

	output, err := stampIdentityLabels([]*metricpb.ResourceMetrics{input})
	if err != nil {
		t.Fatalf("stampIdentityLabels: %v", err)
	}
	points := output[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints
	if len(points) != 2 {
		t.Fatalf("points = %+v, want 2 (proto.Clone must not drop the nil entry, only neutralize it)", points)
	}
	for i, want := range []string{"the cloned nil entry", "the originally non-nil entry"} {
		if points[i] == nil {
			t.Fatalf("points[%d] (%s) is nil after cloning; proto.Clone should always yield a non-nil message for a list element", i, want)
		}
		attrs := points[i].Attributes
		if metricAttrString(attrs, "scion_agent_id") != "agent-nil-point-1" ||
			metricAttrString(attrs, "scion_project_id") != "project-nil-point-1" ||
			metricAttrString(attrs, "scion_agent_slug") != "nil-point-slug" {
			t.Errorf("points[%d] (%s) attributes = %+v, want scion_agent_id/scion_project_id/scion_agent_slug stamped", i, want, attrs)
		}
	}
	if points[1].GetAsInt() != 1 {
		t.Errorf("points[1].AsInt = %d, want 1 (the originally non-nil entry's value must survive)", points[1].GetAsInt())
	}
}

// TestReservedIdentityPointLabelRejectedAtAdmissionBothPaths is F3+F4: a
// producer-supplied value for any of the three exporter-reserved identity
// labels must be rejected where the point is admitted (metricStreams.add),
// on both the GCP and the generic OTLP path — not deferred to export time,
// where a single offending point would otherwise poison the whole batch
// (F3).
//
// Each reserved key is tried under its canonical (underscore) spelling and
// the dotted/dashed variants cloudLabelKey normalizes to the same key
// (round-3 review N4): the GCP path already rejects those via
// validateDescriptor's use of cloudLabelKey, and
// rejectReservedIdentityPointLabel now does the same for the generic path,
// closing a gap where a producer label like scion.agent.id could otherwise
// collide with the stamped scion_agent_id after a backend's own
// dot-to-underscore translation (for example Prometheus/Mimir).
//
// N5: the rejection reason must actually name the reserved-key check, not
// merely be non-nil — a stray extra label would also be rejected on the GCP
// path (as an unsupported dimension), which would let this test pass even
// if the reserved-key check itself broke.
func TestReservedIdentityPointLabelRejectedAtAdmissionBothPaths(t *testing.T) {
	spellingVariants := func(canonical string) []string {
		return []string{canonical, strings.ReplaceAll(canonical, "_", "."), strings.ReplaceAll(canonical, "_", "-")}
	}
	for _, gcp := range []bool{true, false} {
		for _, reserved := range identityLabelKeys {
			for _, spelling := range spellingVariants(reserved) {
				t.Run(fmt.Sprintf("gcp=%v/%s", gcp, spelling), func(t *testing.T) {
					metric := testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1,
						metricStringLabel(spelling, "producer-supplied"))
					input := testMetricResource("native", "scope", "", "", metric)
					s := newMetricStreams()
					s.gcp = gcp
					err := s.add([]*metricpb.ResourceMetrics{input})
					if err == nil {
						t.Fatalf("producer-supplied %s admitted with gcp=%v, want rejection", spelling, gcp)
					}
					if !strings.Contains(err.Error(), "reserved") {
						t.Fatalf("rejection reason = %q, want it to contain %q (a different, unrelated rejection would also make this test pass)", err.Error(), "reserved")
					}
				})
			}
		}
	}
}
