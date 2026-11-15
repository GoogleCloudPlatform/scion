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
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/stretchr/testify/assert"
)

func TestAPIOutcomeTracker_Candidate(t *testing.T) {
	at := func(s int) time.Time { return watchdogLaunch.Add(time.Duration(s) * time.Second) }
	obs := func(tr *apiOutcomeTracker, s int, model string, ok bool, status int) {
		tr.Observe(telemetry.APIOutcome{At: at(s), Model: model, OK: ok, Status: status})
	}

	t.Run("empty", func(t *testing.T) {
		_, ok := (&apiOutcomeTracker{}).Candidate("", watchdogLaunch)
		assert.False(t, ok)
	})
	t.Run("nothing known: latest of any model", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "a", false, 503)
		obs(tr, 2, "b", false, 404)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.Equal(t, 404, got.Status)
	})
	t.Run("configured alias picks its model", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "claude-sonnet-5", false, 403)
		obs(tr, 2, "claude-haiku-4", false, 404)
		got, _ := tr.Candidate("sonnet", watchdogLaunch)
		assert.Equal(t, 403, got.Status)
	})
	t.Run("an exact match wins over containment: flash vs flash-lite", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "gemini-2.5-flash", true, 200)
		obs(tr, 2, "gemini-2.5-flash-lite", false, 404)
		got, _ := tr.Candidate("gemini-2.5-flash", watchdogLaunch)
		assert.True(t, got.OK, "a failing sibling model was judged: %+v", got)
		got, _ = tr.Candidate("vertex_ai/gemini-2.5-flash@001", watchdogLaunch)
		assert.True(t, got.OK, "normalized exact match: %+v", got)
	})
	t.Run("containment still applies when nothing matches exactly", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "gemini-2.5-flash-lite", false, 403)
		got, _ := tr.Candidate("gemini-2.5-flash", watchdogLaunch)
		assert.Equal(t, 403, got.Status)
	})
	t.Run("most successes is primary, not the latest success", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "main", true, 200)
		obs(tr, 2, "main", true, 200)
		obs(tr, 3, "main", false, 400)
		obs(tr, 4, "side", true, 200)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.Equal(t, 400, got.Status)
		assert.Equal(t, "main", got.Model)
	})
	t.Run("side failure after a primary success is ignored", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "main", true, 200)
		obs(tr, 2, "side", false, 404)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.True(t, got.OK)
	})
	t.Run("an outcome without a model is the primary's", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 1, "main", true, 200)
		obs(tr, 2, "", false, 401)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.Equal(t, 401, got.Status)
	})
	t.Run("outcomes before the run are ignored", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, -5, "main", true, 200)
		obs(tr, 2, "side", false, 404)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.Equal(t, 404, got.Status)
	})
	t.Run("an out-of-order older outcome does not replace a newer one", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		obs(tr, 5, "main", false, 400)
		obs(tr, 3, "main", true, 200)
		got, _ := tr.Candidate("", watchdogLaunch)
		assert.Equal(t, 400, got.Status)
	})
	t.Run("bounded", func(t *testing.T) {
		tr := &apiOutcomeTracker{}
		for i := 0; i < apiOutcomeTrackerMaxModels+10; i++ {
			obs(tr, i, "m"+strconv.Itoa(i), false, 500)
		}
		assert.Len(t, tr.models, apiOutcomeTrackerMaxModels)
	})
}

func TestModelsMatch(t *testing.T) {
	for _, tc := range []struct {
		observed, configured string
		want                 bool
	}{
		{"claude-sonnet-5", "claude-sonnet-5", true},
		{"claude-sonnet-5", "sonnet", true},
		{"claude-sonnet-4", "claude-sonnet-4@20250514", true},
		{"gemini-2.5-pro", "vertex_ai/gemini-2.5-pro", true},
		{"GPT-5.1-Codex", "gpt-5.1-codex", true},
		{"claude-haiku-4", "sonnet", false},
		{"gemini-2.5-flash", "gemini-2.5-pro", false},
		{"", "sonnet", false},
		{"claude-sonnet-5", "", false},
	} {
		assert.Equal(t, tc.want, modelsMatch(tc.observed, tc.configured), "%q vs %q", tc.observed, tc.configured)
	}
}
