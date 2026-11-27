/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func hookAnalytics(t *testing.T) (*telemetry.UsageAnalytics, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outbox")
	o, err := telemetry.OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	t.Setenv("SCION_MODEL", "")
	return telemetry.NewUsageAnalytics(o, telemetrycontract.UsageSourceHooks), dir
}

func hookStagedEvents(t *testing.T, dir string) []telemetrycontract.UsageEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "staging.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []telemetrycontract.UsageEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev telemetrycontract.UsageEvent
		if err := telemetrycontract.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// The captured agy 1.2.12 sequence has three PostInvocation (model-end)
// records (invocationNum 0 and 1 in turn one, 0 in turn two) and no token
// fields at all. Hand expectation: exactly three calls-only events with
// every token category omitted, three distinct composite identities, none
// ambiguous. (The auxiliary title request is invisible to hooks: a known,
// explicit capability gap, not something this producer can count.)
func TestHookAnalyticsAntigravityFixtureCallsOnly(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "antigravity")
	analytics, dir := hookAnalytics(t)
	_, events := loadAntigravityFixture(t)

	h := NewTelemetryHandler(nil, nil, nil)
	h.Analytics = analytics
	for _, ev := range events {
		if err := h.Handle(ev); err != nil {
			t.Fatal(err)
		}
	}

	got := hookStagedEvents(t, dir)
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3 (one per PostInvocation)", len(got))
	}
	ids := map[string]bool{}
	for _, ev := range got {
		if ev.SourceKind != "hook.antigravity.model_end" || ev.UsageSource != "hooks" || ev.Harness != "antigravity" {
			t.Errorf("event kind/source = %q %q %q", ev.SourceKind, ev.UsageSource, ev.Harness)
		}
		if ev.Calls != 1 || ev.Status != "success" || ev.TokenStatus != "not_reported" {
			t.Errorf("calls %d status %q token_status %q", ev.Calls, ev.Status, ev.TokenStatus)
		}
		if ev.Input != nil || ev.Output != nil || ev.CacheRead != nil || ev.CacheWrite != nil || ev.Reasoning != nil || ev.InputDerivation != "" {
			t.Errorf("antigravity tokens must be unknown, never zero: %+v", ev)
		}
		if ev.Model != "gemini-3.1-pro-low" || ev.ModelSource != "event" || ev.SessionID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("model %q (%s) session %q", ev.Model, ev.ModelSource, ev.SessionID)
		}
		if ev.SourceIdentity != "composite" || ev.IdentityAmbiguous || ev.SourceOccurrence != 1 {
			t.Errorf("identity = %s ambiguous %v occurrence %d", ev.SourceIdentity, ev.IdentityAmbiguous, ev.SourceOccurrence)
		}
		ids[ev.SourceEventID] = true
	}
	if len(ids) != 3 {
		t.Errorf("composite identities must be distinct per invocation: %v", ids)
	}
	// Replaying the same hook payloads (a harness re-invoking a hook) is
	// never silently collapsed: the repeats are ambiguous occurrences.
	for _, ev := range events {
		_ = h.Handle(ev)
	}
	got = hookStagedEvents(t, dir)
	ambiguous := 0
	for _, ev := range got {
		if ev.IdentityAmbiguous {
			ambiguous++
		}
	}
	if len(got) != 6 || ambiguous != 3 {
		t.Errorf("after replay: %d events, %d ambiguous; want 6 and 3", len(got), ambiguous)
	}
}

// Exactly one producer: the hook path writes analytics only when
// SCION_USAGE_SOURCE=hooks (native: the UsageDeriver owns usage; unset or
// unrecognized: no usage at all), the same gate as its counters.
func TestHookAnalyticsFollowsUsageSourceSelection(t *testing.T) {
	for _, source := range []string{"native", "", "HOOKS"} {
		t.Run("source="+source, func(t *testing.T) {
			t.Setenv("SCION_USAGE_SOURCE", source)
			t.Setenv("SCION_HARNESS", "antigravity")
			analytics, dir := hookAnalytics(t)
			_, events := loadAntigravityFixture(t)
			h := NewTelemetryHandler(nil, nil, nil)
			h.Analytics = analytics
			for _, ev := range events {
				_ = h.Handle(ev)
			}
			if got := hookStagedEvents(t, dir); len(got) != 0 {
				t.Errorf("SCION_USAGE_SOURCE=%q wrote %d hook events", source, len(got))
			}
		})
	}
}

// Hook token values: directly populated positive categories without
// TokenPresence are treated as present while omitted zero fields remain
// unknown (partial); an unvetted dialect has no identity and is explicitly
// ambiguous; errors are failed calls.
func TestHookAnalyticsPositiveTokensAndUnvettedIdentity(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")
	analytics, dir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil)
	h.Analytics = analytics

	_ = h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Dialect: "opencode", Data: hooks.EventData{
		Model: "provider/model", InputTokens: 10, OutputTokens: 7, ReasoningTokens: 3, SessionID: "s",
	}})
	_ = h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Dialect: "opencode", Data: hooks.EventData{Error: "rate limited"}})
	_ = h.Handle(&hooks.Event{Name: hooks.EventToolEnd, Dialect: "opencode", Data: hooks.EventData{ToolName: "x"}})

	got := hookStagedEvents(t, dir)
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2 (tool events are not usage)", len(got))
	}
	ok, failed := got[0], got[1]
	if ok.Input == nil || *ok.Input != 10 || ok.Output == nil || *ok.Output != 7 || ok.Reasoning == nil || *ok.Reasoning != 3 {
		t.Errorf("positive tokens = %+v", ok)
	}
	if ok.CacheRead != nil || ok.CacheWrite != nil || ok.TokenStatus != "partial" || ok.InputDerivation != "reported" {
		t.Errorf("zero/absent categories must be unknown: cr=%v cw=%v status=%q", ok.CacheRead, ok.CacheWrite, ok.TokenStatus)
	}
	if ok.ReasoningOverlapsOutput == nil || !*ok.ReasoningOverlapsOutput {
		t.Error("hook reasoning is included in output")
	}
	if ok.SourceIdentity != "none" || !ok.IdentityAmbiguous || ok.SourceEventID != "" {
		t.Errorf("unvetted dialect identity = %s ambiguous %v", ok.SourceIdentity, ok.IdentityAmbiguous)
	}
	if failed.Status != "error" || failed.Calls != 1 || failed.TokenStatus != "not_reported" || failed.ModelSource != "unknown" {
		t.Errorf("failed call = %+v", failed)
	}
}

func TestRawScalar(t *testing.T) {
	raw := map[string]interface{}{"s": "x", "i": float64(3), "f": 1.5, "b": true, "m": map[string]interface{}{}}
	for key, want := range map[string]string{"s": "x", "i": "3", "f": "1.5", "b": "true", "m": "", "absent": ""} {
		if got := rawScalar(raw, key); got != want {
			t.Errorf("rawScalar(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestHookAnalyticsMeasuredZerosAndTokenStatus(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "opencode")
	analytics, dir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil)
	h.Analytics = analytics

	// 1. All 5 fields present, with measured zeros for cache_write and reasoning -> reported.
	fullData := hooks.EventData{Model: "provider/model", SessionID: "s1"}
	fullData.SetInputTokens(400)
	fullData.SetOutputTokens(25)
	fullData.SetCachedTokens(100)
	fullData.SetCacheWriteTokens(0)
	fullData.SetReasoningTokens(0)
	if err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Dialect: "opencode", Data: fullData}); err != nil {
		t.Fatal(err)
	}

	// 2. Subset of fields present (some measured 0, some absent) -> partial.
	partialData := hooks.EventData{Model: "provider/model", SessionID: "s1"}
	partialData.SetInputTokens(50)
	partialData.SetOutputTokens(0)
	partialData.SetCacheWriteTokens(0)
	if err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Dialect: "opencode", Data: partialData}); err != nil {
		t.Fatal(err)
	}

	// 3. No token fields present -> not_reported.
	if err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Dialect: "opencode", Data: hooks.EventData{Model: "provider/model", SessionID: "s1"}}); err != nil {
		t.Fatal(err)
	}

	// Verify raw JSONL preserves explicit 0s for reported event.
	rawBytes, err := os.ReadFile(filepath.Join(dir, "staging.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawBytes)), "\n")
	if len(lines) != 3 {
		t.Fatalf("staged lines = %d, want 3", len(lines))
	}
	if !strings.Contains(lines[0], `"cache_write":0`) || !strings.Contains(lines[0], `"reasoning":0`) {
		t.Errorf("full event JSONL must serialize measured 0s, got %s", lines[0])
	}

	got := hookStagedEvents(t, dir)
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}

	full := got[0]
	if full.TokenStatus != telemetrycontract.TokenStatusReported {
		t.Errorf("full.TokenStatus = %q, want %q", full.TokenStatus, telemetrycontract.TokenStatusReported)
	}
	if full.Input == nil || *full.Input != 400 || full.Output == nil || *full.Output != 25 ||
		full.CacheRead == nil || *full.CacheRead != 100 ||
		full.CacheWrite == nil || *full.CacheWrite != 0 ||
		full.Reasoning == nil || *full.Reasoning != 0 {
		t.Errorf("full tokens = %+v, want 400/25/100/0/0", full)
	}
	if full.InputDerivation != telemetrycontract.InputDerivationReported {
		t.Errorf("full.InputDerivation = %q, want reported", full.InputDerivation)
	}
	if full.ReasoningOverlapsOutput == nil || !*full.ReasoningOverlapsOutput {
		t.Errorf("full.ReasoningOverlapsOutput = %v, want true", full.ReasoningOverlapsOutput)
	}

	partial := got[1]
	if partial.TokenStatus != telemetrycontract.TokenStatusPartial {
		t.Errorf("partial.TokenStatus = %q, want %q", partial.TokenStatus, telemetrycontract.TokenStatusPartial)
	}
	if partial.Input == nil || *partial.Input != 50 || partial.Output == nil || *partial.Output != 0 ||
		partial.CacheWrite == nil || *partial.CacheWrite != 0 ||
		partial.CacheRead != nil || partial.Reasoning != nil {
		t.Errorf("partial tokens = %+v, want in=50 out=0 cw=0 cr=nil reas=nil", partial)
	}

	none := got[2]
	if none.TokenStatus != telemetrycontract.TokenStatusNotReported {
		t.Errorf("none.TokenStatus = %q, want %q", none.TokenStatus, telemetrycontract.TokenStatusNotReported)
	}
	if none.Input != nil || none.Output != nil || none.CacheRead != nil || none.CacheWrite != nil || none.Reasoning != nil {
		t.Errorf("none tokens must all be nil, got %+v", none)
	}
}
