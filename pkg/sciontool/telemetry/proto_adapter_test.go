/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"cloud.google.com/go/logging"
)

func TestProtoResourceSpansToSDK(t *testing.T) {
	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	parentSpanID := []byte{8, 7, 6, 5, 4, 3, 2, 1}

	startNano := uint64(time.Now().Add(-time.Second).UnixNano())
	endNano := uint64(time.Now().UnixNano())

	resourceSpans := []*tracepb.ResourceSpans{
		{
			SchemaUrl: "https://example.test/resource-schema",
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"}}},
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{
				{
					SchemaUrl: "https://example.test/scope-schema",
					Scope: &commonpb.InstrumentationScope{
						Name:    "test-scope",
						Version: "1.0.0",
						Attributes: []*commonpb.KeyValue{
							{Key: "scope.safe", Value: stringValue("processed")},
						},
					},
					Spans: []*tracepb.Span{
						{
							TraceId:           traceID,
							SpanId:            spanID,
							ParentSpanId:      parentSpanID,
							Name:              "test-span",
							Kind:              tracepb.Span_SPAN_KIND_SERVER,
							StartTimeUnixNano: startNano,
							EndTimeUnixNano:   endNano,
							Attributes: []*commonpb.KeyValue{
								{Key: "http.method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
								{Key: "http.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
								{Key: "debug", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
							},
							Events: []*tracepb.Span_Event{
								{
									Name:         "event1",
									TimeUnixNano: startNano,
									Attributes: []*commonpb.KeyValue{
										{Key: "event.key", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "value"}}},
									},
								},
							},
							Status: &tracepb.Status{
								Code:    tracepb.Status_STATUS_CODE_OK,
								Message: "success",
							},
						},
					},
				},
			},
		},
	}

	sdkSpans := protoResourceSpansToSDK(resourceSpans)

	if len(sdkSpans) != 1 {
		t.Fatalf("Expected 1 SDK span, got %d", len(sdkSpans))
	}

	span := sdkSpans[0]

	// Verify name
	if span.Name() != "test-span" {
		t.Errorf("Name() = %q, want %q", span.Name(), "test-span")
	}

	// Verify span context
	sc := span.SpanContext()
	if !sc.TraceID().IsValid() {
		t.Error("Expected valid TraceID")
	}
	if !sc.SpanID().IsValid() {
		t.Error("Expected valid SpanID")
	}

	// Verify parent
	parent := span.Parent()
	if !parent.SpanID().IsValid() {
		t.Error("Expected valid parent SpanID")
	}

	// Verify kind
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("SpanKind() = %v, want %v", span.SpanKind(), trace.SpanKindServer)
	}

	// Verify attributes
	attrs := span.Attributes()
	if len(attrs) != 3 {
		t.Errorf("Expected 3 attributes, got %d", len(attrs))
	}

	// Verify events
	events := span.Events()
	if len(events) != 1 {
		t.Errorf("Expected 1 event, got %d", len(events))
	}

	// Verify status
	status := span.Status()
	if status.Code != codes.Ok {
		t.Errorf("Status.Code = %v, want %v", status.Code, codes.Ok)
	}
	if status.Description != "success" {
		t.Errorf("Status.Description = %q, want %q", status.Description, "success")
	}

	// Verify scope
	scope := span.InstrumentationScope()
	if scope.Name != "test-scope" {
		t.Errorf("InstrumentationScope.Name = %q, want %q", scope.Name, "test-scope")
	}
	if scope.Version != "1.0.0" {
		t.Errorf("InstrumentationScope.Version = %q, want %q", scope.Version, "1.0.0")
	}
	if scope.SchemaURL != "https://example.test/scope-schema" {
		t.Errorf("InstrumentationScope.SchemaURL = %q", scope.SchemaURL)
	}
	if got, ok := scope.Attributes.Value("scope.safe"); !ok || got.AsString() != "processed" {
		t.Errorf("InstrumentationScope.Attributes = %v", scope.Attributes)
	}
	if got := span.Resource().SchemaURL(); got != "https://example.test/resource-schema" {
		t.Errorf("Resource.SchemaURL() = %q", got)
	}
}

func TestProtoResourceSpansToSDK_Empty(t *testing.T) {
	sdkSpans := protoResourceSpansToSDK(nil)
	if len(sdkSpans) != 0 {
		t.Errorf("Expected empty result for nil input, got %d", len(sdkSpans))
	}

	sdkSpans = protoResourceSpansToSDK([]*tracepb.ResourceSpans{})
	if len(sdkSpans) != 0 {
		t.Errorf("Expected empty result for empty input, got %d", len(sdkSpans))
	}
}

func TestProtoSpanKindToSDK(t *testing.T) {
	tests := []struct {
		input    tracepb.Span_SpanKind
		expected trace.SpanKind
	}{
		{tracepb.Span_SPAN_KIND_CLIENT, trace.SpanKindClient},
		{tracepb.Span_SPAN_KIND_SERVER, trace.SpanKindServer},
		{tracepb.Span_SPAN_KIND_PRODUCER, trace.SpanKindProducer},
		{tracepb.Span_SPAN_KIND_CONSUMER, trace.SpanKindConsumer},
		{tracepb.Span_SPAN_KIND_INTERNAL, trace.SpanKindInternal},
		{tracepb.Span_SPAN_KIND_UNSPECIFIED, trace.SpanKindUnspecified},
	}

	for _, tt := range tests {
		got := protoSpanKindToSDK(tt.input)
		if got != tt.expected {
			t.Errorf("protoSpanKindToSDK(%v) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestProtoAttrsToSDK(t *testing.T) {
	attrs := []*commonpb.KeyValue{
		{Key: "str", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}},
		{Key: "int", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
		{Key: "bool", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
		{Key: "float", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 3.14}}},
		{Key: "nil_val", Value: nil}, // should be skipped
	}

	result := protoAttrsToSDK(attrs)

	if len(result) != 4 {
		t.Fatalf("Expected 4 attrs (nil skipped), got %d", len(result))
	}

	// Check string
	if result[0] != attribute.String("str", "hello") {
		t.Errorf("Expected String attr, got %v", result[0])
	}
	// Check int
	if result[1] != attribute.Int64("int", 42) {
		t.Errorf("Expected Int64 attr, got %v", result[1])
	}
	// Check bool
	if result[2] != attribute.Bool("bool", true) {
		t.Errorf("Expected Bool attr, got %v", result[2])
	}
}

func TestProtoStatusToSDK(t *testing.T) {
	tests := []struct {
		name     string
		input    *tracepb.Status
		wantCode codes.Code
		wantDesc string
	}{
		{"nil status", nil, codes.Unset, ""},
		{"ok", &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK, Message: "ok"}, codes.Ok, "ok"},
		{"error", &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "fail"}, codes.Error, "fail"},
		{"unset", &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET}, codes.Unset, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := protoStatusToSDK(tt.input)
			if got.Code != tt.wantCode {
				t.Errorf("Code = %v, want %v", got.Code, tt.wantCode)
			}
			if got.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", got.Description, tt.wantDesc)
			}
		})
	}
}

func TestProtoLogToCloudEntry(t *testing.T) {
	nowNano := uint64(time.Now().UnixNano())

	lr := &logspb.LogRecord{
		TimeUnixNano:   nowNano,
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "something failed"}},
		Attributes: []*commonpb.KeyValue{
			{Key: "component", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "auth"}}},
		},
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
	}

	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test"}}},
			{Key: "gcp.project_id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "deploy-demo-test"}}},
		},
	}

	scope := &commonpb.InstrumentationScope{
		Name:       "native.scope",
		Version:    "1.2.3",
		Attributes: []*commonpb.KeyValue{{Key: "scope.safe", Value: stringValue("processed")}},
	}
	entry := protoLogToCloudEntry(lr, res, "resource-schema", scope, "scope-schema")

	if entry.Severity != logging.Error {
		t.Errorf("Severity = %v, want %v", entry.Severity, logging.Error)
	}

	payload, ok := entry.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("Payload is not a map, got %T", entry.Payload)
	}

	if msg, ok := payload["message"]; !ok || msg != "something failed" {
		t.Errorf("Payload[message] = %v, want 'something failed'", msg)
	}

	if comp, ok := payload["component"]; !ok || comp != "auth" {
		t.Errorf("Payload[component] = %v, want 'auth'", comp)
	}
	metadata, ok := payload[cloudLoggingOTelMetadataKey].(map[string]interface{})
	if !ok {
		t.Fatalf("Payload[%q] = %#v", cloudLoggingOTelMetadataKey, payload[cloudLoggingOTelMetadataKey])
	}
	if metadata["resource_schema_url"] != "resource-schema" || metadata["scope_schema_url"] != "scope-schema" || metadata["scope_name"] != "native.scope" || metadata["scope_version"] != "1.2.3" {
		t.Fatalf("Cloud Logging OTel metadata = %#v", metadata)
	}
	if attrs, ok := metadata["scope_attributes"].(map[string]interface{}); !ok || attrs["scope.safe"] != "processed" {
		t.Fatalf("Cloud Logging scope attributes = %#v", metadata["scope_attributes"])
	}

	if _, ok := payload["trace_id"]; !ok {
		t.Error("Expected trace_id in payload")
	}
	if got, want := entry.Trace, "projects/deploy-demo-test/traces/0102030405060708090a0b0c0d0e0f10"; got != want {
		t.Errorf("Trace = %q, want %q", got, want)
	}
	if got, want := entry.Labels["appengine.googleapis.com/trace_id"], "0102030405060708090a0b0c0d0e0f10"; got != want {
		t.Errorf("Labels[appengine.googleapis.com/trace_id] = %q, want %q", got, want)
	}

	if entry.Labels["service.name"] != "test" {
		t.Errorf("Labels[service.name] = %q, want 'test'", entry.Labels["service.name"])
	}
}

func TestOtlpSeverityToCloud(t *testing.T) {
	tests := []struct {
		input    logspb.SeverityNumber
		expected logging.Severity
	}{
		{logspb.SeverityNumber_SEVERITY_NUMBER_TRACE, logging.Debug},
		{logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, logging.Debug},
		{logspb.SeverityNumber_SEVERITY_NUMBER_INFO, logging.Info},
		{logspb.SeverityNumber_SEVERITY_NUMBER_WARN, logging.Warning},
		{logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, logging.Error},
		{logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, logging.Critical},
	}

	for _, tt := range tests {
		got := otlpSeverityToCloud(tt.input)
		if got != tt.expected {
			t.Errorf("otlpSeverityToCloud(%v) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}
