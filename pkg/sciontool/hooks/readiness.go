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

// hookReadyHarnesses lists the SCION_HARNESS values whose hooks fire at
// startup, before any user input, so the runtime can wait for the ready
// marker. Every other harness uses settle mode. Absent on purpose:
//   - codex: its SessionStart fires only after the first prompt, so it can
//     never gate the delivery of that prompt.
//   - opencode: its only early event, session.created, fires when a session
//     is created, which the captured runs show only after a client creates
//     one; no capture shows an event before the first prompt.
//   - muse and muse-code: no capture shows an event before the first prompt.
var hookReadyHarnesses = map[string]bool{
	"claude":     true,
	"gemini":     true,
	"gemini-cli": true,
	// copilot: hook mode relies on the documented sessionStart timing (fires
	// when a new or resumed session begins); it has not been confirmed with
	// a startup capture. If sessionStart fires lazily, the first message
	// waits until the delivery deadline.
	"copilot":    true,
	"grok":       true,
	"grok-build": true,
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
// session-start qualifies for every source except those in
// nonReadinessSessionSources; see IsReadinessEvent.
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

// nonReadinessSessionSources are the session-start sources that do not
// count: clear and compact restart a session inside a harness that was
// already running, so they say nothing about a new run being ready. Any
// future source that restarts a session inside a running harness must be
// added here.
var nonReadinessSessionSources = map[string]bool{
	"clear":   true,
	"compact": true,
}

// IsReadinessEvent reports whether a normalized event marks the harness as
// ready for input. A session-start counts for any source (startup, resume,
// new, none, ...) except clear and compact.
func IsReadinessEvent(ev *Event) bool {
	if ev == nil || !readinessEvents[ev.Name] {
		return false
	}
	if ev.Name == EventSessionStart {
		return !nonReadinessSessionSources[strings.ToLower(strings.TrimSpace(ev.Data.Source))]
	}
	return true
}
