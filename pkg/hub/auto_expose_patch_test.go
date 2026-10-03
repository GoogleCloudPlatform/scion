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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Auto-expose on the PATCH /api/v1/agents/{id} config path (ptone/scion#2562
// §3.4, AC7, F3). The configure page sends the SCION_AUTO_EXPOSE_* keys only
// when the user changed the auto-expose control, so an env map without them
// leaves auto-expose untouched; a map with them sets the explicit tier.

const aePorts = "SCION_AUTO_EXPOSE_PORTS"

// autoExposePatchAgent describes the pre-PATCH state of an agent's three env
// maps and its project's auto-expose annotation ("" = unset).
type autoExposePatchAgent struct {
	projectAnno  string
	appliedEnv   map[string]string
	inlineEnv    map[string]string
	createInputs *store.AgentCreateInputs
}

func setupAutoExposePatchAgent(t *testing.T, in autoExposePatchAgent) (*Server, store.Store, *store.Project, *store.Agent) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	if in.projectAnno != "" {
		setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: in.projectAnno})
	}
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "golden-model"
		a.AppliedConfig.Env = in.appliedEnv
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Model: "golden-model", Env: in.inlineEnv}
		a.AppliedConfig.CreateInputs = in.createInputs
	})
	return srv, s, project, agent
}

func patchAndReload(t *testing.T, srv *Server, s store.Store, agentID string, body map[string]interface{}) *store.Agent {
	t.Helper()
	rec := patchAgentConfig(t, srv, agentID, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	updated, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	return updated
}

func ciWithEnv(env map[string]string) *store.AgentCreateInputs {
	return &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Model: "golden-model", Env: env}}
}

func rowEditBody(t *testing.T, env map[string]interface{}) map[string]interface{} {
	body := configureUntouchedBody(t)
	body["env"] = env
	return body
}

// TestApplyAgentUpdate_AutoExposeUntouchedSave is AC7's "saving without
// touching auto-expose records no AE in CreateInputs" and F3's "a
// project-derived AE survives a configure save", for both an untouched save
// (no env key) and a custom-row edit (env without AE keys).
func TestApplyAgentUpdate_AutoExposeUntouchedSave(t *testing.T) {
	bodies := map[string]func(t *testing.T) map[string]interface{}{
		"untouched save": configureUntouchedBody,
		"row edit":       configureRowEditBody,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, s, _, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
				projectAnno:  "true",
				appliedEnv:   map[string]string{"TEMPLATE_KEY": "x", aePorts: "true"},
				createInputs: ciWithEnv(nil),
			})
			updated := patchAndReload(t, srv, s, agent.ID, body(t))

			assert.Equal(t, "true", updated.AppliedConfig.Env[aePorts], "project-derived value must survive the save")
			assert.NotContains(t, inlineEnv(updated), aePorts, "a derived value must not become explicit")
			assert.NotContains(t, createInputsEnv(updated), aePorts, "an untouched control records nothing in CreateInputs")
		})
	}
}

// TestApplyAgentUpdate_AutoExposeRowEditKeepsDerivedValue covers the
// non-explicit value kept or re-derived when the request's env lacks it.
func TestApplyAgentUpdate_AutoExposeRowEditKeepsDerivedValue(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		oldApplied  string
		want        string
	}{
		{"kept when project has no annotation", "", "true", "true"},
		{"re-derived from a changed project annotation", "false", "true", "false"},
		{"project tier fills a key the old env lacked", "true", "", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := map[string]string{"TEMPLATE_KEY": "x"}
			if tc.oldApplied != "" {
				applied[aePorts] = tc.oldApplied
			}
			srv, s, _, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
				projectAnno:  tc.projectAnno,
				appliedEnv:   applied,
				createInputs: ciWithEnv(nil),
			})
			updated := patchAndReload(t, srv, s, agent.ID, configureRowEditBody(t))

			assert.Equal(t, tc.want, updated.AppliedConfig.Env[aePorts])
			assert.Equal(t, "bar", updated.AppliedConfig.Env["FOO"])
			assert.NotContains(t, inlineEnv(updated), aePorts)
			assert.Equal(t, map[string]string{"FOO": "bar"}, createInputsEnv(updated))
		})
	}
}

// TestApplyAgentUpdate_AutoExposeExplicitSurvivesRowEdit pins that an
// explicit value is neither overwritten by the project tier nor lost from
// any of the three maps when the request's env omits it, for an agent with
// CreateInputs and for a legacy agent whose explicit record is
// InlineConfig.Env.
func TestApplyAgentUpdate_AutoExposeExplicitSurvivesRowEdit(t *testing.T) {
	explicit := map[string]string{
		aePorts:                        "false",
		"SCION_AUTO_EXPOSE_MODE":       "denylist",
		"SCION_AUTO_EXPOSE_PORTS_LIST": "22",
		"SCION_AUTO_EXPOSE_INTERVAL":   "9s",
	}
	withTemplate := func() map[string]string {
		m := map[string]string{"TEMPLATE_KEY": "x"}
		for k, v := range explicit {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name string
		ci   *store.AgentCreateInputs
	}{
		{"with CreateInputs", ciWithEnv(explicit)},
		{"legacy agent without CreateInputs", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
				projectAnno:  "true",
				appliedEnv:   withTemplate(),
				inlineEnv:    explicit,
				createInputs: tc.ci,
			})
			updated := patchAndReload(t, srv, s, agent.ID, configureRowEditBody(t))

			for k, v := range explicit {
				assert.Equal(t, v, updated.AppliedConfig.Env[k], "live %s", k)
				assert.Equal(t, v, inlineEnv(updated)[k], "InlineConfig %s", k)
				if tc.ci != nil {
					assert.Equal(t, v, createInputsEnv(updated)[k], "CreateInputs %s", k)
				}
			}
			if tc.ci == nil {
				assert.Nil(t, updated.AppliedConfig.CreateInputs)
			}
		})
	}
}

// TestApplyAgentUpdate_AutoExposeToggleIsExplicit is AC7's "toggling records
// AE as explicit, and the value survives reincarnate": the sent value beats
// the project tier live, lands in all three maps, and is what a fresh
// generation gets. Sending the same value as the project-derived one still
// records it, because the explicit baseline is InlineConfig.Env.
func TestApplyAgentUpdate_AutoExposeToggleIsExplicit(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		oldApplied  string
		send        string
	}{
		{"toggle off against project true", "true", "true", "false"},
		{"explicit true equal to the project-derived value", "true", "true", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
				projectAnno:  tc.projectAnno,
				appliedEnv:   map[string]string{"TEMPLATE_KEY": "x", aePorts: tc.oldApplied},
				createInputs: ciWithEnv(nil),
			})
			updated := patchAndReload(t, srv, s, agent.ID, rowEditBody(t, map[string]interface{}{
				"TEMPLATE_KEY": "x",
				aePorts:        tc.send,
			}))

			assert.Equal(t, tc.send, updated.AppliedConfig.Env[aePorts])
			assert.Equal(t, tc.send, inlineEnv(updated)[aePorts])
			assert.Equal(t, map[string]string{aePorts: tc.send}, createInputsEnv(updated))

			// A later save that does not touch the control keeps it.
			after := patchAndReload(t, srv, s, agent.ID, configureRowEditBody(t))
			assert.Equal(t, tc.send, after.AppliedConfig.Env[aePorts])
			assert.Equal(t, tc.send, createInputsEnv(after)[aePorts])

			// Flip the project annotation the other way: the explicit value
			// still wins at reincarnate.
			opposite := "false"
			if tc.send == "false" {
				opposite = "true"
			}
			setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: opposite})
			fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), after, project, "")
			require.NoError(t, err)
			assert.Equal(t, tc.send, fresh.Env[aePorts], "the explicit value must survive reincarnate")
		})
	}
}

// TestApplyAgentUpdate_PatchEnvDoesNotAliasInline pins the copy: the
// project-derived value written into the live env must not appear in the
// new InlineConfig.Env built from the same request map.
func TestApplyAgentUpdate_PatchEnvDoesNotAliasInline(t *testing.T) {
	srv, s, _, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
		projectAnno:  "true",
		appliedEnv:   map[string]string{"TEMPLATE_KEY": "x"},
		createInputs: ciWithEnv(nil),
	})
	updated := patchAndReload(t, srv, s, agent.ID, configureRowEditBody(t))
	assert.Equal(t, "true", updated.AppliedConfig.Env[aePorts])
	assert.Equal(t, map[string]string{"TEMPLATE_KEY": "x", "FOO": "bar"}, inlineEnv(updated))
}

func TestDiffExplicitEnvKeys(t *testing.T) {
	t.Run("non-auto-expose key compares against AppliedConfig.Env only", func(t *testing.T) {
		added, removed := diffExplicitEnvKeys(
			map[string]string{"A": "1"},
			map[string]string{"B": "2"},
			map[string]string{"A": "1", "B": "2"},
		)
		assert.Equal(t, map[string]string{"B": "2"}, added)
		assert.Empty(t, removed)
	})
	t.Run("auto-expose key compares against InlineConfig.Env only", func(t *testing.T) {
		added, _ := diffExplicitEnvKeys(
			map[string]string{aePorts: "true"},
			map[string]string{"SCION_AUTO_EXPOSE_MODE": "denylist"},
			map[string]string{aePorts: "true", "SCION_AUTO_EXPOSE_MODE": "denylist"},
		)
		assert.Equal(t, map[string]string{aePorts: "true"}, added)
	})
	t.Run("absent auto-expose keys and GITHUB_TOKEN are never removed", func(t *testing.T) {
		old := map[string]string{"GITHUB_TOKEN": "t", "GONE": "x"}
		for k := range autoExposeEnvKeys {
			old[k] = "v"
		}
		added, removed := diffExplicitEnvKeys(old, nil, map[string]string{})
		assert.Empty(t, added)
		assert.Equal(t, []string{"GONE"}, removed)
	})
}
