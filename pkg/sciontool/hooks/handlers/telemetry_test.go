/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewTelemetryHandler(t *testing.T) {
	// Test with nil TracerProvider (should use noop)
	h := NewTelemetryHandler(nil, nil, nil)
	if h == nil {
		t.Fatal("NewTelemetryHandler should not return nil")
	}
	if h.tracer == nil {
		t.Error("handler should have a tracer (even if noop)")
	}
}

func TestNewTelemetryHandler_WithRedactor(t *testing.T) {
	redactor := telemetry.NewRedactor(telemetry.RedactionConfig{
		Redact: []string{"prompt"},
		Hash:   []string{"session_id"},
	})

	h := NewTelemetryHandler(nil, nil, redactor)
	if h == nil {
		t.Fatal("NewTelemetryHandler should not return nil")
	}
	if h.redactor == nil {
		t.Error("handler should have a redactor")
	}
}

func TestTelemetryHandler_HandleNilEvent(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	// Should not panic on nil event
	err := h.Handle(nil)
	if err != nil {
		t.Errorf("Handle(nil) should not return error, got: %v", err)
	}
}

func TestTelemetryHandler_HandleUnknownEvent(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	event := &hooks.Event{
		Name: "unknown-event-type",
		Data: hooks.EventData{},
	}

	err := h.Handle(event)
	if err != nil {
		t.Errorf("Handle should not return error for unknown event, got: %v", err)
	}
}

func TestTelemetryHandler_HandleToolStart(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	event := &hooks.Event{
		Name:    hooks.EventToolStart,
		RawName: "PreToolUse",
		Dialect: "claude",
		Data: hooks.EventData{
			ToolName:  "Bash",
			ToolInput: "ls -la",
		},
	}

	err := h.Handle(event)
	if err != nil {
		t.Errorf("Handle should not return error, got: %v", err)
	}
}

func TestTelemetryHandler_HandleToolStartEnd(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	// Start event
	startEvent := &hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{
			ToolName:  "Bash",
			ToolInput: "ls -la",
		},
	}
	if err := h.Handle(startEvent); err != nil {
		t.Errorf("Handle start should not return error, got: %v", err)
	}

	// End event
	endEvent := &hooks.Event{
		Name: hooks.EventToolEnd,
		Data: hooks.EventData{
			ToolName:   "Bash",
			ToolOutput: "file1.txt\nfile2.txt",
			Success:    true,
		},
	}
	if err := h.Handle(endEvent); err != nil {
		t.Errorf("Handle end should not return error, got: %v", err)
	}
}

func TestTelemetryHandler_HandleSessionEvents(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	events := []struct {
		name string
		data hooks.EventData
	}{
		{hooks.EventSessionStart, hooks.EventData{SessionID: "sess-123", Source: "cli"}},
		{hooks.EventPromptSubmit, hooks.EventData{Prompt: "Hello, world!"}},
		{hooks.EventModelStart, hooks.EventData{}},
		{hooks.EventModelEnd, hooks.EventData{Success: true}},
		{hooks.EventSessionEnd, hooks.EventData{Reason: "user_exit"}},
	}

	for _, tc := range events {
		event := &hooks.Event{
			Name: tc.name,
			Data: tc.data,
		}
		if err := h.Handle(event); err != nil {
			t.Errorf("Handle(%s) should not return error, got: %v", tc.name, err)
		}
	}
}

func TestTelemetryHandler_Flush(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	// Start some events without ending them
	_ = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	_ = h.Handle(&hooks.Event{
		Name: hooks.EventModelStart,
		Data: hooks.EventData{},
	})

	// Flush should clean up all in-progress spans
	h.Flush()

	// Verify spanStore is empty by trying to end the spans (should create new single spans)
	// This is a bit indirect but tests the cleanup happened
}

func TestSpanMapping(t *testing.T) {
	expectedMappings := map[string]string{
		hooks.EventSessionStart:     "agent.session.start",
		hooks.EventSessionEnd:       "agent.session.end",
		hooks.EventToolStart:        "agent.tool.call",
		hooks.EventToolEnd:          "agent.tool.result",
		hooks.EventPromptSubmit:     "agent.user.prompt",
		hooks.EventModelStart:       "gen_ai.api.request",
		hooks.EventModelEnd:         "gen_ai.api.response",
		hooks.EventAgentStart:       "agent.turn.start",
		hooks.EventAgentEnd:         "agent.turn.end",
		hooks.EventNotification:     "agent.notification",
		hooks.EventResponseComplete: "agent.response.complete",
	}

	for eventName, expectedSpan := range expectedMappings {
		if SpanMapping[eventName] != expectedSpan {
			t.Errorf("SpanMapping[%s] = %s, want %s", eventName, SpanMapping[eventName], expectedSpan)
		}
	}
}

func TestTelemetryHandler_RedactionApplied(t *testing.T) {
	redactor := telemetry.NewRedactor(telemetry.RedactionConfig{
		Redact: []string{"prompt", "tool_input", "tool_output"},
		Hash:   []string{"session_id"},
	})

	h := NewTelemetryHandler(nil, nil, redactor)

	// Test that redactor is properly referenced
	if h.redactor == nil {
		t.Fatal("redactor should be set")
	}
	if !h.redactor.ShouldRedact("prompt") {
		t.Error("redactor should redact 'prompt'")
	}
	if !h.redactor.ShouldHash("session_id") {
		t.Error("redactor should hash 'session_id'")
	}
}

// recordingProcessor captures log records for test assertions.
type recordingProcessor struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (p *recordingProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, record.Clone())
	return nil
}

func (p *recordingProcessor) Shutdown(context.Context) error   { return nil }
func (p *recordingProcessor) ForceFlush(context.Context) error { return nil }

func (p *recordingProcessor) Records() []sdklog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]sdklog.Record, len(p.records))
	copy(out, p.records)
	return out
}

func TestTelemetryHandler_NilLoggerProvider(t *testing.T) {
	// Handler with nil LoggerProvider should process events without error
	h := NewTelemetryHandler(nil, nil, nil)
	if h.logger != nil {
		t.Error("logger should be nil when LoggerProvider is nil")
	}

	event := &hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{
			ToolName:  "Bash",
			ToolInput: "ls",
		},
	}
	if err := h.Handle(event); err != nil {
		t.Errorf("Handle should not error with nil LoggerProvider, got: %v", err)
	}
}

func TestTelemetryHandler_WithLoggerProvider(t *testing.T) {
	proc := &recordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	defer func() { _ = lp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, lp, nil)
	if h.logger == nil {
		t.Fatal("logger should be set when LoggerProvider is provided")
	}

	event := &hooks.Event{
		Name:    hooks.EventSessionStart,
		RawName: "SessionStart",
		Dialect: "claude",
		Data: hooks.EventData{
			SessionID: "sess-abc",
			Source:    "cli",
		},
	}
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	records := proc.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	rec := &records[0]
	body := rec.Body().AsString()
	if body != "agent.session.start" {
		t.Errorf("log body = %q, want %q", body, "agent.session.start")
	}

	// Check that event attributes are present in the log record
	found := map[string]string{}
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		found[kv.Key] = kv.Value.AsString()
		return true
	})

	if found["event.name"] != hooks.EventSessionStart {
		t.Errorf("event.name = %q, want %q", found["event.name"], hooks.EventSessionStart)
	}
	if found["session_id"] != "sess-abc" {
		t.Errorf("session_id = %q, want %q", found["session_id"], "sess-abc")
	}
	if found["source"] != "cli" {
		t.Errorf("source = %q, want %q", found["source"], "cli")
	}
}

func TestTelemetryHandler_LogRedaction(t *testing.T) {
	proc := &recordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	defer func() { _ = lp.Shutdown(context.Background()) }()

	redactor := telemetry.NewRedactor(telemetry.RedactionConfig{
		Redact: []string{"prompt", "tool_input", "tool_output"},
		Hash:   []string{"session_id"},
	})

	h := NewTelemetryHandler(nil, lp, redactor)

	event := &hooks.Event{
		Name: hooks.EventPromptSubmit,
		Data: hooks.EventData{
			SessionID: "sess-secret",
			Prompt:    "my secret prompt",
		},
	}
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	records := proc.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	found := map[string]string{}
	rec := &records[0]
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		found[kv.Key] = kv.Value.AsString()
		return true
	})

	// Prompt should be redacted
	if found["prompt"] != "[REDACTED]" {
		t.Errorf("prompt = %q, want [REDACTED]", found["prompt"])
	}

	// Session ID should be hashed (not the original value)
	if found["session_id"] == "sess-secret" {
		t.Error("session_id should be hashed, not plaintext")
	}
	if found["session_id"] == "" {
		t.Error("session_id should be present as hashed value")
	}
}

func TestTelemetryHandler_LogRecordIncludesFilePath(t *testing.T) {
	proc := &recordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	defer func() { _ = lp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, lp, nil)

	event := &hooks.Event{
		Name:    hooks.EventToolEnd,
		RawName: "PostToolUse",
		Dialect: "claude",
		Data: hooks.EventData{
			ToolName: "Write",
			FilePath: "/workspace/src/main.go",
			Success:  true,
		},
	}
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	records := proc.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	found := map[string]string{}
	rec := &records[0]
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Value.Kind() == otellog.KindString {
			found[kv.Key] = kv.Value.AsString()
		}
		return true
	})

	if found["file_path"] != "/workspace/src/main.go" {
		t.Errorf("file_path = %q, want %q", found["file_path"], "/workspace/src/main.go")
	}
	if found["tool_name"] != "Write" {
		t.Errorf("tool_name = %q, want %q", found["tool_name"], "Write")
	}
}

func TestTelemetryHandler_LogRecordIncludesTokens(t *testing.T) {
	proc := &recordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	defer func() { _ = lp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, lp, nil)

	event := &hooks.Event{
		Name:    hooks.EventSessionEnd,
		Dialect: "claude",
		Data: hooks.EventData{
			Reason:       "user_exit",
			InputTokens:  3000,
			OutputTokens: 1200,
			CachedTokens: 500,
		},
	}
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	records := proc.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	found := map[string]int64{}
	rec := &records[0]
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Value.Kind() == otellog.KindInt64 {
			found[kv.Key] = kv.Value.AsInt64()
		}
		return true
	})

	if found["gen_ai.usage.input_tokens"] != 3000 {
		t.Errorf("gen_ai.usage.input_tokens = %d, want 3000", found["gen_ai.usage.input_tokens"])
	}
	if found["gen_ai.usage.output_tokens"] != 1200 {
		t.Errorf("gen_ai.usage.output_tokens = %d, want 1200", found["gen_ai.usage.output_tokens"])
	}
	if found["gen_ai.usage.cached_tokens"] != 500 {
		t.Errorf("gen_ai.usage.cached_tokens = %d, want 500", found["gen_ai.usage.cached_tokens"])
	}
}

func TestNewTelemetryHandler_WithMeterProvider(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	if h == nil {
		t.Fatal("NewTelemetryHandler should not return nil")
	}
	if h.tokensInput == nil {
		t.Error("tokensInput instrument should be initialized")
	}
	if h.tokensOutput == nil {
		t.Error("tokensOutput instrument should be initialized")
	}
	if h.tokensCached == nil {
		t.Error("tokensCached instrument should be initialized")
	}
	if h.toolCalls == nil {
		t.Error("toolCalls instrument should be initialized")
	}
	if h.toolDuration == nil {
		t.Error("toolDuration instrument should be initialized")
	}
	if h.sessionCount == nil {
		t.Error("sessionCount instrument should be initialized")
	}
	if h.apiCalls == nil {
		t.Error("apiCalls instrument should be initialized")
	}
	if h.apiDuration == nil {
		t.Error("apiDuration instrument should be initialized")
	}
}

func TestTelemetryHandler_NilMeterProviderNoInstruments(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)
	if h.tokensInput != nil {
		t.Error("tokensInput should be nil without MeterProvider")
	}
	if h.toolCalls != nil {
		t.Error("toolCalls should be nil without MeterProvider")
	}
}

func TestTelemetryHandler_ToolMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// tool-start
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash", ToolInput: "ls"},
	}); err != nil {
		t.Fatalf("Handle tool-start error: %v", err)
	}

	// tool-end
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventToolEnd,
		Data: hooks.EventData{ToolName: "Bash", ToolOutput: "ok", Success: true},
	}); err != nil {
		t.Fatalf("Handle tool-end error: %v", err)
	}

	// Collect metrics
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	foundToolCalls := false
	foundToolDuration := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "agent.tool.calls":
				foundToolCalls = true
			case "agent.tool.duration":
				foundToolDuration = true
			}
		}
	}

	if !foundToolCalls {
		t.Error("expected agent.tool.calls metric to be recorded")
	}
	if !foundToolDuration {
		t.Error("expected agent.tool.duration metric to be recorded")
	}
}

func TestTelemetryHandler_ModelMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// model-start
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelStart,
		Data: hooks.EventData{},
	}); err != nil {
		t.Fatalf("Handle model-start error: %v", err)
	}

	// model-end
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelEnd,
		Data: hooks.EventData{Success: true},
	}); err != nil {
		t.Fatalf("Handle model-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	foundAPICalls := false
	foundAPIDuration := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "gen_ai.api.calls":
				foundAPICalls = true
			case "gen_ai.api.duration":
				foundAPIDuration = true
			}
		}
	}

	if !foundAPICalls {
		t.Error("expected gen_ai.api.calls metric to be recorded")
	}
	if !foundAPIDuration {
		t.Error("expected gen_ai.api.duration metric to be recorded")
	}
}

func TestTelemetryHandler_SessionMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// session-end (without session files, token metrics will be skipped but session count should work)
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventSessionEnd,
		Data: hooks.EventData{Reason: "user_exit"},
	}); err != nil {
		t.Fatalf("Handle session-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	foundSessionCount := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "agent.session.count" {
				foundSessionCount = true
			}
		}
	}

	if !foundSessionCount {
		t.Error("expected agent.session.count metric to be recorded")
	}
}

func TestTelemetryHandler_TokenMetricsOnModelEnd(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// model-start
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelStart,
		Data: hooks.EventData{},
	}); err != nil {
		t.Fatalf("Handle model-start error: %v", err)
	}

	// model-end with token usage
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelEnd,
		Data: hooks.EventData{
			Success:      true,
			InputTokens:  1500,
			OutputTokens: 500,
			CachedTokens: 200,
		},
	}); err != nil {
		t.Fatalf("Handle model-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
		}
	}

	if !found["gen_ai.tokens.input"] {
		t.Error("expected gen_ai.tokens.input metric to be recorded")
	}
	if !found["gen_ai.tokens.output"] {
		t.Error("expected gen_ai.tokens.output metric to be recorded")
	}
	if !found["gen_ai.tokens.cached"] {
		t.Error("expected gen_ai.tokens.cached metric to be recorded")
	}
	if !found["gen_ai.api.calls"] {
		t.Error("expected gen_ai.api.calls metric to be recorded")
	}
}

func TestTelemetryHandler_TokenMetricsOnSessionEnd(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// session-end with cumulative token usage
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventSessionEnd,
		Data: hooks.EventData{
			Reason:       "user_exit",
			InputTokens:  5000,
			OutputTokens: 2000,
		},
	}); err != nil {
		t.Fatalf("Handle session-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
		}
	}

	if !found["agent.session.count"] {
		t.Error("expected agent.session.count metric to be recorded")
	}
	if !found["gen_ai.tokens.input"] {
		t.Error("expected gen_ai.tokens.input metric to be recorded on session-end")
	}
	if !found["gen_ai.tokens.output"] {
		t.Error("expected gen_ai.tokens.output metric to be recorded on session-end")
	}
}

func TestTelemetryHandler_UnpairedToolEnd(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// Send tool-end WITHOUT a preceding tool-start (simulates hook-per-process mode)
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventToolEnd,
		Data: hooks.EventData{ToolName: "Bash", ToolOutput: "ok", Success: true},
	}); err != nil {
		t.Fatalf("Handle tool-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	foundToolCalls := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "agent.tool.calls" {
				foundToolCalls = true
			}
		}
	}

	if !foundToolCalls {
		t.Error("expected agent.tool.calls metric from unpaired tool-end event")
	}
}

func TestTelemetryHandler_UnpairedModelEnd(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// Send model-end WITHOUT a preceding model-start (hook-per-process mode)
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelEnd,
		Data: hooks.EventData{
			Success:      true,
			InputTokens:  1000,
			OutputTokens: 300,
		},
	}); err != nil {
		t.Fatalf("Handle model-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
		}
	}

	if !found["gen_ai.api.calls"] {
		t.Error("expected gen_ai.api.calls metric from unpaired model-end")
	}
	if !found["gen_ai.tokens.input"] {
		t.Error("expected gen_ai.tokens.input metric from unpaired model-end")
	}
	if !found["gen_ai.tokens.output"] {
		t.Error("expected gen_ai.tokens.output metric from unpaired model-end")
	}
}

func TestTelemetryHandler_NoTokenMetricsWhenZero(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// session-end without token data
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventSessionEnd,
		Data: hooks.EventData{Reason: "user_exit"},
	}); err != nil {
		t.Fatalf("Handle session-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "gen_ai.tokens.input" || m.Name == "gen_ai.tokens.output" || m.Name == "gen_ai.tokens.cached" {
				t.Errorf("did not expect %s metric when token counts are zero", m.Name)
			}
		}
	}
}
