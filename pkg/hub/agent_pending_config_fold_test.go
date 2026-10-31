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
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// Unit tests for ptone/scion#5864: the held-edit classifier and the fold
// plan.

// foldTestApplied is the applied inline config the classifier tests run
// against: the provision-rendered keys, two skills, a duration limit, two
// env keys and resource requests.
func foldTestApplied() *api.ScionConfig {
	return &api.ScionConfig{
		Harness:           "claude",
		Model:             "old-model",
		SystemPrompt:      "old prompt",
		AgentInstructions: "old instructions",
		Skills:            []api.SkillReference{{URI: "hub://skills/review"}, {URI: "hub://skills/test"}},
		MaxDuration:       "2h",
		MaxTurns:          api.IntPtr(3),
		Env:               map[string]string{"KEEP": "1", "DROP": "2"},
		Resources:         &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "1Gi"}},
	}
}

// TestClassifyHeldEdits_SaveParity (C0): the classifier splits a save's
// changed keys as holdAgentConfigPatch split them before the extraction
// (the cases TestAgentPendingConfig_HeldSaveUsesTheStartRules pins over
// HTTP, plus env-view and count-limit cases).
func TestClassifyHeldEdits_SaveParity(t *testing.T) {
	rerender := brokerStartSupport{ChangedFields: true}
	clearing := brokerStartSupport{ClearedFields: true}
	cases := []struct {
		name         string
		body         string
		sup          brokerStartSupport
		canAttachEnv bool
		held         []string
		reincarnate  []string
	}{
		{"re-rendering broker: clear and skill removal", `{"agent_instructions":"","skills":[{"uri":"hub://skills/review"}]}`, rerender, true, nil, []string{"config.agent_instructions", "config.skills"}},
		{"clearing broker: clear and recorded env removal", `{"env":{"KEEP":"1"},"max_duration":null}`, clearing, true, []string{"config.env", "config.max_duration"}, nil},
		// A saver who cannot view env records no env removal: the edit is
		// classified on its additions alone.
		{"saver who cannot view env: removal not reported", `{"env":{"KEEP":"1","NEW":"1"}}`, brokerStartSupport{}, false, []string{"config.env"}, nil},
		{"old broker: env addition and removal", `{"env":{"KEEP":"1","NEW":"1"}}`, brokerStartSupport{}, true, nil, []string{"config.env"}},
		{"old broker: env addition only", `{"env":{"KEEP":"1","DROP":"2","NEW":"1"}}`, brokerStartSupport{}, true, []string{"config.env"}, nil},
		{"resources dropping a sub-field", `{"resources":{"requests":{"cpu":"1"}}}`, clearing, true, nil, []string{"config.resources"}},
		{"re-rendering broker: secrets", `{"secrets":[{"key":"API_TOKEN"}]}`, rerender, true, nil, []string{"config.secrets"}},
		{"old broker: system prompt", `{"system_prompt":"new"}`, brokerStartSupport{}, true, nil, []string{"config.system_prompt"}},
		{"plain container key", `{"model":"new-model"}`, brokerStartSupport{}, true, []string{"config.model"}, nil},
		{"old broker: count limit clear", `{"max_turns":null}`, brokerStartSupport{}, true, nil, []string{"config.max_turns"}},
		{"clearing broker: count limit clear", `{"max_turns":null}`, clearing, true, []string{"config.max_turns"}, nil},
		{"count limit of 0 is a set value", `{"max_turns":0}`, brokerStartSupport{UnlimitedCountLimits: true}, true, []string{"config.max_turns"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := decodeConfigPatch(t, tc.body)
			var candidates []string
			for _, f := range configPatchKeys(p.raw) {
				candidates = append(candidates, f.Key)
			}
			c := classifyHeldEdits(foldTestApplied(), p.cfg, p.raw, p.present, tc.canAttachEnv, tc.sup, candidates)
			assert.Equal(t, tc.held, c.held, "held")
			assert.Equal(t, tc.reincarnate, c.heldForReincarnate, "heldForReincarnate")
		})
	}
}

// TestClassifyHeldEdits_OnlyCandidates: keys outside candidates are in
// neither list, and their reasons are dropped.
func TestClassifyHeldEdits_OnlyCandidates(t *testing.T) {
	p := decodeConfigPatch(t, `{"max_turns":null,"model":"m"}`)
	c := classifyHeldEdits(foldTestApplied(), p.cfg, p.raw, p.present, true, brokerStartSupport{}, []string{"config.model"})
	assert.Equal(t, []string{"config.model"}, c.held)
	assert.Empty(t, c.heldForReincarnate)
	assert.Empty(t, c.cleared)
}

// TestPlanPendingFold_KeepsReincarnateKeysPending: the plan keeps pending
// a key only a reincarnation brings into effect, applies the others, and
// leaves a waiting Unlimited out of the reincarnation list.
func TestPlanPendingFold_KeepsReincarnateKeysPending(t *testing.T) {
	agent := &store.Agent{
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: foldTestApplied()},
		PendingConfig: &store.AgentPendingConfig{Config: map[string]json.RawMessage{
			"max_duration":    json.RawMessage(`null`),
			"max_model_calls": json.RawMessage(`0`),
			"model":           json.RawMessage(`"new-model"`),
		}},
	}
	plan := (&Server{}).planPendingFold(context.Background(), agent, brokerStartSupport{})
	assert.Equal(t, []string{"config.max_model_calls"}, plan.waiting)
	assert.Equal(t, []string{"config.max_duration"}, plan.reincarnate)
	assert.True(t, plan.keep(agentEditFieldByKey["config.model"]))
	assert.False(t, plan.keep(agentEditFieldByKey["config.max_duration"]))
	assert.False(t, plan.keep(agentEditFieldByKey["config.max_model_calls"]))

	folded, _ := (&Server{}).foldPendingConfig(context.Background(), agent, plan)
	assert.Equal(t, []string{"config.model"}, folded)
	assert.Equal(t, "2h", agent.AppliedConfig.InlineConfig.MaxDuration, "the clear is not applied")
	assert.Equal(t, []string{"config.max_duration", "config.max_model_calls"}, pendingConfigKeys(agent.PendingConfig))
}
