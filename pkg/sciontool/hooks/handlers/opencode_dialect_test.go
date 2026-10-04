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
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gopkg.in/yaml.v3"
)

// loadOpencodeHookPayloadFixture reads
// pkg/sciontool/hooks/dialects/testdata/opencode/hook-payloads-1.18.33.jsonl,
// one JSON object per line in the exact shape scion-bridge.js's
// emitHookEvent sends as `sciontool hook --dialect=opencode`'s stdin.
//
// Provenance: this is the real stdin scion-bridge.js wrote to a stub
// `sciontool` binary at capture time, while driving a real npm-installed
// opencode-ai@1.18.33 through a 3-step tool loop (bash, then read, then a
// final answer with no more tool calls) against a local, credential-free
// mock OpenAI-compatible model server, followed by a real
// `POST /session/{id}/fork`. Every value here — the three step-finish
// records' token counts, the tool names and inputs, both session.created
// records — is exactly what that real run produced, except for one scrub
// (the capture's scratch directory path in the `read` tool's tool_input,
// replaced with a placeholder). The comparison script and its clean output
// verifying this (field by field, against the unscrubbed raw capture log)
// are private artifacts (not shipped) and are not a fact this comment
// relies on beyond what's stated above. scion-bridge.js has since changed
// (busy/retry-gated agent-end, unmapped session.error, child-session
// filtering); replaying the same underlying raw bus events (this fixture's
// sibling bus-events-1.18.33.json's run2 records) through the current
// route() reproduces the same emission sequence and values shown below,
// modulo session/message IDs (which differ because the two fixtures come
// from separate capture runs).
//
// Ten records, three step-finish "model calls": the second session.created
// is OpenCode's fork of the first session at its last message, and
// correctly has no corresponding step-finish here — scion-bridge.js's
// fork-replay exclusion (see that file's routeMessageUpdated doc comment)
// already dropped the two replayed step-finish parts before ever invoking
// sciontool, so this fixture is also the evidence that exclusion works
// against a real `Session.fork` call, not just a synthetic one.
func loadOpencodeHookPayloadFixture(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	path := filepath.Join(root, "pkg", "sciontool", "hooks", "dialects", "testdata", "opencode", "hook-payloads-1.18.33.jsonl")

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			t.Fatalf("decode fixture line %q: %v", line, err)
		}
		records = append(records, data)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(records) != 10 {
		t.Fatalf("fixture record count = %d, want 10", len(records))
	}
	return records
}

func loadOpencodeDialect(t *testing.T) *dialects.MappingDialect {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "opencode", "dialect.yaml")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "opencode" {
		t.Fatalf("dialect name = %q, want opencode", md.Name())
	}
	return md
}

// apiCallTotal sums every gen_ai.api.calls data point, regardless of status.
func apiCallTotal(rm metricdata.ResourceMetrics) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetrycontract.MetricAPICalls {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				total += point.Value
			}
		}
	}
	return total
}

// TestOpencodeDialectFixtureDrivesCanonicalUsage covers design §9 phase 3b's
// ACs against the real captured fixture (see loadOpencodeHookPayloadFixture
// for provenance):
//   - N completed LLM steps give calls = N (three step-finish records here,
//     since scion-bridge.js already excluded the fork's two replayed ones
//     before this fixture was captured);
//   - tokens per token_type match the step-finish parts, with output
//     already summed with reasoning by the bridge (design §3.2's canonical
//     "output" includes reasoning) -- this dialect and handler do no
//     further summation;
//   - agent-end and session events fire (session.created x2, session.idle);
//   - tool_input is populated (not the old bridge's fixed "{}").
func TestOpencodeDialectFixtureDrivesCanonicalUsage(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")

	md := loadOpencodeDialect(t)
	records := loadOpencodeHookPayloadFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(nil, nil, nil, mp)

	var sessionStarts, sessionIdles int
	var toolInputs []string
	var toolSuccesses []bool

	for _, raw := range records {
		event, err := md.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%v): %v", raw, err)
		}
		switch event.Name {
		case hooks.EventSessionStart:
			sessionStarts++
		case hooks.EventAgentEnd:
			sessionIdles++
		case hooks.EventToolStart:
			toolInputs = append(toolInputs, event.Data.ToolInput)
		case hooks.EventToolEnd:
			toolSuccesses = append(toolSuccesses, event.Data.Success)
		}
		// Every fixture record here is a hook-per-process delivery (the
		// normal case: each opencode event invokes a separate sciontool
		// process), so every model-end is "unpaired" -- there is no
		// matching model-start in this process, matching production
		// behavior for opencode (design §5: opencode's dialect never maps
		// anything to model-start).
		if err := h.Handle(event); err != nil {
			t.Fatalf("Handle(%v): %v", event, err)
		}
	}

	if sessionStarts != 2 {
		t.Errorf("session-start count = %d, want 2 (original session + its fork)", sessionStarts)
	}
	if sessionIdles != 1 {
		t.Errorf("agent-end (session.idle) count = %d, want 1", sessionIdles)
	}
	if len(toolInputs) != 2 {
		t.Fatalf("tool-start count = %d, want 2", len(toolInputs))
	}
	if toolInputs[0] != `{"command":"ls -la"}` {
		t.Errorf("first tool_input = %q, want the real bash args (not \"{}\")", toolInputs[0])
	}
	if toolInputs[1] != `{"filePath":"/workspace/project/README.md"}` {
		t.Errorf("second tool_input = %q, want the real read args", toolInputs[1])
	}
	if len(toolSuccesses) != 2 || !toolSuccesses[0] || !toolSuccesses[1] {
		t.Errorf("tool-end successes = %v, want [true, true]", toolSuccesses)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if calls := apiCallTotal(rm); calls != 3 {
		t.Errorf("gen_ai.api.calls total = %d, want 3 (N completed LLM steps -> calls = N)", calls)
	}

	totals := usageTokenTotals(rm)
	// Real captured step-finish tokens (see the fixture provenance comment):
	// step1 input=400 output=25+5(reasoning) cache.read=0 reasoning=5;
	// step2 input=516 output=25 cache.read=384;
	// step3 input=148 output=18 cache.read=1152.
	// The bridge already summed reasoning into output_tokens (30 = 25+5),
	// so this dialect/handler test must see the *summed* value, not 25+25+18.
	wantInput := int64(400 + 516 + 148)
	wantOutput := int64(30 + 25 + 18)
	wantCacheRead := int64(0 + 384 + 1152)
	wantReasoning := int64(5)
	if got := totals[telemetrycontract.TokenTypeInput]; got != wantInput {
		t.Errorf("token_type=input total = %d, want %d", got, wantInput)
	}
	if got := totals[telemetrycontract.TokenTypeOutput]; got != wantOutput {
		t.Errorf("token_type=output total = %d, want %d (already includes reasoning, design §3.2)", got, wantOutput)
	}
	if got := totals[telemetrycontract.TokenTypeCacheRead]; got != wantCacheRead {
		t.Errorf("token_type=cache_read total = %d, want %d", got, wantCacheRead)
	}
	if got := totals[telemetrycontract.TokenTypeReasoning]; got != wantReasoning {
		t.Errorf("token_type=reasoning total = %d, want %d (informational only, not added into output)", got, wantReasoning)
	}
	if got, ok := totals[telemetrycontract.TokenTypeCacheWrite]; ok && got != 0 {
		t.Errorf("token_type=cache_write total = %d, want 0 or absent (this mock model never reports cache writes)", got)
	}
}

// TestOpencodeDialectSuppressesUsageWhenSourceUnset pins design D10 (the
// vetting gate): with SCION_USAGE_SOURCE unset, no usage is published, but
// every other hook signal (tool calls, sessions) still is.
func TestOpencodeDialectSuppressesUsageWhenSourceUnset(t *testing.T) {
	t.Setenv("SCION_HARNESS", "opencode")
	// An empty value is treated as unset.
	t.Setenv("SCION_USAGE_SOURCE", "")

	md := loadOpencodeDialect(t)
	records := loadOpencodeHookPayloadFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(nil, nil, nil, mp)

	for _, raw := range records {
		event, err := md.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%v): %v", raw, err)
		}
		if err := h.Handle(event); err != nil {
			t.Fatalf("Handle(%v): %v", event, err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if calls := apiCallTotal(rm); calls != 0 {
		t.Errorf("gen_ai.api.calls total = %d, want 0 when SCION_USAGE_SOURCE is unset (D10)", calls)
	}
	totals := usageTokenTotals(rm)
	for tokenType, n := range totals {
		if n != 0 {
			t.Errorf("token_type=%s total = %d, want 0 when SCION_USAGE_SOURCE is unset (D10)", tokenType, n)
		}
	}
	// Tool metrics are unaffected by the usage gate (D4, narrow).
	var toolCalls int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "agent.tool.calls" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				toolCalls += point.Value
			}
		}
	}
	if toolCalls != 2 {
		t.Errorf("agent.tool.calls total = %d, want 2 (unaffected by the usage gate)", toolCalls)
	}
}

// TestOpencodeHandlerHasNoDedupeReplayDoubles re-parses and re-handles the
// whole fixture a second time (simulating a retried export, e.g. a
// hook-invocation retry after a transient sciontool error) and asserts the
// canonical totals exactly double. The handler-level counters have no
// dedupe of their own for hook-per-process delivery -- scion-bridge.js is
// solely responsible for not re-invoking sciontool for the same
// (sessionID, messageID, part.id) (see route()'s dedupeKey) -- so this
// pins that the fixture, if replayed, produces a clean, predictable
// doubling rather than some other inconsistency, documenting where the
// dedupe responsibility actually lives.
func TestOpencodeHandlerHasNoDedupeReplayDoubles(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")

	md := loadOpencodeDialect(t)
	records := loadOpencodeHookPayloadFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(nil, nil, nil, mp)

	replay := func() {
		for _, raw := range records {
			event, err := md.Parse(raw)
			if err != nil {
				t.Fatalf("Parse(%v): %v", raw, err)
			}
			if err := h.Handle(event); err != nil {
				t.Fatalf("Handle(%v): %v", event, err)
			}
		}
	}
	replay()
	replay()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if calls := apiCallTotal(rm); calls != 6 {
		t.Errorf("gen_ai.api.calls total after replay = %d, want 6 (2x3; the dedupe boundary is scion-bridge.js, not the handler)", calls)
	}
}

// TestOpencodeDialectStepFinishMappingOmitsExtractTokensFields guards the
// extractTokens override pitfall directly: dialects/common.go's
// extractTokens runs *after* a mapping entry's `fields:`, and reads
// input_tokens/output_tokens/cached_tokens from the raw payload's top level
// regardless of what `fields:` says -- so a `fields:` entry for any of
// those three under message.part.updated.step-finish would be silently
// overridden by extractTokens the moment it ran, which is exactly why
// dialect.yaml has no such entries (the bridge already emits the final
// summed values under those exact top-level keys). This test parses the raw
// YAML directly (bypassing dialects.LoadMappingDialect, which does not
// expose its parsed spec) so a future edit that reintroduces one of these
// keys fails loudly instead of silently zeroing a token type.
func TestOpencodeDialectStepFinishMappingOmitsExtractTokensFields(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "opencode", "dialect.yaml")

	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", specPath, err)
	}
	var spec dialects.MappingDialectSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	entry, ok := spec.Mappings["message.part.updated.step-finish"]
	if !ok {
		t.Fatal(`dialect.yaml has no "message.part.updated.step-finish" mapping`)
	}
	for _, forbidden := range []string{"input_tokens", "output_tokens", "cached_tokens"} {
		if _, present := entry.Fields[forbidden]; present {
			t.Errorf("mapping.fields contains %q; extractTokens (dialects/common.go) runs after `fields:` and reads this key from the top-level payload unconditionally, so this would silently override the bridge's already-correct value", forbidden)
		}
	}
}

// TestOpencodeDialectCacheWriteMapsToNonZeroTokenType covers the
// cache_write mapping path at a non-zero value. The real capture only ever
// has tokens.cache.write=0 (an OpenAI-compatible mock model has no
// cache-write concept), so without this test a typo in either
// dialect.yaml's `cache_write_tokens` field name or scion-bridge.js's
// `cache.write` read would silently zero token_type=cache_write with
// nothing failing. This is a labelled synthetic case: a model-end payload
// shaped like what the bridge would emit for a step-finish with
// tokens.cache.write=7 (see scion-bridge.test.mjs's matching synthetic case
// for the bridge side of this same path).
func TestOpencodeDialectCacheWriteMapsToNonZeroTokenType(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")

	md := loadOpencodeDialect(t)
	payload := map[string]interface{}{
		"hook_event_name":    "message.part.updated.step-finish",
		"session_id":         "ses_synthetic_cache_write",
		"cache_write_tokens": float64(7),
	}
	event, err := md.Parse(payload)
	if err != nil {
		t.Fatalf("Parse(%v): %v", payload, err)
	}
	if event.Name != hooks.EventModelEnd {
		t.Fatalf("event.Name = %q, want %q", event.Name, hooks.EventModelEnd)
	}
	if event.Data.CacheWriteTokens != 7 {
		t.Fatalf("event.Data.CacheWriteTokens = %d, want 7", event.Data.CacheWriteTokens)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(nil, nil, nil, mp)
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle(%v): %v", event, err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	totals := usageTokenTotals(rm)
	if got := totals[telemetrycontract.TokenTypeCacheWrite]; got != 7 {
		t.Errorf("token_type=cache_write total = %d, want 7", got)
	}
}

// TestOpencodeDialectHasNoSessionErrorMapping guards dialect.yaml's
// deliberate omission of a session.error mapping. scion-bridge.js's route()
// never sends this event at all (a session's turn ends exactly once, on
// session.idle, gated on a prior busy/retry -- see that file's
// routeSessionIdle), but this pins the dialect-level fallback: if a raw
// session.error payload ever reached this dialect anyway, it must not
// resolve to agent-end (which would double-count a turn on every error) or
// session-end (which would mark the whole agent Stopped on a recoverable
// error). It stays an unrecognized, inert event name instead.
func TestOpencodeDialectHasNoSessionErrorMapping(t *testing.T) {
	md := loadOpencodeDialect(t)

	payload := map[string]interface{}{
		"hook_event_name": "session.error",
		"session_id":      "ses_synthetic",
		"error":           "synthetic error",
		"reason":          "error",
	}
	event, err := md.Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name == hooks.EventAgentEnd {
		t.Error("session.error must not map to agent-end (would double-count a turn on every error)")
	}
	if event.Name == hooks.EventSessionEnd {
		t.Error("session.error must not map to session-end (would mark the agent Stopped on a recoverable error)")
	}
	if event.Name != "session.error" {
		t.Errorf("event.Name = %q, want the raw name unchanged (no mapping entry exists for session.error)", event.Name)
	}
}

// opencodeRun3AgentEndPayload is the exact sciontool stdin scion-bridge.js
// produces for bus-events-1.18.33.json's run3 (a prompt failing after
// OpenCode's provider retries are exhausted): the captured session.error
// ({name: "APIError", data: {message: "mock upstream failure", ...}}) is
// remembered and attached to that turn's one gated session.idle. The bridge
// test "run3: the captured session.error is carried onto the one agent-end
// as a bounded error string" (scion-bridge.test.mjs) pins that the bridge
// emits exactly this, from the real capture; this replays it through the
// real dialect.yaml and telemetry handler.
var opencodeRun3AgentEndPayload = map[string]interface{}{
	"hook_event_name": "session.idle",
	"session_id":      "ses_f12e82b0fffeTWfXQtofqh9VxA",
	"error":           "APIError: mock upstream failure",
}

// TestOpencodeDialectCarriesSessionErrorOntoTurnEndSpan is ptone/scion#2244:
// a failed turn's error status reaches the agent.turn.end span, while the
// turn is still a single agent-end.
func TestOpencodeDialectCarriesSessionErrorOntoTurnEndSpan(t *testing.T) {
	md := loadOpencodeDialect(t)

	event, err := md.Parse(opencodeRun3AgentEndPayload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name != hooks.EventAgentEnd {
		t.Fatalf("event.Name = %q, want %q", event.Name, hooks.EventAgentEnd)
	}
	if event.Data.Error != "APIError: mock upstream failure" {
		t.Errorf("event.Data.Error = %q, want the bridge's carried session.error text", event.Data.Error)
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(tp, nil, nil)
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended span count = %d, want 1 (one turn, one agent.turn.end)", len(spans))
	}
	if spans[0].Name() != "agent.turn.end" {
		t.Errorf("span name = %q, want agent.turn.end", spans[0].Name())
	}
	if got := spans[0].Status(); got.Code != codes.Error || got.Description != "APIError: mock upstream failure" {
		t.Errorf("span status = %+v, want Error with the carried session.error text", got)
	}

	// A clean turn's session.idle (run2's, from the hook-payload fixture)
	// still maps to agent-end with no error.
	clean, err := md.Parse(map[string]interface{}{"hook_event_name": "session.idle", "session_id": "ses_clean"})
	if err != nil {
		t.Fatalf("Parse clean: %v", err)
	}
	if clean.Name != hooks.EventAgentEnd || clean.Data.Error != "" {
		t.Errorf("clean session.idle = (%q, error %q), want agent-end with no error", clean.Name, clean.Data.Error)
	}
}

// TestOpencodeDialectModelLabelFromPayload is ptone/scion#2242 for
// opencode: the bridge's joined provider/model on each step-finish wins
// over SCION_MODEL for the model label on both usage metrics (design §3.2).
func TestOpencodeDialectModelLabelFromPayload(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")
	t.Setenv("SCION_MODEL", "configured-model-should-lose")

	md := loadOpencodeDialect(t)
	records := loadOpencodeHookPayloadFixture(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	h := NewTelemetryHandler(nil, nil, nil, mp)

	for _, raw := range records {
		event, err := md.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%v): %v", raw, err)
		}
		if event.Name == hooks.EventModelEnd && event.Data.Model != "mockprov/mock-model" {
			t.Errorf("model-end event.Data.Model = %q, want mockprov/mock-model", event.Data.Model)
		}
		if err := h.Handle(event); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, name := range []string{telemetrycontract.MetricAPICalls, telemetrycontract.MetricUsageTokens} {
		points := int64CounterDataPoints(rm, name)
		if len(points) == 0 {
			t.Fatalf("%s has no data points", name)
		}
		for _, p := range points {
			got, _ := p.Attributes.Value(attribute.Key(telemetrycontract.ModelLabel))
			if got.AsString() != "mockprov/mock-model" {
				t.Errorf("%s model label = %q, want mockprov/mock-model (payload over SCION_MODEL)", name, got.AsString())
			}
		}
	}
}
