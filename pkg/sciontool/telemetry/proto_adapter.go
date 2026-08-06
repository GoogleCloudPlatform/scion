/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"encoding/hex"
	"time"

	"cloud.google.com/go/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// protoResourceSpansToSDK converts OTLP proto ResourceSpans to SDK ReadOnlySpan slices
// using tracetest.SpanStub as the adapter (the public API for creating ReadOnlySpan
// from external data).
func protoResourceSpansToSDK(resourceSpans []*tracepb.ResourceSpans) []sdktrace.ReadOnlySpan {
	var result []sdktrace.ReadOnlySpan
	for _, rs := range resourceSpans {
		res := protoResourceToSDK(rs.Resource)
		for _, ss := range rs.ScopeSpans {
			scope := protoScopeToSDK(ss.Scope)
			for _, span := range ss.Spans {
				stub := protoSpanToStub(span, res, scope)
				result = append(result, stub.Snapshot())
			}
		}
	}
	return result
}

// protoSpanToStub converts an OTLP proto Span to a tracetest.SpanStub.
func protoSpanToStub(span *tracepb.Span, res *resource.Resource, scope instrumentation.Scope) tracetest.SpanStub {
	return tracetest.SpanStub{
		Name:                   span.Name,
		SpanContext:            protoSpanContext(span.TraceId, span.SpanId, span.Flags),
		Parent:                 protoParentSpanContext(span.TraceId, span.ParentSpanId),
		SpanKind:               protoSpanKindToSDK(span.Kind),
		StartTime:              time.Unix(0, int64(span.StartTimeUnixNano)),
		EndTime:                time.Unix(0, int64(span.EndTimeUnixNano)),
		Attributes:             protoAttrsToSDK(span.Attributes),
		Events:                 protoEventsToSDK(span.Events),
		Links:                  protoLinksToSDK(span.Links),
		Status:                 protoStatusToSDK(span.Status),
		DroppedAttributes:      int(span.DroppedAttributesCount),
		DroppedEvents:          int(span.DroppedEventsCount),
		DroppedLinks:           int(span.DroppedLinksCount),
		Resource:               res,
		InstrumentationLibrary: scope,
	}
}

// --- Span context helpers ---

func protoSpanContext(traceID, spanID []byte, flags uint32) trace.SpanContext {
	var tid trace.TraceID
	var sid trace.SpanID
	if len(traceID) == 16 {
		copy(tid[:], traceID)
	}
	if len(spanID) == 8 {
		copy(sid[:], spanID)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.TraceFlags(flags & 0xFF),
		Remote:     true,
	})
}

func protoParentSpanContext(traceID, parentSpanID []byte) trace.SpanContext {
	if len(parentSpanID) == 0 {
		return trace.SpanContext{}
	}
	return protoSpanContext(traceID, parentSpanID, 0)
}

// --- Kind/Status/Events/Links conversion ---

func protoSpanKindToSDK(k tracepb.Span_SpanKind) trace.SpanKind {
	switch k {
	case tracepb.Span_SPAN_KIND_CLIENT:
		return trace.SpanKindClient
	case tracepb.Span_SPAN_KIND_SERVER:
		return trace.SpanKindServer
	case tracepb.Span_SPAN_KIND_PRODUCER:
		return trace.SpanKindProducer
	case tracepb.Span_SPAN_KIND_CONSUMER:
		return trace.SpanKindConsumer
	case tracepb.Span_SPAN_KIND_INTERNAL:
		return trace.SpanKindInternal
	default:
		return trace.SpanKindUnspecified
	}
}

func protoStatusToSDK(s *tracepb.Status) sdktrace.Status {
	if s == nil {
		return sdktrace.Status{Code: codes.Unset}
	}
	var code codes.Code
	switch s.Code {
	case tracepb.Status_STATUS_CODE_OK:
		code = codes.Ok
	case tracepb.Status_STATUS_CODE_ERROR:
		code = codes.Error
	default:
		code = codes.Unset
	}
	return sdktrace.Status{
		Code:        code,
		Description: s.Message,
	}
}

func protoEventsToSDK(events []*tracepb.Span_Event) []sdktrace.Event {
	result := make([]sdktrace.Event, 0, len(events))
	for _, e := range events {
		result = append(result, sdktrace.Event{
			Name:       e.Name,
			Time:       time.Unix(0, int64(e.TimeUnixNano)),
			Attributes: protoAttrsToSDK(e.Attributes),
		})
	}
	return result
}

func protoLinksToSDK(links []*tracepb.Span_Link) []sdktrace.Link {
	result := make([]sdktrace.Link, 0, len(links))
	for _, l := range links {
		result = append(result, sdktrace.Link{
			SpanContext: protoSpanContext(l.TraceId, l.SpanId, l.Flags),
			Attributes:  protoAttrsToSDK(l.Attributes),
		})
	}
	return result
}

// --- Attribute conversion ---

func protoAttrsToSDK(attrs []*commonpb.KeyValue) []attribute.KeyValue {
	result := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if kv.Value == nil {
			continue
		}
		result = append(result, protoKVToSDK(kv))
	}
	return result
}

func protoKVToSDK(kv *commonpb.KeyValue) attribute.KeyValue {
	key := attribute.Key(kv.Key)
	if kv.Value == nil {
		return key.String("")
	}
	switch v := kv.Value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return key.String(v.StringValue)
	case *commonpb.AnyValue_BoolValue:
		return key.Bool(v.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return key.Int64(v.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return key.Float64(v.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return key.String(hex.EncodeToString(v.BytesValue))
	case *commonpb.AnyValue_ArrayValue:
		if v.ArrayValue == nil || len(v.ArrayValue.Values) == 0 {
			return key.StringSlice(nil)
		}
		strs := make([]string, 0, len(v.ArrayValue.Values))
		for _, av := range v.ArrayValue.Values {
			strs = append(strs, anyValueToString(av))
		}
		return key.StringSlice(strs)
	default:
		return key.String("<unsupported>")
	}
}

func anyValueToString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		if val.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return attribute.Int64Value(val.IntValue).String()
	case *commonpb.AnyValue_DoubleValue:
		return attribute.Float64Value(val.DoubleValue).String()
	default:
		return "<complex>"
	}
}

// --- Resource/Scope conversion ---

func protoResourceToSDK(r *resourcepb.Resource) *resource.Resource {
	if r == nil {
		return resource.Empty()
	}
	attrs := protoAttrsToSDK(r.Attributes)
	res, _ := resource.New(
		context.Background(),
		resource.WithAttributes(attrs...),
	)
	if res == nil {
		return resource.Empty()
	}
	return res
}

func protoScopeToSDK(s *commonpb.InstrumentationScope) instrumentation.Scope {
	if s == nil {
		return instrumentation.Scope{}
	}
	return instrumentation.Scope{
		Name:    s.Name,
		Version: s.Version,
	}
}

// --- Log conversion ---

// protoLogToCloudEntry converts an OTLP log record to a Cloud Logging entry.
func protoLogToCloudEntry(lr *logspb.LogRecord, res *resourcepb.Resource) logging.Entry {
	entry := logging.Entry{
		Severity: otlpSeverityToCloud(lr.SeverityNumber),
	}

	if lr.TimeUnixNano > 0 {
		entry.Timestamp = time.Unix(0, int64(lr.TimeUnixNano))
	} else if lr.ObservedTimeUnixNano > 0 {
		entry.Timestamp = time.Unix(0, int64(lr.ObservedTimeUnixNano))
	}

	payload := make(map[string]interface{})
	if lr.Body != nil {
		payload["message"] = anyValueToString(lr.Body)
	}

	for _, kv := range lr.Attributes {
		if kv.Value != nil {
			payload[kv.Key] = anyValueToString(kv.Value)
		}
	}

	labels := make(map[string]string)
	projectID := ""
	if res != nil {
		for _, kv := range res.Attributes {
			if kv.Value != nil {
				val := anyValueToString(kv.Value)
				labels[kv.Key] = val
				if kv.Key == "gcp.project_id" && val != "" {
					projectID = val
				}
			}
		}
	}

	if len(lr.TraceId) == 16 {
		traceID := hex.EncodeToString(lr.TraceId)
		payload["trace_id"] = traceID
		if projectID != "" {
			entry.Trace = "projects/" + projectID + "/traces/" + traceID
		} else {
			entry.Trace = traceID
		}
		labels["appengine.googleapis.com/trace_id"] = traceID
	}
	if len(lr.SpanId) == 8 {
		payload["span_id"] = hex.EncodeToString(lr.SpanId)
	}

	entry.Payload = payload
	entry.Labels = labels

	return entry
}

func otlpSeverityToCloud(sev logspb.SeverityNumber) logging.Severity {
	switch {
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_TRACE4:
		return logging.Debug
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG4:
		return logging.Debug
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_INFO4:
		return logging.Info
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_WARN4:
		return logging.Warning
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_ERROR4:
		return logging.Error
	case sev <= logspb.SeverityNumber_SEVERITY_NUMBER_FATAL4:
		return logging.Critical
	default:
		return logging.Default
	}
}
