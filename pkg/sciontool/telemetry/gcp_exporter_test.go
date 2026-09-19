/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"testing"

	"cloud.google.com/go/logging"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

type captureTraceExporter struct {
	spans []sdktrace.ReadOnlySpan
}

func (c *captureTraceExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	c.spans = append(c.spans, spans...)
	return nil
}

func (*captureTraceExporter) Shutdown(context.Context) error { return nil }

func TestGCPExporter_PostAdapterCapturesProcessedTraceScopeAndSchema(t *testing.T) {
	traceCapture := &captureTraceExporter{}
	exporter := &GCPExporter{traceExporter: traceCapture}
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{Redact: []string{"scope.secret"}}})
	decision := policy.processSpans([]*tracepb.ResourceSpans{{
		SchemaUrl: "https://example.test/resource",
		ScopeSpans: []*tracepb.ScopeSpans{{
			SchemaUrl: "https://example.test/scope",
			Scope:     &commonpb.InstrumentationScope{Name: "native.scope", Attributes: secretAttr("scope.secret", "SCOPE_SECRET")},
			Spans:     []*tracepb.Span{{Name: "safe.span"}},
		}},
	}})
	if decision.Reason != "" {
		t.Fatal(decision.Reason)
	}
	if err := exporter.ExportProtoSpans(context.Background(), decision.Data); err != nil {
		t.Fatal(err)
	}
	if len(traceCapture.spans) != 1 {
		t.Fatalf("captured %d spans", len(traceCapture.spans))
	}
	got := traceCapture.spans[0]
	scope := got.InstrumentationScope()
	if got.Resource().SchemaURL() != "https://example.test/resource" || scope.SchemaURL != "https://example.test/scope" {
		t.Fatalf("post-adapter schema: resource=%q scope=%q", got.Resource().SchemaURL(), scope.SchemaURL)
	}
	if value, ok := scope.Attributes.Value("scope.secret"); !ok || value.AsString() != "[REDACTED]" {
		t.Fatalf("post-adapter scope attributes: %v", scope.Attributes)
	}
}

func TestGCPExporter_PostAdapterCapturesProcessedMetricScopeAndSchema(t *testing.T) {
	metricCapture := &captureMetricExporter{}
	exporter := &GCPExporter{metricExporter: metricCapture}
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{Redact: []string{"scope.secret"}}})
	decision := policy.processMetrics([]*metricpb.ResourceMetrics{{
		SchemaUrl: "https://example.test/resource",
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			SchemaUrl: "https://example.test/scope",
			Scope:     &commonpb.InstrumentationScope{Name: "native.scope", Attributes: secretAttr("scope.secret", "SCOPE_SECRET")},
			Metrics:   []*metricpb.Metric{{Name: "safe.metric", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}}}}}}},
		}},
	}})
	if decision.Reason != "" {
		t.Fatal(decision.Reason)
	}
	if err := exporter.ExportProtoMetrics(context.Background(), decision.Data); err != nil {
		t.Fatal(err)
	}
	if len(metricCapture.exports) != 1 {
		t.Fatalf("captured %d exports", len(metricCapture.exports))
	}
	got := metricCapture.exports[0]
	if got.Resource.SchemaURL() != "https://example.test/resource" || got.ScopeMetrics[0].Scope.SchemaURL != "https://example.test/scope" {
		t.Fatalf("post-adapter schema: resource=%q scope=%q", got.Resource.SchemaURL(), got.ScopeMetrics[0].Scope.SchemaURL)
	}
	if value, ok := got.ScopeMetrics[0].Scope.Attributes.Value("scope.secret"); !ok || value.AsString() != "[REDACTED]" {
		t.Fatalf("post-adapter scope attributes: %v", got.ScopeMetrics[0].Scope.Attributes)
	}
}

func TestGCPExporter_PostAdapterCapturesProcessedLogMetadata(t *testing.T) {
	var captured []logging.Entry
	exporter := &GCPExporter{logSink: func(entry logging.Entry) { captured = append(captured, entry) }}
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{Redact: []string{"scope.secret", "tool_output"}}})
	decision := policy.processLogs([]*logspb.ResourceLogs{{
		SchemaUrl: "https://example.test/resource",
		ScopeLogs: []*logspb.ScopeLogs{{
			SchemaUrl:  "https://example.test/scope",
			Scope:      &commonpb.InstrumentationScope{Name: "native.scope", Attributes: secretAttr("scope.secret", "SCOPE_SECRET")},
			LogRecords: []*logspb.LogRecord{{EventName: "safe.event", Attributes: secretAttr("output", "OUTPUT_SECRET")}},
		}},
	}})
	if decision.Reason != "" {
		t.Fatal(decision.Reason)
	}
	if err := exporter.ExportProtoLogs(context.Background(), decision.Data); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d entries", len(captured))
	}
	payload := captured[0].Payload.(map[string]interface{})
	if payload["output"] != "[REDACTED]" {
		t.Fatalf("post-adapter output = %#v", payload["output"])
	}
	metadata := payload[cloudLoggingOTelMetadataKey].(map[string]interface{})
	if metadata["resource_schema_url"] != "https://example.test/resource" || metadata["scope_schema_url"] != "https://example.test/scope" || metadata["scope_name"] != "native.scope" {
		t.Fatalf("post-adapter log metadata = %#v", metadata)
	}
	attrs := metadata["scope_attributes"].(map[string]interface{})
	if attrs["scope.secret"] != "[REDACTED]" {
		t.Fatalf("post-adapter scope = %#v", attrs)
	}
}

type captureMetricExporter struct {
	exports []*metricdata.ResourceMetrics
}

func (c *captureMetricExporter) Temporality(metric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

func (c *captureMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}

func (c *captureMetricExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	c.exports = append(c.exports, rm)
	return nil
}

func (c *captureMetricExporter) ForceFlush(context.Context) error { return nil }
func (c *captureMetricExporter) Shutdown(context.Context) error   { return nil }

func TestGCPExporter_ExportProtoMetrics(t *testing.T) {
	exp := &captureMetricExporter{}
	exporter := &GCPExporter{metricExporter: exp}

	err := exporter.ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{
		{
			ScopeMetrics: []*metricpb.ScopeMetrics{
				{
					Metrics: []*metricpb.Metric{
						{
							Name: "gemini_cli.token.usage",
							Data: &metricpb.Metric_Sum{
								Sum: &metricpb.Sum{
									AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
									DataPoints: []*metricpb.NumberDataPoint{
										{
											TimeUnixNano: 1,
											Value:        &metricpb.NumberDataPoint_AsInt{AsInt: 100},
										},
									},
								},
							},
						},
						{
							Name: "gen_ai.client.token.usage",
							Data: &metricpb.Metric_Sum{
								Sum: &metricpb.Sum{
									AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
									DataPoints: []*metricpb.NumberDataPoint{
										{
											TimeUnixNano: 2,
											Value:        &metricpb.NumberDataPoint_AsInt{AsInt: 55},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ExportProtoMetrics() error = %v", err)
	}
	if len(exp.exports) != 1 {
		t.Fatalf("len(exports) = %d, want 1", len(exp.exports))
	}
	if len(exp.exports[0].ScopeMetrics) != 1 || len(exp.exports[0].ScopeMetrics[0].Metrics) != 2 {
		t.Fatalf("unexpected exported metrics structure: %+v", exp.exports[0])
	}
	if got := exp.exports[0].ScopeMetrics[0].Metrics[0].Name; got != "gemini_cli.token.usage" {
		t.Fatalf("first exported metric name = %q, want gemini_cli.token.usage", got)
	}
	if got := exp.exports[0].ScopeMetrics[0].Metrics[1].Name; got != "gen_ai.client.token.usage" {
		t.Fatalf("second exported metric name = %q, want gen_ai.client.token.usage", got)
	}
}

func TestGCPExporter_ExportProtoMetrics_FiltersUnsupportedSummary(t *testing.T) {
	exp := &captureMetricExporter{}
	exporter := &GCPExporter{metricExporter: exp}

	err := exporter.ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{
		{
			ScopeMetrics: []*metricpb.ScopeMetrics{
				{
					Metrics: []*metricpb.Metric{
						{
							Name: "summary_only",
							Data: &metricpb.Metric_Summary{
								Summary: &metricpb.Summary{
									DataPoints: []*metricpb.SummaryDataPoint{
										{
											TimeUnixNano: 1,
											Attributes: []*commonpb.KeyValue{
												{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v"}}},
											},
											Count: 1,
											Sum:   2,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ExportProtoMetrics() error = %v", err)
	}
	if len(exp.exports) != 0 {
		t.Fatalf("len(exports) = %d, want 0", len(exp.exports))
	}
}
