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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// This file is the single home of the harness-readiness tables: which
// harnesses report readiness through hooks, and which normalized hook events
// count as "the harness is up and reading input". `sciontool init` records the
// mode in state.HarnessInitRunFile; `sciontool hook` writes
// state.HarnessReadyFile on the first qualifying event. The agent runtime
// waits on these files before pasting a buffered message.

// hookReadyHarnesses lists the SCION_HARNESS values whose hooks fire once the
// harness is reading input, so the runtime can wait for the ready marker.
// Every other harness uses settle mode. codex is deliberately absent: its
// SessionStart fires only after the first prompt, so it can never gate the
// delivery of that prompt.
var hookReadyHarnesses = map[string]bool{
	"claude":     true,
	"gemini":     true,
	"gemini-cli": true,
	"copilot":    true,
	"muse":       true,
	"muse-code":  true,
	"grok":       true,
	"grok-build": true,
	"opencode":   true,
}

// HarnessReadyMode returns the readiness mode (state.ReadyModeHook or
// state.ReadyModeSettle) for a SCION_HARNESS value. Unknown and empty values
// use settle mode.
func HarnessReadyMode(harness string) string {
	if hookReadyHarnesses[strings.ToLower(strings.TrimSpace(harness))] {
		return state.ReadyModeHook
	}
	return state.ReadyModeSettle
}

// readinessEvents lists the normalized events (after dialect mapping) that
// show the harness is running and reading input. Scion lifecycle events
// (pre-start, post-start, pre-stop) and session-end never qualify.
// session-start qualifies only for a startup or resume source; see
// IsReadinessEvent.
var readinessEvents = map[string]bool{
	EventSessionStart:     true,
	EventPromptSubmit:     true,
	EventAgentStart:       true,
	EventModelStart:       true,
	EventModelEnd:         true,
	EventToolStart:        true,
	EventToolEnd:          true,
	EventAgentEnd:         true,
	EventNotification:     true,
	EventResponseComplete: true,
}

// IsReadinessEvent reports whether a normalized event marks the harness as
// ready for input. A session-start counts only when its source is startup or
// resume (or absent, for dialects that do not report one); clear and compact
// restart a session inside a harness that was already ready, and are not
// counted.
func IsReadinessEvent(ev *Event) bool {
	if ev == nil || !readinessEvents[ev.Name] {
		return false
	}
	if ev.Name == EventSessionStart {
		switch strings.ToLower(ev.Data.Source) {
		case "", "startup", "resume":
			return true
		default:
			return false
		}
	}
	return true
}
