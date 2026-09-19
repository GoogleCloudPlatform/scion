/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var claudeIdentityKeys = []string{"organization.id", "user.account_uuid", "user.account_id", "user.id"}

func claudeIdentityAttributes() []*commonpb.KeyValue {
	attrs := make([]*commonpb.KeyValue, 0, len(claudeIdentityKeys)+3)
	for _, key := range claudeIdentityKeys {
		attrs = append(attrs, secretKV(key, "RAW_"+key))
	}
	return append(attrs, secretKV("session.id", "session-123"), secretKV("user.email", "private@example.test"), secretKV("safe.key", "safe-value"))
}

func assertClaudeIdentityPolicy(t *testing.T, attrs []*commonpb.KeyValue) {
	t.Helper()
	for _, key := range claudeIdentityKeys {
		assertRedacted(t, attrs, key)
	}
	assertAttr(t, attrs, "session.id", HashValue("session-123"))
	assertRedacted(t, attrs, "user.email")
	assertAttr(t, attrs, "safe.key", "safe-value")
}

// Source-shaped synthetic OTLP crosses receiver, policy and fake Cloud egress.
// It does not establish what an installed Claude binary actually emits.
func TestReceiverClaudeIdentityCannotReachEgress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		redaction RedactionConfig
	}{
		{name: "default", redaction: RedactionConfig{Redact: DefaultRedactFields, Hash: DefaultHashFields}},
		{name: "custom omit and hash", redaction: RedactionConfig{Redact: []string{"user.email"}, Hash: append([]string{"session_id"}, claudeIdentityKeys...)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithConfig(&Config{Enabled: true, Redaction: tc.redaction})
			p.retryConfig = fastRetryConfig()
			var traces []*tracepb.ResourceSpans
			var logs []*logspb.ResourceLogs
			var metrics []*metricpb.ResourceMetrics
			p.exporter = &CloudExporter{
				grpcClient: &mockTraceClient{exportFunc: func(_ context.Context, req *coltracepb.ExportTraceServiceRequest, _ ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
					traces = req.ResourceSpans
					return &coltracepb.ExportTraceServiceResponse{}, nil
				}},
				logClient: &mockLogClient{exportFunc: func(_ context.Context, req *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
					logs = req.ResourceLogs
					return &collogspb.ExportLogsServiceResponse{}, nil
				}},
				metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
					metrics = req.ResourceMetrics
					return &colmetricpb.ExportMetricsServiceResponse{}, nil
				}},
			}
			post := func(path string, request proto.Message, handler func(http.ResponseWriter, *http.Request)) {
				t.Helper()
				body, err := proto.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				handler(response, otlpHTTPRequest(path, bytes.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("%s status %d: %s", path, response.Code, response.Body.String())
				}
			}
			receiver := &Receiver{handler: p.handleSpans, logHandler: p.handleLogs, metricHandler: p.handleMetrics}
			nested := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: claudeIdentityAttributes()}}}
			traceRequest := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
				Resource: &resourcepb.Resource{Attributes: claudeIdentityAttributes()},
				ScopeSpans: []*tracepb.ScopeSpans{{
					Scope: &commonpb.InstrumentationScope{Name: "claude.scope", Attributes: claudeIdentityAttributes()},
					Spans: []*tracepb.Span{{
						Name: "claude.safe", Attributes: claudeIdentityAttributes(),
						Events: []*tracepb.Span_Event{{Name: "claude.event", Attributes: append(claudeIdentityAttributes(), &commonpb.KeyValue{Key: "nested", Value: nested})}},
						Links:  []*tracepb.Span_Link{{Attributes: claudeIdentityAttributes()}},
					}},
				}},
			}}}
			post("/v1/traces", traceRequest, receiver.handleHTTPTraces)
			if len(traces) != 1 || len(traces[0].ScopeSpans) != 1 || len(traces[0].ScopeSpans[0].Spans) != 1 {
				t.Fatalf("trace egress = %#v", traces)
			}
			span := traces[0].ScopeSpans[0].Spans[0]
			for _, attrs := range [][]*commonpb.KeyValue{traces[0].Resource.Attributes, traces[0].ScopeSpans[0].Scope.Attributes, span.Attributes, span.Events[0].Attributes, span.Events[0].Attributes[len(span.Events[0].Attributes)-1].Value.GetKvlistValue().Values, span.Links[0].Attributes} {
				assertClaudeIdentityPolicy(t, attrs)
			}

			logBody := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{nested}}}}
			logRequest := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: claudeIdentityAttributes()},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      &commonpb.InstrumentationScope{Name: "claude.scope", Attributes: claudeIdentityAttributes()},
					LogRecords: []*logspb.LogRecord{{EventName: "claude.safe", Attributes: claudeIdentityAttributes(), Body: logBody}},
				}},
			}}}
			post("/v1/logs", logRequest, receiver.handleHTTPLogs)
			if len(logs) != 1 || len(logs[0].ScopeLogs) != 1 || len(logs[0].ScopeLogs[0].LogRecords) != 1 {
				t.Fatalf("log egress = %#v", logs)
			}
			record := logs[0].ScopeLogs[0].LogRecords[0]
			for _, attrs := range [][]*commonpb.KeyValue{logs[0].Resource.Attributes, logs[0].ScopeLogs[0].Scope.Attributes, record.Attributes, record.Body.GetArrayValue().Values[0].GetKvlistValue().Values} {
				assertClaudeIdentityPolicy(t, attrs)
			}

			metricRequest := &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{
				Resource: &resourcepb.Resource{Attributes: claudeIdentityAttributes()},
				ScopeMetrics: []*metricpb.ScopeMetrics{{
					Scope: &commonpb.InstrumentationScope{Name: "claude.scope", Attributes: claudeIdentityAttributes()},
					Metrics: []*metricpb.Metric{{
						Name: "claude.safe.metric",
						Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricpb.NumberDataPoint{{
							StartTimeUnixNano: 1, TimeUnixNano: 2, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}, Attributes: claudeIdentityAttributes(), Exemplars: []*metricpb.Exemplar{{FilteredAttributes: claudeIdentityAttributes()}},
						}}}},
					}},
				}},
			}}}
			post("/v1/metrics", metricRequest, receiver.handleHTTPMetrics)
			p.flushMetricBuffer(context.Background(), true)
			if len(metrics) != 1 || len(metrics[0].ScopeMetrics) != 1 || len(metrics[0].ScopeMetrics[0].Metrics) != 1 {
				t.Fatalf("metric egress = %#v", metrics)
			}
			point := metrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
			for _, attrs := range [][]*commonpb.KeyValue{metrics[0].Resource.Attributes, metrics[0].ScopeMetrics[0].Scope.Attributes, point.Attributes, point.Exemplars[0].FilteredAttributes} {
				assertClaudeIdentityPolicy(t, attrs)
			}
		})
	}
}
