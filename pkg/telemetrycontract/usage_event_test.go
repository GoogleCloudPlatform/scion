// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetrycontract

import (
	"reflect"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

func validUsageEvent() *UsageEvent {
	env := NewEnvelope(EventTypeUsage, "u1-0123", "sciontool/project/p/agent/a", testEventTS, testObservedTS)
	env.HubID, env.ProjectID, env.AgentID, env.LaunchID = "hub-1", "p", "a", "launch-1"
	return &UsageEvent{
		Envelope:             env,
		UsageSource:          UsageSourceNative,
		SourceKind:           "claude.api_request",
		SourceSchemaVersion:  "1",
		SourceEventID:        "rid1:abcd",
		SourceIdentity:       SourceIdentityRequestID,
		SourceOccurrence:     1,
		SourceTime:           "2026-10-22T00:05:00.123456789Z",
		Harness:              "claude",
		Model:                "claude-sonnet-5",
		ModelSource:          ModelSourceEvent,
		SessionID:            "s1",
		Status:               StatusSuccess,
		Calls:                1,
		Input:                int64p(2),
		Output:               int64p(41),
		CacheRead:            int64p(0),
		CacheWrite:           int64p(26607),
		InputDerivation:      InputDerivationReported,
		TokenStatus:          TokenStatusReported,
		ReportedCostMicros:   int64p(66931),
		ReportedCostCurrency: "USD",
		ReportedCostSource:   "harness",
	}
}

// Hand-written golden: field order, omitted reasoning (Claude reports
// none), a reported zero cache_read, full-precision source_time.
const usageGolden = `{"schema_version":1,"event_type":"agent.usage","event_id":"u1-0123","producer_namespace":"sciontool/project/p/agent/a","event_ts":"2026-10-22T00:05:00Z","observed_ts":"2026-10-22T00:20:00.123456Z","hub_id":"hub-1","project_id":"p","agent_id":"a","launch_id":"launch-1","usage_source":"native","source_kind":"claude.api_request","source_schema_version":"1","source_event_id":"rid1:abcd","source_identity":"request_id","source_occurrence":1,"source_time":"2026-10-22T00:05:00.123456789Z","harness":"claude","model":"claude-sonnet-5","model_source":"event","session_id":"s1","status":"success","calls":1,"input":2,"output":41,"cache_read":0,"cache_write":26607,"input_derivation":"reported","token_status":"reported","reported_cost_micros":66931,"reported_cost_currency":"USD","reported_cost_source":"harness"}`

func TestUsageEventGoldenAndRoundTrip(t *testing.T) {
	ev := validUsageEvent()
	got, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != usageGolden {
		t.Errorf("golden mismatch\n got %s\nwant %s", got, usageGolden)
	}
	var back UsageEvent
	if err := Unmarshal(got, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&back, ev) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, *ev)
	}
}

// Antigravity-style calls-only event: every token field omitted, never 0.
func TestUsageEventCallsOnlyOmitsTokens(t *testing.T) {
	ev := validUsageEvent()
	ev.UsageSource, ev.SourceKind, ev.Harness = UsageSourceHooks, "hook.antigravity.model_end", "antigravity"
	ev.SourceIdentity, ev.SourceEventID = SourceIdentityComposite, "cmp1:ff"
	ev.Input, ev.Output, ev.CacheRead, ev.CacheWrite, ev.InputDerivation = nil, nil, nil, nil, ""
	ev.ReportedCostMicros, ev.ReportedCostCurrency, ev.ReportedCostSource = nil, "", ""
	ev.TokenStatus = TokenStatusNotReported
	got, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{`"input"`, `"output"`, `"cache_read"`, `"cache_write"`, `"reasoning"`, `null`} {
		if strings.Contains(string(got), field) {
			t.Errorf("calls-only event contains %s: %s", field, got)
		}
	}
	if !strings.Contains(string(got), `"calls":1`) || !strings.Contains(string(got), `"token_status":"not_reported"`) {
		t.Errorf("calls-only event = %s", got)
	}
}

func TestUsageEventValidate(t *testing.T) {
	cases := map[string]func(*UsageEvent){
		"wrong event type":          func(e *UsageEvent) { e.EventType = EventTypeLifecycle },
		"bad usage source":          func(e *UsageEvent) { e.UsageSource = "both" },
		"missing source kind":       func(e *UsageEvent) { e.SourceKind = "" },
		"request id without id":     func(e *UsageEvent) { e.SourceEventID = "" },
		"none with id":              func(e *UsageEvent) { e.SourceIdentity = SourceIdentityNone; e.IdentityAmbiguous = true },
		"none not ambiguous":        func(e *UsageEvent) { e.SourceIdentity, e.SourceEventID = SourceIdentityNone, "" },
		"unknown identity":          func(e *UsageEvent) { e.SourceIdentity = "fuzzy" },
		"occurrence zero":           func(e *UsageEvent) { e.SourceOccurrence = 0 },
		"repeat not ambiguous":      func(e *UsageEvent) { e.SourceOccurrence = 2 },
		"bad model source":          func(e *UsageEvent) { e.ModelSource = "guess" },
		"bad status":                func(e *UsageEvent) { e.Status = "ok" },
		"negative calls":            func(e *UsageEvent) { e.Calls = -1 },
		"negative output":           func(e *UsageEvent) { e.Output = int64p(-1) },
		"overlap without reasoning": func(e *UsageEvent) { e.ReasoningOverlapsOutput = boolp(true) },
		"input without derivation":  func(e *UsageEvent) { e.InputDerivation = "" },
		"derivation without input":  func(e *UsageEvent) { e.Input = nil },
		"bad token status":          func(e *UsageEvent) { e.TokenStatus = "ok" },
		"malformed without fields":  func(e *UsageEvent) { e.TokenStatus = TokenStatusMalformed },
		"fields without malformed":  func(e *UsageEvent) { e.MalformedFields = []string{"input_tokens"} },
		"not reported with tokens":  func(e *UsageEvent) { e.TokenStatus = TokenStatusNotReported },
		"bad source time":           func(e *UsageEvent) { e.SourceTime = "yesterday" },
		"interval without end":      func(e *UsageEvent) { e.IntervalStart, e.Temporality = FormatTS(testEventTS), TemporalityDelta },
		"interval bad temporality": func(e *UsageEvent) {
			e.IntervalStart, e.IntervalEnd, e.Temporality = FormatTS(testEventTS), FormatTS(testEventTS), "gauge"
		},
		"cost without currency": func(e *UsageEvent) { e.ReportedCostCurrency = "" },
		"currency without cost": func(e *UsageEvent) { e.ReportedCostMicros = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := validUsageEvent()
			mutate(ev)
			if err := ev.Validate(); err == nil {
				t.Error("Validate() = nil, want error")
			}
			if _, err := Marshal(ev); err == nil {
				t.Error("Marshal accepted an invalid usage event")
			}
		})
	}
	ok := validUsageEvent()
	ok.Reasoning, ok.ReasoningOverlapsOutput = int64p(5), boolp(true)
	ok.SourceOccurrence, ok.IdentityAmbiguous = 2, true
	ok.IntervalStart, ok.IntervalEnd, ok.Temporality = FormatTS(testEventTS), FormatTS(testObservedTS), TemporalityCumulativeDelta
	ok.TokenStatus, ok.MalformedFields = TokenStatusMalformed, []string{"cache_read_tokens"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid variant rejected: %v", err)
	}
}

// The shared mock seed's usage row (mock-events.jsonl u1) decodes into
// UsageEvent with its token values intact.
func TestUsageEventDecodesMockSeedRow(t *testing.T) {
	row := `{"schema_version": 1, "event_id": "u1", "producer_namespace": "mock-hub/agent-a/inc-1", "event_type": "agent.usage", "event_ts": "2026-10-22T00:05:00Z", "observed_ts": "2026-10-22T00:20:00Z", "hub_id": "mock-hub", "project_id": "mock-project", "agent_id": "agent-a", "incarnation_id": "inc-1", "launch_id": "launch-1", "broker_id": "mock-broker", "producer_build": "mock-v0", "source_kind": "synthetic", "source_event_id": "u1-source", "source_schema_version": "fixture-1", "metadata_revision": 1, "harness": "claude", "model": "mock-model", "model_source": "fixture", "calls": 1, "status": "success", "input": 100, "output": 20, "cache_read": 80, "cache_write": 10, "reasoning": 2, "reasoning_overlaps_output": true}`
	var ev UsageEvent
	// The seed predates the WP2 enums (model_source "fixture", no
	// usage_source/source_identity), so decode without Validate.
	if err := jsonUnmarshalForTest([]byte(row), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if *ev.Input != 100 || *ev.Output != 20 || *ev.CacheRead != 80 || *ev.CacheWrite != 10 || *ev.Reasoning != 2 || !*ev.ReasoningOverlapsOutput {
		t.Errorf("decoded tokens = %+v", ev)
	}
}
