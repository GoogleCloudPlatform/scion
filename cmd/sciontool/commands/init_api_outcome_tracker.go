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

package commands

import (
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// apiOutcomeTrackerMaxModels bounds the per-model state. A harness talks to
// a handful of models; past the cap the least recently seen model is
// dropped.
const apiOutcomeTrackerMaxModels = 32

// apiOutcomeTracker keeps, per model, the latest model API outcome the
// usage deriver reports and the count of successes. Observe is called from
// OTLP receiver goroutines.
type apiOutcomeTracker struct {
	mu     sync.Mutex
	models map[string]*modelOutcomes
}

type modelOutcomes struct {
	latest      telemetry.APIOutcome
	successes   int
	lastSuccess time.Time
}

// Observe records o for its model when it is not older than that model's
// latest outcome. Exports are batched, so outcomes can arrive out of order
// across batches.
func (t *apiOutcomeTracker) Observe(o telemetry.APIOutcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.models == nil {
		t.models = make(map[string]*modelOutcomes)
	}
	m := t.models[o.Model]
	if m == nil {
		if len(t.models) >= apiOutcomeTrackerMaxModels {
			t.evictOldestLocked()
		}
		m = &modelOutcomes{}
		t.models[o.Model] = m
	}
	if o.OK {
		m.successes++
		if o.At.After(m.lastSuccess) {
			m.lastSuccess = o.At
		}
	}
	if !o.At.Before(m.latest.At) {
		m.latest = o
	}
}

func (t *apiOutcomeTracker) evictOldestLocked() {
	var oldestModel string
	var oldest time.Time
	first := true
	for model, m := range t.models {
		if first || m.latest.At.Before(oldest) {
			oldestModel, oldest, first = model, m.latest.At, false
		}
	}
	delete(t.models, oldestModel)
}

// Candidate returns the outcome the API-error watchdog judges: the latest
// outcome of the agent's primary model, so a failed side or sub-agent call
// (a 404 for a small/fast model the project has not enabled) is not taken
// for the agent's turn failing. Outcomes before since belong to an earlier
// run and are not considered. The primary model is, in order:
//
//  1. configured (SCION_MODEL) when an outcome of a matching model was seen:
//     the models that match it exactly after normalizing (provider prefix
//     and @version stripped) when there are any, else those that match it
//     as an alias (modelsMatch: SCION_MODEL may be "sonnet");
//  2. else the model with the most successes in this run (latest success
//     breaks a tie). Counting rather than taking the latest success keeps a
//     side call that succeeds after the main model failed from becoming
//     "primary" and hiding the failure;
//  3. else, when nothing matched and nothing succeeded, every model: the
//     latest outcome of any model. The primary model failing before
//     anything ever succeeded must still be reported.
//
// An outcome with no model attribute counts as the primary model's.
func (t *apiOutcomeTracker) Candidate(configured string, since time.Time) (telemetry.APIOutcome, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var configuredMatches, exactMatches []string
	bestModel, bestCount := "", 0
	var bestAt time.Time
	for model, m := range t.models {
		if m.latest.At.Before(since) {
			continue
		}
		if configured != "" && model != "" && modelsMatch(model, configured) {
			configuredMatches = append(configuredMatches, model)
			if modelsMatchExactly(model, configured) {
				exactMatches = append(exactMatches, model)
			}
		}
		if m.successes > bestCount || (m.successes == bestCount && m.successes > 0 && m.lastSuccess.After(bestAt)) {
			bestModel, bestCount, bestAt = model, m.successes, m.lastSuccess
		}
	}

	// An exact match wins over alias containment, so SCION_MODEL
	// "gemini-2.5-flash" does not also claim a failing
	// "gemini-2.5-flash-lite".
	if len(exactMatches) > 0 {
		configuredMatches = exactMatches
	}

	var primary []string
	switch {
	case len(configuredMatches) > 0:
		primary = configuredMatches
	case bestCount > 0:
		primary = []string{bestModel}
	default:
		for model := range t.models {
			primary = append(primary, model)
		}
	}
	if bestCount > 0 || len(configuredMatches) > 0 {
		// An outcome without a model attribute is the primary model's.
		primary = append(primary, "")
	}

	var out telemetry.APIOutcome
	found := false
	for _, model := range primary {
		m := t.models[model]
		if m == nil || m.latest.At.Before(since) {
			continue
		}
		if !found || m.latest.At.After(out.At) {
			out, found = m.latest, true
		}
	}
	return out, found
}

// modelsMatch reports whether an observed model label and the configured
// model name the same model. The configured value may be an alias or carry
// a provider prefix or version suffix the harness's label lacks (or the
// reverse): "sonnet" matches "claude-sonnet-5", "vertex_ai/gemini-2.5-pro"
// matches "gemini-2.5-pro", "claude-sonnet-4@20250514" matches
// "claude-sonnet-4".
func modelsMatch(observed, configured string) bool {
	a, b := normalizeModelName(observed), normalizeModelName(configured)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

// modelsMatchExactly reports whether the two names are equal after
// normalizing (case, provider prefix, @version).
func modelsMatchExactly(observed, configured string) bool {
	a := normalizeModelName(observed)
	return a != "" && a == normalizeModelName(configured)
}

func normalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	return s
}
