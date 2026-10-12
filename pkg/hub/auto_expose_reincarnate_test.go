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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Auto-expose at reincarnate (ptone/scion#2562 §3.6, AC8). The fresh
// generation starts from the explicit record only and re-derives the project
// and template tiers through the create pipeline; the hub default is never
// written to the record (it rides HubAgentDefaults at dispatch).

// autoExposeReincarnateAgent is the outgoing generation's state.
type autoExposeReincarnateAgent struct {
	projectAnno  string            // "" = no annotation at reincarnate time
	templateEnv  map[string]string // nil = no template
	appliedEnv   map[string]string
	inlineEnv    map[string]string
	createInputs *store.AgentCreateInputs // nil = legacy agent
}

func reincarnateAutoExpose(t *testing.T, in autoExposeReincarnateAgent) (old *store.Agent, fresh *store.AgentAppliedConfig, warnings []string) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	if in.projectAnno != "" {
		setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: in.projectAnno})
		var err error
		project, err = s.GetProject(context.Background(), project.ID)
		require.NoError(t, err)
	}
	if in.templateEnv != nil {
		createAutoExposeTemplate(t, s, "ae-reinc-tmpl", in.templateEnv)
	}
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		if in.templateEnv != nil {
			a.Template = "ae-reinc-tmpl"
		}
		a.AppliedConfig.Env = in.appliedEnv
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: in.inlineEnv}
		a.AppliedConfig.CreateInputs = in.createInputs
	})
	loaded, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	fresh, warnings, err = srv.buildFreshAppliedConfig(context.Background(), loaded, project, "")
	require.NoError(t, err)
	return loaded, fresh, warnings
}

func freshCreateInputsEnv(fresh *store.AgentAppliedConfig) map[string]string {
	if fresh.CreateInputs == nil || fresh.CreateInputs.InlineConfig == nil {
		return nil
	}
	return fresh.CreateInputs.InlineConfig.Env
}

func freshInlineEnv(fresh *store.AgentAppliedConfig) map[string]string {
	if fresh.InlineConfig == nil {
		return nil
	}
	return fresh.InlineConfig.Env
}

// TestReincarnate_AutoExposeRederivesProjectTier is AC8's first half: a
// project annotation changed between generations is picked up, and a derived
// value from generation N is never carried forward as if it were explicit.
func TestReincarnate_AutoExposeRederivesProjectTier(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		templateEnv map[string]string
		want        string // "" = absent (the hub default applies at dispatch)
	}{
		{name: "annotation flipped false to true", projectAnno: "true", want: "true"},
		{name: "annotation removed: falls back to inherited", projectAnno: "", want: ""},
		{name: "annotation removed: template tier re-derived", templateEnv: map[string]string{aeKey: "false"}, want: "false"},
		{name: "annotation beats template", projectAnno: "true", templateEnv: map[string]string{aeKey: "false"}, want: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Generation N resolved AE=false from the project annotation then
			// in force; CreateInputs never held it.
			old, fresh, _ := reincarnateAutoExpose(t, autoExposeReincarnateAgent{
				projectAnno:  tc.projectAnno,
				templateEnv:  tc.templateEnv,
				appliedEnv:   map[string]string{"KEEP": "1", aeKey: "false"},
				inlineEnv:    map[string]string{"KEEP": "1"},
				createInputs: &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Env: map[string]string{"KEEP": "1"}}},
			})

			if tc.want == "" {
				assert.NotContains(t, fresh.Env, aeKey)
			} else {
				assert.Equal(t, tc.want, fresh.Env[aeKey])
			}
			assert.Equal(t, "1", fresh.Env["KEEP"], "explicit non-AE keys carry over")
			assert.NotContains(t, freshInlineEnv(fresh), aeKey, "a derived value must not become explicit")
			assert.NotContains(t, freshCreateInputsEnv(fresh), aeKey, "a derived value must not enter CreateInputs")

			plan := computeReincarnationPlan(old.AppliedConfig, fresh, nil, "")
			switch {
			case tc.want == "":
				assert.Contains(t, plan.EnvKeys.Removed, aeKey)
			case tc.want != "false":
				assert.Contains(t, plan.EnvKeys.Changed, aeKey)
			default:
				assert.NotContains(t, plan.EnvKeys.Changed, aeKey)
			}
		})
	}
}

// TestReincarnate_AutoExposeExplicitCarriesOver is AC8's second half: an
// explicit user value carries over unchanged, whatever the project annotation
// or template says now.
func TestReincarnate_AutoExposeExplicitCarriesOver(t *testing.T) {
	for _, explicit := range []string{"false", "true"} {
		t.Run("explicit "+explicit, func(t *testing.T) {
			opposite := "true"
			if explicit == "true" {
				opposite = "false"
			}
			_, fresh, _ := reincarnateAutoExpose(t, autoExposeReincarnateAgent{
				projectAnno:  opposite,
				templateEnv:  map[string]string{aeKey: opposite},
				appliedEnv:   map[string]string{aeKey: explicit},
				inlineEnv:    map[string]string{aeKey: explicit},
				createInputs: &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Env: map[string]string{aeKey: explicit}}},
			})
			assert.Equal(t, explicit, fresh.Env[aeKey])
			assert.Equal(t, explicit, freshInlineEnv(fresh)[aeKey])
			assert.Equal(t, map[string]string{aeKey: explicit}, freshCreateInputsEnv(fresh))
		})
	}
}

// TestReincarnate_AutoExposeDerivedValueDoesNotLeakIntoExplicitRecord pins
// that the fresh Env is its own map: the project tier written into it must
// not reach the fresh InlineConfig.Env or the replayed CreateInputs.
func TestReincarnate_AutoExposeDerivedValueDoesNotLeakIntoExplicitRecord(t *testing.T) {
	ci := &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Env: map[string]string{"KEEP": "1"}}}
	_, fresh, _ := reincarnateAutoExpose(t, autoExposeReincarnateAgent{
		projectAnno:  "true",
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1"},
		createInputs: ci,
	})
	require.Equal(t, "true", fresh.Env[aeKey])
	assert.Equal(t, map[string]string{"KEEP": "1"}, freshInlineEnv(fresh))
	assert.Equal(t, map[string]string{"KEEP": "1"}, freshCreateInputsEnv(fresh))
}

// TestReincarnate_AutoExposeLegacyRecordIsStripped covers an agent that
// predates CreateInputs: its InlineConfig.Env AE cannot be told apart from a
// hub or project stamp, so it is dropped from the reconstructed explicit
// record and the tiers are re-derived as for any other agent.
func TestReincarnate_AutoExposeLegacyRecordIsStripped(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		want        string
	}{
		{name: "project annotation now false", projectAnno: "false", want: "false"},
		{name: "no annotation: inherited", projectAnno: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, fresh, warnings := reincarnateAutoExpose(t, autoExposeReincarnateAgent{
				projectAnno: tc.projectAnno,
				appliedEnv:  map[string]string{"KEEP": "1"},
				inlineEnv:   map[string]string{"KEEP": "1", aeKey: "true"},
			})
			if tc.want == "" {
				assert.NotContains(t, fresh.Env, aeKey)
			} else {
				assert.Equal(t, tc.want, fresh.Env[aeKey])
			}
			assert.Equal(t, "1", fresh.Env["KEEP"])
			assert.NotContains(t, freshInlineEnv(fresh), aeKey)
			assert.NotContains(t, freshCreateInputsEnv(fresh), aeKey)
			assert.Contains(t, warnings, "stripped SCION_AUTO_EXPOSE_PORTS (a project/hub default, not an explicit input) from the reconstructed config")
		})
	}
}
