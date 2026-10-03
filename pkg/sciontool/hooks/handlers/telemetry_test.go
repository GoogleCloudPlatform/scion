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
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

func TestNewTelemetryHandler_AcceptsLegacyRedactorArgument(t *testing.T) {
	redactor := telemetry.NewRedactor(telemetry.RedactionConfig{
		Redact: []string{"prompt"},
		Hash:   []string{"session_id"},
	})

	h := NewTelemetryHandler(nil, nil, redactor)
	if h == nil {
		t.Fatal("NewTelemetryHandler should not return nil")
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

func TestTelemetryHandler_MetricAttrsPreferCanonicalProject(t *testing.T) {
	t.Setenv("SCION_PROJECT_ID", "canonical-project")
	h := NewTelemetryHandler(nil, nil, nil)
	attrs := h.metricAttrs()
	foundProject := false
	for _, attr := range attrs {
		if string(attr.Key) == "project_id" {
			foundProject = true
			if attr.Value.AsString() != "canonical-project" {
				t.Fatalf("project_id = %q, want canonical-project", attr.Value.AsString())
			}
		}
	}
	if !foundProject {
		t.Fatal("project_id metric dimension missing")
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

func (p *recordingProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (p *recordingProcessor) Shutdown(context.Context) error                         { return nil }
func (p *recordingProcessor) ForceFlush(context.Context) error                       { return nil }

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
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		found[string(kv.Key)] = kv.Value.AsString()
		return true
	})

	if found["event.name"] != "agent.session.start" {
		t.Errorf("event.name = %q, want %q", found["event.name"], "agent.session.start")
	}
	if found["session_id"] != "sess-abc" {
		t.Errorf("session_id = %q, want %q", found["session_id"], "sess-abc")
	}
	if found["source"] != "cli" {
		t.Errorf("source = %q, want %q", found["source"], "cli")
	}
}

func TestTelemetryHandler_LeavesPolicyTransformationToReceiver(t *testing.T) {
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
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		found[string(kv.Key)] = kv.Value.AsString()
		return true
	})

	// Producers retain raw values so the receiver is the single authoritative
	// policy boundary and hashes each accepted field exactly once.
	if found["prompt"] != "my secret prompt" {
		t.Errorf("prompt = %q, want unprocessed producer value", found["prompt"])
	}
	if found["session_id"] != "sess-secret" {
		t.Errorf("session_id = %q, want unprocessed producer value", found["session_id"])
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
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Value.Type() == attribute.STRING {
			found[string(kv.Key)] = kv.Value.AsString()
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
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Value.Type() == attribute.INT64 {
			found[string(kv.Key)] = kv.Value.AsInt64()
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
	if h.usageTokens == nil {
		t.Error("usageTokens instrument should be initialized")
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
	if h.usageTokens != nil {
		t.Error("usageTokens should be nil without MeterProvider")
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
	// Usage (gen_ai.api.calls / scion.usage.tokens) is gated by
	// SCION_USAGE_SOURCE=hooks (design D10 vetting gate); see
	// TestTelemetryHandler_UsageSourceGate for the unset/native states.
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
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

func TestSessionMetricScopesSeparateHookAndLifecycleSources(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		newHandler  func(metric.MeterProvider) *TelemetryHandler
	}{
		{"hook", hookMetricScope, func(mp metric.MeterProvider) *TelemetryHandler { return NewTelemetryHandler(nil, nil, nil, mp) }},
		{"lifecycle", telemetry.LifecycleMetricScope, func(mp metric.MeterProvider) *TelemetryHandler {
			return NewLifecycleTelemetryHandler(nil, nil, nil, mp)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			if err := tc.newHandler(mp).Handle(&hooks.Event{Name: hooks.EventSessionEnd}); err != nil {
				t.Fatal(err)
			}
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != tc.scope || len(rm.ScopeMetrics[0].Metrics) != 1 || rm.ScopeMetrics[0].Metrics[0].Name != "agent.session.count" {
				t.Fatalf("session metric scope or name changed: %+v", rm.ScopeMetrics)
			}
		})
	}
}

// usageTokenTotals sums scion.usage.tokens data points by their token_type
// attribute value.
func usageTokenTotals(rm metricdata.ResourceMetrics) map[string]int64 {
	totals := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetrycontract.MetricUsageTokens {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				tokenType, _ := point.Attributes.Value(attribute.Key(telemetrycontract.TokenTypeLabel))
				totals[tokenType.AsString()] += point.Value
			}
		}
	}
	return totals
}

func TestTelemetryHandler_TokenMetricsOnModelEnd(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
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

	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 1500 {
		t.Errorf("expected %s token_type=%s = 1500, got %d", telemetrycontract.MetricUsageTokens, telemetrycontract.TokenTypeInput, totals[telemetrycontract.TokenTypeInput])
	}
	if totals[telemetrycontract.TokenTypeOutput] != 500 {
		t.Errorf("expected %s token_type=%s = 500, got %d", telemetrycontract.MetricUsageTokens, telemetrycontract.TokenTypeOutput, totals[telemetrycontract.TokenTypeOutput])
	}
	// "cached" maps to the canonical "cache_read" (design §3.5).
	if totals[telemetrycontract.TokenTypeCacheRead] != 200 {
		t.Errorf("expected %s token_type=%s = 200, got %d", telemetrycontract.MetricUsageTokens, telemetrycontract.TokenTypeCacheRead, totals[telemetrycontract.TokenTypeCacheRead])
	}
	if !found[telemetrycontract.MetricAPICalls] {
		t.Error("expected gen_ai.api.calls metric to be recorded")
	}
	for _, oldName := range []string{"gen_ai.tokens.input", "gen_ai.tokens.output", "gen_ai.tokens.cached", "scion.hook.tokens.input", "scion.hook.tokens.output", "scion.hook.tokens.cached"} {
		if found[oldName] {
			t.Errorf("normalized hook emitted retired token name %s", oldName)
		}
	}
}

// TestTelemetryHandler_UsageTokenLabelsMatchContract asserts that every
// scion.usage.tokens point the real handler emits carries exactly the
// contract's producer label set -- UsageTokenPointAttrs(harness, model) plus
// token_type -- and nothing else, on both the paired and unpaired model-end
// paths. This is the handler-side complement to
// pkg/sciontool/telemetry.TestHookUsageTokensPassStrictGCPCloudAdmission,
// which guards the UsageTokenPointAttrs helper against real GCP admission
// but never runs recordTokenMetrics itself (the two packages can't import
// each other in tests; see that test's doc comment). Without this test nothing
// would catch a regression where the handler goes back to including
// agent_id/project_id, since the admission test would keep passing.
//
// gen_ai.api.calls is asserted to still carry agent_id and project_id in the
// same run, documenting the intended asymmetry: it keeps them for Cloud
// descriptor compatibility (design §3.2); scion.usage.tokens does not,
// because its GCP allowlist doesn't permit them.
func TestTelemetryHandler_UsageTokenLabelsMatchContract(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "project-1")
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_MODEL", "test-model")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	// Paired path.
	if err := h.Handle(&hooks.Event{Name: hooks.EventModelStart, Data: hooks.EventData{}}); err != nil {
		t.Fatalf("Handle model-start error: %v", err)
	}
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelEnd,
		Data: hooks.EventData{Success: true, InputTokens: 100, OutputTokens: 50, CachedTokens: 10},
	}); err != nil {
		t.Fatalf("Handle model-end error: %v", err)
	}
	// Unpaired path (recordUnpairedEndMetrics): a second model-end with no
	// matching start, the normal hook-per-process case.
	if err := h.Handle(&hooks.Event{
		Name: hooks.EventModelEnd,
		Data: hooks.EventData{Success: true, InputTokens: 5, OutputTokens: 2, CachedTokens: 1},
	}); err != nil {
		t.Fatalf("Handle unpaired model-end error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect error: %v", err)
	}

	wantTokenKeys := map[string]bool{
		telemetrycontract.HarnessLabel:   true,
		telemetrycontract.ModelLabel:     true,
		telemetrycontract.TokenTypeLabel: true,
	}
	sawUsageTokenPoint := false
	sawAPICallPoint := false
	// Both model-end events share the same attribute set, so the SDK merges
	// them into one series per token_type: summed totals prove both the
	// paired (input=100, output=50, cached=10) and unpaired (input=5,
	// output=2, cached=1) calls actually contributed, which the key-set
	// check alone doesn't show.
	tokenTotals := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case telemetrycontract.MetricUsageTokens:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
				}
				for _, point := range sum.DataPoints {
					sawUsageTokenPoint = true
					gotKeys := map[string]bool{}
					for _, kv := range point.Attributes.ToSlice() {
						gotKeys[string(kv.Key)] = true
					}
					if len(gotKeys) != len(wantTokenKeys) {
						t.Errorf("%s point attribute keys = %v, want exactly %v", m.Name, gotKeys, wantTokenKeys)
						continue
					}
					for key := range wantTokenKeys {
						if !gotKeys[key] {
							t.Errorf("%s point attribute keys = %v, missing %q", m.Name, gotKeys, key)
						}
					}
					if tokenType, ok := point.Attributes.Value(attribute.Key(telemetrycontract.TokenTypeLabel)); ok {
						tokenTotals[tokenType.AsString()] += point.Value
					}
				}
			case telemetrycontract.MetricAPICalls:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
				}
				for _, point := range sum.DataPoints {
					sawAPICallPoint = true
					if _, ok := point.Attributes.Value(attribute.Key("agent_id")); !ok {
						t.Errorf("%s point missing agent_id (intended asymmetry vs. %s)", m.Name, telemetrycontract.MetricUsageTokens)
					}
					if _, ok := point.Attributes.Value(attribute.Key("project_id")); !ok {
						t.Errorf("%s point missing project_id (intended asymmetry vs. %s)", m.Name, telemetrycontract.MetricUsageTokens)
					}
				}
			}
		}
	}
	if !sawUsageTokenPoint {
		t.Fatalf("expected at least one %s point", telemetrycontract.MetricUsageTokens)
	}
	if !sawAPICallPoint {
		t.Fatalf("expected at least one %s point", telemetrycontract.MetricAPICalls)
	}
	wantTotals := map[string]int64{
		telemetrycontract.TokenTypeInput:     105, // 100 (paired) + 5 (unpaired)
		telemetrycontract.TokenTypeOutput:    52,  // 50 (paired) + 2 (unpaired)
		telemetrycontract.TokenTypeCacheRead: 11,  // 10 (paired) + 1 (unpaired)
	}
	for tokenType, want := range wantTotals {
		if got := tokenTotals[tokenType]; got != want {
			t.Errorf("token_type=%s total = %d, want %d (both the paired and unpaired model-end must have contributed)", tokenType, got, want)
		}
	}
}

// TestTelemetryHandler_RecordsCacheWriteAndReasoningTokens pins design §3.7
// "Hook token plumbing" (GoogleCloudPlatform/scion#2057 review): a model-end
// with all five token fields populated emits exactly five
// scion.usage.tokens points -- input, output, cache_read, cache_write and
// reasoning -- each with the exact contract label set
// {harness, model, token_type}, gated the same way as the other three by
// SCION_USAGE_SOURCE (none when unset or native, all five when hooks).
func TestTelemetryHandler_RecordsCacheWriteAndReasoningTokens(t *testing.T) {
	for _, tc := range []struct {
		name            string
		usageSource     string
		wantTokenPoints int
	}{
		{name: "unset (vetting gate default)", usageSource: "", wantTokenPoints: 0},
		{name: "native (deriver owns usage)", usageSource: "native", wantTokenPoints: 0},
		{name: "hooks (opted in)", usageSource: "hooks", wantTokenPoints: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_USAGE_SOURCE", tc.usageSource)
			t.Setenv("SCION_HARNESS", "test-harness")
			t.Setenv("SCION_MODEL", "test-model")

			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()

			h := NewTelemetryHandler(nil, nil, nil, mp)
			// Unpaired model-end (recordUnpairedEndMetrics): the normal
			// hook-per-process case, same as
			// TestTelemetryHandler_UnpairedModelEnd.
			if err := h.Handle(&hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{
					Success:          true,
					InputTokens:      100,
					OutputTokens:     50,
					CachedTokens:     10,
					CacheWriteTokens: 30,
					ReasoningTokens:  15,
				},
			}); err != nil {
				t.Fatalf("Handle model-end error: %v", err)
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect error: %v", err)
			}

			wantTokenKeys := map[string]bool{
				telemetrycontract.HarnessLabel:   true,
				telemetrycontract.ModelLabel:     true,
				telemetrycontract.TokenTypeLabel: true,
			}
			gotByType := map[string]int64{}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != telemetrycontract.MetricUsageTokens {
						continue
					}
					sum, ok := m.Data.(metricdata.Sum[int64])
					if !ok {
						t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
					}
					for _, point := range sum.DataPoints {
						gotKeys := map[string]bool{}
						for _, kv := range point.Attributes.ToSlice() {
							gotKeys[string(kv.Key)] = true
						}
						if len(gotKeys) != len(wantTokenKeys) {
							t.Errorf("point attribute keys = %v, want exactly %v", gotKeys, wantTokenKeys)
							continue
						}
						for key := range wantTokenKeys {
							if !gotKeys[key] {
								t.Errorf("point attribute keys = %v, missing %q", gotKeys, key)
							}
						}
						if tokenType, ok := point.Attributes.Value(attribute.Key(telemetrycontract.TokenTypeLabel)); ok {
							gotByType[tokenType.AsString()] = point.Value
						}
					}
				}
			}

			if len(gotByType) != tc.wantTokenPoints {
				t.Fatalf("%s token points = %d (%v), want %d", telemetrycontract.MetricUsageTokens, len(gotByType), gotByType, tc.wantTokenPoints)
			}
			if tc.wantTokenPoints == 0 {
				return
			}
			want := map[string]int64{
				telemetrycontract.TokenTypeInput:      100,
				telemetrycontract.TokenTypeOutput:     50,
				telemetrycontract.TokenTypeCacheRead:  10,
				telemetrycontract.TokenTypeCacheWrite: 30,
				telemetrycontract.TokenTypeReasoning:  15,
			}
			for tokenType, want := range want {
				if got := gotByType[tokenType]; got != want {
					t.Errorf("token_type=%s = %d, want %d", tokenType, got, want)
				}
			}
		})
	}
}

// TestTelemetryHandler_UsageSourceGate pins design D4/D10 and the Phase 2 AC:
// hook-sourced usage (gen_ai.api.calls, scion.usage.tokens) is recorded only
// when SCION_USAGE_SOURCE=hooks. It is suppressed both when the variable is
// unset (the vetting-gate default) and when it is "native" (the deriver owns
// usage instead). Tool and session hook metrics are unaffected in every case.
func TestTelemetryHandler_UsageSourceGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usageSource string
		wantUsage   bool
	}{
		{name: "unset (vetting gate default)", usageSource: "", wantUsage: false},
		{name: "native (deriver owns usage)", usageSource: "native", wantUsage: false},
		{name: "hooks (opted in)", usageSource: "hooks", wantUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An empty value is equivalent to unset for usageHookRecordingEnabled
			// (both compare != "hooks"), and t.Setenv restores the previous
			// value automatically, so this also covers the true-unset default.
			t.Setenv("SCION_USAGE_SOURCE", tc.usageSource)

			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()

			h := NewTelemetryHandler(nil, nil, nil, mp)

			// Paired model-start/model-end.
			if err := h.Handle(&hooks.Event{Name: hooks.EventModelStart, Data: hooks.EventData{}}); err != nil {
				t.Fatalf("Handle model-start error: %v", err)
			}
			if err := h.Handle(&hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Success: true, InputTokens: 100, OutputTokens: 50},
			}); err != nil {
				t.Fatalf("Handle model-end error: %v", err)
			}
			// Unpaired model-end: the normal hook-per-process case
			// (recordUnpairedEndMetrics), gated the same way as the paired
			// path above.
			if err := h.Handle(&hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Success: true, InputTokens: 10, OutputTokens: 5},
			}); err != nil {
				t.Fatalf("Handle unpaired model-end error: %v", err)
			}
			if err := h.Handle(&hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}}); err != nil {
				t.Fatalf("Handle tool-start error: %v", err)
			}
			if err := h.Handle(&hooks.Event{Name: hooks.EventToolEnd, Data: hooks.EventData{ToolName: "Bash", Success: true}}); err != nil {
				t.Fatalf("Handle tool-end error: %v", err)
			}
			// session-end: agent.session.count must be recorded in every
			// state, since the gate is scoped to usage only (D4, narrow).
			if err := h.Handle(&hooks.Event{Name: hooks.EventSessionEnd, Data: hooks.EventData{Reason: "user_exit"}}); err != nil {
				t.Fatalf("Handle session-end error: %v", err)
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect error: %v", err)
			}

			found := map[string]bool{}
			var foundAPIDuration bool
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					found[m.Name] = true
					if m.Name == "gen_ai.api.duration" {
						foundAPIDuration = true
					}
				}
			}

			if found[telemetrycontract.MetricAPICalls] != tc.wantUsage {
				t.Errorf("%s recorded=%v, want %v", telemetrycontract.MetricAPICalls, found[telemetrycontract.MetricAPICalls], tc.wantUsage)
			}
			if found[telemetrycontract.MetricUsageTokens] != tc.wantUsage {
				t.Errorf("%s recorded=%v, want %v", telemetrycontract.MetricUsageTokens, found[telemetrycontract.MetricUsageTokens], tc.wantUsage)
			}
			// gen_ai.api.duration is not part of the usage contract (design
			// D4 "the switch covers only usage"), so it is unaffected.
			if !foundAPIDuration {
				t.Error("expected gen_ai.api.duration to be recorded regardless of SCION_USAGE_SOURCE")
			}
			// Tool and session metrics are unaffected either way (D4, narrow).
			if !found["agent.tool.calls"] {
				t.Error("expected agent.tool.calls to be recorded regardless of SCION_USAGE_SOURCE")
			}
			if !found[telemetrycontract.MetricSessionCount] {
				t.Error("expected agent.session.count to be recorded regardless of SCION_USAGE_SOURCE")
			}
		})
	}
}

func TestTelemetryHandler_SessionTotalsDoNotDuplicateModelTokens(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	if err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Success: true, InputTokens: 1500, OutputTokens: 500}}); err != nil {
		t.Fatal(err)
	}

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
	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 1500 || totals[telemetrycontract.TokenTypeOutput] != 500 {
		t.Errorf("session totals duplicated model increments: input=%d output=%d", totals[telemetrycontract.TokenTypeInput], totals[telemetrycontract.TokenTypeOutput])
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
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
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

	if !found[telemetrycontract.MetricAPICalls] {
		t.Error("expected gen_ai.api.calls metric from unpaired model-end")
	}
	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 1000 {
		t.Errorf("expected scion.usage.tokens token_type=input from unpaired model-end, got %d", totals[telemetrycontract.TokenTypeInput])
	}
	if totals[telemetrycontract.TokenTypeOutput] != 300 {
		t.Errorf("expected scion.usage.tokens token_type=output from unpaired model-end, got %d", totals[telemetrycontract.TokenTypeOutput])
	}
}

func TestTelemetryHandler_NoTokenMetricsWhenZero(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
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
			if m.Name == telemetrycontract.MetricUsageTokens {
				t.Errorf("did not expect %s metric when token counts are zero", m.Name)
			}
		}
	}
}
