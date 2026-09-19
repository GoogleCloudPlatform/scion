/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"crypto/sha256"
	"encoding/hex"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// nativeFieldAliases maps vendor and semantic-convention field names to the
// stable policy field configured by Scion. These aliases are intentionally
// finite: field policy is not arbitrary secret detection.
var nativeFieldAliases = map[string]string{
	"gen_ai.prompt":              "prompt",
	"gen_ai.input.messages":      "prompt",
	"input.value":                "prompt",
	"gen_ai.completion":          "tool_output",
	"gen_ai.output.messages":     "tool_output",
	"output.value":               "tool_output",
	"tool.input":                 "tool_input",
	"tool.call.arguments":        "tool_input",
	"gen_ai.tool.call.arguments": "tool_input",
	"tool.output":                "tool_output",
	"tool.call.result":           "tool_output",
	"gen_ai.tool.call.result":    "tool_output",
	"session.id":                 "session_id",
	"gen_ai.conversation.id":     "session_id",
}

// Filter provides include/exclude filtering for event types.
type Filter struct {
	include map[string]bool // nil = include all
	exclude map[string]bool
}

// RedactionConfig holds configuration for field-level redaction and hashing.
type RedactionConfig struct {
	// Redact is a list of field names to replace with "[REDACTED]"
	Redact []string
	// Hash is a list of field names to hash (SHA256)
	Hash []string
}

// Redactor provides field-level redaction and hashing for telemetry attributes.
type Redactor struct {
	redactFields map[string]bool
	hashFields   map[string]bool
}

// NewRedactor creates a new Redactor from configuration.
func NewRedactor(config RedactionConfig) *Redactor {
	r := &Redactor{
		redactFields: make(map[string]bool),
		hashFields:   make(map[string]bool),
	}

	for _, f := range config.Redact {
		r.redactFields[f] = true
	}
	for _, f := range config.Hash {
		r.hashFields[f] = true
	}

	return r
}

// ShouldRedact returns true if the field should be redacted.
func (r *Redactor) ShouldRedact(key string) bool {
	if r == nil {
		return false
	}
	return r.redactFields[key] || r.redactFields[nativeFieldAliases[key]]
}

// ShouldHash returns true if the field should be hashed.
func (r *Redactor) ShouldHash(key string) bool {
	if r == nil {
		return false
	}
	return r.hashFields[key] || r.hashFields[nativeFieldAliases[key]]
}

// HashValue returns the SHA256 hash of a value as a hex string.
func HashValue(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

// RedactProtoAttributes applies redaction and hashing to OTLP proto attributes.
func (r *Redactor) RedactProtoAttributes(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	if r == nil || len(attrs) == 0 {
		return attrs
	}

	result := make([]*commonpb.KeyValue, len(attrs))
	for i, kv := range attrs {
		result[i] = r.redactProtoKeyValue(kv)
	}
	return result
}

// redactProtoKeyValue redacts or hashes a single KeyValue.
func (r *Redactor) redactProtoKeyValue(kv *commonpb.KeyValue) *commonpb.KeyValue {
	if kv == nil {
		return nil
	}

	return &commonpb.KeyValue{Key: kv.Key, Value: r.RedactProtoValue(kv.Key, kv.Value)}
}

// RedactProtoValue recursively applies field policy inside OTLP arrays and
// key-value lists. The field key controls replacement of scalar values; nested
// key-value entries are evaluated using their own keys.
func (r *Redactor) RedactProtoValue(key string, value *commonpb.AnyValue) *commonpb.AnyValue {
	if r == nil || value == nil {
		return value
	}
	if r.ShouldRedact(key) {
		return stringProtoValue("[REDACTED]")
	}
	if r.ShouldHash(key) {
		if text, ok := value.Value.(*commonpb.AnyValue_StringValue); ok {
			return stringProtoValue(HashValue(text.StringValue))
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
		if err == nil {
			return stringProtoValue(HashValue(string(encoded)))
		}
		return stringProtoValue("[REDACTED]")
	}
	return r.redactProtoValue(value)
}

func (r *Redactor) redactProtoValue(value *commonpb.AnyValue) *commonpb.AnyValue {
	if value == nil {
		return nil
	}
	switch typed := value.Value.(type) {
	case *commonpb.AnyValue_ArrayValue:
		if typed.ArrayValue == nil {
			return value
		}
		copyValue := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(typed.ArrayValue.Values))}
		for i, item := range typed.ArrayValue.Values {
			copyValue.Values[i] = r.redactProtoValue(item)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: copyValue}}
	case *commonpb.AnyValue_KvlistValue:
		if typed.KvlistValue == nil {
			return value
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: r.RedactProtoAttributes(typed.KvlistValue.Values),
		}}}
	default:
		return value
	}
}

func stringProtoValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

// RedactSpan applies redaction and hashing to a span's attributes.
func (r *Redactor) RedactSpan(span *tracepb.Span) *tracepb.Span {
	if r == nil || span == nil {
		return span
	}

	redactedSpan := proto.Clone(span).(*tracepb.Span)
	redactedSpan.Attributes = r.RedactProtoAttributes(redactedSpan.Attributes)

	for _, event := range redactedSpan.Events {
		if event != nil {
			event.Attributes = r.RedactProtoAttributes(event.Attributes)
		}
	}
	for _, link := range redactedSpan.Links {
		if link != nil {
			link.Attributes = r.RedactProtoAttributes(link.Attributes)
		}
	}
	if redactedSpan.Status != nil && redactedSpan.Status.Message != "" {
		redactedSpan.Status.Message = r.RedactProtoValue("span.status.message", stringProtoValue(redactedSpan.Status.Message)).GetStringValue()
	}

	return redactedSpan
}

// NewFilter creates a new filter from configuration.
func NewFilter(config FilterConfig) *Filter {
	f := &Filter{}

	// Build include set (nil means include all)
	if len(config.Include) > 0 {
		f.include = make(map[string]bool, len(config.Include))
		for _, t := range config.Include {
			f.include[t] = true
		}
	}

	// Build exclude set
	if len(config.Exclude) > 0 {
		f.exclude = make(map[string]bool, len(config.Exclude))
		for _, t := range config.Exclude {
			f.exclude[t] = true
		}
	}

	return f
}

// ShouldProcess returns true if the event type should be processed.
// An event is processed if:
// 1. It's in the include list (or include list is empty, meaning include all)
// 2. AND it's not in the exclude list
func (f *Filter) ShouldProcess(eventType string) bool {
	if f == nil {
		return true
	}

	// Check include list first (nil = include all)
	if f.include != nil && !f.include[eventType] {
		return false
	}

	// Check exclude list
	if f.exclude != nil && f.exclude[eventType] {
		return false
	}

	return true
}

// ShouldProcessSpan checks if a span should be processed based on its name.
// This is a convenience method that treats span name as the event type.
func (f *Filter) ShouldProcessSpan(spanName string) bool {
	return f.ShouldProcess(spanName)
}

// HasIncludes reports whether event admission is restricted to named events.
func (f *Filter) HasIncludes() bool {
	return f != nil && f.include != nil
}
