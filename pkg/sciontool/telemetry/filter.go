/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// nativeFieldAliases maps vendor and semantic-convention field names to the
// stable policy field configured by Scion. These aliases are intentionally
// finite: field policy is not arbitrary secret detection.
var nativeFieldAliases = map[string]string{
	"conversation.id":            "session_id",
	"gen_ai.prompt":              "prompt",
	"gen_ai.input.messages":      "prompt",
	"input.value":                "prompt",
	"gen_ai.completion":          "tool_output",
	"gen_ai.output.messages":     "tool_output",
	"output.value":               "tool_output",
	"output":                     "tool_output",
	"tool.input":                 "tool_input",
	"tool.call.arguments":        "tool_input",
	"gen_ai.tool.call.arguments": "tool_input",
	"tool.output":                "tool_output",
	"tool.call.result":           "tool_output",
	"gen_ai.tool.call.result":    "tool_output",
	"session.id":                 "session_id",
	"gen_ai.conversation.id":     "session_id",
}

const (
	maxAnyValueDepth = 32
	maxAnyValueNodes = 4096
)

var errAnyValueLimit = errors.New("OTLP value exceeds receiver complexity limits")

type pendingAnyValue struct {
	value *commonpb.AnyValue
	depth int
}

// anyValueValidator bounds traversal of untrusted OTLP values before a
// recursive clone or transformation is attempted. One validator is shared by
// a whole request so the node budget cannot be bypassed with many shallow
// values.
type anyValueValidator struct {
	nodes int
}

func newAnyValueValidator() *anyValueValidator {
	return &anyValueValidator{}
}

func (v *anyValueValidator) Validate(value *commonpb.AnyValue) error {
	stack := []pendingAnyValue{{value: value, depth: 1}}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.depth > maxAnyValueDepth {
			return errAnyValueLimit
		}
		v.nodes++
		if v.nodes > maxAnyValueNodes {
			return errAnyValueLimit
		}
		if current.value == nil {
			continue
		}
		switch typed := current.value.Value.(type) {
		case *commonpb.AnyValue_ArrayValue:
			if typed.ArrayValue == nil {
				continue
			}
			if len(typed.ArrayValue.Values) > maxAnyValueNodes-v.nodes {
				return errAnyValueLimit
			}
			for _, item := range typed.ArrayValue.Values {
				stack = append(stack, pendingAnyValue{value: item, depth: current.depth + 1})
			}
		case *commonpb.AnyValue_KvlistValue:
			if typed.KvlistValue == nil {
				continue
			}
			if len(typed.KvlistValue.Values) > maxAnyValueNodes-v.nodes {
				return errAnyValueLimit
			}
			for _, item := range typed.KvlistValue.Values {
				if item == nil {
					stack = append(stack, pendingAnyValue{depth: current.depth + 1})
					continue
				}
				stack = append(stack, pendingAnyValue{value: item.Value, depth: current.depth + 1})
			}
		}
	}
	return nil
}

func (v *anyValueValidator) ValidateAttributes(attrs []*commonpb.KeyValue) error {
	for _, attr := range attrs {
		if attr == nil {
			if err := v.Validate(nil); err != nil {
				return err
			}
			continue
		}
		if err := v.Validate(attr.Value); err != nil {
			return err
		}
	}
	return nil
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

// RedactStructuredProtoValue preserves keyed structure while applying the
// supplied field context to scalar or byte values found in unkeyed arrays.
func (r *Redactor) RedactStructuredProtoValue(key string, value *commonpb.AnyValue) (*commonpb.AnyValue, error) {
	validator := newAnyValueValidator()
	if err := validator.Validate(value); err != nil {
		return nil, err
	}
	return r.redactProtoValueWithContext(key, value, true), nil
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
	// Callers outside receiver ingress do not have the request-wide admission
	// result; fail closed on an over-limit value before recursion or marshaling.
	if err := newAnyValueValidator().Validate(value); err != nil {
		return stringProtoValue("[REDACTED]")
	}
	return r.redactProtoValueWithContext(key, value, false)
}

func (r *Redactor) redactProtoValueWithContext(key string, value *commonpb.AnyValue, preserveContainer bool) *commonpb.AnyValue {
	if value == nil {
		return nil
	}
	_, arrayValue := value.Value.(*commonpb.AnyValue_ArrayValue)
	_, kvlistValue := value.Value.(*commonpb.AnyValue_KvlistValue)
	if !preserveContainer || (!arrayValue && !kvlistValue) {
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
	}
	switch typed := value.Value.(type) {
	case *commonpb.AnyValue_ArrayValue:
		if typed.ArrayValue == nil {
			return value
		}
		copyValue := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(typed.ArrayValue.Values))}
		for i, item := range typed.ArrayValue.Values {
			copyValue.Values[i] = r.redactProtoValueWithContext(key, item, true)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: copyValue}}
	case *commonpb.AnyValue_KvlistValue:
		if typed.KvlistValue == nil {
			return value
		}
		values := make([]*commonpb.KeyValue, len(typed.KvlistValue.Values))
		for i, item := range typed.KvlistValue.Values {
			if item != nil {
				values[i] = &commonpb.KeyValue{Key: item.Key, Value: r.redactProtoValueWithContext(item.Key, item.Value, false)}
			}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: values}}}
	default:
		return value
	}
}

func stringProtoValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

// RedactSpan applies redaction and hashing to a span's attributes. It returns
// nil when the span exceeds the value complexity budget, before cloning it.
func (r *Redactor) RedactSpan(span *tracepb.Span) *tracepb.Span {
	if r == nil || span == nil {
		return span
	}
	validator := newAnyValueValidator()
	if err := validator.ValidateAttributes(span.Attributes); err != nil {
		return nil
	}
	for _, event := range span.Events {
		if event != nil {
			if err := validator.ValidateAttributes(event.Attributes); err != nil {
				return nil
			}
		}
	}
	for _, link := range span.Links {
		if link != nil {
			if err := validator.ValidateAttributes(link.Attributes); err != nil {
				return nil
			}
		}
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
