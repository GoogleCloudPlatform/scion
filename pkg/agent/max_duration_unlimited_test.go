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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A max_duration of "0" is how the hub and the web Edit page give an agent
// no duration limit (ptone/scion#3972). These tests pin the broker side of
// that: provisioning keeps "0" instead of filling in a default, and the
// start passes it to the container, where sciontool sets no timer for it
// (cmd/sciontool/commands maxDurationLimit).

// TestProvision_UnlimitedDurationBeatsDefaults: a "0" from the template is a
// set value, so neither the hub's agent defaults nor the broker's
// settings.yaml default replaces it. Max turns of 0, by contrast, is unset
// and gets the default.
func TestProvision_UnlimitedDurationBeatsDefaults(t *testing.T) {
	const settingsYAML = `schema_version: "1"
default_harness_config: test-harness
default_max_turns: 10
default_max_duration: 10m
harness_configs:
  test-harness:
    harness: test-harness
`
	const templateJSON = `{
		"default_harness_config": "test-harness",
		"max_turns": 0,
		"max_duration": "0"
	}`

	for _, tc := range []struct {
		name string
		hub  *api.HubAgentDefaults
	}{
		{name: "hub default and settings default", hub: &api.HubAgentDefaults{MaxTurns: 50, MaxDuration: "50m"}},
		{name: "settings default only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir := hubDefaultsFixture(t, settingsYAML, templateJSON)
			ctx := context.Background()
			if tc.hub != nil {
				ctx = api.ContextWithHubAgentDefaults(ctx, tc.hub)
			}
			_, _, cfg, err := ProvisionAgent(ctx, "unlimited-agent", "limits-tpl", "", "test-harness",
				projectScionDir, "", "", "", "")
			require.NoError(t, err)
			require.NotNil(t, cfg)
			assert.Equal(t, "0", cfg.MaxDuration, "a duration of \"0\" (no limit) must not be replaced by a default")
			assert.NotZero(t, cfg.MaxTurns, "max turns of 0 is unset, so a default fills it")
		})
	}
}

// TestStart_UnlimitedDurationReachesContainerEnv: the start sends a
// max_duration of "0" to the container as SCION_MAX_DURATION=0 (so sciontool
// sees no limit), sends none for an unset duration, and sends no
// SCION_MAX_TURNS for max turns of 0.
func TestStart_UnlimitedDurationReachesContainerEnv(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentJSON string
		want      string
		wantSet   bool
	}{
		{name: "no limit", agentJSON: `{"harness": "generic", "max_duration": "0", "max_turns": 0}`, want: "0", wantSet: true},
		{name: "unset", agentJSON: `{"harness": "generic"}`, wantSet: false},
		{name: "a limit", agentJSON: `{"harness": "generic", "max_duration": "2h"}`, want: "2h", wantSet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_MAX_DURATION", "")
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
			require.NoError(t, os.MkdirAll(hcDir, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644))
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			require.NoError(t, os.MkdirAll(tplDir, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644))

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			agentDir := filepath.Join(projectScionDir, "agents", "dur-test")
			require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(tc.agentJSON), 0644))

			var capturedEnv []string
			mockRT := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, config runtime.RunConfig) (string, error) {
					capturedEnv = config.Env
					return "mock-id", nil
				},
			}
			_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name: "dur-test", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true,
			})
			require.NoError(t, err)

			got, set := envValue(capturedEnv, "SCION_MAX_DURATION")
			assert.Equal(t, tc.wantSet, set, "SCION_MAX_DURATION present")
			assert.Equal(t, tc.want, got)
			_, turnsSet := envValue(capturedEnv, "SCION_MAX_TURNS")
			assert.False(t, turnsSet, "max turns of 0 sends no SCION_MAX_TURNS")
		})
	}
}
