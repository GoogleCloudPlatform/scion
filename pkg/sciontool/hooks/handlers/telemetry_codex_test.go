/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestTelemetryHandler_CodexNativeUsageSuppressesHookModelEnd pins design
// §9 phase 3c's AC: "with SCION_USAGE_SOURCE=native a hook model-end would
// not add calls (codex has none today, and the test pins it); tool hooks
// are unaffected."
//
// Codex's notify hook does not emit a model-start or model-end event today
// (its real notify events are agent-turn-complete, notification, and the
// UserPromptSubmit/PreToolUse/PostToolUse/Stop hooks; "model-end"/
// "AfterModel" is recognized by dialects.CodexDialect.normalizeEventName
// only because that mapping is shared code with other dialects, not because
// codex sends it). This test
// drives the real codex dialect with a hypothetical model-end payload to
// prove the generic SCION_USAGE_SOURCE gate (usageHookRecordingEnabled,
// telemetry.go) would suppress hook-sourced usage even if codex ever added
// one, while its real tool-start/tool-end events keep recording (D4: the
// gate is narrow to usage).
func TestTelemetryHandler_CodexNativeUsageSuppressesHookModelEnd(t *testing.T) {
	t.Setenv("SCION_HARNESS", "codex")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	d := dialects.NewCodexDialect()

	modelEnd, err := d.Parse(map[string]interface{}{
		"type": "AfterModel",
		"usage": map[string]interface{}{
			"prompt_tokens":     float64(100),
			"completion_tokens": float64(50),
		},
	})
	if err != nil {
		t.Fatalf("parsing codex model-end payload: %v", err)
	}
	if modelEnd.Name != hooks.EventModelEnd {
		t.Fatalf("codex dialect did not normalize AfterModel to %s, got %s", hooks.EventModelEnd, modelEnd.Name)
	}
	// Unpaired: codex, like every harness sending a hook-per-process, has no
	// preceding model-start to pair against (recordUnpairedEndMetrics).
	if err := h.Handle(modelEnd); err != nil {
		t.Fatalf("Handle model-end: %v", err)
	}

	toolStart, err := d.Parse(map[string]interface{}{"type": "BeforeTool", "tool_name": "shell"})
	if err != nil {
		t.Fatalf("parsing codex tool-start payload: %v", err)
	}
	if toolStart.Name != hooks.EventToolStart {
		t.Fatalf("codex dialect did not normalize BeforeTool to %s, got %s", hooks.EventToolStart, toolStart.Name)
	}
	if err := h.Handle(toolStart); err != nil {
		t.Fatalf("Handle tool-start: %v", err)
	}
	toolEnd, err := d.Parse(map[string]interface{}{"type": "AfterTool", "tool_name": "shell", "success": true})
	if err != nil {
		t.Fatalf("parsing codex tool-end payload: %v", err)
	}
	if err := h.Handle(toolEnd); err != nil {
		t.Fatalf("Handle tool-end: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
		}
	}
	if found[telemetrycontract.MetricAPICalls] {
		t.Error("gen_ai.api.calls must not be recorded from a codex hook model-end when SCION_USAGE_SOURCE=native")
	}
	if found[telemetrycontract.MetricUsageTokens] {
		t.Error("scion.usage.tokens must not be recorded from a codex hook model-end when SCION_USAGE_SOURCE=native")
	}
	if !found["agent.tool.calls"] {
		t.Error("agent.tool.calls must still be recorded: tool hooks are unaffected by SCION_USAGE_SOURCE (design D4, narrow)")
	}
}
