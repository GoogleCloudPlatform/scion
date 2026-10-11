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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestHarnessReadyMode(t *testing.T) {
	hook := []string{"claude", "gemini", "gemini-cli", "copilot", "grok", "grok-build", "Claude"}
	settle := []string{"codex", "opencode", "muse", "muse-code", "antigravity", "hermes", "generic", "", "unknown-harness"}
	for _, h := range hook {
		if got := HarnessReadyMode(h); got != state.ReadyModeHook {
			t.Errorf("HarnessReadyMode(%q) = %q, want hook", h, got)
		}
	}
	for _, h := range settle {
		if got := HarnessReadyMode(h); got != state.ReadyModeSettle {
			t.Errorf("HarnessReadyMode(%q) = %q, want settle", h, got)
		}
	}
}

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
