/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
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
)

func TestReceiverPolicy_AllSignalsReachDestinationsProcessed(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "authoritative-agent")
	t.Setenv("SCION_AGENT_SLUG", "agent-slug")
	t.Setenv("SCION_PROJECT_ID", "canonical-project")
	t.Setenv("SCION_HARNESS", "synthetic-fixture")
	t.Setenv("SCION_BROKER_NAME", "broker-one")

	config := &Config{
		Enabled: true,
		Filter: FilterConfig{
			Include: []string{"allowed.event"},
			Exclude: []string{"excluded.event"},
		},
		Redaction: RedactionConfig{
			Redact: []string{
				"prompt", "tool_output", "resource.secret", "scope.secret",
				"record.secret", "event.secret", "link.secret", "point.secret",
				"exemplar.secret", "nested.secret", "log.body", "span.status.message",
			},
			Hash: []string{"session_id"},
		},
	}
	p := NewWithConfig(config)
	p.retryConfig = fastRetryConfig()

	var capturedSpans []*tracepb.ResourceSpans
	var capturedLogs []*logspb.ResourceLogs
	var capturedMetrics []*metricpb.ResourceMetrics
	p.exporter = &CloudExporter{
		grpcClient: &mockTraceClient{exportFunc: func(_ context.Context, request *coltracepb.ExportTraceServiceRequest, _ ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
			capturedSpans = request.ResourceSpans
			return &coltracepb.ExportTraceServiceResponse{}, nil
		}},
		logClient: &mockLogClient{exportFunc: func(_ context.Context, request *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
			capturedLogs = request.ResourceLogs
			return &collogspb.ExportLogsServiceResponse{}, nil
		}},
		metricClient: &mockMetricClient{exportFunc: func(_ context.Context, request *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
			capturedMetrics = request.ResourceMetrics
			return &colmetricpb.ExportMetricsServiceResponse{}, nil
		}},
	}

	spanResource := syntheticResource("native-trace-service")
	spans := []*tracepb.ResourceSpans{{
		Resource:  spanResource,
		SchemaUrl: "resource-schema",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: "native.scope", Version: "1.2.3", Attributes: secretAttr("scope.secret", "SCOPE_MARKER")},
			SchemaUrl: "scope-schema",
			Spans: []*tracepb.Span{
				{
					Name:       "allowed.event",
					Attributes: append(secretAttr("gen_ai.input.messages", "SPAN_MARKER"), secretKV("session_id", "session-raw")),
					Events:     []*tracepb.Span_Event{{Name: "safe.event", Attributes: secretAttr("event.secret", "EVENT_MARKER")}},
					Links:      []*tracepb.Span_Link{{Attributes: secretAttr("link.secret", "LINK_MARKER")}},
					Status:     &tracepb.Status{Message: "STATUS_MARKER"},
				},
				{Name: "excluded.event"},
				{Name: "unsafe user supplied name"},
			},
		}},
	}}
	if err := p.handleSpans(context.Background(), spans); err != nil {
		t.Fatalf("handleSpans: %v", err)
	}
	if len(capturedSpans) != 1 || len(capturedSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("captured spans = %#v, want one allowed safe span", capturedSpans)
	}
	gotSpan := capturedSpans[0].ScopeSpans[0].Spans[0]
	assertCommonIdentity(t, capturedSpans[0].Resource.Attributes, "native-trace-service")
	assertRedacted(t, capturedSpans[0].Resource.Attributes, "resource.secret")
	assertRedacted(t, capturedSpans[0].ScopeSpans[0].Scope.Attributes, "scope.secret")
	if scope := capturedSpans[0].ScopeSpans[0].Scope; scope.Name != "native.scope" || scope.Version != "1.2.3" {
		t.Fatalf("trace scope = %q@%q, want native.scope@1.2.3", scope.Name, scope.Version)
	}
	assertRedacted(t, gotSpan.Attributes, "gen_ai.input.messages")
	assertAttr(t, gotSpan.Attributes, "session_id", HashValue("session-raw"))
	assertRedacted(t, gotSpan.Events[0].Attributes, "event.secret")
	assertRedacted(t, gotSpan.Links[0].Attributes, "link.secret")
	if gotSpan.Status.Message != "[REDACTED]" {
		t.Fatalf("status message = %q, want redacted", gotSpan.Status.Message)
	}
	if capturedSpans[0].SchemaUrl != "resource-schema" || capturedSpans[0].ScopeSpans[0].SchemaUrl != "scope-schema" {
		t.Fatal("trace schema URLs were not preserved")
	}

	logs := []*logspb.ResourceLogs{{
		Resource:  syntheticResource("native-log-service"),
		SchemaUrl: "log-resource-schema",
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:     &commonpb.InstrumentationScope{Name: "native.log.scope", Version: "2.0", Attributes: secretAttr("scope.secret", "LOG_SCOPE_MARKER")},
			SchemaUrl: "log-scope-schema",
			LogRecords: []*logspb.LogRecord{
				{Attributes: append(secretAttr("record.secret", "RECORD_MARKER"), secretKV("event_name", "allowed.event"), secretKV("session_id", "log-session-raw")), Body: nestedBody("nested.secret", "BODY_MARKER")},
				{EventName: "excluded.event"},
				{Body: stringValue("UNNAMED_MARKER")},
			},
		}},
	}}
	if err := p.handleLogs(context.Background(), logs); err != nil {
		t.Fatalf("handleLogs: %v", err)
	}
	if len(capturedLogs) != 1 || len(capturedLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("captured logs = %#v, want one allowed named log", capturedLogs)
	}
	gotLog := capturedLogs[0].ScopeLogs[0].LogRecords[0]
	assertCommonIdentity(t, capturedLogs[0].Resource.Attributes, "native-log-service")
	if scope := capturedLogs[0].ScopeLogs[0].Scope; scope.Name != "native.log.scope" || scope.Version != "2.0" {
		t.Fatalf("log scope = %q@%q, want native.log.scope@2.0", scope.Name, scope.Version)
	}
	if capturedLogs[0].SchemaUrl != "log-resource-schema" || capturedLogs[0].ScopeLogs[0].SchemaUrl != "log-scope-schema" {
		t.Fatal("log schema URLs were not preserved")
	}
	assertRedacted(t, gotLog.Attributes, "record.secret")
	assertAttr(t, gotLog.Attributes, normalizedEventNameAttribute, "allowed.event")
	assertAttr(t, gotLog.Attributes, "session_id", HashValue("log-session-raw"))
	assertRedacted(t, gotLog.Body.GetKvlistValue().Values, "nested.secret")

	metrics := []*metricpb.ResourceMetrics{{
		Resource:  syntheticResource("native-metric-service"),
		SchemaUrl: "metric-resource-schema",
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope:     &commonpb.InstrumentationScope{Name: "native.metric.scope", Version: "3.0", Attributes: secretAttr("scope.secret", "METRIC_SCOPE_MARKER")},
			SchemaUrl: "metric-scope-schema",
			Metrics: []*metricpb.Metric{{
				Name: "synthetic.native.counter",
				Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{DataPoints: []*metricpb.NumberDataPoint{{
					Attributes: append(secretAttr("point.secret", "POINT_MARKER"), secretKV("session_id", "metric-session-raw")),
					Exemplars:  []*metricpb.Exemplar{{FilteredAttributes: secretAttr("exemplar.secret", "EXEMPLAR_MARKER")}},
				}}}},
			}},
		}},
	}}
	if err := p.handleMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("handleMetrics: %v", err)
	}
	// The in-memory buffer is a local sink and must contain only processed data.
	assertRedacted(t, p.metricBuf[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, "point.secret")
	p.flushMetricBuffer(context.Background(), true)
	if len(capturedMetrics) != 1 {
		t.Fatalf("captured metrics = %#v, want one batch despite event include policy", capturedMetrics)
	}
	gotMetric := capturedMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	assertCommonIdentity(t, capturedMetrics[0].Resource.Attributes, "native-metric-service")
	assertRedacted(t, capturedMetrics[0].ScopeMetrics[0].Scope.Attributes, "scope.secret")
	if scope := capturedMetrics[0].ScopeMetrics[0].Scope; scope.Name != "native.metric.scope" || scope.Version != "3.0" {
		t.Fatalf("metric scope = %q@%q, want native.metric.scope@3.0", scope.Name, scope.Version)
	}
	assertRedacted(t, gotMetric.Attributes, "point.secret")
	assertAttr(t, gotMetric.Attributes, "session_id", HashValue("metric-session-raw"))
	assertRedacted(t, gotMetric.Exemplars[0].FilteredAttributes, "exemplar.secret")
	if capturedMetrics[0].SchemaUrl != "metric-resource-schema" || capturedMetrics[0].ScopeMetrics[0].SchemaUrl != "metric-scope-schema" {
		t.Fatal("metric schema URLs were not preserved")
	}
}

func TestReceiverPolicy_HashesOnceAcrossRetries(t *testing.T) {
	config := &Config{Enabled: true, Redaction: RedactionConfig{Hash: []string{"session_id"}}}
	p := NewWithConfig(config)
	p.retryConfig = fastRetryConfig()
	var values []string
	p.exporter = &CloudExporter{grpcClient: &mockTraceClient{exportFunc: func(_ context.Context, request *coltracepb.ExportTraceServiceRequest, _ ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
		values = append(values, attrValue(request.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes, "session_id"))
		if len(values) == 1 {
			return nil, context.DeadlineExceeded
		}
		return &coltracepb.ExportTraceServiceResponse{}, nil
	}}}

	inputAttrs := append(
		secretAttr("session_id", "session-raw"),
		&commonpb.KeyValue{Key: "already_redacted", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
	)
	input := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
		Name:       "allowed.event",
		Attributes: inputAttrs,
	}}}}}}
	if err := p.handleSpans(context.Background(), input); err != nil {
		t.Fatalf("handleSpans: %v", err)
	}
	want := HashValue("session-raw")
	if len(values) != 2 || values[0] != want || values[1] != want {
		t.Fatalf("retry values = %v, want identical single hash %q", values, want)
	}
	if got := attrValue(input[0].ScopeSpans[0].Spans[0].Attributes, "session_id"); got != "session-raw" {
		t.Fatalf("input mutated to %q", got)
	}
}

func TestReceiverPolicy_DefaultContentFieldsProtectUnstructuredBodyAndStatus(t *testing.T) {
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{Redact: DefaultRedactFields}})
	logs := policy.processLogs([]*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
		EventName: "allowed.event",
		Body:      stringValue("UNSTRUCTURED_BODY_MARKER"),
	}}}}}})
	if got := logs[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue(); got != "[REDACTED]" {
		t.Fatalf("unstructured body = %q, want redacted", got)
	}
	spans := policy.processSpans([]*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
		Name:   "allowed.event",
		Status: &tracepb.Status{Message: "STATUS_MARKER"},
	}}}}}})
	if got := spans[0].ScopeSpans[0].Spans[0].Status.Message; got != "[REDACTED]" {
		t.Fatalf("status message = %q, want redacted", got)
	}
}

func syntheticResource(serviceName string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		secretKV("service.name", serviceName),
		secretKV("service.instance.id", "native-instance"),
		secretKV("scion.agent.id", "spoofed-agent"),
		secretKV("scion.project.id", "spoofed-project"),
		secretKV("resource.secret", "RESOURCE_MARKER"),
	}}
}

func nestedBody(key, value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: secretAttr(key, value)}}}
}

func secretAttr(key, value string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{secretKV(key, value)}
}

func secretKV(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: stringValue(value)}
}

func attrValue(attrs []*commonpb.KeyValue, key string) string {
	for _, attr := range attrs {
		if attr != nil && attr.Key == key && attr.Value != nil {
			return attr.Value.GetStringValue()
		}
	}
	return ""
}

func assertAttr(t *testing.T, attrs []*commonpb.KeyValue, key, want string) {
	t.Helper()
	if got := attrValue(attrs, key); got != want {
		t.Fatalf("attribute %q = %q, want %q", key, got, want)
	}
}

func assertRedacted(t *testing.T, attrs []*commonpb.KeyValue, key string) {
	t.Helper()
	assertAttr(t, attrs, key, "[REDACTED]")
}

func assertCommonIdentity(t *testing.T, attrs []*commonpb.KeyValue, serviceName string) {
	t.Helper()
	assertAttr(t, attrs, "service.name", serviceName)
	assertAttr(t, attrs, "service.instance.id", "native-instance")
	assertAttr(t, attrs, "scion.agent.id", "authoritative-agent")
	assertAttr(t, attrs, "scion.project.id", "canonical-project")
	assertAttr(t, attrs, "scion.harness", "synthetic-fixture")
	assertAttr(t, attrs, "scion.agent.slug", "agent-slug")
	assertAttr(t, attrs, "scion.broker.name", "broker-one")
}
