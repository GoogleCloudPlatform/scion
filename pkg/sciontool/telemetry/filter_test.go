/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func TestFilter_ShouldProcess(t *testing.T) {
	tests := []struct {
		name      string
		config    FilterConfig
		eventType string
		expected  bool
	}{
		{
			name:      "nil filter allows all",
			config:    FilterConfig{},
			eventType: "any.event",
			expected:  true,
		},
		{
			name: "empty include allows all",
			config: FilterConfig{
				Include: []string{},
			},
			eventType: "any.event",
			expected:  true,
		},
		{
			name: "include list filters",
			config: FilterConfig{
				Include: []string{"event.a", "event.b"},
			},
			eventType: "event.a",
			expected:  true,
		},
		{
			name: "include list excludes non-matching",
			config: FilterConfig{
				Include: []string{"event.a", "event.b"},
			},
			eventType: "event.c",
			expected:  false,
		},
		{
			name: "exclude list filters",
			config: FilterConfig{
				Exclude: []string{"event.private"},
			},
			eventType: "event.private",
			expected:  false,
		},
		{
			name: "exclude list allows non-matching",
			config: FilterConfig{
				Exclude: []string{"event.private"},
			},
			eventType: "event.public",
			expected:  true,
		},
		{
			name: "exclude takes precedence over include",
			config: FilterConfig{
				Include: []string{"event.a", "event.b"},
				Exclude: []string{"event.b"},
			},
			eventType: "event.b",
			expected:  false,
		},
		{
			name: "include and exclude combined - allowed",
			config: FilterConfig{
				Include: []string{"event.a", "event.b", "event.c"},
				Exclude: []string{"event.b"},
			},
			eventType: "event.a",
			expected:  true,
		},
		{
			name: "default exclude list",
			config: FilterConfig{
				Exclude: DefaultFilterExclude,
			},
			eventType: "agent.user.prompt",
			expected:  false,
		},
		{
			name: "default exclude list allows other events",
			config: FilterConfig{
				Exclude: DefaultFilterExclude,
			},
			eventType: "agent.tool.invoke",
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFilter(tt.config)
			if got := f.ShouldProcess(tt.eventType); got != tt.expected {
				t.Errorf("ShouldProcess(%q) = %v, want %v", tt.eventType, got, tt.expected)
			}
		})
	}
}

func TestFilter_NilFilter(t *testing.T) {
	var f *Filter
	if !f.ShouldProcess("any.event") {
		t.Error("nil filter should allow all events")
	}
}

func TestFilter_ShouldProcessSpan(t *testing.T) {
	f := NewFilter(FilterConfig{
		Exclude: []string{"private.span"},
	})

	if f.ShouldProcessSpan("private.span") {
		t.Error("ShouldProcessSpan should exclude private.span")
	}
	if !f.ShouldProcessSpan("public.span") {
		t.Error("ShouldProcessSpan should allow public.span")
	}
}

// Redactor tests

func TestNewRedactor(t *testing.T) {
	config := RedactionConfig{
		Redact: []string{"prompt", "user.email"},
		Hash:   []string{"session_id"},
	}

	r := NewRedactor(config)
	if r == nil {
		t.Fatal("NewRedactor returned nil")
	}

	if !r.ShouldRedact("prompt") {
		t.Error("ShouldRedact should return true for 'prompt'")
	}
	if !r.ShouldRedact("user.email") {
		t.Error("ShouldRedact should return true for 'user.email'")
	}
	if r.ShouldRedact("unknown") {
		t.Error("ShouldRedact should return false for 'unknown'")
	}

	if !r.ShouldHash("session_id") {
		t.Error("ShouldHash should return true for 'session_id'")
	}
	if r.ShouldHash("prompt") {
		t.Error("ShouldHash should return false for 'prompt'")
	}
}

func TestRedactor_NilSafe(t *testing.T) {
	var r *Redactor

	if r.ShouldRedact("anything") {
		t.Error("nil Redactor should not redact")
	}
	if r.ShouldHash("anything") {
		t.Error("nil Redactor should not hash")
	}

	// RedactProtoAttributes should return input unchanged
	attrs := []*commonpb.KeyValue{
		{Key: "test", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "value"}}},
	}
	result := r.RedactProtoAttributes(attrs)
	if len(result) != 1 || result[0].Key != "test" {
		t.Error("nil Redactor should return attributes unchanged")
	}
}

func TestHashValue(t *testing.T) {
	// SHA256 of "test" = 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
	hash := HashValue("test")
	expected := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if hash != expected {
		t.Errorf("HashValue('test') = %s, want %s", hash, expected)
	}

	// Same input should always produce same output
	hash2 := HashValue("test")
	if hash != hash2 {
		t.Error("HashValue should be deterministic")
	}

	// Different input should produce different output
	hash3 := HashValue("test2")
	if hash == hash3 {
		t.Error("HashValue should produce different output for different input")
	}
}

func TestRedactor_RedactProtoAttributes(t *testing.T) {
	r := NewRedactor(RedactionConfig{
		Redact: []string{"secret"},
		Hash:   []string{"id"},
	})

	attrs := []*commonpb.KeyValue{
		{Key: "secret", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "my-secret"}}},
		{Key: "id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "session-123"}}},
		{Key: "public", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}},
	}

	result := r.RedactProtoAttributes(attrs)

	// Check secret is redacted
	if getStringValue(result[0]) != "[REDACTED]" {
		t.Errorf("secret should be redacted, got %s", getStringValue(result[0]))
	}

	// Check id is hashed (not the original value and not [REDACTED])
	idValue := getStringValue(result[1])
	if idValue == "session-123" {
		t.Error("id should be hashed, not original")
	}
	if idValue == "[REDACTED]" {
		t.Error("id should be hashed, not redacted")
	}
	// Verify it's a valid SHA256 hash (64 hex chars)
	if len(idValue) != 64 {
		t.Errorf("hashed id should be 64 chars, got %d", len(idValue))
	}

	// Check public is unchanged
	if getStringValue(result[2]) != "hello" {
		t.Errorf("public should be unchanged, got %s", getStringValue(result[2]))
	}
}

func TestRedactor_RecursivelyAppliesNativeContentAliases(t *testing.T) {
	r := NewRedactor(RedactionConfig{
		Redact: []string{"prompt", "tool_output"},
		Hash:   []string{"session_id"},
	})

	attrs := []*commonpb.KeyValue{
		{Key: "gen_ai.input.messages", Value: stringValue("secret prompt")},
		{Key: "conversation.id", Value: stringValue("current-session")},
		{Key: "output", Value: stringValue("current output")},
		{Key: "nested", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
			{Key: "output.value", Value: stringValue("secret output")},
			{Key: "gen_ai.conversation.id", Value: stringValue("session-123")},
		}}}}},
	}

	result := r.RedactProtoAttributes(attrs)
	if got := getStringValue(result[0]); got != "[REDACTED]" {
		t.Fatalf("gen_ai.input.messages = %q, want redacted", got)
	}
	if got := getStringValue(result[1]); got != HashValue("current-session") {
		t.Fatalf("conversation.id = %q, want one SHA-256 hash", got)
	}
	if got := getStringValue(result[2]); got != "[REDACTED]" {
		t.Fatalf("output = %q, want redacted", got)
	}
	nested := result[3].Value.GetKvlistValue().Values
	if got := getStringValue(nested[0]); got != "[REDACTED]" {
		t.Fatalf("output.value = %q, want redacted", got)
	}
	if got := getStringValue(nested[1]); got != HashValue("session-123") {
		t.Fatalf("gen_ai.conversation.id = %q, want one SHA-256 hash", got)
	}
}

func TestRedactor_StructuredBodyKeepsContextForArrayScalars(t *testing.T) {
	r := NewRedactor(RedactionConfig{Redact: []string{"log.body", "prompt"}})
	body := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
		stringValue("UNKEYED_SECRET"),
		{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("BYTE_SECRET")}},
		{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
			{Key: "prompt", Value: stringValue("KEYED_SECRET")},
			{Key: "public", Value: stringValue("visible")},
		}}}},
	}}}}

	got, err := r.RedactStructuredProtoValue("log.body", body)
	if err != nil {
		t.Fatalf("RedactStructuredProtoValue: %v", err)
	}
	values := got.GetArrayValue().Values
	if values[0].GetStringValue() != "[REDACTED]" || values[1].GetStringValue() != "[REDACTED]" {
		t.Fatalf("unkeyed body values were not redacted: %#v", values)
	}
	nested := values[2].GetKvlistValue().Values
	if getStringValue(nested[0]) != "[REDACTED]" || getStringValue(nested[1]) != "visible" {
		t.Fatalf("keyed body values = %#v", nested)
	}
}

func TestAnyValueValidator_Bounds(t *testing.T) {
	atLimit := stringValue("leaf")
	for range maxAnyValueDepth - 1 {
		atLimit = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{atLimit}}}}
	}
	if err := newAnyValueValidator().Validate(atLimit); err != nil {
		t.Fatalf("depth at limit rejected: %v", err)
	}
	overDepth := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{atLimit}}}}
	if err := newAnyValueValidator().Validate(overDepth); err == nil {
		t.Fatal("depth over limit accepted")
	}

	wide := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, maxAnyValueNodes-1)}}}
	if err := newAnyValueValidator().Validate(wide); err != nil {
		t.Fatalf("node count at limit rejected: %v", err)
	}
	wide.GetArrayValue().Values = append(wide.GetArrayValue().Values, nil)
	if err := newAnyValueValidator().Validate(wide); err == nil {
		t.Fatal("node count over limit accepted")
	}
	wideMap := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: make([]*commonpb.KeyValue, maxAnyValueNodes-1)}}}
	if err := newAnyValueValidator().Validate(wideMap); err != nil {
		t.Fatalf("map width at limit rejected: %v", err)
	}
	wideMap.GetKvlistValue().Values = append(wideMap.GetKvlistValue().Values, &commonpb.KeyValue{Value: stringValue("over")})
	if err := newAnyValueValidator().Validate(wideMap); err == nil {
		t.Fatal("map width over limit accepted")
	}

	for name, value := range map[string]*commonpb.AnyValue{
		"nil":    nil,
		"string": stringValue("ok"),
		"bytes":  {Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("ok")}},
		"bool":   {Value: &commonpb.AnyValue_BoolValue{BoolValue: true}},
		"int":    {Value: &commonpb.AnyValue_IntValue{IntValue: 7}},
		"double": {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.25}},
		"map":    {Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{Key: "value", Value: stringValue("ok")}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := newAnyValueValidator().Validate(value); err != nil {
				t.Fatalf("valid typed value rejected: %v", err)
			}
		})
	}
}

func stringValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

func getStringValue(kv *commonpb.KeyValue) string {
	if sv, ok := kv.Value.Value.(*commonpb.AnyValue_StringValue); ok {
		return sv.StringValue
	}
	return ""
}

func TestRedactor_DefaultFields(t *testing.T) {
	// Test that default fields are configured correctly
	if len(DefaultRedactFields) == 0 {
		t.Error("DefaultRedactFields should not be empty")
	}
	if len(DefaultHashFields) == 0 {
		t.Error("DefaultHashFields should not be empty")
	}

	// Check expected defaults
	foundPrompt := false
	for _, f := range DefaultRedactFields {
		if f == "prompt" {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Error("DefaultRedactFields should contain 'prompt'")
	}
	for _, field := range []string{"log.body", "span.status.message"} {
		found := false
		for _, configured := range DefaultRedactFields {
			if configured == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultRedactFields should contain %q", field)
		}
	}

	foundSessionID := false
	for _, f := range DefaultHashFields {
		if f == "session_id" {
			foundSessionID = true
			break
		}
	}
	if !foundSessionID {
		t.Error("DefaultHashFields should contain 'session_id'")
	}
}
