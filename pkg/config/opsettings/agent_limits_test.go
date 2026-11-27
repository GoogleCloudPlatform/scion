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

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAgentLimitsDoc_Valid(t *testing.T) {
	doc, err := ParseAgentLimitsDoc(json.RawMessage(`{"max_turns":50,"max_model_calls":200,"max_duration":"2h"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.MaxTurns != 50 || doc.MaxModelCalls != 200 || doc.MaxDuration != "2h" {
		t.Fatalf("got %+v", doc)
	}
	if doc.IsEmpty() {
		t.Error("expected non-empty")
	}
	wire := doc.ToAPI()
	if wire == nil || wire.MaxTurns != 50 || wire.MaxModelCalls != 200 || wire.MaxDuration != "2h" {
		t.Fatalf("ToAPI: got %+v", wire)
	}

	empty, err := ParseAgentLimitsDoc(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty doc error: %v", err)
	}
	if !empty.IsEmpty() || empty.ToAPI() != nil {
		t.Errorf("empty doc: IsEmpty=%v ToAPI=%+v", empty.IsEmpty(), empty.ToAPI())
	}
}

func TestParseAgentLimitsDoc_RefusesInvalid(t *testing.T) {
	for _, raw := range []string{
		`{"unknown_key": 1}`,
		`{"max_turns": -1}`,
		`{"max_model_calls": -5}`,
		`{"max_duration": "0"}`,
		`{"max_duration": "0s"}`,
		`{"max_duration": "-5m"}`,
		`{"max_duration": "not-a-duration"}`,
		`{"max_duration": " 2h"}`,
		`{"max_duration": "2h "}`,
		`{"max_duration": "2h\t"}`,
		`{"max_turns": 10} {"extra": true}`,
	} {
		if _, err := ParseAgentLimitsDoc(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseAgentLimitsDoc(%s): want error, got nil", raw)
		}
		if errs := Validate(AgentLimitsSectionName, json.RawMessage(raw)); len(errs) == 0 {
			t.Errorf("Validate(agent_limits, %s): want error, got nil", raw)
		}
	}
}

func TestValidateAgentDefaultsDocAgainstLimits(t *testing.T) {
	limits := AgentLimitsSettings{
		MaxTurns:      50,
		MaxModelCalls: 100,
		MaxDuration:   "1h",
	}

	t.Run("over ceiling refused and names both keys", func(t *testing.T) {
		errs := ValidateAgentDefaultsDocAgainstLimits(json.RawMessage(`{"default_max_turns":80}`), limits)
		if len(errs) != 1 {
			t.Fatalf("want 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Message, "agent_defaults.default_max_turns") ||
			!strings.Contains(errs[0].Message, "agent_limits.max_turns") {
			t.Errorf("error should name both keys: %q", errs[0].Message)
		}
	})

	t.Run("explicit unlimited refused while ceiling exists", func(t *testing.T) {
		errs := ValidateAgentDefaultsDocAgainstLimits(
			json.RawMessage(`{"default_max_turns":0,"default_max_model_calls":0,"default_max_duration":""}`),
			limits,
		)
		if len(errs) != 3 {
			t.Fatalf("want 3 errors, got %d: %v", len(errs), errs)
		}
	})

	t.Run("duration zero and over ceiling refused", func(t *testing.T) {
		if errs := ValidateAgentDefaultsDocAgainstLimits(json.RawMessage(`{"default_max_duration":"0"}`), limits); len(errs) != 1 {
			t.Errorf("default_max_duration=0: want 1 error, got %v", errs)
		}
		if errs := ValidateAgentDefaultsDocAgainstLimits(json.RawMessage(`{"default_max_duration":"2h"}`), limits); len(errs) != 1 {
			t.Errorf("default_max_duration=2h: want 1 error, got %v", errs)
		}
		for _, padded := range []string{`{"default_max_duration":" 5m"}`, `{"default_max_duration":"5m "}`, `{"default_max_duration":"30m\t"}`} {
			if errs := ValidateAgentDefaultsDocAgainstLimits(json.RawMessage(padded), limits); len(errs) != 1 {
				t.Errorf("%s: want 1 error, got %v", padded, errs)
			}
		}
	})

	t.Run("within ceilings or omitted allowed", func(t *testing.T) {
		for _, doc := range []string{
			`{}`,
			`{"default_max_turns":50,"default_max_model_calls":100,"default_max_duration":"1h"}`,
			`{"default_max_turns":25,"default_max_duration":"30m"}`,
		} {
			if errs := ValidateAgentDefaultsDocAgainstLimits(json.RawMessage(doc), limits); len(errs) != 0 {
				t.Errorf("doc %s: unexpected errors: %v", doc, errs)
			}
		}
	})

	t.Run("no ceilings allows anything", func(t *testing.T) {
		errs := ValidateAgentDefaultsDocAgainstLimits(
			json.RawMessage(`{"default_max_turns":0,"default_max_model_calls":999,"default_max_duration":"0"}`),
			AgentLimitsSettings{},
		)
		if len(errs) != 0 {
			t.Errorf("expected no errors without ceilings, got %v", errs)
		}
	})
}
