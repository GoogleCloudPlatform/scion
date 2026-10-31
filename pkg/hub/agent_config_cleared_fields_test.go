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

package hub

import (
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearedFieldsFor runs nextClearedFields for a PATCH whose config object is
// body, against the stored inline config old, as applyAgentUpdate does.
func clearedFieldsFor(t *testing.T, old *api.ScionConfig, body string, canViewEnv bool) []string {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	req := &api.ScionConfig{}
	require.NoError(t, json.Unmarshal([]byte(body), req))
	present := map[string]bool{}
	for k := range raw {
		present[k] = true
	}
	dropFixedConfigKeys(req, raw, present)
	_, cleared := reincarnateOnlyConfigEdits(raw, req, old)
	return nextClearedFields(old, req, raw, cleared, canViewEnv)
}

func TestNextClearedFields(t *testing.T) {
	stored := func() *api.ScionConfig {
		return &api.ScionConfig{
			Harness:       "claude",
			MaxTurns:      api.IntPtr(5),
			ThinkingLevel: clearedTestIntPtr(2),
			Resources:     &api.ResourceSpec{Requests: api.ResourceList{CPU: "1"}},
			SystemPrompt:  "be terse",
			Telemetry:     &api.TelemetryConfig{},
			Volumes:       []api.VolumeMount{{Source: "/a", Target: "/a"}},
			Env:           map[string]string{"FOO": "1", "BAR": "2", "SCION_AUTO_EXPOSE_PORTS": "true", "TZ": "UTC"},
		}
	}

	t.Run("clears are recorded", func(t *testing.T) {
		got := clearedFieldsFor(t, stored(), `{"max_turns":null,"thinking_level":null,"resources":null,"system_prompt":"","env":{"BAR":"2"}}`, true)
		assert.Equal(t, []string{"env.FOO", "max_turns", "resources", "system_prompt", "thinking_level"}, got)
	})

	// A count limit of 0 is Unlimited, a value rather than a clear
	// (ptone/scion#5605): it is not recorded, and it takes an earlier clear
	// of the key off the list, since the broker applies cleared_fields
	// after the merge and would otherwise restore the inherited limit.
	t.Run("unlimited is a value, not a clear", func(t *testing.T) {
		assert.Nil(t, clearedFieldsFor(t, stored(), `{"max_turns":0,"max_model_calls":0}`, true))
		old := stored()
		old.MaxTurns = nil
		old.ClearedFields = []string{"max_turns"}
		assert.Nil(t, clearedFieldsFor(t, old, `{"max_turns":0}`, true))
	})

	t.Run("an edit that sets values records nothing", func(t *testing.T) {
		assert.Nil(t, clearedFieldsFor(t, stored(), `{"max_turns":7,"thinking_level":0,"env":{"FOO":"1","BAR":"3"}}`, true))
	})

	t.Run("an unchanged empty value is not a clear", func(t *testing.T) {
		old := stored()
		old.MaxDuration = ""
		assert.Nil(t, clearedFieldsFor(t, old, `{"max_duration":""}`, true))
	})

	t.Run("setting a cleared field again drops it", func(t *testing.T) {
		old := stored()
		old.MaxTurns = nil
		old.ThinkingLevel = nil
		old.ClearedFields = []string{"env.FOO", "max_turns", "thinking_level"}
		got := clearedFieldsFor(t, old, `{"max_turns":4,"thinking_level":0,"env":{"FOO":"x","BAR":"2"}}`, true)
		assert.Nil(t, got)
	})

	t.Run("earlier clears are kept by an unrelated edit", func(t *testing.T) {
		old := stored()
		old.ClearedFields = []string{"max_turns"}
		assert.Equal(t, []string{"max_turns"}, clearedFieldsFor(t, old, `{"model":"m"}`, true))
	})

	t.Run("removing every env key clears each", func(t *testing.T) {
		assert.Equal(t, []string{"env.BAR", "env.FOO"}, clearedFieldsFor(t, stored(), `{"env":{}}`, true))
	})

	t.Run("env removals need env access", func(t *testing.T) {
		assert.Nil(t, clearedFieldsFor(t, stored(), `{"env":{"BAR":"2"}}`, false))
	})

	t.Run("collections and telemetry are not cleared wholesale", func(t *testing.T) {
		assert.Nil(t, clearedFieldsFor(t, stored(), `{"volumes":[],"telemetry":null}`, true))
	})

	t.Run("a client cannot set the list", func(t *testing.T) {
		assert.Nil(t, clearedFieldsFor(t, stored(), `{"cleared_fields":["harness","max_turns"]}`, true))
	})
}

func clearedTestIntPtr(v int) *int { return &v }

// TestLegacyCreateInputsDropClearedFields: a reincarnation of an agent that
// predates CreateInputs rebuilds them from the live inline config; the
// rebuilt inputs carry no cleared_fields, so the new generation starts with
// no clears, and the live config is not changed.
func TestLegacyCreateInputsDropClearedFields(t *testing.T) {
	old := &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{
		MaxTurns:      api.IntPtr(4),
		ClearedFields: []string{"env.FOO", "resources"},
		ChangedFields: []string{"system_prompt"},
	}}
	ci, _ := legacyCreateInputsFromAppliedConfig(old)
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Nil(t, ci.InlineConfig.ClearedFields)
	// changed_fields is reset by a reincarnation too (ptone/scion#3982).
	assert.Nil(t, ci.InlineConfig.ChangedFields)
	assert.Equal(t, api.IntPtr(4), ci.InlineConfig.MaxTurns)
	assert.Equal(t, []string{"env.FOO", "resources"}, old.InlineConfig.ClearedFields)
	assert.Equal(t, []string{"system_prompt"}, old.InlineConfig.ChangedFields)
}
