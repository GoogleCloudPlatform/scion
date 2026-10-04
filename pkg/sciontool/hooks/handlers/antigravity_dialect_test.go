/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// loadAntigravityFixture loads and parses every record of the real,
// captured agy 1.2.12 hook payload fixture (design §9 "3d", ptone/scion#2053
// phase 3d) through the real harnesses/antigravity/dialect.yaml, in capture
// order. See pkg/sciontool/hooks/dialects/testdata/antigravity/README.md for
// what the fixture contains and how it was captured.
func loadAntigravityFixture(t *testing.T) (*dialects.MappingDialect, []*hooks.Event) {
	t.Helper()

	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "antigravity", "dialect.yaml")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "antigravity" {
		t.Fatalf("dialect name = %q, want antigravity", md.Name())
	}

	fixturePath := filepath.Join(root, "pkg", "sciontool", "hooks", "dialects",
		"testdata", "antigravity", "hook-payloads-1.2.12.jsonl")
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var events []*hooks.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("unmarshal fixture line: %v", err)
		}
		event, err := md.Parse(payload)
		if err != nil {
			t.Fatalf("Parse fixture line: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(events) != 10 {
		t.Fatalf("fixture record count = %d, want 10 (see testdata README)", len(events))
	}
	return md, events
}

// TestAntigravityFixture_EventSequence pins the real, captured event
// sequence and its normalized names -- in particular the granularity
// finding (design §9 3d AC): a single turn with a tool call produces two
// full PreInvocation/PostInvocation pairs (one per main-loop model request, not
// one for the whole turn), and a second, separate turn resets back to one
// pair. If agy ever changes this shape, this test is the first thing to
// fail.
func TestAntigravityFixture_EventSequence(t *testing.T) {
	_, events := loadAntigravityFixture(t)

	wantNames := []string{
		hooks.EventModelStart, // PreInvocation, invocationNum=0
		hooks.EventToolStart,  // PreToolUse (run_command)
		hooks.EventToolEnd,    // PostToolUse (run_command)
		hooks.EventModelEnd,   // PostInvocation, invocationNum=0
		hooks.EventModelStart, // PreInvocation, invocationNum=1
		hooks.EventModelEnd,   // PostInvocation, invocationNum=1
		hooks.EventAgentEnd,   // Stop (turn 1)
		hooks.EventModelStart, // PreInvocation, invocationNum=0 (turn 2)
		hooks.EventModelEnd,   // PostInvocation, invocationNum=0 (turn 2)
		hooks.EventAgentEnd,   // Stop (turn 2)
	}
	if len(events) != len(wantNames) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantNames))
	}
	for i, e := range events {
		if e.Name != wantNames[i] {
			t.Errorf("events[%d].Name = %q, want %q (raw=%q)", i, e.Name, wantNames[i], e.RawName)
		}
	}

	// None of the three real model responses behind this fixture's
	// PostInvocation events carried any usage/token field to the hook --
	// two had a full Gemini usageMetadata block server-side, one had none,
	// and the hook payload was identical in shape either way (see testdata
	// README). Pin that the parsed events agree: no dialect field mapping
	// exists for tokens on either PreInvocation or PostInvocation, so every
	// token field on every event here is zero.
	for i, e := range events {
		if e.Data.InputTokens != 0 || e.Data.OutputTokens != 0 || e.Data.CachedTokens != 0 ||
			e.Data.CacheWriteTokens != 0 || e.Data.ReasoningTokens != 0 {
			t.Errorf("events[%d] (%s) has non-zero token fields; antigravity ships calls-only (design §9 3d)", i, e.Name)
		}
	}
}

// TestAntigravityFixture_CallsPerInvocationNoDoubleCount is design §9 3d's
// core AC: the fixture gives the exact canonical increments (calls per
// invocation), and one PostInvocation gives exactly one call with no second
// source. The fixture's three PostInvocation records (two in turn 1, one in
// turn 2) must produce exactly three gen_ai.api.calls -- not six (double
// counted from some second path) and not fewer (a dropped invocation).
// scion.usage.tokens must have no data points at all: there is nothing to
// report, and nothing must synthesize a value.
func TestAntigravityFixture_CallsPerInvocationNoDoubleCount(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	// Pin the full label set metricAttrs()/recordEndMetrics stamp on
	// gen_ai.api.calls (design §3.2): agent_id, project_id and harness from
	// metricAttrs(), model and status from the event (always success here).
	// SCION_AGENT_ID and SCION_PROJECT_ID are set to fixed values, not just
	// cleared, so this pins that they *are* stamped, not only that nothing
	// extra leaks in -- this sandbox itself runs as a Scion agent, with both
	// set ambiently in its real environment, so a bare assertion that they're
	// present wouldn't by itself prove metricAttrs() is what stamped them.
	t.Setenv("SCION_HARNESS", "antigravity")
	// Deliberately different from the fixture's modelName: the payload's
	// own model must win over SCION_MODEL (design §3.2, ptone/scion#2242).
	t.Setenv("SCION_MODEL", "configured-model-should-lose")
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "project-1")
	_, events := loadAntigravityFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	for i, e := range events {
		if err := h.Handle(e); err != nil {
			t.Fatalf("Handle(events[%d]): %v", i, err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	gotCalls := sumInt64Counter(rm, telemetrycontract.MetricAPICalls)
	if gotCalls != 3 {
		t.Errorf("gen_ai.api.calls = %d, want 3 (one per PostInvocation: 2 in turn 1, 1 in turn 2)", gotCalls)
	}

	// Pin the exact producer label set (design §3.2, §7.4): all three calls
	// share identical attributes, so a correct implementation merges them
	// into one data point with value 3. A regression that leaked a payload
	// attribute onto gen_ai.api.calls (for example conversationId) would
	// either add an unexpected key here or split this into more than one
	// data point. modelName is the one payload value that is used: it is
	// the model label (see the model-label note in
	// harnesses/antigravity/README.md).
	callPoints := int64CounterDataPoints(rm, telemetrycontract.MetricAPICalls)
	if len(callPoints) != 1 {
		t.Fatalf("gen_ai.api.calls data point count = %d, want 1 (identical attributes across all 3 calls)", len(callPoints))
	}
	if callPoints[0].Value != 3 {
		t.Errorf("gen_ai.api.calls single data point value = %d, want 3", callPoints[0].Value)
	}
	wantAttrs := map[string]string{
		// agent_id/project_id are metricAttrs()'s producer-side labels (kept
		// for Cloud descriptor compatibility, design §3.2); they are
		// distinct from the exporter-stamped canonical scion_agent_id/
		// scion_project_id, which have their own contract constants and are
		// out of hooks.Handle's scope entirely.
		"agent_id":                     "agent-1",
		"project_id":                   "project-1",
		telemetrycontract.HarnessLabel: "antigravity",
		telemetrycontract.ModelLabel:   "gemini-3.1-pro-low",
		telemetrycontract.StatusLabel:  telemetrycontract.StatusSuccess,
	}
	gotAttrs := callPoints[0].Attributes
	if gotAttrs.Len() != len(wantAttrs) {
		t.Errorf("gen_ai.api.calls attribute count = %d, want %d (got %v)", gotAttrs.Len(), len(wantAttrs), gotAttrs.ToSlice())
	}
	for key, want := range wantAttrs {
		got, ok := gotAttrs.Value(attribute.Key(key))
		if !ok {
			t.Errorf("gen_ai.api.calls missing expected attribute %q", key)
			continue
		}
		if got.AsString() != want {
			t.Errorf("gen_ai.api.calls attribute %q = %q, want %q", key, got.AsString(), want)
		}
	}

	gotToolCalls := sumInt64Counter(rm, "agent.tool.calls")
	if gotToolCalls != 1 {
		t.Errorf("agent.tool.calls = %d, want 1 (the one real PreToolUse/PostToolUse pair)", gotToolCalls)
	}
	// ptone/scion#2243: the tool-end metric is labelled with the real tool
	// name from PostToolUse's toolCall.name, not left empty.
	for _, p := range int64CounterDataPoints(rm, "agent.tool.calls") {
		got, _ := p.Attributes.Value(attribute.Key("tool_name"))
		if got.AsString() != "run_command" {
			t.Errorf("agent.tool.calls tool_name = %q, want run_command", got.AsString())
		}
	}

	totals := usageTokenTotals(rm)
	for tokenType, n := range totals {
		if n != 0 {
			t.Errorf("scion.usage.tokens{token_type=%s} = %d, want 0: antigravity's PostInvocation carries no usage fields", tokenType, n)
		}
	}
	if hasUsageTokenPoints(rm) {
		t.Error("expected zero scion.usage.tokens data points from a fixture with no token fields anywhere, got at least one")
	}
}

// TestAntigravityFixture_PostToolUseMapsToolName is ptone/scion#2243: agy
// 1.2.12's PostToolUse payload carries toolCall.name (the earlier dialect
// comment claimed it did not), and the tool-end event's ToolName matches
// its tool-start's.
func TestAntigravityFixture_PostToolUseMapsToolName(t *testing.T) {
	_, events := loadAntigravityFixture(t)
	start, end := events[1], events[2]
	if start.Name != hooks.EventToolStart || end.Name != hooks.EventToolEnd {
		t.Fatalf("events[1..2] = (%s, %s), want (tool-start, tool-end)", start.Name, end.Name)
	}
	if end.Data.ToolName != "run_command" {
		t.Errorf("PostToolUse ToolName = %q, want run_command", end.Data.ToolName)
	}
	if end.Data.ToolName != start.Data.ToolName {
		t.Errorf("PostToolUse ToolName = %q, PreToolUse ToolName = %q; want equal", end.Data.ToolName, start.Data.ToolName)
	}
	if end.Data.Error != "" {
		t.Errorf("PostToolUse Error = %q, want empty (the captured call succeeded)", end.Data.Error)
	}
}

// TestAntigravityFixture_ModelFromPayload is ptone/scion#2242: every
// Pre/PostInvocation event carries the payload's modelName as its Model.
func TestAntigravityFixture_ModelFromPayload(t *testing.T) {
	_, events := loadAntigravityFixture(t)
	for i, e := range events {
		if e.Name != hooks.EventModelStart && e.Name != hooks.EventModelEnd {
			continue
		}
		if e.Data.Model != "gemini-3.1-pro-low" {
			t.Errorf("events[%d] (%s) Model = %q, want gemini-3.1-pro-low", i, e.Name, e.Data.Model)
		}
	}
}

// TestAntigravityDialect_ModelLabelFallsBackToUnknown pins the rest of
// design §3.2's precedence for a payload without modelName: SCION_MODEL
// when set, otherwise "unknown" -- never an absent label.
func TestAntigravityDialect_ModelLabelFallsBackToUnknown(t *testing.T) {
	md, _ := loadAntigravityFixture(t)
	for _, tc := range []struct{ envModel, want string }{
		{"configured-model", "configured-model"},
		{"", telemetrycontract.UnknownModel},
	} {
		t.Run("SCION_MODEL="+tc.envModel, func(t *testing.T) {
			t.Setenv("SCION_USAGE_SOURCE", "hooks")
			t.Setenv("SCION_MODEL", tc.envModel)
			event, err := md.Parse(map[string]interface{}{
				"hook_event_name": "PostInvocation",
				"conversationId":  "c",
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			h := NewTelemetryHandler(nil, nil, nil, mp)
			if err := h.Handle(event); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			points := int64CounterDataPoints(rm, telemetrycontract.MetricAPICalls)
			if len(points) != 1 {
				t.Fatalf("gen_ai.api.calls points = %d, want 1", len(points))
			}
			got, ok := points[0].Attributes.Value(attribute.Key(telemetrycontract.ModelLabel))
			if !ok || got.AsString() != tc.want {
				t.Errorf("model label = %q (present=%t), want %q", got.AsString(), ok, tc.want)
			}
		})
	}
}

// TestAntigravityFixture_UsageSourceUnsetPublishesNothing is design D10's
// vetting-gate default applied to a real antigravity capture: with
// SCION_USAGE_SOURCE unset, the exact same fixture that produces 3 calls
// under "hooks" (above) must publish none at all. Tool hooks are
// unaffected either way (design D4, narrow): the one real tool call still
// shows up.
func TestAntigravityFixture_UsageSourceUnsetPublishesNothing(t *testing.T) {
	// Explicitly unset, not just left alone: the antigravity provision.py sets
	// SCION_USAGE_SOURCE=hooks in every antigravity agent, so a bare `go
	// test` run inside one would otherwise see it ambiently set and fail
	// here -- exactly the environment this change creates. The existing
	// D10 tests in telemetry_test.go set this explicitly for every case,
	// including "", for the same reason.
	t.Setenv("SCION_USAGE_SOURCE", "")
	_, events := loadAntigravityFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	for i, e := range events {
		if err := h.Handle(e); err != nil {
			t.Fatalf("Handle(events[%d]): %v", i, err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if got := sumInt64Counter(rm, telemetrycontract.MetricAPICalls); got != 0 {
		t.Errorf("gen_ai.api.calls = %d, want 0 with SCION_USAGE_SOURCE unset (D10 default)", got)
	}
	if hasUsageTokenPoints(rm) {
		t.Error("expected zero scion.usage.tokens data points with SCION_USAGE_SOURCE unset")
	}

	// Tool, session and turn hook metrics are unaffected by the usage gate
	// (design D4, narrow): the real PreToolUse/PostToolUse pair still
	// produces its counter.
	if got := sumInt64Counter(rm, "agent.tool.calls"); got != 1 {
		t.Errorf("agent.tool.calls = %d, want 1: the usage-source gate must not touch tool metrics", got)
	}
}

// TestAntigravityDialect_MalformedTokenFieldsAreIgnored proves antigravity
// gets the same malformed/missing-usage handling as every other hook
// dialect for free: it declares no token field mappings at all (there is
// nothing to map, per the fixture), so a payload with garbage-typed
// token-shaped values falls through to the shared extractTokens/
// resolveFieldPathInt64 code path (already covered generically by
// dialects.TestMappingDialect_Parse_CacheWriteAndReasoningTokenFieldMappings
// and friends) and is simply ignored rather than panicking or coercing
// garbage into a number.
func TestAntigravityDialect_MalformedTokenFieldsAreIgnored(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "antigravity", "dialect.yaml")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}

	payload := map[string]interface{}{
		"hook_event_name": "PostInvocation",
		"conversationId":  "test-conversation",
		"invocationNum":   float64(0),
		// usageMetadata is not a key any dialect maps -- antigravity's real
		// PostInvocation never carries one (see the fixture) -- so it's
		// inert here regardless of its (garbage) contents. input_tokens
		// *is* a recognized shared top-level name (dialects/common.go's
		// extractTokens), included here to prove that a non-numeric value
		// for it is ignored by the shared getInt64 path rather than
		// panicking or coercing garbage into a number -- the same handling
		// every hook dialect gets for free.
		"usageMetadata": map[string]interface{}{
			"promptTokenCount": "not-a-number",
		},
		"input_tokens": "also-not-a-number",
	}
	event, err := md.Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name != hooks.EventModelEnd {
		t.Fatalf("event.Name = %q, want %q", event.Name, hooks.EventModelEnd)
	}
	if event.Data.InputTokens != 0 || event.Data.OutputTokens != 0 || event.Data.CachedTokens != 0 {
		t.Errorf("expected zero token fields from a malformed/unmapped payload, got InputTokens=%d OutputTokens=%d CachedTokens=%d",
			event.Data.InputTokens, event.Data.OutputTokens, event.Data.CachedTokens)
	}
}

// sumInt64Counter sums every data point of the named Sum metric across all
// scope metrics in rm.
func sumInt64Counter(rm metricdata.ResourceMetrics, name string) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// int64CounterDataPoints returns every data point of the named Sum[int64]
// metric across all scope metrics in rm, for tests that need to inspect
// attributes or value per point rather than just a total.
func int64CounterDataPoints(rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	var points []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				points = append(points, sum.DataPoints...)
			}
		}
	}
	return points
}

// hasUsageTokenPoints reports whether scion.usage.tokens has any data point
// at all, regardless of value -- distinct from usageTokenTotals summing to
// zero, which a stray zero-valued point would also satisfy.
func hasUsageTokenPoints(rm metricdata.ResourceMetrics) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetrycontract.MetricUsageTokens {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
				return true
			}
		}
	}
	return false
}
