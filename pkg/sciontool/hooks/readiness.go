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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// This file is the single home of the harness-readiness rules: how the
// harness's ready spec reaches `sciontool init`, which normalized hook events
// count as "the harness is up and reading input", and how a session-start is
// classified by its source. `sciontool init` records the spec in
// state.HarnessInitRunFile; `sciontool hook` writes state.HarnessReadyFile on
// the first qualifying event. The agent runtime waits on these files before
// pasting a buffered message. No harness name is checked here: whether a
// harness reports readiness through hooks is its config's ready.strategy.

// LoadReadySpec returns the ready spec the broker passed in
// state.HarnessReadyEnv (see state.ParseReadySpecEnv), read with getenv. When
// the variable is missing (a broker older than the ready spec) or is not a
// valid spec, it logs at Info and returns the default delay spec, which is
// safe for every harness: hook harnesses then wait the delay settle instead
// of their ready marker until the broker is upgraded. Unknown JSON fields are
// ignored.
func LoadReadySpec(getenv func(string) string) api.ReadySpec {
	fallback := state.DefaultReadySpec(state.ReadyStrategyDelay)
	raw := getenv(state.HarnessReadyEnv)
	if raw == "" {
		log.Info("Harness readiness: %s not set (broker predates the ready spec); using strategy %s with settle %s",
			state.HarnessReadyEnv, fallback.Strategy, fallback.Settle)
		return fallback
	}
	spec, err := state.ParseReadySpecEnv(raw)
	if err != nil {
		log.Info("Harness readiness: %s is not a valid ready spec (%v); using strategy %s with settle %s",
			state.HarnessReadyEnv, err, fallback.Strategy, fallback.Settle)
		return fallback
	}
	return spec
}

// SessionStartKind classifies a session-start by its source.
type SessionStartKind int

const (
	// SessionNew is a new harness session or run: startup, resume, new,
	// fork, no source, and any unknown source.
	SessionNew SessionStartKind = iota
	// SessionInPlace restarts a session inside a harness that was already
	// running: clear and compact.
	SessionInPlace
)

// sessionStartSources maps the known normalized session-start sources
// (lower case, after dialect mapping) to their kind. A harness that spells a
// source differently maps it in its dialect. Any future source that restarts
// a session inside a running harness must be added as SessionInPlace.
var sessionStartSources = map[string]SessionStartKind{
	"startup": SessionNew,
	"resume":  SessionNew,
	"new":     SessionNew,
	"fork":    SessionNew,
	"":        SessionNew,
	"clear":   SessionInPlace,
	"compact": SessionInPlace,
}

// ClassifySessionStart returns the kind of a session-start with source,
// compared after trimming space and lower-casing. An unknown source is
// SessionNew, as every session-start was before sources were classified; it
// is logged at Debug.
func ClassifySessionStart(source string) SessionStartKind {
	key := strings.ToLower(strings.TrimSpace(source))
	kind, known := sessionStartSources[key]
	if !known {
		log.Debug("Unknown session-start source %q; treating it as a new session", source)
		return SessionNew
	}
	return kind
}

// readinessEvents lists the normalized events (after dialect mapping) that
// show the harness is running and reading input. Scion lifecycle events
// (pre-start, post-start, pre-stop) and session-end never qualify.
// session-start qualifies only for a new session; see IsReadinessEvent.
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
// ready for input. A session-start counts only when it is a new session
// (ClassifySessionStart): clear and compact restart a session inside a
// harness that was already running, so they say nothing about a new run
// being ready.
func IsReadinessEvent(ev *Event) bool {
	if ev == nil || !readinessEvents[ev.Name] {
		return false
	}
	if ev.Name == EventSessionStart {
		return ClassifySessionStart(ev.Data.Source) == SessionNew
	}
	return true
}
