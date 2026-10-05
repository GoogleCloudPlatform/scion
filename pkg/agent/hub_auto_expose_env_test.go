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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

// envValue returns the value of key in a KEY=VALUE env list.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// TestBuildAgentEnv_HubAutoExposeDefaultIsLowestTier pins the broker half of
// the auto-expose precedence (settings-precedence.md B1): the hub default,
// carried in api.HubAgentDefaults, only fills SCION_AUTO_EXPOSE_PORTS when no
// higher tier set it. extraEnv stands for opts.Env (hub-resolved
// AppliedConfig.Env: explicit, project and template tiers, plus harness-config
// env injected by resolveAuthEnvOverlay); scionCfg.Env stands for the
// scion-agent.json layer (template chain, harness-config, inline).
func TestBuildAgentEnv_HubAutoExposeDefaultIsLowestTier(t *testing.T) {
	const ae = api.EnvAutoExposePorts
	cases := []struct {
		name      string
		scionEnv  map[string]string
		extraEnv  map[string]string
		hubAE     *bool
		want      string
		wantFound bool
	}{
		{
			// AC1: template sets false (lands in AppliedConfig.Env, so opts.Env),
			// hub default true.
			name:     "AC1 template false beats hub default true",
			extraEnv: map[string]string{ae: "false"},
			hubAE:    boolPtr(true), want: "false", wantFound: true,
		},
		{
			// AC1 via the scion-agent.json template layer instead of opts.Env.
			name:     "AC1 template layer in scion-agent.json beats hub default",
			scionEnv: map[string]string{ae: "false"},
			hubAE:    boolPtr(true), want: "false", wantFound: true,
		},
		{
			// AC2/AC3: project annotation or explicit value, resolved into
			// AppliedConfig.Env by the hub.
			name:     "AC2/AC3 hub-resolved value beats hub default",
			extraEnv: map[string]string{ae: "true"},
			hubAE:    boolPtr(false), want: "true", wantFound: true,
		},
		{
			// AC4: harness-config env (e.g. Hermes) reaches the container
			// through the scion-agent.json layer.
			name:     "AC4 harness-config env beats hub default false",
			scionEnv: map[string]string{ae: "true"},
			hubAE:    boolPtr(false), want: "true", wantFound: true,
		},
		{
			name:  "AC5 hub default true applies when nothing else sets it",
			hubAE: boolPtr(true), want: "true", wantFound: true,
		},
		{
			name:  "AC5 hub default false applies when nothing else sets it",
			hubAE: boolPtr(false), want: "false", wantFound: true,
		},
		{
			name:      "no hub default adds nothing",
			wantFound: false,
		},
		{
			// An empty value would be omitted from the container, so it does
			// not count as set.
			name:     "empty value is filled by the hub default",
			extraEnv: map[string]string{ae: ""},
			hubAE:    boolPtr(true), want: "true", wantFound: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ae, "") // no host passthrough for the empty-value case
			var cfg *api.ScionConfig
			if tc.scionEnv != nil {
				cfg = &api.ScionConfig{Env: tc.scionEnv}
			}
			hd := &api.HubAgentDefaults{AutoExposePorts: tc.hubAE}
			env, _, missing, _ := buildAgentEnv(cfg, tc.extraEnv, hd.DefaultEnv(), true)
			got, found := envValue(env, ae)
			if found != tc.wantFound || got != tc.want {
				t.Errorf("%s = %q (found=%v), want %q (found=%v); env=%v", ae, got, found, tc.want, tc.wantFound, env)
			}
			for _, k := range missing {
				if k == ae && tc.wantFound {
					t.Errorf("%s reported missing although the hub default filled it", ae)
				}
			}
		})
	}
}

// TestHubAutoExposeDefault_LosesToHarnessConfigEnvInOptsEnv is AC4 through the
// start sequence's own helper: resolveAuthEnvOverlay injects harness-config env
// into opts.Env (the path Hermes's SCION_AUTO_EXPOSE_PORTS=true takes), and the
// hub default false must not override it.
func TestHubAutoExposeDefault_LosesToHarnessConfigEnvInOptsEnv(t *testing.T) {
	settings := g3TestSettings(map[string]string{api.EnvAutoExposePorts: "true"})
	opts := api.StartOptions{Name: "hermes-agent", BrokerMode: true, Env: map[string]string{}}
	_, _ = resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	hd := &api.HubAgentDefaults{AutoExposePorts: boolPtr(false)}
	env, _, _, _ := buildAgentEnv(nil, opts.Env, hd.DefaultEnv(), true)
	if got, _ := envValue(env, api.EnvAutoExposePorts); got != "true" {
		t.Errorf("%s = %q, want harness-config value %q over hub default false", api.EnvAutoExposePorts, got, "true")
	}
}

func TestHubAgentDefaults_DefaultEnv(t *testing.T) {
	var nilDefaults *api.HubAgentDefaults
	if got := nilDefaults.DefaultEnv(); got != nil {
		t.Errorf("nil defaults: want nil env, got %v", got)
	}
	if got := (&api.HubAgentDefaults{MaxTurns: 5}).DefaultEnv(); got != nil {
		t.Errorf("no auto-expose default: want nil env, got %v", got)
	}
	got := (&api.HubAgentDefaults{AutoExposePorts: boolPtr(true)}).DefaultEnv()
	if len(got) != 1 || got[api.EnvAutoExposePorts] != "true" {
		t.Errorf("want {%s: true}, got %v", api.EnvAutoExposePorts, got)
	}
	if (&api.HubAgentDefaults{AutoExposePorts: boolPtr(false)}).IsEmpty() {
		t.Error("a defaults value carrying only AutoExposePorts must not be empty")
	}
}

// TestStart_AppliesHubAutoExposeDefaultFromContext pins the join between the
// start context and buildAgentEnv: Manager.Start reads the hub default from
// api.HubAgentDefaults on ctx (put there by the broker's start, restart and
// create handlers) and applies it below the scion-agent.json layer.
func TestStart_AppliesHubAutoExposeDefaultFromContext(t *testing.T) {
	cases := []struct {
		name     string
		agentEnv string // scion-agent.json env block
		hubAE    bool
		want     string
	}{
		{"hub default applies when nothing sets the key", `{"GOOD_KEY": "v"}`, true, "true"},
		{"scion-agent.json value beats hub default", `{"` + api.EnvAutoExposePorts + `": "true"}`, false, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(api.EnvAutoExposePorts, "")
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
			agentDir := filepath.Join(projectScionDir, "agents", "ae-test")
			require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
				[]byte(`{"harness": "generic", "env": `+tc.agentEnv+`}`), 0644))

			var capturedEnv []string
			mockRT := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, config runtime.RunConfig) (string, error) {
					capturedEnv = config.Env
					return "mock-id", nil
				},
			}
			ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{AutoExposePorts: boolPtr(tc.hubAE)})
			_, err := NewManager(mockRT).Start(ctx, api.StartOptions{
				Name: "ae-test", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true,
				Env: map[string]string{"FROM_HUB": "x"},
			})
			require.NoError(t, err)
			got, _ := envValue(capturedEnv, api.EnvAutoExposePorts)
			if got != tc.want {
				t.Errorf("%s = %q, want %q", api.EnvAutoExposePorts, got, tc.want)
			}
		})
	}
}
