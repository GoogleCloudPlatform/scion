/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// claudeToolCallStatuses collects agent.tool.calls by status, checking that
// every point is for the Bash tool the synthetic Claude payloads use.
func claudeToolCallStatuses(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	statuses := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "agent.tool.calls" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("agent.tool.calls data is %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, p := range sum.DataPoints {
				status, _ := p.Attributes.Value(attribute.Key("status"))
				tool, _ := p.Attributes.Value(attribute.Key("tool_name"))
				if tool.AsString() != "Bash" {
					t.Errorf("agent.tool.calls tool_name = %q, want Bash", tool.AsString())
				}
				statuses[status.AsString()] += p.Value
			}
		}
	}
	return statuses
}

// TestClaudeToolFailure_ToolCallCountedAsError covers the one handler that
// treats a failed tool-end differently from a successful one: telemetry
// records the tool call with status "error" when the event carries an error.
// A Claude Code PostToolUseFailure, parsed through the real claude dialect,
// must end the PreToolUse call and be counted as an error.
func TestClaudeToolFailure_ToolCallCountedAsError(t *testing.T) {
	scrubTelemetryEnv(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)

	if err := h.Handle(waitClearClaudeToolStart(t)); err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	failure := waitClearClaudeToolFailure(t)
	if failure.Data.Error == "" {
		t.Fatal("PostToolUseFailure parsed with an empty error")
	}
	if err := h.Handle(failure); err != nil {
		t.Fatalf("Handle PostToolUseFailure: %v", err)
	}

	if statuses := claudeToolCallStatuses(t, reader); statuses["error"] != 1 || statuses["success"] != 0 {
		t.Errorf("agent.tool.calls by status = %v, want exactly one error", statuses)
	}
}

// TestClaudeToolFailure_UnpairedToolCallCountedAsError covers the path
// production takes: each Claude hook runs in its own sciontool process, so
// the PostToolUseFailure handler has no PreToolUse span to pair with. The
// unpaired tool-end must still count exactly one tool call, as an error.
func TestClaudeToolFailure_UnpairedToolCallCountedAsError(t *testing.T) {
	scrubTelemetryEnv(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	// A fresh handler that only sees the failure, as in the hook process.
	h := NewTelemetryHandler(nil, nil, nil, mp)

	failure := waitClearClaudeToolFailure(t)
	if failure.Data.Error == "" {
		t.Fatal("PostToolUseFailure parsed with an empty error")
	}
	if err := h.Handle(failure); err != nil {
		t.Fatalf("Handle PostToolUseFailure: %v", err)
	}

	if statuses := claudeToolCallStatuses(t, reader); statuses["error"] != 1 || statuses["success"] != 0 {
		t.Errorf("unpaired agent.tool.calls by status = %v, want exactly one error", statuses)
	}
}
