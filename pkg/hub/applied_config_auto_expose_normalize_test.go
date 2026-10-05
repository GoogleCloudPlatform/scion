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
	"bytes"
	"context"
	"maps"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The env cleanup's auto-expose handling (ptone/scion#2562 §5(b) as amended
// by design A8): a stamped SCION_AUTO_EXPOSE_PORTS is re-derived, and the
// auto-expose keys are exempt from the AppliedConfig.Env allowlist.

type aeNormalizeAgent struct {
	projectAnno  string            // "" = no annotation
	templateEnv  map[string]string // nil = no template
	appliedEnv   map[string]string
	inlineEnv    map[string]string
	createInputs *store.AgentCreateInputs // nil = legacy agent
}

func setupAENormalizeAgent(t *testing.T, in aeNormalizeAgent) (*Server, store.Store, *store.Project, *store.Agent) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	if in.projectAnno != "" {
		setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: in.projectAnno})
		var err error
		project, err = s.GetProject(context.Background(), project.ID)
		require.NoError(t, err)
	}
	if in.templateEnv != nil {
		createAutoExposeTemplate(t, s, "ae-norm-tmpl", in.templateEnv)
	}
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		if in.templateEnv != nil {
			a.Template = "ae-norm-tmpl"
			a.AppliedConfig.TemplateID = tid("template-ae-norm-tmpl-" + t.Name())
		}
		a.AppliedConfig.Env = in.appliedEnv
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: in.inlineEnv}
		a.AppliedConfig.CreateInputs = in.createInputs
	})
	loaded, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	return srv, s, project, loaded
}

func runAECleanup(t *testing.T, s store.Store, params map[string]string) string {
	t.Helper()
	exec := &AppliedConfigEnvCleanupExecutor{Store: s}
	var buf bytes.Buffer
	require.NoError(t, exec.Run(context.Background(), &buf, params))
	return buf.String()
}

func reloadAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func explicitKeep() *store.AgentCreateInputs {
	return &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace", InlineConfig: &api.ScionConfig{Env: map[string]string{"KEEP": "1"}}}
}

// TestEnvCleanup_AutoExposeNormalizationMatchesReincarnate pins that the
// normalized SCION_AUTO_EXPOSE_PORTS equals what reincarnate derives for the
// same agent, that the stamp leaves InlineConfig.Env, and that a second run
// is a no-op. A stale stamp with no project or template value drops to
// inherited (the hub default at dispatch).
func TestEnvCleanup_AutoExposeNormalizationMatchesReincarnate(t *testing.T) {
	cases := []struct {
		name        string
		projectAnno string
		templateEnv map[string]string
		appliedAE   string // "" = stamp in InlineConfig.Env only
		want        string // "" = absent
	}{
		{name: "inline-only hub stamp drops to inherited", want: ""},
		{name: "inline-only stamp, project annotation wins", projectAnno: "true", want: "true"},
		{name: "stamp in both maps drops to inherited", appliedAE: "false", want: ""},
		{name: "stamp in both maps, template tier re-derived", templateEnv: map[string]string{aeKey: "false"}, appliedAE: "true", want: "false"},
		{name: "stamp in both maps, project beats template", projectAnno: "false", templateEnv: map[string]string{aeKey: "true"}, appliedAE: "true", want: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stamp := "false"
			if tc.want == "false" {
				stamp = "true"
			}
			applied := map[string]string{"KEEP": "1"}
			if tc.appliedAE != "" {
				applied[aeKey] = tc.appliedAE
				stamp = tc.appliedAE
			}
			srv, s, project, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
				projectAnno:  tc.projectAnno,
				templateEnv:  tc.templateEnv,
				appliedEnv:   applied,
				inlineEnv:    map[string]string{"KEEP": "1", aeKey: stamp},
				createInputs: explicitKeep(),
			})
			fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
			require.NoError(t, err)

			log := runAECleanup(t, s, nil)
			assert.Contains(t, log, "RE-DERIVE agent="+agent.ID+" field=inlineConfig.env key="+aeKey)
			assert.Contains(t, log, "re-derived auto-expose on 1 agent(s)")

			got := reloadAgent(t, s, agent.ID)
			freshAE, freshHas := fresh.Env[aeKey]
			gotAE, gotHas := got.AppliedConfig.Env[aeKey]
			assert.Equal(t, freshHas, gotHas, "presence must match reincarnate")
			assert.Equal(t, freshAE, gotAE, "value must match reincarnate")
			if tc.want == "" {
				assert.False(t, gotHas)
			} else {
				assert.Equal(t, tc.want, gotAE)
			}
			assert.Equal(t, map[string]string{"KEEP": "1"}, inlineEnv(got), "the stamp leaves InlineConfig.Env")
			assert.Equal(t, map[string]string{"KEEP": "1"}, createInputsEnv(got), "CreateInputs is untouched")
			assert.Equal(t, "1", got.AppliedConfig.Env["KEEP"])

			version := got.StateVersion
			log2 := runAECleanup(t, s, nil)
			assert.NotContains(t, log2, "RE-DERIVE")
			assert.Equal(t, version, reloadAgent(t, s, agent.ID).StateVersion, "second run is a no-op")
		})
	}
}

// TestEnvCleanup_AutoExposeNormalizationLeavesOthersAlone covers the agents
// normalization must not touch: an explicit value (in CreateInputs), and an
// agent without CreateInputs, which cannot tell a stamp from an explicit
// value.
func TestEnvCleanup_AutoExposeNormalizationLeavesOthersAlone(t *testing.T) {
	cases := []struct {
		name string
		in   aeNormalizeAgent
	}{
		{"explicit value", aeNormalizeAgent{
			projectAnno: "true",
			appliedEnv:  map[string]string{aeKey: "false"},
			inlineEnv:   map[string]string{aeKey: "false"},
			createInputs: &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace",
				InlineConfig: &api.ScionConfig{Env: map[string]string{aeKey: "false"}}},
		}},
		{"no CreateInputs", aeNormalizeAgent{
			projectAnno: "false",
			appliedEnv:  map[string]string{"KEEP": "1"},
			inlineEnv:   map[string]string{"KEEP": "1", aeKey: "true"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _, agent := setupAENormalizeAgent(t, tc.in)
			wantApplied := maps.Clone(agent.AppliedConfig.Env)
			wantInline := maps.Clone(inlineEnv(agent))

			log := runAECleanup(t, s, nil)
			assert.NotContains(t, log, "RE-DERIVE")

			got := reloadAgent(t, s, agent.ID)
			assert.Equal(t, wantApplied, got.AppliedConfig.Env)
			assert.Equal(t, wantInline, inlineEnv(got))
			assert.Equal(t, agent.StateVersion, got.StateVersion, "no write")
		})
	}
}

// TestEnvCleanup_AutoExposeNormalizationDryRun reports the re-derivation
// without writing.
func TestEnvCleanup_AutoExposeNormalizationDryRun(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true"},
		createInputs: explicitKeep(),
	})
	log := runAECleanup(t, s, map[string]string{"dryRun": "true"})
	assert.Contains(t, log, "WOULD RE-DERIVE agent="+agent.ID+" field=inlineConfig.env key="+aeKey)
	got := reloadAgent(t, s, agent.ID)
	assert.Equal(t, "true", inlineEnv(got)[aeKey])
	assert.Equal(t, agent.StateVersion, got.StateVersion)
}

// TestEnvCleanup_AutoExposeKeysExemptFromAllowlist is the drift test: a
// project-derived SCION_AUTO_EXPOSE_PORTS in AppliedConfig.Env has no plain
// source the allowlist can match, and neither do kept _PORTS_LIST and
// _INTERVAL values, yet the cleanup must keep them. The exemption is by exact
// name: SCION_AUTO_EXPOSE_MODE and other unsourced keys are still stripped.
func TestEnvCleanup_AutoExposeKeysExemptFromAllowlist(t *testing.T) {
	derived := map[string]string{
		aeKey:                          "true",
		"SCION_AUTO_EXPOSE_PORTS_LIST": "8080",
		"SCION_AUTO_EXPOSE_INTERVAL":   "5s",
	}
	stripped := map[string]string{
		"SCION_AUTO_EXPOSE_MODE":     "all",
		"SCION_AUTO_EXPOSE_MIN_PORT": "1024",
		"UNSOURCED_VAR":              "x",
	}
	applied := maps.Clone(derived)
	maps.Copy(applied, stripped)
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		projectAnno:  "true",
		appliedEnv:   applied,
		createInputs: &store.AgentCreateInputs{Workspace: "/tmp/reincarnate-workspace"},
	})

	snapshot := func() *store.AgentAppliedConfig {
		return &store.AgentAppliedConfig{Env: maps.Clone(applied), CreateInputs: &store.AgentCreateInputs{}}
	}
	recID := tid("ae-exempt-record")
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		ID: recID, AgentID: agent.ID, FromGeneration: 1, ToGeneration: 2,
		RequestedAt: time.Now(), State: store.AgentReincarnationStateCompleted,
		PreviousAppliedConfig: snapshot(), NewAppliedConfig: snapshot(),
	}))

	runAECleanup(t, s, nil)

	assert.Equal(t, derived, reloadAgent(t, s, agent.ID).AppliedConfig.Env)
	rec, err := s.GetAgentReincarnation(context.Background(), recID)
	require.NoError(t, err)
	assert.Equal(t, derived, rec.PreviousAppliedConfig.Env, "previous snapshot")
	assert.Equal(t, derived, rec.NewAppliedConfig.Env, "new snapshot")
}

// TestEnvCleanup_AutoExposeNormalizationLeavesInlineTZToTZCleanup pins F2:
// normalization touches SCION_AUTO_EXPOSE_PORTS only, so a historical TZ held
// only in InlineConfig.Env stays for adoptLegacyTZ, which the TZ cleanup (and
// every TZ reader) runs, and it becomes a legacy pin.
func TestEnvCleanup_AutoExposeNormalizationLeavesInlineTZToTZCleanup(t *testing.T) {
	_, s, _, agent := setupAENormalizeAgent(t, aeNormalizeAgent{
		appliedEnv:   map[string]string{"KEEP": "1"},
		inlineEnv:    map[string]string{"KEEP": "1", aeKey: "true", agentTZEnvKey: "Asia/Tokyo"},
		createInputs: explicitKeep(),
	})

	runAECleanup(t, s, nil)
	got := reloadAgent(t, s, agent.ID)
	assert.NotContains(t, inlineEnv(got), aeKey)
	assert.Equal(t, "Asia/Tokyo", inlineEnv(got)[agentTZEnvKey], "env cleanup keeps an explicit-config TZ")

	var buf bytes.Buffer
	_, err := (&AppliedConfigTZCleanupExecutor{Store: s}).run(context.Background(), &buf, nil)
	require.NoError(t, err)
	got = reloadAgent(t, s, agent.ID)
	assert.Equal(t, "Asia/Tokyo", got.AppliedConfig.ExplicitTimezone)
	assert.True(t, got.AppliedConfig.ExplicitTimezoneLegacy)
	assert.NotContains(t, inlineEnv(got), agentTZEnvKey)
	assert.NotContains(t, inlineEnv(got), aeKey)
}
