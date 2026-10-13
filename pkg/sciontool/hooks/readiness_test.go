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

package hooks

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestIsReadinessEvent(t *testing.T) {
	yes := []*Event{
		{Name: EventSessionStart, Data: EventData{Source: "startup"}},
		{Name: EventSessionStart, Data: EventData{Source: "resume"}},
		{Name: EventSessionStart, Data: EventData{Source: "new"}},
		{Name: EventSessionStart, Data: EventData{Source: "something-else"}},
		{Name: EventSessionStart},
		{Name: EventPromptSubmit}, {Name: EventAgentStart}, {Name: EventModelStart},
		{Name: EventModelEnd}, {Name: EventToolStart}, {Name: EventToolEnd},
		{Name: EventAgentEnd}, {Name: EventNotification}, {Name: EventResponseComplete},
	}
	no := []*Event{
		nil,
		{Name: EventSessionStart, Data: EventData{Source: "clear"}},
		{Name: EventSessionStart, Data: EventData{Source: "compact"}},
		{Name: EventSessionStart, Data: EventData{Source: "Clear"}},
		{Name: EventPreStart}, {Name: EventPostStart}, {Name: EventPreStop},
		{Name: EventSessionEnd}, {Name: EventSubagentEnd}, {Name: "SessionStart"},
	}
	for _, ev := range yes {
		if !IsReadinessEvent(ev) {
			t.Errorf("IsReadinessEvent(%+v) = false, want true", ev)
		}
	}
	for _, ev := range no {
		if IsReadinessEvent(ev) {
			t.Errorf("IsReadinessEvent(%+v) = true, want false", ev)
		}
	}
}

// TestClassifySessionStart covers every source in the design's table, plus
// case and space handling and unknown values.
func TestClassifySessionStart(t *testing.T) {
	tests := []struct {
		source string
		want   SessionStartKind
	}{
		{"startup", SessionNew},
		{"resume", SessionNew},
		{"new", SessionNew},
		{"fork", SessionNew},
		{"", SessionNew},
		{"   ", SessionNew},
		{"something-else", SessionNew},
		{"weird", SessionNew},
		{"clear", SessionInPlace},
		{"compact", SessionInPlace},
		{"Clear", SessionInPlace},
		{" COMPACT\n", SessionInPlace},
		{"Startup", SessionNew},
	}
	for _, tt := range tests {
		if got := ClassifySessionStart(tt.source); got != tt.want {
			t.Errorf("ClassifySessionStart(%q) = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestLoadReadySpec_MissingEnvDefaultsDelay(t *testing.T) {
	want := api.ReadySpec{Strategy: state.ReadyStrategyDelay, Settle: 8 * time.Second}
	for name, env := range map[string]map[string]string{
		"missing":              {},
		"empty":                {state.HarnessReadyEnv: ""},
		"not JSON":             {state.HarnessReadyEnv: "strategy=hook"},
		"JSON null":            {state.HarnessReadyEnv: "null"},
		"Go field names":       {state.HarnessReadyEnv: `{"Strategy":"hook","Settle":1500000000,"Timeout":90000000000}`},
		"unknown strategy":     {state.HarnessReadyEnv: `{"strategy":"pane","settle_ms":500,"timeout_ms":30000}`},
		"hook without timeout": {state.HarnessReadyEnv: `{"strategy":"hook","settle_ms":1500}`},
		"delay with timeout":   {state.HarnessReadyEnv: `{"strategy":"delay","settle_ms":8000,"timeout_ms":30000}`},
		"settle over max":      {state.HarnessReadyEnv: `{"strategy":"delay","settle_ms":61000}`},
		"negative settle":      {state.HarnessReadyEnv: `{"strategy":"delay","settle_ms":-1}`},
	} {
		got := LoadReadySpec(func(k string) string { return env[k] })
		if got != want {
			t.Errorf("%s: LoadReadySpec = %+v, want %+v", name, got, want)
		}
	}
}

func TestLoadReadySpec_ValidEnv(t *testing.T) {
	for _, spec := range []api.ReadySpec{
		{Strategy: state.ReadyStrategyHook, Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second},
		{Strategy: state.ReadyStrategyHook, Settle: 0, Timeout: 10 * time.Second},
		{Strategy: state.ReadyStrategyDelay, Settle: 3 * time.Second},
	} {
		raw := state.FormatReadySpecEnv(spec)
		got := LoadReadySpec(func(k string) string {
			if k == state.HarnessReadyEnv {
				return raw
			}
			return ""
		})
		if got != spec {
			t.Errorf("LoadReadySpec(%s) = %+v, want %+v", raw, got, spec)
		}
	}
	// Unknown fields from a newer broker are ignored.
	got := LoadReadySpec(func(string) string {
		return `{"strategy":"hook","settle_ms":2000,"timeout_ms":30000,"pane_regex":"x"}`
	})
	if want := (api.ReadySpec{Strategy: "hook", Settle: 2 * time.Second, Timeout: 30 * time.Second}); got != want {
		t.Errorf("unknown field: got %+v, want %+v", got, want)
	}
}
