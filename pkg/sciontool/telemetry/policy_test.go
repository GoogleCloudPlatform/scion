/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
					StartTimeUnixNano: 1,
					TimeUnixNano:      2,
					Value:             &metricpb.NumberDataPoint_AsInt{AsInt: 7},
					Attributes:        append(secretAttr("point.secret", "POINT_MARKER"), secretKV("session_id", "metric-session-raw")),
					Exemplars:         []*metricpb.Exemplar{{FilteredAttributes: secretAttr("exemplar.secret", "EXEMPLAR_MARKER")}},
				}}, AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE}},
			}},
		}},
	}}
	if err := p.handleMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("handleMetrics: %v", err)
	}
	// The stream state is a local sink and must contain only processed data.
	for _, stream := range p.metricStreams.streams {
		assertRedacted(t, stream.metric.GetSum().DataPoints[0].Attributes, "point.secret")
	}
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
	if got := logs.Data[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue(); got != "[REDACTED]" {
		t.Fatalf("unstructured body = %q, want redacted", got)
	}
	spans := policy.processSpans([]*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
		Name:   "allowed.event",
		Status: &tracepb.Status{Message: "STATUS_MARKER"},
	}}}}}})
	if got := spans.Data[0].ScopeSpans[0].Spans[0].Status.Message; got != "[REDACTED]" {
		t.Fatalf("status message = %q, want redacted", got)
	}
}

func TestReceiverPolicy_StructuredBodyAndPinnedAliasesSafeAtBothDestinations(t *testing.T) {
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{
		Redact: []string{"log.body", "tool_output"},
		Hash:   []string{"session_id"},
	}})
	body := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
		stringValue("UNKEYED_MARKER"),
		{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("BYTE_MARKER")}},
	}}}}
	result := policy.processLogs([]*logspb.ResourceLogs{{
		SchemaUrl: "resource-schema",
		ScopeLogs: []*logspb.ScopeLogs{{
			SchemaUrl: "scope-schema",
			Scope: &commonpb.InstrumentationScope{Name: "scope", Attributes: []*commonpb.KeyValue{
				secretKV("scope.safe", "safe"),
			}},
			LogRecords: []*logspb.LogRecord{{
				EventName: "allowed.event",
				Attributes: []*commonpb.KeyValue{
					secretKV("conversation.id", "SESSION_MARKER"),
					secretKV("output", "OUTPUT_MARKER"),
				},
				Body: body,
			}},
		}},
	}})
	if result.Rejected != 0 || len(result.Data) != 1 {
		t.Fatalf("policy result = %#v", result)
	}
	record := result.Data[0].ScopeLogs[0].LogRecords[0]
	genericText := record.String()
	for _, marker := range []string{"UNKEYED_MARKER", "BYTE_MARKER", "SESSION_MARKER", "OUTPUT_MARKER"} {
		if strings.Contains(genericText, marker) {
			t.Fatalf("generic OTLP destination leaked %q: %s", marker, genericText)
		}
	}
	entry := protoLogToCloudEntry(record, result.Data[0].Resource, result.Data[0].SchemaUrl, result.Data[0].ScopeLogs[0].Scope, result.Data[0].ScopeLogs[0].SchemaUrl)
	gcpText := entry.Payload.(map[string]interface{})
	for _, marker := range []string{"UNKEYED_MARKER", "BYTE_MARKER", "SESSION_MARKER", "OUTPUT_MARKER"} {
		if strings.Contains(fmt.Sprint(gcpText), marker) {
			t.Fatalf("GCP-shaped destination leaked %q: %#v", marker, gcpText)
		}
	}
}

func TestPipeline_F1MarkersSafeAtGenericAndGCPLogDestinations(t *testing.T) {
	config := &Config{Enabled: true, Filter: FilterConfig{Include: []string{"allowed.event"}}, Redaction: RedactionConfig{
		Redact: []string{"log.body", "tool_output"},
		Hash:   []string{"session_id"},
	}}
	input := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
		EventName: "allowed.event",
		Attributes: []*commonpb.KeyValue{
			secretKV("control", "SAFE_CONTROL"),
			secretKV("conversation.id", "SESSION_MARKER"),
			secretKV("output", "OUTPUT_MARKER"),
		},
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
			stringValue("UNKEYED_MARKER"),
			{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("BYTE_MARKER")}},
		}}}},
	}}}}}}
	markers := []string{"UNKEYED_MARKER", "BYTE_MARKER", "SESSION_MARKER", "OUTPUT_MARKER"}

	var genericRequests []*collogspb.ExportLogsServiceRequest
	generic := NewWithConfig(config)
	generic.exporter = &CloudExporter{logClient: &mockLogClient{exportFunc: func(_ context.Context, req *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		genericRequests = append(genericRequests, req)
		return &collogspb.ExportLogsServiceResponse{}, nil
	}}}
	if err := generic.handleLogs(context.Background(), input); err != nil {
		t.Fatalf("generic handleLogs: %v", err)
	}
	if len(genericRequests) != 1 || len(genericRequests[0].ResourceLogs) != 1 || len(genericRequests[0].ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("generic destination did not receive one positive-control record: %#v", genericRequests)
	}
	genericRecord := genericRequests[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if attrValue(genericRecord.Attributes, "control") != "SAFE_CONTROL" || genericRecord.EventName != "allowed.event" {
		t.Fatalf("generic positive control missing: %#v", genericRecord)
	}
	if attrValue(genericRecord.Attributes, "conversation.id") != HashValue("SESSION_MARKER") || attrValue(genericRecord.Attributes, "output") != "[REDACTED]" {
		t.Fatalf("generic native aliases not processed: %#v", genericRecord.Attributes)
	}
	for _, value := range genericRecord.Body.GetArrayValue().Values {
		if value.GetStringValue() != "[REDACTED]" {
			t.Fatalf("generic body value not redacted: %#v", value)
		}
	}
	for _, marker := range markers {
		if strings.Contains(genericRequests[0].String(), marker) {
			t.Fatalf("generic destination leaked %q", marker)
		}
	}

	var gcpEntries []logging.Entry
	gcp := NewWithConfig(config)
	gcp.exporter = &CloudExporter{gcpExporter: &GCPExporter{logSink: func(entry logging.Entry) {
		gcpEntries = append(gcpEntries, entry)
	}}}
	if err := gcp.handleLogs(context.Background(), input); err != nil {
		t.Fatalf("GCP handleLogs: %v", err)
	}
	if len(gcpEntries) != 1 {
		t.Fatalf("GCP destination received %d records, want one positive control", len(gcpEntries))
	}
	payload, ok := gcpEntries[0].Payload.(map[string]interface{})
	if !ok || payload["control"] != "SAFE_CONTROL" || payload["event.name"] != "allowed.event" {
		t.Fatalf("GCP positive control missing: %#v", gcpEntries[0].Payload)
	}
	if payload["conversation.id"] != HashValue("SESSION_MARKER") || payload["output"] != "[REDACTED]" {
		t.Fatalf("GCP native aliases not processed: %#v", payload)
	}
	for _, marker := range markers {
		if strings.Contains(fmt.Sprint(payload), marker) {
			t.Fatalf("GCP destination leaked %q: %#v", marker, payload)
		}
	}
}

func TestReceiverPolicy_NormalizesEveryEventNameRepresentation(t *testing.T) {
	for _, storage := range []string{"record", "event.name", "event_name", "event.type"} {
		for native, canonical := range map[string]string{
			"codex.user_prompt":      "agent.user.prompt",
			"codex.tool_result":      "agent.tool.result",
			"gemini_cli.user_prompt": "agent.user.prompt",
		} {
			t.Run(storage+"/"+native, func(t *testing.T) {
				policy := newReceiverPolicy(&Config{Enabled: true, Filter: FilterConfig{Include: []string{canonical}}})
				record := &logspb.LogRecord{}
				if storage == "record" {
					record.EventName = native
				} else {
					record.Attributes = []*commonpb.KeyValue{secretKV(storage, native)}
				}
				result := policy.processLogs([]*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{record}}}}})
				if result.Rejected != 0 || len(result.Data) != 1 {
					t.Fatalf("normalized event rejected: %#v", result)
				}
				got := result.Data[0].ScopeLogs[0].LogRecords[0]
				if got.EventName != canonical || countAttr(got.Attributes, normalizedEventNameAttribute) != 1 || attrValue(got.Attributes, normalizedEventNameAttribute) != canonical {
					t.Fatalf("canonical record = %#v", got)
				}
				for _, legacy := range []string{"event_name", "event.type"} {
					if countAttr(got.Attributes, legacy) != 0 {
						t.Fatalf("legacy event alias %q retained", legacy)
					}
				}
			})
		}
	}
}

func TestReceiverPolicy_RejectsConflictingAndTypedEventNames(t *testing.T) {
	tests := map[string]*logspb.LogRecord{
		"record conflicts with alias": {EventName: "allowed.event", Attributes: []*commonpb.KeyValue{secretKV("event.name", "agent.user.prompt")}},
		"duplicate aliases conflict":  {Attributes: []*commonpb.KeyValue{secretKV("event_name", "allowed.event"), secretKV("event_name", "other.event")}},
		"typed alias":                 {Attributes: []*commonpb.KeyValue{{Key: "event.type", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}}},
	}
	for name, record := range tests {
		t.Run(name, func(t *testing.T) {
			policy := newReceiverPolicy(&Config{Enabled: true})
			result := policy.processLogs([]*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{record}}}}})
			if result.Rejected != 1 || len(result.Data) != 0 || result.Reason != policyAdmissionReason {
				t.Fatalf("conflict result = %#v", result)
			}
		})
	}
}

func TestReceiverPolicy_StripsSpoofedIdentityBeforeAuthoritativeReplacement(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "trusted-agent")
	t.Setenv("SCION_AGENT_SLUG", "")
	t.Setenv("SCION_PROJECT_ID", "trusted-project")
	t.Setenv("SCION_GROVE_ID", "legacy-project")
	t.Setenv("SCION_HARNESS", "trusted-harness")
	t.Setenv("SCION_MODEL", "")
	t.Setenv("SCION_BROKER_ID", "")
	t.Setenv("SCION_BROKER_NAME", "trusted-broker")
	policy := newReceiverPolicy(&Config{Enabled: true, Redaction: RedactionConfig{Redact: []string{"scion.agent.id", "scion.project.id", "scion.broker.name"}}})
	attrs := []*commonpb.KeyValue{
		secretKV("scion.agent.id", "spoof-1"),
		{Key: "scion.agent.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
		secretKV("scion.agent.slug", "spoof-slug"),
		secretKV("scion.project.id", "spoof-project"),
		secretKV(projectcompat.LegacyTelemetryIdentityKeys()[1], "spoof-project"),
		secretKV("scion.broker", "spoof-broker"),
		secretKV("scion.broker.id", "spoof-broker-id"),
		secretKV("scion.broker.name", "spoof-broker-name"),
		secretKV("scion.model", "spoof-model"),
	}
	result := policy.processSpans([]*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: attrs}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "safe.span"}}}}}})
	if result.Rejected != 0 || len(result.Data) != 1 {
		t.Fatalf("identity result = %#v", result)
	}
	got := result.Data[0].Resource.Attributes
	for key, want := range map[string]string{
		"scion.agent.id":    "trusted-agent",
		"scion.project.id":  "trusted-project",
		"scion.harness":     "trusted-harness",
		"scion.broker.name": "trusted-broker",
	} {
		if countAttr(got, key) != 1 || attrValue(got, key) != want {
			t.Fatalf("identity %q = %#v, want one %q", key, got, want)
		}
	}
	for _, absent := range []string{"scion.agent.slug", projectcompat.LegacyTelemetryIdentityKeys()[1], "scion.broker", "scion.broker.id", "scion.model"} {
		if countAttr(got, absent) != 0 {
			t.Fatalf("spoofed or legacy identity %q retained: %#v", absent, got)
		}
	}
}

func TestReceiverPolicy_InvalidInputIsProducerVisibleAndNeverForwarded(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true})
	forwarded := false
	p.exporter = &CloudExporter{logClient: &mockLogClient{exportFunc: func(_ context.Context, _ *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		forwarded = true
		return &collogspb.ExportLogsServiceResponse{}, nil
	}}}
	request := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "unsafe event name"}}}}}}}
	response, err := (&logsServiceServer{handler: p.handleLogs}).Export(context.Background(), request)
	if response != nil || status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != policyAdmissionReason {
		t.Fatalf("Export() = (%#v, %v), want bounded InvalidArgument", response, err)
	}
	if forwarded {
		t.Fatal("invalid request was partially forwarded")
	}
	if got := p.policyRejectedLogs.Load(); got != 1 {
		t.Fatalf("local rejected log count = %d, want 1", got)
	}
}

func TestReceiverPolicy_RejectsWholeRequestAcrossTransportsAndSignals(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true})
	forwarded := 0
	p.exporter = &CloudExporter{
		grpcClient: &mockTraceClient{exportFunc: func(context.Context, *coltracepb.ExportTraceServiceRequest, ...grpc.CallOption) (*coltracepb.ExportTraceServiceResponse, error) {
			forwarded++
			return &coltracepb.ExportTraceServiceResponse{}, nil
		}},
		logClient: &mockLogClient{exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
			forwarded++
			return &collogspb.ExportLogsServiceResponse{}, nil
		}},
	}
	traceRequest := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "safe.span"}, {Name: "unsafe span"},
	}}}}}}
	if response, err := (&traceServiceServer{handler: p.handleSpans}).Export(context.Background(), traceRequest); response != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("trace RPC response=%#v err=%v", response, err)
	}
	if got := p.policyRejectedSpans.Load(); got != 2 {
		t.Fatalf("trace rejection accounting = %d", got)
	}
	logsRequest := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{EventName: "safe.event"}, {EventName: "unsafe event"},
	}}}}}}
	if response, err := (&logsServiceServer{handler: p.handleLogs}).Export(context.Background(), logsRequest); response != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("logs RPC response=%#v err=%v", response, err)
	}
	receiver := &Receiver{handler: p.handleSpans}
	body, err := proto.Marshal(traceRequest)
	if err != nil {
		t.Fatal(err)
	}
	httpResponse := httptest.NewRecorder()
	receiver.handleHTTPTraces(httpResponse, httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body)))
	if httpResponse.Code != http.StatusBadRequest || strings.TrimSpace(httpResponse.Body.String()) != policyAdmissionReason {
		t.Fatalf("HTTP response = (%d, %q)", httpResponse.Code, httpResponse.Body.String())
	}
	if forwarded != 0 {
		t.Fatalf("invalid mixed requests forwarded %d batches", forwarded)
	}
	metricPoint := func(name string, value int64) *metricpb.Metric {
		return &metricpb.Metric{
			Name: name,
			Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{
				DataPoints: []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsInt{AsInt: value}}},
			}},
		}
	}
	metricRequest := &colmetricpb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricpb.ResourceMetrics{{
			ScopeMetrics: []*metricpb.ScopeMetrics{{
				Metrics: []*metricpb.Metric{metricPoint("safe.metric", 1), metricPoint("unsafe metric", 2)},
			}},
		}},
	}
	if response, err := (&metricsServiceServer{handler: p.handleMetrics}).Export(context.Background(), metricRequest); response != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("metrics RPC response=%#v err=%v", response, err)
	}
	if got := p.policyRejectedDataPoints.Load(); got != 2 || p.metricStreams != nil {
		t.Fatalf("metric rejection accounting=%d state=%v", got, p.metricStreams)
	}
	if got := p.policyRejectedRequests.Load(); got != 4 {
		t.Fatalf("rejected request count=%d, want 4 (trace RPC, log RPC, trace HTTP, metric RPC)", got)
	}
}

func TestReceiverPolicy_RejectsOverBudgetBeforeForwarding(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true})
	forwarded := false
	p.exporter = &CloudExporter{logClient: &mockLogClient{exportFunc: func(_ context.Context, _ *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		forwarded = true
		return &collogspb.ExportLogsServiceResponse{}, nil
	}}}
	body := stringValue("leaf")
	for range maxAnyValueDepth {
		body = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{body}}}}
	}
	err := p.handleLogs(context.Background(), []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "safe.event", Body: body}}}}}})
	if status.Code(err) != codes.InvalidArgument || forwarded {
		t.Fatalf("handleLogs() = %v, forwarded=%v", err, forwarded)
	}
}

func TestReceiverPolicy_RejectsUnsafeSpanEventsAndCrossAttributeBudgets(t *testing.T) {
	policy := newReceiverPolicy(&Config{Enabled: true})
	unsafeEvent := policy.processSpans([]*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
		Name: "safe.span", Events: []*tracepb.Span_Event{{Name: "unsafe event"}},
	}}}}}})
	if unsafeEvent.Rejected != 1 || len(unsafeEvent.Data) != 0 || unsafeEvent.Reason != policyAdmissionReason {
		t.Fatalf("unsafe span event result = %#v", unsafeEvent)
	}
	wideAttrs := make([]*commonpb.KeyValue, maxAnyValueNodes+1)
	for i := range wideAttrs {
		wideAttrs[i] = secretKV("public", "safe")
	}
	overResource := policy.processMetrics([]*metricpb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: wideAttrs},
		ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{
			Name: "safe.metric", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}}}}},
		}}}},
	}})
	if overResource.Rejected != 1 || len(overResource.Data) != 0 || overResource.Reason != policyAdmissionReason {
		t.Fatalf("over-budget metric resource result = %#v", overResource)
	}
	filtered := newReceiverPolicy(&Config{Enabled: true, Filter: FilterConfig{Exclude: []string{"safe.span"}}}).processSpans([]*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "safe.span"}}}}}})
	if filtered.Reason != "" || filtered.Rejected != 0 || filtered.Filtered != 1 || len(filtered.Data) != 0 {
		t.Fatalf("configured filtering confused with rejection: %#v", filtered)
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

func countAttr(attrs []*commonpb.KeyValue, key string) int {
	count := 0
	for _, attr := range attrs {
		if attr != nil && attr.Key == key {
			count++
		}
	}
	return count
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
