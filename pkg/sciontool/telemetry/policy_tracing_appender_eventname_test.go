/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// These tests pin the callsite-default EventName exemption:
// opentelemetry-appender-tracing unconditionally sets LogRecord.EventName
// to the tracing callsite string ("event <file>:<line>") for a Rust
// tracing::event! with no explicit name: field, and codex-rs's
// log_event!/log_and_trace_event! macros never set one, so every real
// codex log record's EventName is this callsite default -- alongside the
// real event name carried separately as an event.name attribute. Without
// this exemption, normalizedLogEventName treats that pairing as
// "conflicting event name representations" and rejects the whole batch,
// so every codex native log record (not just usage) is dropped. See
// usage.go's codexUsageRule and TestPipelineDerivesCodexUsageThroughValidation
// in usage_codex_test.go for the end-to-end consequence.

// TestNormalizedLogEventName_AcceptsCallsiteDefaultWithAttribute is the
// accept case: a tracing-appender callsite EventName alongside a real
// event-name attribute is not a conflict, and the attribute wins.
func TestNormalizedLogEventName_AcceptsCallsiteDefaultWithAttribute(t *testing.T) {
	record := &logspb.LogRecord{
		EventName:  "event otel/src/events/session_telemetry.rs:1103",
		Attributes: []*commonpb.KeyValue{secretKV("event.name", "codex.sse_event")},
	}
	got, err := normalizedLogEventName(record, "")
	if err != nil {
		t.Fatalf("normalizedLogEventName error: %v", err)
	}
	if got != "codex.sse_event" {
		t.Fatalf("normalizedLogEventName = %q, want %q", got, "codex.sse_event")
	}
}

// TestNormalizedLogEventName_StillRejectsARealConflict is the still-reject
// case: an EventName that does not match the callsite-default shape stays
// a genuine conflict with a differing attribute, exactly as before this
// fix.
func TestNormalizedLogEventName_StillRejectsARealConflict(t *testing.T) {
	record := &logspb.LogRecord{
		EventName:  "a_real_event_name",
		Attributes: []*commonpb.KeyValue{secretKV("event.name", "codex.sse_event")},
	}
	if _, err := normalizedLogEventName(record, ""); err == nil {
		t.Fatal("expected a conflicting event name representations error")
	}
}

// TestNormalizedLogEventName_CallsiteDefaultAloneIsUnchanged is the no-op
// case: a callsite-default-shaped EventName with no
// event-name attribute at all has nothing to prefer over it, so it still
// participates as the (only) event name -- exactly today's behavior,
// unaffected by this fix. A record whose only "event name" is this kind of
// callsite string was already, and remains, treated as having that literal
// string as its event name.
func TestNormalizedLogEventName_CallsiteDefaultAloneIsUnchanged(t *testing.T) {
	record := &logspb.LogRecord{EventName: "event otel/src/events/session_telemetry.rs:1103"}
	got, err := normalizedLogEventName(record, "")
	if err != nil {
		t.Fatalf("normalizedLogEventName error: %v", err)
	}
	if got != "event otel/src/events/session_telemetry.rs:1103" {
		t.Fatalf("normalizedLogEventName = %q, want the EventName verbatim (no attribute to prefer)", got)
	}
}

// TestIsTracingAppenderCallsiteEventName pins the narrow pattern match
// itself: a "event " prefix and a ":<digits>" suffix, nothing looser.
func TestIsTracingAppenderCallsiteEventName(t *testing.T) {
	for name, want := range map[string]bool{
		"event otel/src/events/session_telemetry.rs:1103": true,
		"event a:0":              true,
		"api_request":            false,
		"event without a suffix": false,
		"event trailing-colon:":  false,
		"eventmissingspace:12":   false,
		"event line:12abc":       false,
	} {
		if got := isTracingAppenderCallsiteEventName(name); got != want {
			t.Errorf("isTracingAppenderCallsiteEventName(%q) = %v, want %v", name, got, want)
		}
	}
}
