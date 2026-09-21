/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestProtoResourceMetricsToSDK(t *testing.T) {
	resourceMetrics := []*metricpb.ResourceMetrics{
		{
			SchemaUrl: "https://example.test/resource-schema",
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude"}}},
				},
			},
			ScopeMetrics: []*metricpb.ScopeMetrics{
				{
					SchemaUrl: "https://example.test/scope-schema",
					Scope: &commonpb.InstrumentationScope{
						Name:       "native.metric.scope",
						Version:    "1.0.0",
						Attributes: []*commonpb.KeyValue{{Key: "scope.safe", Value: stringValue("processed")}},
					},
					Metrics: []*metricpb.Metric{
						{
							Name: "gemini_cli.token.usage",
							Unit: "tokens",
							Data: &metricpb.Metric_Sum{
								Sum: &metricpb.Sum{
									AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
									IsMonotonic:            true,
									DataPoints: []*metricpb.NumberDataPoint{
										{
											Attributes: []*commonpb.KeyValue{
												{Key: "type", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "input"}}},
											},
											TimeUnixNano: 123,
											Value:        &metricpb.NumberDataPoint_AsInt{AsInt: 42},
										},
									},
								},
							},
						},
						{
							Name: "claude_code.cost.usage",
							Unit: "USD",
							Data: &metricpb.Metric_Gauge{
								Gauge: &metricpb.Gauge{
									DataPoints: []*metricpb.NumberDataPoint{
										{
											TimeUnixNano: 456,
											Value:        &metricpb.NumberDataPoint_AsDouble{AsDouble: 1.25},
										},
									},
								},
							},
						},
						{
							Name: "latency",
							Unit: "ms",
							Data: &metricpb.Metric_Histogram{
								Histogram: &metricpb.Histogram{
									AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
									DataPoints: []*metricpb.HistogramDataPoint{
										{
											TimeUnixNano:   789,
											Count:          2,
											Sum:            float64Ptr(15),
											BucketCounts:   []uint64{1, 1},
											ExplicitBounds: []float64{10},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	got := protoResourceMetricsToSDK(resourceMetrics)
	if len(got) != 1 {
		t.Fatalf("len(protoResourceMetricsToSDK) = %d, want 1", len(got))
	}
	if len(got[0].ScopeMetrics) != 1 {
		t.Fatalf("len(ScopeMetrics) = %d, want 1", len(got[0].ScopeMetrics))
	}
	if len(got[0].ScopeMetrics[0].Metrics) != 3 {
		t.Fatalf("len(Metrics) = %d, want 3", len(got[0].ScopeMetrics[0].Metrics))
	}

	if _, ok := got[0].ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]); !ok {
		t.Fatalf("first metric data type = %T, want metricdata.Sum[int64]", got[0].ScopeMetrics[0].Metrics[0].Data)
	}
	if _, ok := got[0].ScopeMetrics[0].Metrics[1].Data.(metricdata.Gauge[float64]); !ok {
		t.Fatalf("second metric data type = %T, want metricdata.Gauge[float64]", got[0].ScopeMetrics[0].Metrics[1].Data)
	}
	if _, ok := got[0].ScopeMetrics[0].Metrics[2].Data.(metricdata.Histogram[float64]); !ok {
		t.Fatalf("third metric data type = %T, want metricdata.Histogram[float64]", got[0].ScopeMetrics[0].Metrics[2].Data)
	}
	if got[0].ScopeMetrics[0].Metrics[0].Name != "gemini_cli.token.usage" {
		t.Fatalf("first metric name = %q, want gemini_cli.token.usage", got[0].ScopeMetrics[0].Metrics[0].Name)
	}
	if got[0].Resource.SchemaURL() != "https://example.test/resource-schema" {
		t.Fatalf("resource schema URL = %q", got[0].Resource.SchemaURL())
	}
	scope := got[0].ScopeMetrics[0].Scope
	if scope.Name != "native.metric.scope" || scope.Version != "1.0.0" || scope.SchemaURL != "https://example.test/scope-schema" {
		t.Fatalf("scope metadata = %#v", scope)
	}
	if value, ok := scope.Attributes.Value("scope.safe"); !ok || value.AsString() != "processed" {
		t.Fatalf("scope attributes = %v", scope.Attributes)
	}
}

func float64Ptr(v float64) *float64 {
	return &v
}
